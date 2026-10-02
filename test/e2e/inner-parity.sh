#!/bin/sh
# e2e suite for systemd-parity features. Builds one extension image
# (squashfs, ext4 fallback) and exercises:
#   1. /etc/systemd/sysext.conf + conf.d drop-ins (Mutable=) and flag priority
#   2. SYSTEMD_SYSEXT_HIERARCHIES environment variable (valid; invalid is an
#      error, like in systemd)
#   3. Locking: concurrent merges, a held lock blocks merge but not status,
#      parallel refreshes leave one overlay
#   4. SYSEXT_SCOPE: extensions for another scope are ignored
#   5. EXTENSION_RELOAD_MANAGER without a running OpenRC, --no-reload
#   6. status output shape
. /work/test/e2e/lib.sh

echo "=== Installing build dependencies ==="
pkg_add squashfs-tools e2fsprogs kmod openrc jq
install_sysext
need_overlayfs
mkdir -p /var/lib/extensions

ROUTING=/var/lib/extensions.mutable
MARKER=/usr/.systemd-sysext
CONF=/etc/systemd/sysext.conf
CONFD=/etc/systemd/sysext.conf.d

# The container's /var sits on docker's overlay2 filesystem, and the kernel
# rejects an overlayfs upperdir that itself lives on overlayfs (EINVAL).
# Emulate a real host by mounting a tmpfs over the routing base so any
# mutable-mode behaviour is testable.
mkdir -p "$ROUTING"
mount -t tmpfs -o mode=0755 tmpfs "$ROUTING" || fatal "cannot mount tmpfs over $ROUTING"
track_mount "$ROUTING"

# clean_configs — remove every sysext.conf / drop-in left behind by a test.
clean_configs() {
    rm -f "$CONF"
    rm -rf "$CONFD"
}

# Pick an image filesystem once: squashfs preferred, ext4 fallback.
if fs_supported squashfs; then
    FSTYPE=squashfs
elif fs_supported ext4; then
    FSTYPE=ext4
    skip fs:squashfs "squashfs not supported, using ext4 images"
else
    skip fs:squashfs "neither squashfs nor ext4 supported; skipping all parity tests"
    finish
fi
echo "=== Using image filesystem: $FSTYPE ==="

# make_tree NAME DIR RELEASE — sysext payload for NAME with RELEASE as its
# extension-release contents.
make_tree() {
    mk_tree "$2" "$1" "$3"
}

# build_image NAME DIR — build /var/lib/extensions/NAME.raw from DIR.
build_image() {
    if [ "$FSTYPE" = squashfs ]; then
        img_squashfs "$2" "/var/lib/extensions/$1.raw"
    else
        img_ext4 "$2" "/var/lib/extensions/$1.raw" 8
    fi
}

echo "=== Building test image ==="
EXT=test-parity
make_tree "$EXT" "$WORKDIR/$EXT" 'ID=_any\nARCHITECTURE=_any\n'
build_image "$EXT" "$WORKDIR/$EXT" || fatal "could not build $EXT.raw"

# merged_payload — sanity check that the test-parity extension is merged.
merged_payload() { [ -f "/usr/share/$EXT/hello.txt" ]; }

echo "=== Running tests ==="

# ---------------------------------------------------------------------------
# 1. sysext.conf: Mutable= from main config, conf.d drop-in override, and
#    command-line flag priority over config.
# ---------------------------------------------------------------------------
echo "--- 1. sysext.conf / conf.d / flag priority ---"

# 1a. Mutable=ephemeral via main config; no flag → writable, nothing routed.
mkdir -p /etc/systemd "$ROUTING/usr"
printf '[SysExt]\nMutable=ephemeral\n' > "$CONF"
if sysext merge; then
    pass "merge with sysext.conf Mutable=ephemeral"
else
    fail "merge with sysext.conf Mutable=ephemeral"
fi
merged_payload && pass "payload merged (config ephemeral)" \
    || fail "payload missing (config ephemeral)"
if touch /usr/parity-test-file 2>/dev/null; then
    pass "/usr writable via config Mutable=ephemeral"
else
    fail "/usr not writable via config Mutable=ephemeral"
fi
if [ -e "$ROUTING/usr/parity-test-file" ]; then
    fail "ephemeral write leaked into $ROUTING/usr"
else
    pass "ephemeral write not present in $ROUTING/usr"
fi
sysext unmerge || fail "unmerge after config ephemeral"

# 1b. conf.d drop-in Mutable=no overrides the main config → read-only.
mkdir -p "$CONFD"
printf '[SysExt]\nMutable=no\n' > "$CONFD/99-no.conf"
if sysext merge; then
    pass "merge with drop-in Mutable=no"
else
    fail "merge with drop-in Mutable=no"
fi
if touch /usr/parity-test-file 2>/dev/null; then
    fail "/usr writable but drop-in Mutable=no must win"
    rm -f /usr/parity-test-file
else
    pass "/usr read-only via drop-in Mutable=no"
fi
sysext unmerge || fail "unmerge after drop-in Mutable=no"
clean_configs

# 1c. Config Mutable=ephemeral + explicit --mutable=no → flag wins.
printf '[SysExt]\nMutable=ephemeral\n' > "$CONF"
if sysext merge --mutable=no; then
    pass "merge --mutable=no with config Mutable=ephemeral"
else
    fail "merge --mutable=no with config Mutable=ephemeral"
fi
if touch /usr/parity-test-file 2>/dev/null; then
    fail "/usr writable but --mutable=no flag must override config"
    rm -f /usr/parity-test-file
else
    pass "flag --mutable=no overrides config ephemeral (read-only)"
fi
sysext unmerge || fail "unmerge after flag-over-config test"
clean_configs

# ---------------------------------------------------------------------------
# 2. SYSTEMD_SYSEXT_HIERARCHIES environment variable
# ---------------------------------------------------------------------------
echo "--- 2. SYSTEMD_SYSEXT_HIERARCHIES ---"

# 2a. Restrict to /usr only: /opt must not become a mount point.
if SYSTEMD_SYSEXT_HIERARCHIES=/usr sysext merge; then
    pass "merge with SYSTEMD_SYSEXT_HIERARCHIES=/usr"
else
    fail "merge with SYSTEMD_SYSEXT_HIERARCHIES=/usr"
fi
merged_payload && pass "/usr merged under restricted hierarchies" \
    || fail "/usr not merged under restricted hierarchies"
if mountpoint -q /opt 2>/dev/null; then
    fail "/opt is a mount point but hierarchies were restricted to /usr"
else
    pass "/opt not a mount point under SYSTEMD_SYSEXT_HIERARCHIES=/usr"
fi
if [ -e /opt/.systemd-sysext ]; then
    fail "/opt/.systemd-sysext exists but /opt must be untouched"
else
    pass "no .systemd-sysext marker in /opt"
fi
if SYSTEMD_SYSEXT_HIERARCHIES=/usr sysext unmerge; then
    pass "unmerge with SYSTEMD_SYSEXT_HIERARCHIES=/usr"
else
    fail "unmerge with SYSTEMD_SYSEXT_HIERARCHIES=/usr"
fi

# 2b. Invalid (relative) value: an error, nothing is merged.
out=$(SYSTEMD_SYSEXT_HIERARCHIES=relative/path sysext merge 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && echo "$out" | grep -q "Failed to determine sysext hierarchies"; then
    pass "merge with an invalid SYSTEMD_SYSEXT_HIERARCHIES fails"
else
    fail "merge with an invalid SYSTEMD_SYSEXT_HIERARCHIES: rc=$rc $out"
    sysext unmerge >/dev/null 2>&1
fi
if mountpoint -q /usr 2>/dev/null; then
    fail "/usr merged despite an invalid hierarchy list"
else
    pass "nothing merged with an invalid hierarchy list"
fi

# ---------------------------------------------------------------------------
# 3. Locking: two concurrent merges must not corrupt state or wedge.
# ---------------------------------------------------------------------------
echo "--- 3. concurrent merge locking ---"

timeout 30 sysext merge >"$WORKDIR/merge1.log" 2>&1 &
pid1=$!
timeout 30 sysext merge >"$WORKDIR/merge2.log" 2>&1 &
pid2=$!
wait "$pid1"; rc1=$?
wait "$pid2"; rc2=$?

if [ "$rc1" = 124 ] || [ "$rc2" = 124 ]; then
    fail "a concurrent merge wedged (killed by timeout: rc1=$rc1 rc2=$rc2)"
else
    pass "both concurrent merges finished without wedging"
fi
if [ "$rc1" = 0 ] && [ "$rc2" = 0 ]; then
    fail "both concurrent merges succeeded; exactly one must"
elif [ "$rc1" = 0 ] || [ "$rc2" = 0 ]; then
    pass "exactly one concurrent merge succeeded (rc1=$rc1 rc2=$rc2)"
else
    fail "no concurrent merge succeeded (rc1=$rc1 rc2=$rc2)"
    cat "$WORKDIR/merge1.log" "$WORKDIR/merge2.log"
fi
merged_payload && pass "payload merged after concurrent merges" \
    || fail "payload missing after concurrent merges"
if [ -f "$MARKER/extensions" ]; then
    count=$(grep -c "$EXT" "$MARKER/extensions" 2>/dev/null) || count=0
    if [ "$count" = 1 ]; then
        pass "marker extensions file lists $EXT exactly once"
    else
        fail "marker extensions file lists $EXT $count times (want 1)"
    fi
else
    fail "marker extensions file missing after concurrent merges"
fi
sysext unmerge || fail "unmerge after concurrency test"

# A held lock blocks merge, but neither status nor the other class.
mkdir -p /run/systemd
(flock -x 9; sleep 3) 9>/run/systemd/sysext.lock &
lockpid=$!
sleep 0.5
timeout 1 sysext merge >/dev/null 2>&1
rc=$?
if [ "$rc" = 124 ] || [ "$rc" = 143 ]; then
    pass "merge waits while the lock is held"
else
    fail "merge did not wait for the lock (rc=$rc)"
fi
timeout 1 sysext status >/dev/null 2>&1 && pass "status does not take the lock" \
    || fail "status blocked by the lock"
timeout 1 sysext --confext status >/dev/null 2>&1 && pass "confext not blocked by the sysext lock" \
    || fail "confext blocked by the sysext lock"
wait "$lockpid"
sysext merge >/dev/null 2>&1 && pass "merge after the lock is released" || fail "merge after the lock is released"
for _ in 1 2 3 4; do
    sysext refresh --always-refresh=yes >/dev/null 2>&1 &
done
wait
count=$(awk '$5 == "/usr" && / - overlay /' /proc/self/mountinfo | wc -l)
if [ "$count" = 1 ]; then
    pass "parallel refreshes leave exactly one overlay on /usr"
else
    fail "parallel refreshes left $count overlays on /usr"
fi
merged_payload && pass "payload merged after parallel refreshes" || fail "payload missing after parallel refreshes"
sysext unmerge || fail "unmerge after parallel refreshes"

# ---------------------------------------------------------------------------
# 4. SYSEXT_SCOPE enforcement
# ---------------------------------------------------------------------------
echo "--- 4. SYSEXT_SCOPE enforcement ---"

SCOPE_EXT=test-scope
make_tree "$SCOPE_EXT" "$WORKDIR/$SCOPE_EXT" 'ID=_any\nSYSEXT_SCOPE=initrd\n'
if build_image "$SCOPE_EXT" "$WORKDIR/$SCOPE_EXT"; then
    pass "built $SCOPE_EXT.raw (SYSEXT_SCOPE=initrd)"
else
    fail "build $SCOPE_EXT.raw"
fi
out=$(sysext merge 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
    pass "merge ignores the initrd-only scope extension"
else
    fail "merge with an initrd-only scope extension failed (output: $out)"
fi
if [ -f "/usr/share/$SCOPE_EXT/hello.txt" ]; then
    fail "$SCOPE_EXT merged although its scope excludes system"
else
    pass "initrd-only scope extension not merged"
fi
merged_payload && pass "compatible extension merged next to it" || fail "compatible extension missing"
sysext unmerge >/dev/null 2>&1 || true
mv "/var/lib/extensions/$EXT.raw" "$WORKDIR/$EXT.raw"
out=$(sysext merge 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && [ "$out" = "No suitable extensions found (1 ignored due to incompatible image(s))." ]; then
    pass "only an initrd-only scope extension: nothing merged"
else
    fail "only an initrd-only scope extension: rc=$rc $out"
    sysext unmerge >/dev/null 2>&1
fi
mv "$WORKDIR/$EXT.raw" "/var/lib/extensions/$EXT.raw"

# Same extension, scope includes "system": merge must succeed.
make_tree "$SCOPE_EXT" "$WORKDIR/$SCOPE_EXT" 'ID=_any\nSYSEXT_SCOPE=system initrd\n'
if build_image "$SCOPE_EXT" "$WORKDIR/$SCOPE_EXT"; then
    pass "rebuilt $SCOPE_EXT.raw (SYSEXT_SCOPE=system initrd)"
else
    fail "rebuild $SCOPE_EXT.raw"
fi
if sysext merge; then
    pass "merge succeeds with SYSEXT_SCOPE=\"system initrd\""
else
    fail "merge fails with SYSEXT_SCOPE=\"system initrd\""
fi
if [ -f "/usr/share/$SCOPE_EXT/hello.txt" ]; then
    pass "scoped extension payload merged"
else
    fail "scoped extension payload missing"
fi
sysext unmerge || fail "unmerge after scope test"
rm -f "/var/lib/extensions/$SCOPE_EXT.raw"

# ---------------------------------------------------------------------------
# 5. EXTENSION_RELOAD_MANAGER without a running OpenRC (inner-openrc.sh
#    covers the service manager itself)
# ---------------------------------------------------------------------------
echo "--- 5. EXTENSION_RELOAD_MANAGER without OpenRC ---"

RELOAD_EXT=test-reload
make_tree "$RELOAD_EXT" "$WORKDIR/$RELOAD_EXT" 'ID=_any\nEXTENSION_RELOAD_MANAGER=yes\nEXTENSION_RESTART_UNITS=foo.service\n'
if build_image "$RELOAD_EXT" "$WORKDIR/$RELOAD_EXT"; then
    pass "built $RELOAD_EXT.raw (EXTENSION_RELOAD_MANAGER=yes)"
else
    fail "build $RELOAD_EXT.raw"
fi
mkdir -p /run/openrc   # OpenRC installed, but it did not boot this container
out=$(sysext merge 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && [ "$out" = "Using extensions '$EXT.raw', '$RELOAD_EXT.raw'.
Merged extensions into '/usr'." ]; then
    pass "merge with EXTENSION_RELOAD_MANAGER=yes skips the absent service manager silently"
else
    fail "merge with EXTENSION_RELOAD_MANAGER=yes: rc=$rc $out"
fi
if [ -f "/usr/share/$RELOAD_EXT/hello.txt" ]; then
    pass "reload extension payload merged"
else
    fail "reload extension payload missing"
fi
out=$(SYSTEMD_LOG_LEVEL=debug sysext unmerge 2>&1)
if echo "$out" | grep -qx "OpenRC is not running, not reloading the service manager."; then
    pass "unmerge notes the absent service manager at debug level"
else
    fail "unmerge debug output: $out"
fi
out=$(sysext merge --no-reload 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && ! echo "$out" | grep -q -e Debug -e OpenRC -e no-reload; then
    pass "merge --no-reload succeeds quietly"
else
    fail "merge --no-reload: rc=$rc $out"
fi
sysext unmerge || fail "unmerge after --no-reload merge"
rm -f "/var/lib/extensions/$RELOAD_EXT.raw"

# ---------------------------------------------------------------------------
# 6. status output shape (systemd 262: extensions is always an array)
# ---------------------------------------------------------------------------
echo "--- 6. status --json=short shape ---"

if sysext merge 2>/dev/null; then
    pass "merge before json status check"
else
    fail "merge before json status check"
fi
out=$(sysext status --json=short)
rc=$?
if [ "$rc" -eq 0 ] && echo "$out" | jq -e '.[] | select(.hierarchy == "/usr") | (.extensions == ["'"$EXT"'"]) and (.since | type == "number")' >/dev/null; then
    pass "merged json: extensions array, since in usec"
else
    fail "merged json (rc=$rc): $out"
fi
expect_keys=$(echo "$out" | jq -c '[.[] | keys] | unique')
if [ "$expect_keys" = '[["extensions","hierarchy","since"]]' ]; then
    pass "json objects carry exactly hierarchy, extensions, since"
else
    fail "json keys: $expect_keys"
fi
if [ "$(sysext status --no-legend | awk '$1 == "/usr" {print $2}')" = "$EXT" ]; then
    pass "table lists the merged extension"
else
    fail "table: $(sysext status)"
fi
sysext unmerge 2>/dev/null || fail "unmerge before unmerged json check"

out=$(sysext status --json=short)
if [ "$out" = '[{"hierarchy":"/opt","extensions":[],"since":null},{"hierarchy":"/usr","extensions":[],"since":null}]' ]; then
    pass "unmerged json matches systemd 262"
else
    fail "unmerged json: $out"
fi
out=$(sysext status)
if [ "$out" = "HIERARCHY EXTENSIONS SINCE
/opt      -          -
/usr      -          -" ]; then
    pass "unmerged table shows '-'"
else
    fail "unmerged table: $out"
fi
if rmdir /opt 2>/dev/null; then
    out=$(sysext status --json=short | jq -r '.[].hierarchy')
    mkdir /opt
    if [ "$out" = /usr ]; then
        pass "a missing hierarchy is left out of status"
    else
        fail "status with /opt missing: $out"
    fi
else
    skip opt-removable "/opt not removable"
fi

clean_configs
finish

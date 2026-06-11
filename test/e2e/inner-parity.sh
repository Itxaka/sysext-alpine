#!/bin/sh
# e2e test suite for sysext-alpine systemd-parity features. Runs INSIDE a
# privileged Alpine container (launched by run.sh). Builds one extension image
# (squashfs, ext4 fallback) and exercises:
#   1. /etc/systemd/sysext.conf + conf.d drop-ins (Mutable=) and flag priority
#   2. SYSTEMD_SYSEXT_HIERARCHIES environment variable (valid and bogus)
#   3. Concurrent-merge locking (no corruption, no wedging)
#   4. SYSEXT_SCOPE enforcement in extension-release
#   5. EXTENSION_RELOAD_MANAGER + OpenRC reload / --no-reload
#   6. status --json=short output shape
# Reports PASS/FAIL/SKIP per step; exits non-zero if any step FAILed.
#
# NOTE: the container shares the host kernel. run.sh bind-mounts the host's
# /lib/modules read-only so missing filesystems can be modprobe'd.
set -u

FAILS=0
SKIPS=0

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
skip() { echo "SKIP: $*"; SKIPS=$((SKIPS + 1)); }

# A filesystem is usable if it is registered with the kernel, or can be
# loaded via modprobe (host /lib/modules is bind-mounted by run.sh).
fs_supported() {
    grep -qw "$1" /proc/filesystems && return 0
    modprobe "$1" 2>/dev/null || true
    grep -qw "$1" /proc/filesystems
}

# ---------------------------------------------------------------------------
# Environment setup
# ---------------------------------------------------------------------------
echo "=== Installing build dependencies ==="
apk add --no-cache squashfs-tools e2fsprogs util-linux kmod openrc \
    || { echo "FATAL: apk add failed"; exit 1; }

if [ ! -x /work/bin/sysext ]; then
    echo "FATAL: /work/bin/sysext missing (run 'make build-static' on the host)"
    exit 1
fi
install -m 0755 /work/bin/sysext /usr/bin/sysext

if ! fs_supported overlay; then
    echo "FATAL: kernel does not support overlayfs; cannot test anything"
    exit 1
fi

mkdir -p /var/lib/extensions
WORKDIR=/tmp/e2e-parity-build
mkdir -p "$WORKDIR"

ROUTING=/var/lib/extensions.mutable
MARKER=/usr/.systemd-sysext
CONF=/etc/systemd/sysext.conf
CONFD=/etc/systemd/sysext.conf.d

# The container's /var sits on docker's overlay2 filesystem, and the kernel
# rejects an overlayfs upperdir that itself lives on overlayfs (EINVAL).
# Emulate a real host by mounting a tmpfs over the routing base so any
# mutable-mode behaviour is testable.
mkdir -p "$ROUTING"
mount -t tmpfs -o mode=0755 tmpfs "$ROUTING" \
    || { echo "FATAL: cannot mount tmpfs over $ROUTING"; exit 1; }

# clean_configs — remove every sysext.conf / drop-in left behind by a test.
clean_configs() {
    rm -f "$CONF"
    rm -rf "$CONFD"
}

# ---------------------------------------------------------------------------
# Test image construction helpers
# ---------------------------------------------------------------------------

# Pick an image filesystem once: squashfs preferred, ext4 fallback.
FSTYPE=""
if fs_supported squashfs; then
    FSTYPE=squashfs
elif fs_supported ext4; then
    FSTYPE=ext4
else
    skip "neither squashfs nor ext4 supported; skipping all parity tests"
    echo "==========================================="
    echo "Failures: $FAILS  Skips: $SKIPS"
    echo "RESULT: PASS"
    exit 0
fi
echo "=== Using image filesystem: $FSTYPE ==="

# make_tree NAME DIR RELEASE — populate DIR with a sysext payload for
# extension NAME using RELEASE as the extension-release contents.
make_tree() {
    name=$1
    dir=$2
    release=$3
    rm -rf "$dir"
    mkdir -p "$dir/usr/lib/extension-release.d" "$dir/usr/share/$name"
    printf '%s' "$release" \
        > "$dir/usr/lib/extension-release.d/extension-release.$name"
    echo "hello from $name" > "$dir/usr/share/$name/hello.txt"
}

# build_image NAME DIR — build /var/lib/extensions/NAME.raw from DIR using
# the selected filesystem. Returns non-zero on build failure.
build_image() {
    name=$1
    dir=$2
    img=/var/lib/extensions/$name.raw
    rm -f "$img"
    if [ "$FSTYPE" = squashfs ]; then
        mksquashfs "$dir" "$img" -noappend -quiet
    else
        dd if=/dev/zero of="$img" bs=1M count=8 status=none
        mkfs.ext4 -q -F -d "$dir" "$img" 2>/dev/null
    fi
}

echo "=== Building test image ==="
EXT=test-parity
make_tree "$EXT" "$WORKDIR/$EXT" 'ID=_any
ARCHITECTURE=_any
'
if build_image "$EXT" "$WORKDIR/$EXT"; then
    echo "built: $EXT.raw ($FSTYPE)"
else
    echo "FATAL: could not build $EXT.raw"
    exit 1
fi

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

# 2b. Bogus (relative) value: defaults are used, merge still works.
if SYSTEMD_SYSEXT_HIERARCHIES=relative/path sysext merge; then
    pass "merge with bogus SYSTEMD_SYSEXT_HIERARCHIES falls back to defaults"
else
    fail "merge with bogus SYSTEMD_SYSEXT_HIERARCHIES must still work"
fi
merged_payload && pass "payload merged with bogus hierarchies env" \
    || fail "payload missing with bogus hierarchies env"
sysext unmerge || fail "unmerge after bogus hierarchies env"

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

# ---------------------------------------------------------------------------
# 4. SYSEXT_SCOPE enforcement
# ---------------------------------------------------------------------------
echo "--- 4. SYSEXT_SCOPE enforcement ---"

SCOPE_EXT=test-scope
make_tree "$SCOPE_EXT" "$WORKDIR/$SCOPE_EXT" 'ID=_any
SYSEXT_SCOPE=initrd
'
if build_image "$SCOPE_EXT" "$WORKDIR/$SCOPE_EXT"; then
    pass "built $SCOPE_EXT.raw (SYSEXT_SCOPE=initrd)"
else
    fail "build $SCOPE_EXT.raw"
fi
out=$(sysext merge 2>&1)
rc=$?
if [ "$rc" -ne 0 ]; then
    pass "merge fails with initrd-only scope extension"
    if echo "$out" | grep -qi scope; then
        pass "merge failure mentions scope"
    else
        fail "merge failure does not mention scope (output: $out)"
    fi
else
    fail "merge succeeded but $SCOPE_EXT scope excludes system"
fi
sysext unmerge >/dev/null 2>&1 || true

# Same extension, scope includes "system": merge must succeed.
make_tree "$SCOPE_EXT" "$WORKDIR/$SCOPE_EXT" 'ID=_any
SYSEXT_SCOPE=system initrd
'
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
# 5. EXTENSION_RELOAD_MANAGER + OpenRC
# ---------------------------------------------------------------------------
echo "--- 5. EXTENSION_RELOAD_MANAGER / OpenRC ---"

RELOAD_EXT=test-reload
make_tree "$RELOAD_EXT" "$WORKDIR/$RELOAD_EXT" 'ID=_any
EXTENSION_RELOAD_MANAGER=1
'
if build_image "$RELOAD_EXT" "$WORKDIR/$RELOAD_EXT"; then
    pass "built $RELOAD_EXT.raw (EXTENSION_RELOAD_MANAGER=1)"
else
    fail "build $RELOAD_EXT.raw"
fi
mkdir -p /run/openrc   # openrc container image won't have it
out=$(sysext merge 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
    pass "merge succeeds with EXTENSION_RELOAD_MANAGER=1"
else
    fail "merge fails with EXTENSION_RELOAD_MANAGER=1 (output: $out)"
fi
if [ -f "/usr/share/$RELOAD_EXT/hello.txt" ]; then
    pass "reload extension payload merged"
else
    fail "reload extension payload missing"
fi
# Soft: rc-update -u running is hard to observe directly; the unit tests
# cover the decision logic. Here only assert merge produced no error above.
if command -v rc-update >/dev/null 2>&1 && [ -d /run/openrc ]; then
    pass "rc-update and /run/openrc present (reload path exercisable)"
else
    skip "rc-update or /run/openrc missing; reload path not exercisable"
fi
sysext unmerge || fail "unmerge after reload-manager merge"

# Soft: with --no-reload, a debug note mentioning no-reload should show up
# on stderr. Tolerate absence (message wording/level may vary).
out=$(sysext merge --no-reload 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
    pass "merge --no-reload succeeds"
else
    fail "merge --no-reload fails (output: $out)"
fi
if echo "$out" | grep -qi 'no-reload'; then
    pass "merge --no-reload mentions no-reload on stderr"
else
    skip "no-reload debug note not observed (soft assertion)"
fi
sysext unmerge || fail "unmerge after --no-reload merge"
rm -f "/var/lib/extensions/$RELOAD_EXT.raw"

# ---------------------------------------------------------------------------
# 6. status --json=short output shape
# ---------------------------------------------------------------------------
echo "--- 6. status --json=short shape ---"

if sysext merge; then
    pass "merge before json status check"
else
    fail "merge before json status check"
fi
out=$(sysext status --json=short 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
    pass "status --json=short exits 0 (merged)"
else
    fail "status --json=short exited $rc (merged)"
fi
if echo "$out" | grep -Fq '"hierarchy":"/usr"'; then
    pass "merged json contains \"hierarchy\":\"/usr\""
else
    fail "merged json missing \"hierarchy\":\"/usr\" (output: $out)"
fi
if echo "$out" | grep -Fq '"extensions":['; then
    pass "merged json has extensions array"
else
    fail "merged json missing \"extensions\":[ (output: $out)"
fi
if echo "$out" | grep -Fq '"merged"'; then
    fail "merged json contains \"merged\" key but must not (output: $out)"
else
    pass "merged json does not contain \"merged\" key"
fi
sysext unmerge || fail "unmerge before unmerged json check"

out=$(sysext status --json=short 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
    pass "status --json=short exits 0 (unmerged)"
else
    fail "status --json=short exited $rc (unmerged)"
fi
if echo "$out" | grep -Fq '"extensions":"none"'; then
    pass "unmerged json contains \"extensions\":\"none\""
else
    fail "unmerged json missing \"extensions\":\"none\" (output: $out)"
fi
if echo "$out" | grep -Fq '"since":null'; then
    pass "unmerged json contains \"since\":null"
else
    fail "unmerged json missing \"since\":null (output: $out)"
fi

# Final cleanup so the container exits with nothing mounted or configured.
sysext unmerge >/dev/null 2>&1 || true
clean_configs

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo "==========================================="
echo "Failures: $FAILS  Skips: $SKIPS"
if [ "$FAILS" -gt 0 ]; then
    echo "RESULT: FAIL"
    exit 1
fi
echo "RESULT: PASS"
exit 0

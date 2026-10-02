#!/bin/sh
# e2e suite for the --mutable= modes (no, auto, yes, import, ephemeral) with
# systemd-sysext 262 semantics: workdir naming and the work_dir marker,
# read-only metadata, the hierarchy mode, every hierarchy merged in mutable
# modes, merging without extensions, confext.
. /work/test/e2e/lib.sh

echo "=== Installing build dependencies ==="
pkg_add squashfs-tools e2fsprogs kmod
install_sysext
need_overlayfs

ROUTING=/var/lib/extensions.mutable
MARKER=/usr/.systemd-sysext

# The container's /var sits on docker's overlay2 filesystem, and the kernel
# rejects an overlayfs upperdir that itself lives on overlayfs (EINVAL). On a
# real Alpine host /var is a regular filesystem; emulate that with a tmpfs on
# /var/lib, so the routing base is an ordinary directory that the tests can
# remove and the persistent-upper modes are testable.
mount -t tmpfs -o mode=0755 tmpfs /var/lib || fatal "cannot mount tmpfs on /var/lib"
track_mount /var/lib
mkdir -p /var/lib/extensions "$ROUTING"

# Mutable modes merge every hierarchy. An overlay without an upper directory
# needs two layers, which the container's empty /opt does not provide (systemd
# fails the same way), so give /opt some host content like a real system.
mkdir -p /opt/host
echo host > /opt/host/f

echo "=== Building test image ==="
EXT=test-mutable
mk_tree "$WORKDIR/$EXT" "$EXT"
img=/var/lib/extensions/$EXT.raw
if fs_supported squashfs; then
    img_squashfs "$WORKDIR/$EXT" "$img" || fatal "could not build the squashfs test image"
elif fs_supported ext4; then
    skip fs:squashfs "squashfs not supported, using an ext4 image"
    img_ext4 "$WORKDIR/$EXT" "$img" 8 || fatal "could not build the ext4 test image"
else
    skip fs:squashfs "neither squashfs nor ext4 supported; skipping all mutable tests"
    finish
fi

# merged_payload — sanity check that the extension is actually merged.
merged_payload() { [ -f "/usr/share/$EXT/hello.txt" ]; }

echo "=== Running tests ==="

# ---------------------------------------------------------------------------
# 1. --mutable=no (default): /usr is read-only; routing dirs are IGNORED.
# ---------------------------------------------------------------------------
mkdir -p "$ROUTING/usr"   # present but must be ignored in "no" mode
if sysext merge --mutable=no; then
    pass "merge --mutable=no"
else
    fail "merge --mutable=no"
fi
merged_payload && pass "payload merged (no)" || fail "payload missing (no)"
if touch /usr/mutable-no-test 2>/dev/null; then
    fail "write to /usr succeeded under --mutable=no"
    rm -f /usr/mutable-no-test
else
    pass "/usr read-only under --mutable=no"
fi
if [ -e "$MARKER/work_dir" ]; then
    fail "work_dir marker present under --mutable=no"
else
    pass "no work_dir marker under --mutable=no"
fi
sysext unmerge || fail "unmerge after --mutable=no"

# ---------------------------------------------------------------------------
# 2. --mutable=auto with routing dir: writes land in the routing dir and
#    persist across unmerge/remerge.
# ---------------------------------------------------------------------------
mkdir -p "$ROUTING/usr"
if sysext merge --mutable=auto; then
    pass "merge --mutable=auto (routing dir present)"
else
    fail "merge --mutable=auto (routing dir present)"
fi
merged_payload && pass "payload merged (auto)" || fail "payload missing (auto)"
if echo "mutable data" > /usr/newfile 2>/dev/null; then
    pass "write to /usr succeeds under --mutable=auto"
else
    fail "write to /usr fails under --mutable=auto"
fi
if [ -f "$ROUTING/usr/newfile" ]; then
    pass "write routed to $ROUTING/usr/newfile"
else
    fail "write not routed to $ROUTING/usr/newfile"
fi
wd=$(cat "$MARKER/work_dir" 2>/dev/null)
if [ "$wd" = var/lib/extensions.mutable/.systemd-usr-workdir ]; then
    pass "work_dir marker records the workdir relative to the root"
else
    fail "work_dir marker: '$wd'"
fi
if [ -d "$ROUTING/.systemd-usr-workdir" ]; then
    pass "workdir $ROUTING/.systemd-usr-workdir exists"
else
    fail "workdir $ROUTING/.systemd-usr-workdir missing"
fi
if echo 1 > "$MARKER/dev" 2>/dev/null; then
    fail "dev marker writable in mutable mode"
else
    pass "dev marker read-only in mutable mode"
fi
if touch "$MARKER/x" 2>/dev/null; then
    fail "metadata directory writable in mutable mode"
else
    pass "metadata directory read-only in mutable mode"
fi
if [ -e "$ROUTING/usr/.systemd-sysext" ]; then
    fail "metadata copied up into the mutable directory"
else
    pass "mutable directory free of metadata"
fi
if touch /opt/auto-ro-test 2>/dev/null; then
    fail "/opt writable under auto without its mutable directory"
    rm -f /opt/auto-ro-test
else
    pass "/opt read-only under auto without its mutable directory"
fi
sysext unmerge || fail "unmerge after --mutable=auto"
if [ -e /usr/newfile ]; then
    fail "/usr/newfile still present after unmerge"
else
    pass "/usr/newfile gone from /usr after unmerge"
fi
if [ -f "$ROUTING/usr/newfile" ]; then
    pass "newfile persists in routing dir after unmerge"
else
    fail "newfile lost from routing dir after unmerge"
fi
if [ -e "$ROUTING/.systemd-usr-workdir" ]; then
    fail "workdir not removed on unmerge"
else
    pass "workdir removed on unmerge"
fi
if awk '$5 == "/usr" && / - overlay /' /proc/self/mountinfo | grep -q .; then
    fail "overlay left on /usr after unmerge"
else
    pass "no overlay left on /usr after unmerge"
fi

# Remerge: persisted upper content must be visible again.
if sysext merge --mutable=auto; then
    pass "remerge --mutable=auto"
else
    fail "remerge --mutable=auto"
fi
if [ -f /usr/newfile ]; then
    pass "newfile visible again after remerge (upper persists)"
else
    fail "newfile not visible after remerge"
fi
sysext unmerge || fail "unmerge after remerge"
rm -f "$ROUTING/usr/newfile"

# ---------------------------------------------------------------------------
# 3. --mutable=auto without routing dir: hierarchy stays read-only.
# ---------------------------------------------------------------------------
rm -rf "$ROUTING"
[ -e "$ROUTING" ] && fail "could not remove $ROUTING for the absent case"
if sysext merge --mutable=auto; then
    pass "merge --mutable=auto (no routing dir)"
else
    fail "merge --mutable=auto (no routing dir)"
fi
if touch /usr/auto-ro-test 2>/dev/null; then
    fail "write succeeded under auto without routing dir"
    rm -f /usr/auto-ro-test
else
    pass "/usr read-only under auto without routing dir"
fi
if [ -e "$ROUTING" ]; then
    fail "auto created the routing dir but must not"
else
    pass "auto did not create the routing dir"
fi
sysext unmerge || fail "unmerge after auto (no routing dir)"

# ---------------------------------------------------------------------------
# 4. --mutable=ephemeral: writable, but changes vanish on unmerge and never
#    touch the routing dir.
# ---------------------------------------------------------------------------
mkdir -p "$ROUTING/usr"   # exists, but ephemeral must ignore it
if sysext merge --mutable=ephemeral; then
    pass "merge --mutable=ephemeral"
else
    fail "merge --mutable=ephemeral"
fi
if echo "ephemeral data" > /usr/ephfile 2>/dev/null; then
    pass "write to /usr succeeds under --mutable=ephemeral"
else
    fail "write to /usr fails under --mutable=ephemeral"
fi
if [ -e "$ROUTING/usr/ephfile" ]; then
    fail "ephemeral write leaked into routing dir"
else
    pass "ephemeral write not routed to routing dir"
fi
if [ -e "$MARKER/work_dir" ]; then
    fail "work_dir marker written under ephemeral"
else
    pass "no work_dir marker under ephemeral"
fi
sysext unmerge || fail "unmerge after --mutable=ephemeral"
if [ -e "$ROUTING/usr/ephfile" ]; then
    fail "ephfile present in routing dir after ephemeral unmerge"
else
    pass "routing dir clean after ephemeral unmerge"
fi
if sysext merge --mutable=ephemeral; then
    pass "remerge --mutable=ephemeral"
else
    fail "remerge --mutable=ephemeral"
fi
if [ -e /usr/ephfile ]; then
    fail "ephemeral change survived unmerge"
else
    pass "ephemeral change gone after remerge"
fi
sysext unmerge || fail "unmerge after ephemeral remerge"

# ---------------------------------------------------------------------------
# 5. --mutable=import: routing dir contents merged in, but /usr stays
#    read-only.
# ---------------------------------------------------------------------------
mkdir -p "$ROUTING/usr"
echo "seeded" > "$ROUTING/usr/imported.txt"
if sysext merge --mutable=import; then
    pass "merge --mutable=import"
else
    fail "merge --mutable=import"
fi
if [ -f /usr/imported.txt ]; then
    pass "routing dir content visible under import"
else
    fail "routing dir content not visible under import"
fi
if touch /usr/import-ro-test 2>/dev/null; then
    fail "write succeeded under --mutable=import"
    rm -f /usr/import-ro-test
else
    pass "/usr read-only under --mutable=import"
fi
if [ -e "$MARKER/work_dir" ]; then
    fail "work_dir marker present under import (read-only mode)"
else
    pass "no work_dir marker under import"
fi
sysext unmerge || fail "unmerge after --mutable=import"
rm -f "$ROUTING/usr/imported.txt"

# ---------------------------------------------------------------------------
# 6. --mutable=yes with routing dirs absent: dirs are created, writes work.
# ---------------------------------------------------------------------------
rm -rf "$ROUTING"
[ -e "$ROUTING" ] && fail "could not remove $ROUTING for the absent case"
if sysext merge --mutable=yes; then
    pass "merge --mutable=yes (routing dirs absent)"
else
    fail "merge --mutable=yes (routing dirs absent)"
fi
[ -d "$ROUTING" ] && pass "routing base directory created by --mutable=yes" || fail "routing base directory not created"
if [ -d "$ROUTING/usr" ]; then
    pass "routing dir created by --mutable=yes"
else
    fail "routing dir not created by --mutable=yes"
fi
if echo "yes data" > /usr/yesfile 2>/dev/null; then
    pass "write to /usr succeeds under --mutable=yes"
else
    fail "write to /usr fails under --mutable=yes"
fi
if [ -f "$ROUTING/usr/yesfile" ]; then
    pass "write routed to created routing dir"
else
    fail "write not routed to created routing dir"
fi
if [ -d "$ROUTING/opt" ] && echo opt > /opt/yesfile 2>/dev/null && [ -f "$ROUTING/opt/yesfile" ]; then
    pass "--mutable=yes makes /opt writable too"
else
    fail "--mutable=yes did not route /opt"
fi
sysext unmerge || fail "unmerge after --mutable=yes"
rm -f "$ROUTING/usr/yesfile" "$ROUTING/opt/yesfile"

# ---------------------------------------------------------------------------
# 6b. hierarchy mode: a strict umask does not leak into the merged /usr, a
#     mutable directory with another mode is refused.
# ---------------------------------------------------------------------------
rm -rf "${ROUTING:?}/usr" "${ROUTING:?}/opt"
(umask 077; sysext merge --mutable=yes) >/dev/null 2>&1 || fail "merge --mutable=yes under umask 077"
mode=$(stat -c %a /usr)
if [ "$mode" = 755 ] && [ "$(stat -c %a "$ROUTING/usr")" = 755 ]; then
    pass "merged /usr and its mutable directory keep mode 755 under umask 077"
else
    fail "mode under umask 077: /usr=$mode $ROUTING/usr=$(stat -c %a "$ROUTING/usr")"
fi
sysext unmerge >/dev/null 2>&1
chmod 0700 "$ROUTING/usr"
out=$(sysext merge --mutable=auto 2>&1)
if [ $? -ne 0 ] && echo "$out" | grep -q "ought to have mode 0755"; then
    pass "mutable directory with another mode refused"
else
    fail "mutable directory mode mismatch: $out"
    sysext unmerge >/dev/null 2>&1
fi
chmod 0755 "$ROUTING/usr"

# ---------------------------------------------------------------------------
# 6c. metadata left in the mutable directory by an older merge does not
#     shadow the markers.
# ---------------------------------------------------------------------------
mkdir -p "$ROUTING/usr/.systemd-sysext"
echo 1 > "$ROUTING/usr/.systemd-sysext/dev"
if sysext merge --mutable=yes >/dev/null 2>&1 && [ "$(cat "$MARKER/dev")" = "$(mountpoint -d /usr)" ]; then
    pass "stale metadata in the mutable directory ignored"
else
    fail "stale metadata in the mutable directory: $(cat "$MARKER/dev" 2>&1)"
fi
sysext unmerge >/dev/null 2>&1

# ---------------------------------------------------------------------------
# 6d. mutable modes merge without any extension.
# ---------------------------------------------------------------------------
mv "$img" "$WORKDIR/"
out=$(sysext merge --mutable=yes 2>&1)
if [ "$out" = "No extensions found, proceeding in mutable mode.
Merged extensions into '/usr'.
Merged extensions into '/opt'." ] && touch /usr/noext && [ -f "$ROUTING/usr/noext" ]; then
    pass "--mutable=yes merges without extensions"
else
    fail "--mutable=yes without extensions: $out"
fi
if [ -f "$MARKER/extensions" ] && [ ! -s "$MARKER/extensions" ]; then
    pass "empty extensions marker without extensions"
else
    fail "extensions marker without extensions: '$(cat "$MARKER/extensions" 2>&1)'"
fi
out=$(sysext refresh --mutable=yes 2>&1)
case "$out" in
    Skipping*) pass "refresh keeps the merge without extensions" ;;
    *) fail "refresh without extensions: $out" ;;
esac
sysext unmerge >/dev/null 2>&1
rm -f "$ROUTING/usr/noext"
mv "$WORKDIR/${img##*/}" "$img"

# ---------------------------------------------------------------------------
# 6e. confext: /etc writes routed to its mutable directory.
# ---------------------------------------------------------------------------
c=/var/lib/confexts/mc
mkdir -p "$c/etc/extension-release.d" "$c/etc/mc"
printf 'ID=_any\n' > "$c/etc/extension-release.d/extension-release.mc"
echo mc > "$c/etc/mc/f"
mkdir -p "$ROUTING/etc"
if sysext --confext merge --mutable=auto >/dev/null 2>&1 && echo c > /etc/mutable-test 2>/dev/null \
    && [ -f "$ROUTING/etc/mutable-test" ] && [ "$(cat /etc/mc/f)" = mc ]; then
    pass "confext --mutable=auto routes /etc writes"
else
    fail "confext --mutable=auto"
fi
sysext --confext unmerge >/dev/null 2>&1
rm -rf "$c" "${ROUTING:?}/etc"

# ---------------------------------------------------------------------------
# 7. invalid mode is rejected
# ---------------------------------------------------------------------------
if sysext merge --mutable=bogus 2>/dev/null; then
    fail "merge --mutable=bogus succeeded but must fail"
    sysext unmerge
else
    pass "merge --mutable=bogus rejected"
fi

finish

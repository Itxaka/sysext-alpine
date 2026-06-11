#!/bin/sh
# e2e test suite for sysext-alpine --mutable= modes. Runs INSIDE a privileged
# Alpine container (launched by run.sh). Builds one extension image (squashfs,
# ext4 fallback) and exercises the mutable modes: no, auto, yes, import,
# ephemeral. Reports PASS/FAIL/SKIP per step; exits non-zero if any step
# FAILed.
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
apk add --no-cache squashfs-tools e2fsprogs util-linux kmod \
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
WORKDIR=/tmp/e2e-mutable-build
mkdir -p "$WORKDIR"

ROUTING=/var/lib/extensions.mutable
MARKER=/usr/.systemd-sysext

# The container's /var sits on docker's overlay2 filesystem, and the kernel
# rejects an overlayfs upperdir that itself lives on overlayfs (EINVAL).
# On a real Alpine host /var is a regular filesystem; emulate that with a
# tmpfs over the routing base so the persistent-upper modes are testable.
mkdir -p "$ROUTING"
mount -t tmpfs -o mode=0755 tmpfs "$ROUTING" \
    || { echo "FATAL: cannot mount tmpfs over $ROUTING"; exit 1; }

# ---------------------------------------------------------------------------
# Test image construction: one extension image, squashfs preferred, ext4
# fallback.
# ---------------------------------------------------------------------------
echo "=== Building test image ==="
EXT=test-mutable
tree=$WORKDIR/$EXT
mkdir -p "$tree/usr/lib/extension-release.d" "$tree/usr/share/$EXT"
printf 'ID=_any\nARCHITECTURE=_any\n' \
    > "$tree/usr/lib/extension-release.d/extension-release.$EXT"
echo "hello from $EXT" > "$tree/usr/share/$EXT/hello.txt"

img=/var/lib/extensions/$EXT.raw
if fs_supported squashfs && mksquashfs "$tree" "$img" -noappend -quiet; then
    echo "built: $EXT.raw (squashfs)"
elif fs_supported ext4; then
    dd if=/dev/zero of="$img" bs=1M count=8 status=none
    if mkfs.ext4 -q -F -d "$tree" "$img" 2>/dev/null; then
        echo "built: $EXT.raw (ext4)"
    else
        echo "FATAL: could not build ext4 test image"
        exit 1
    fi
else
    skip "neither squashfs nor ext4 supported; skipping all mutable tests"
    echo "==========================================="
    echo "Failures: $FAILS  Skips: $SKIPS"
    echo "RESULT: PASS"
    exit 0
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
if [ -f "$MARKER/work_dir" ]; then
    pass "work_dir marker present when mutable"
    wd=$(cat "$MARKER/work_dir")
    if [ -d "$wd" ]; then
        pass "work_dir marker points at existing workdir ($wd)"
    else
        fail "work_dir marker target missing ($wd)"
    fi
else
    fail "work_dir marker missing when mutable"
fi
if [ -d "$ROUTING/.usr-workdir" ]; then
    pass "hidden workdir sibling exists ($ROUTING/.usr-workdir)"
else
    fail "hidden workdir sibling missing"
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
if [ -e "$ROUTING/.usr-workdir" ]; then
    fail "workdir not removed on unmerge"
else
    pass "workdir removed on unmerge"
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
if [ -e "$ROUTING/usr" ]; then
    fail "auto created routing dir but must not"
else
    pass "auto did not create routing dir"
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
if [ -f "$MARKER/work_dir" ]; then
    pass "work_dir marker present under ephemeral"
else
    fail "work_dir marker missing under ephemeral"
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
if sysext merge --mutable=yes; then
    pass "merge --mutable=yes (routing dirs absent)"
else
    fail "merge --mutable=yes (routing dirs absent)"
fi
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
sysext unmerge || fail "unmerge after --mutable=yes"
rm -f "$ROUTING/usr/yesfile"

# ---------------------------------------------------------------------------
# 7. invalid mode is rejected
# ---------------------------------------------------------------------------
if sysext merge --mutable=bogus 2>/dev/null; then
    fail "merge --mutable=bogus succeeded but must fail"
    sysext unmerge || true
else
    pass "merge --mutable=bogus rejected"
fi

# Final cleanup so the container exits with nothing mounted.
sysext unmerge || true

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

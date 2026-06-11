#!/bin/sh
# e2e test suite for sysext-alpine. Runs INSIDE a privileged Alpine container
# (launched by run.sh). Builds test extension images in several formats,
# exercises merge/unmerge/refresh/status/list and masking, and reports
# PASS/FAIL/SKIP per step. Exits non-zero if any step FAILed.
#
# NOTE: the container shares the host kernel. run.sh bind-mounts the host's
# /lib/modules read-only so missing filesystems (squashfs/erofs/...) can be
# modprobe'd from inside the privileged container. If a filesystem still
# cannot be made available, the affected image type is SKIPped, not failed.
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
apk add --no-cache squashfs-tools e2fsprogs erofs-utils util-linux kmod sgdisk \
    || apk add --no-cache squashfs-tools e2fsprogs erofs-utils util-linux kmod \
    || { echo "FATAL: apk add failed"; exit 1; }

if [ ! -x /work/bin/sysext ]; then
    echo "FATAL: /work/bin/sysext missing (run 'make build-static' on the host)"
    exit 1
fi
install -m 0755 /work/bin/sysext /usr/bin/sysext
ln -sf sysext /usr/bin/confext

if ! fs_supported overlay; then
    echo "FATAL: kernel does not support overlayfs; cannot test anything"
    exit 1
fi

mkdir -p /var/lib/extensions /var/lib/confexts /etc/extensions
WORKDIR=/tmp/e2e-build
mkdir -p "$WORKDIR"

# ---------------------------------------------------------------------------
# Test image construction
# ---------------------------------------------------------------------------

# make_tree NAME DIR — populate DIR with a sysext payload for extension NAME:
# release file, a marker file and an executable tool.
make_tree() {
    name=$1
    dir=$2
    mkdir -p "$dir/usr/lib/extension-release.d" \
             "$dir/usr/share/$name" \
             "$dir/usr/bin"
    printf 'ID=_any\nARCHITECTURE=_any\n' \
        > "$dir/usr/lib/extension-release.d/extension-release.$name"
    echo "hello from $name" > "$dir/usr/share/$name/hello.txt"
    printf '#!/bin/sh\necho %s-tool\n' "$name" > "$dir/usr/bin/$name-tool"
    chmod 0755 "$dir/usr/bin/$name-tool"
}

# Names of raw sysext images that were actually built (fs supported).
BUILT=""

built() {
    case " $BUILT " in
        *" $1 "*) return 0 ;;
        *) return 1 ;;
    esac
}

echo "=== Building test images ==="

# --- squashfs ---------------------------------------------------------------
if fs_supported squashfs; then
    tree=$WORKDIR/test-squashfs
    make_tree test-squashfs "$tree"
    if mksquashfs "$tree" /var/lib/extensions/test-squashfs.raw -noappend -quiet; then
        BUILT="$BUILT test-squashfs"
        echo "built: test-squashfs.raw (squashfs)"
    else
        fail "build test-squashfs.raw"
    fi
else
    skip "squashfs not supported by host kernel; skipping test-squashfs"
fi

# --- ext4 (bare filesystem image) -------------------------------------------
if fs_supported ext4; then
    tree=$WORKDIR/test-ext4
    make_tree test-ext4 "$tree"
    img=/var/lib/extensions/test-ext4.raw
    dd if=/dev/zero of="$img" bs=1M count=8 status=none
    if mkfs.ext4 -q -F -d "$tree" "$img" 2>/dev/null; then
        BUILT="$BUILT test-ext4"
        echo "built: test-ext4.raw (ext4, mkfs -d)"
    else
        # Older mkfs.ext4 without -d: format then loop-mount and copy.
        mkfs.ext4 -q -F "$img"
        mnt=$WORKDIR/mnt-ext4
        mkdir -p "$mnt"
        if mount -o loop "$img" "$mnt"; then
            cp -a "$tree"/. "$mnt"/
            umount "$mnt"
            BUILT="$BUILT test-ext4"
            echo "built: test-ext4.raw (ext4, loop copy)"
        else
            fail "build test-ext4.raw (loop mount)"
            rm -f "$img"
        fi
    fi
else
    skip "ext4 not supported by host kernel; skipping test-ext4"
fi

# --- erofs -------------------------------------------------------------------
if fs_supported erofs; then
    tree=$WORKDIR/test-erofs
    make_tree test-erofs "$tree"
    if mkfs.erofs /var/lib/extensions/test-erofs.raw "$tree" >/dev/null; then
        BUILT="$BUILT test-erofs"
        echo "built: test-erofs.raw (erofs)"
    else
        fail "build test-erofs.raw"
    fi
else
    skip "erofs not supported by host kernel; skipping test-erofs"
fi

# --- GPT disk image with root x86-64 partition ------------------------------
# Type GUID per UAPI Discoverable Partitions Spec (root x86-64).
ROOT_X86_64_GUID=4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709

# ensure_partnode DEV — make sure DEVp1 exists; in a container /dev is a
# tmpfs that does not receive new kernel device nodes, so mknod from sysfs.
ensure_partnode() {
    dev=$1
    part="${dev}p1"
    [ -b "$part" ] && return 0
    base=${dev##*/}
    devnum=$(cat "/sys/block/$base/${base}p1/dev" 2>/dev/null) || return 1
    [ -n "$devnum" ] || return 1
    mknod "$part" b "${devnum%%:*}" "${devnum##*:}"
}

if fs_supported ext4; then
    tree=$WORKDIR/test-gpt
    make_tree test-gpt "$tree"
    img=/var/lib/extensions/test-gpt.raw
    dd if=/dev/zero of="$img" bs=1M count=16 status=none
    if printf 'label: gpt\ntype=%s\n' "$ROOT_X86_64_GUID" | sfdisk -q "$img"; then
        loopdev=$(losetup -fP --show "$img") || loopdev=""
        if [ -n "$loopdev" ] && ensure_partnode "$loopdev"; then
            part="${loopdev}p1"
            ok=1
            mkfs.ext4 -q -F -d "$tree" "$part" 2>/dev/null || {
                # Fallback without -d
                mkfs.ext4 -q -F "$part" || ok=0
                if [ "$ok" = 1 ]; then
                    mnt=$WORKDIR/mnt-gpt
                    mkdir -p "$mnt"
                    if mount "$part" "$mnt"; then
                        cp -a "$tree"/. "$mnt"/
                        umount "$mnt"
                    else
                        ok=0
                    fi
                fi
            }
            losetup -d "$loopdev"
            if [ "$ok" = 1 ]; then
                BUILT="$BUILT test-gpt"
                echo "built: test-gpt.raw (GPT + ext4 root partition)"
            else
                fail "build test-gpt.raw (mkfs/populate)"
                rm -f "$img"
            fi
        else
            fail "build test-gpt.raw (losetup -P / partition node)"
            [ -n "$loopdev" ] && losetup -d "$loopdev"
            rm -f "$img"
        fi
    else
        fail "build test-gpt.raw (sfdisk)"
        rm -f "$img"
    fi
else
    skip "ext4 not supported by host kernel; skipping test-gpt"
fi

# --- Directory-based extension ----------------------------------------------
make_tree test-dir /var/lib/extensions/test-dir
echo "built: test-dir (directory)"

# --- Confext image (squashfs) ------------------------------------------------
CONFEXT_OK=0
if fs_supported squashfs; then
    tree=$WORKDIR/conf-test
    mkdir -p "$tree/etc/extension-release.d" "$tree/etc/conf-test"
    printf 'ID=_any\nARCHITECTURE=_any\n' \
        > "$tree/etc/extension-release.d/extension-release.conf-test"
    echo "marker=1" > "$tree/etc/conf-test/marker.conf"
    if mksquashfs "$tree" /var/lib/confexts/conf-test.raw -noappend -quiet; then
        CONFEXT_OK=1
        echo "built: conf-test.raw (confext, squashfs)"
    else
        fail "build conf-test.raw"
    fi
else
    skip "squashfs not supported; skipping confext test image"
fi

# All extension names expected to be visible/merged.
ALL_EXTS="$BUILT test-dir"
echo "=== Test extensions: $ALL_EXTS ==="

# ---------------------------------------------------------------------------
# Test sequence
# ---------------------------------------------------------------------------
echo "=== Running tests ==="

# 1. list shows all extensions
out=$(sysext list 2>&1)
rc=$?
if [ $rc -ne 0 ]; then
    fail "sysext list exited $rc"
else
    pass "sysext list exits 0"
fi
for name in $ALL_EXTS; do
    if echo "$out" | grep -q "$name"; then
        pass "sysext list shows $name"
    else
        fail "sysext list missing $name (output: $out)"
    fi
done

# 2. merge succeeds
if sysext merge; then
    pass "sysext merge"
else
    fail "sysext merge"
fi

# 3. payload files present for every extension
for name in $ALL_EXTS; do
    if [ -f "/usr/share/$name/hello.txt" ]; then
        pass "merged payload present: /usr/share/$name/hello.txt"
    else
        fail "merged payload missing: /usr/share/$name/hello.txt"
    fi
done

# 4. merged tools execute
for name in $ALL_EXTS; do
    got=$("/usr/bin/$name-tool" 2>&1)
    if [ "$got" = "$name-tool" ]; then
        pass "tool executes: $name-tool"
    else
        fail "tool $name-tool: expected '$name-tool', got '$got'"
    fi
done

# 5. status shows merged extensions
out=$(sysext status 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && echo "$out" | grep -q "test-dir"; then
    pass "sysext status shows merged extensions"
else
    fail "sysext status does not show merged extensions (output: $out)"
fi

# 6. second merge must fail (already merged)
if sysext merge 2>/dev/null; then
    fail "second sysext merge succeeded but must fail when already merged"
else
    pass "second sysext merge fails as expected"
fi

# 7. unmerge and verify files gone
if sysext unmerge; then
    pass "sysext unmerge"
else
    fail "sysext unmerge"
fi
for name in $ALL_EXTS; do
    if [ -e "/usr/share/$name/hello.txt" ]; then
        fail "file still present after unmerge: /usr/share/$name/hello.txt"
    else
        pass "file gone after unmerge: /usr/share/$name/hello.txt"
    fi
done

# 8. refresh merges again
if sysext refresh; then
    pass "sysext refresh"
else
    fail "sysext refresh"
fi
for name in $ALL_EXTS; do
    if [ -f "/usr/share/$name/hello.txt" ]; then
        pass "payload back after refresh: $name"
    else
        fail "payload missing after refresh: $name"
    fi
done

# 9. confext merge / verify / unmerge
if [ "$CONFEXT_OK" = 1 ]; then
    if confext merge; then
        pass "confext merge"
    else
        fail "confext merge"
    fi
    if [ -f /etc/conf-test/marker.conf ]; then
        pass "confext payload present: /etc/conf-test/marker.conf"
    else
        fail "confext payload missing: /etc/conf-test/marker.conf"
    fi
    if confext unmerge; then
        pass "confext unmerge"
    else
        fail "confext unmerge"
    fi
    if [ -e /etc/conf-test/marker.conf ]; then
        fail "confext payload still present after unmerge"
    else
        pass "confext payload gone after unmerge"
    fi
else
    skip "confext tests (no squashfs support)"
fi

# 10. masking: an empty dir in /etc/extensions masks the same-named extension
MASK_NAME=""
if built test-ext4; then
    MASK_NAME=test-ext4
else
    # Fall back to any built raw image.
    for name in $BUILT; do MASK_NAME=$name; break; done
fi

if [ -n "$MASK_NAME" ]; then
    mkdir -p "/etc/extensions/$MASK_NAME"
    if sysext refresh; then
        pass "sysext refresh with $MASK_NAME masked"
    else
        fail "sysext refresh with $MASK_NAME masked"
    fi
    if [ -e "/usr/share/$MASK_NAME/hello.txt" ]; then
        fail "masked extension $MASK_NAME still merged"
    else
        pass "masked extension $MASK_NAME not merged"
    fi
    for name in $ALL_EXTS; do
        [ "$name" = "$MASK_NAME" ] && continue
        if [ -f "/usr/share/$name/hello.txt" ]; then
            pass "unmasked extension still merged: $name"
        else
            fail "unmasked extension missing while $MASK_NAME masked: $name"
        fi
    done
    rmdir "/etc/extensions/$MASK_NAME"
    if sysext refresh; then
        pass "sysext refresh after unmasking"
    else
        fail "sysext refresh after unmasking"
    fi
    if [ -f "/usr/share/$MASK_NAME/hello.txt" ]; then
        pass "extension $MASK_NAME merged again after unmasking"
    else
        fail "extension $MASK_NAME missing after unmasking"
    fi
else
    skip "masking test (no raw images built)"
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

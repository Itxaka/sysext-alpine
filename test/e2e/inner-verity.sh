#!/bin/sh
# e2e suite for dm-verity protected GPT DDIs. Builds a GPT image with a root
# data partition for the host architecture and a root-verity partition
# formatted with veritysetup, embeds the verity root hash in the partitions'
# unique GUIDs per the UAPI Discoverable Partitions Spec, and exercises:
#   (a) merge with --image-policy=root=verity (verity device active)
#   (b) root-hash reconstruction from the unique partition GUIDs (implicit:
#       a byte-order bug in the reconstruction makes (a) fail)
#   (c) tamper detection: corrupting payload blocks must surface as an
#       EIO read or a failed merge
#   (d) policy enforcement: root=signed rejected (no signature partition),
#       root=unprotected mounts the data partition directly
. /work/test/e2e/lib.sh

echo "=== Installing build dependencies ==="
pkg_add cryptsetup device-mapper e2fsprogs e2fsprogs-extra kmod
install_sysext
need_overlayfs
fs_supported ext4 || fatal "kernel lacks ext4"
[ -n "$ROOT_GUID" ] || fatal "no partition types known for $HOST_ARCH"
if ! verity_supported; then
    skip dm-verity "dm-verity target or veritysetup unavailable; skipping the verity suite"
    finish
fi
echo "dm-verity target available"

verity_gone() { released; }

mkdir -p /var/lib/extensions

echo "=== Building verity test image ==="
NAME=test-verity
IMG=/var/lib/extensions/$NAME.raw
# The data partition starts at 1 MiB (sector 2048), see ddi_build.
DATA_START=2048

mk_tree "$WORKDIR/tree" "$NAME"
ddi_build "$WORKDIR/$NAME.raw" "$WORKDIR/tree" -v || fatal "building $NAME.raw failed"
DATA=$DDI_DATA
HASH=$DDI_HASH
ROOTHASH=$DDI_ROOTHASH
if [ "${#ROOTHASH}" != 64 ]; then
    fail "veritysetup format did not yield a 64-hex root hash (got '$ROOTHASH')"
    finish
fi
echo "verity root hash: $ROOTHASH"

# Sanity: the hash tree must verify against the untampered data.
if veritysetup verify "$DATA" "$HASH" "$ROOTHASH" >/dev/null 2>&1; then
    pass "veritysetup verify (image build sanity)"
else
    fail "veritysetup verify rejected freshly formatted image"
fi
cp "$WORKDIR/$NAME.raw" "$IMG"
echo "built: $NAME.raw (GPT: root + root-verity, root hash in unique GUIDs)"

# ---------------------------------------------------------------------------
# (a) + (b): merge with root=verity, payload visible, dm device active
# ---------------------------------------------------------------------------
echo "=== Running tests ==="

if out=$(sysext --image-policy=root=verity merge 2>&1); then
    pass "merge with --image-policy=root=verity"
else
    # (b): a failure here with an untampered image means the root-hash
    # reconstruction (UUID byte order) is wrong in the code.
    fail "merge with --image-policy=root=verity (roothash reconstruction?): $out"
fi

if [ -f "/usr/share/$NAME/hello.txt" ] \
   && [ "$(cat "/usr/share/$NAME/hello.txt")" = "hello from $NAME" ]; then
    pass "payload readable through verity device"
else
    fail "payload missing/corrupt: /usr/share/$NAME/hello.txt"
fi

if [ -n "$(verity_devs)" ]; then
    pass "dm-verity device active: $(verity_devs)"
else
    fail "no dm-verity device active"
fi

out=$(sysext status 2>&1)
if [ $? -eq 0 ] && echo "$out" | grep -q "$NAME"; then
    pass "sysext status shows $NAME"
else
    fail "sysext status does not show $NAME (output: $out)"
fi

if sysext unmerge; then
    pass "unmerge after verity merge"
else
    fail "unmerge after verity merge"
fi

if verity_gone; then
    pass "dm-verity device removed after unmerge"
else
    fail "dm-verity device still present after unmerge: $(verity_devs)"
fi

if [ -e "/usr/share/$NAME/hello.txt" ]; then
    fail "payload still present after unmerge"
else
    pass "payload gone after unmerge"
fi

# ---------------------------------------------------------------------------
# (d) policy enforcement (on the untampered image)
# ---------------------------------------------------------------------------

# root=signed must fail: the image has no verity-signature partition.
if sysext --image-policy=root=signed merge 2>/dev/null; then
    fail "merge with --image-policy=root=signed succeeded but image is unsigned"
    sysext unmerge
else
    pass "merge with --image-policy=root=signed rejected (no signature partition)"
fi

# root=unprotected: data partition mounted directly, no dm device.
if sysext --image-policy=root=unprotected merge; then
    pass "merge with --image-policy=root=unprotected"
    if [ -n "$(verity_devs)" ]; then
        fail "dm-verity device present despite root=unprotected"
    else
        pass "no dm-verity device with root=unprotected (direct mount)"
    fi
    if [ -f "/usr/share/$NAME/hello.txt" ]; then
        pass "payload visible with root=unprotected"
    else
        fail "payload missing with root=unprotected"
    fi
    sysext unmerge || fail "unmerge after root=unprotected merge"
else
    fail "merge with --image-policy=root=unprotected"
fi

# ---------------------------------------------------------------------------
# (c) tamper detection
# ---------------------------------------------------------------------------

# Locate the filesystem block holding hello.txt so the corruption is
# guaranteed to sit on the read path (debugfs from e2fsprogs-extra).
blk=$(debugfs -R "blocks /usr/share/$NAME/hello.txt" "$DATA" 2>/dev/null \
      | tr -s ' \n' ' ' | sed 's/^ *//' | cut -d' ' -f1)
case "$blk" in
    ''|*[!0-9]*)
        # Fallback: corrupt a 64 KiB stretch in the middle of the data
        # partition and hope it covers used blocks.
        echo "debugfs gave no block number; falling back to bulk corruption"
        offset=$(( DATA_START * 512 + 4 * 1024 * 1024 ))
        count=65536
        ;;
    *)
        offset=$(( DATA_START * 512 + blk * 4096 + 7 ))
        count=64
        echo "tampering with fs block $blk of hello.txt (image offset $offset)"
        ;;
esac
dd if=/dev/urandom of="$IMG" bs=1 seek=$offset count=$count conv=notrunc status=none

if sysext --image-policy=root=verity merge 2>/dev/null; then
    # Mount may succeed (superblock blocks untouched): the read itself
    # must then fail with EIO.
    if cat "/usr/share/$NAME/hello.txt" >/dev/null 2>&1; then
        fail "tampered payload read succeeded (dm-verity did not catch corruption)"
    else
        pass "tampered payload read fails (dm-verity EIO)"
    fi
    sysext unmerge
else
    pass "merge of tampered image fails"
fi

finish

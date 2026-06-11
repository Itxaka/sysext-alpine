#!/bin/sh
# e2e test suite for dm-verity protected GPT DDIs. Runs INSIDE a privileged
# Alpine container (launched by run.sh, which auto-runs every inner*.sh).
#
# Builds a GPT image with a root (x86-64) data partition and a root-verity
# partition formatted with veritysetup, embeds the verity root hash in the
# partitions' unique GUIDs per the UAPI Discoverable Partitions Spec, and
# exercises:
#   (a) merge with --image-policy=root=verity (verity device active)
#   (b) root-hash reconstruction from the unique partition GUIDs (implicit:
#       a byte-order bug in the reconstruction makes (a) fail)
#   (c) tamper detection: corrupting payload blocks must surface as an
#       EIO read or a failed merge
#   (d) policy enforcement: root=signed rejected (no signature partition),
#       root=unprotected mounts the data partition directly
#
# The whole suite SKIPs when the dm-verity target or veritysetup is
# unavailable. PASS/FAIL/SKIP accounting as in inner.sh.
set -u

FAILS=0
SKIPS=0

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
skip() { echo "SKIP: $*"; SKIPS=$((SKIPS + 1)); }

finish() {
    echo "==========================================="
    echo "Failures: $FAILS  Skips: $SKIPS"
    if [ "$FAILS" -gt 0 ]; then
        echo "RESULT: FAIL"
        exit 1
    fi
    echo "RESULT: PASS"
    exit 0
}

fs_supported() {
    grep -qw "$1" /proc/filesystems && return 0
    modprobe "$1" 2>/dev/null || true
    grep -qw "$1" /proc/filesystems
}

# ---------------------------------------------------------------------------
# Environment setup
# ---------------------------------------------------------------------------
echo "=== Installing build dependencies ==="
apk add --no-cache cryptsetup device-mapper e2fsprogs e2fsprogs-extra \
    util-linux kmod \
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

# ---------------------------------------------------------------------------
# Capability checks — SKIP the whole suite if dm-verity cannot work here
# ---------------------------------------------------------------------------
if ! command -v veritysetup >/dev/null 2>&1; then
    skip "veritysetup not available; skipping verity suite"
    finish
fi

if ! fs_supported ext4; then
    skip "ext4 not supported by host kernel; skipping verity suite"
    finish
fi

modprobe dm-verity 2>/dev/null || modprobe dm_verity 2>/dev/null || true
if ! dmsetup targets 2>/dev/null | grep -qw verity; then
    skip "dm-verity target unavailable (modprobe failed?); skipping verity suite"
    finish
fi
echo "dm-verity target available"

mkdir -p /var/lib/extensions
WORKDIR=/tmp/e2e-verity
mkdir -p "$WORKDIR"

# ---------------------------------------------------------------------------
# Test image construction
# ---------------------------------------------------------------------------
echo "=== Building verity test image ==="

NAME=test-verity
IMG=/var/lib/extensions/$NAME.raw
DATA=$WORKDIR/data.img
HASH=$WORKDIR/hash.img
DM_NODE=/dev/mapper/sysext-$NAME-verity

# UAPI Discoverable Partitions Spec type GUIDs (x86-64).
ROOT_GUID=4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709
ROOT_VERITY_GUID=2C7357ED-EBD2-46D9-AEC1-23D437EC2BF5

# Partition layout (512-byte sectors): data 8 MiB @ 2048, verity 4 MiB
# @ 18432, image 14 MiB total (room for the GPT backup header).
DATA_START=2048
DATA_SECTORS=16384
VERITY_START=18432
VERITY_SECTORS=8192
IMG_MIB=14

# Payload tree (same conventions as inner.sh make_tree).
tree=$WORKDIR/tree
mkdir -p "$tree/usr/lib/extension-release.d" \
         "$tree/usr/share/$NAME" \
         "$tree/usr/bin"
printf 'ID=_any\nARCHITECTURE=_any\n' \
    > "$tree/usr/lib/extension-release.d/extension-release.$NAME"
echo "hello from $NAME" > "$tree/usr/share/$NAME/hello.txt"
printf '#!/bin/sh\necho %s-tool\n' "$NAME" > "$tree/usr/bin/$NAME-tool"
chmod 0755 "$tree/usr/bin/$NAME-tool"

# Data filesystem (4096-byte blocks so debugfs block numbers map directly
# to veritysetup's default 4096-byte data blocks).
dd if=/dev/zero of="$DATA" bs=512 count=$DATA_SECTORS status=none
if ! mkfs.ext4 -q -F -b 4096 -d "$tree" "$DATA" 2>/dev/null; then
    # Older mkfs.ext4 without -d: format then loop-mount and copy.
    mkfs.ext4 -q -F -b 4096 "$DATA" || { echo "FATAL: mkfs.ext4 failed"; exit 1; }
    mnt=$WORKDIR/mnt
    mkdir -p "$mnt"
    mount -o loop "$DATA" "$mnt" || { echo "FATAL: loop mount failed"; exit 1; }
    cp -a "$tree"/. "$mnt"/
    umount "$mnt"
fi

# veritysetup format writes the superblock + hash tree into $HASH and
# prints the root hash.
ROOTHASH=$(veritysetup format "$DATA" "$HASH" | awk '/^Root hash/{print $3}')
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

# Root-hash discovery rule: data partition unique UUID = first 128 bits of
# the root hash, verity partition unique UUID = last 128 bits, both in
# canonical textual UUID form.
uuid_of() {
    h=$1
    printf '%s-%s-%s-%s-%s' \
        "$(echo "$h" | cut -c1-8)" \
        "$(echo "$h" | cut -c9-12)" \
        "$(echo "$h" | cut -c13-16)" \
        "$(echo "$h" | cut -c17-20)" \
        "$(echo "$h" | cut -c21-32)"
}
DATA_UUID=$(uuid_of "$(echo "$ROOTHASH" | cut -c1-32)")
VERITY_UUID=$(uuid_of "$(echo "$ROOTHASH" | cut -c33-64)")
echo "data partition uuid:   $DATA_UUID"
echo "verity partition uuid: $VERITY_UUID"

dd if=/dev/zero of="$IMG" bs=1M count=$IMG_MIB status=none
sfdisk -q "$IMG" <<EOF || { fail "sfdisk failed building $IMG"; finish; }
label: gpt
start=$DATA_START, size=$DATA_SECTORS, type=$ROOT_GUID, uuid=$DATA_UUID
start=$VERITY_START, size=$VERITY_SECTORS, type=$ROOT_VERITY_GUID, uuid=$VERITY_UUID
EOF

dd if="$DATA" of="$IMG" bs=512 seek=$DATA_START conv=notrunc status=none
dd if="$HASH" of="$IMG" bs=512 seek=$VERITY_START conv=notrunc status=none
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

if [ -b "$DM_NODE" ]; then
    pass "dm-verity device node exists: $DM_NODE"
else
    fail "dm-verity device node missing: $DM_NODE"
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

if [ -e "$DM_NODE" ]; then
    fail "dm-verity device still present after unmerge: $DM_NODE"
else
    pass "dm-verity device removed after unmerge"
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
    sysext unmerge || true
else
    pass "merge with --image-policy=root=signed rejected (no signature partition)"
fi

# root=unprotected: data partition mounted directly, no dm device.
if sysext --image-policy=root=unprotected merge; then
    pass "merge with --image-policy=root=unprotected"
    if [ -e "$DM_NODE" ]; then
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
    sysext unmerge || true
else
    pass "merge of tampered image fails"
    sysext unmerge >/dev/null 2>&1 || true
fi

# Final cleanup so the container exits with nothing mounted.
sysext unmerge >/dev/null 2>&1 || true

finish

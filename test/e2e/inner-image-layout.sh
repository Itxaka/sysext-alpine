#!/bin/sh
# e2e suite for raw image dissection and device lifetime.
#
# Exercises, against systemd v262 dissect-image.c semantics:
#   - 4096-byte sector DDIs (loop block size follows the GPT)
#   - usr-only and root+usr DDIs (usr mounted at <root>/usr)
#   - partition selection: generic Linux type, "_empty" labels, the
#     no-auto attribute, secondary and foreign architectures, label versions
#   - a corrupted primary GPT header (alternate header is used)
#   - MBR images with a bootable Linux partition
#   - read-only mounts of ext4 images whose journal needs recovery
#   - refused (ext2) and additional (xfs, btrfs) filesystems
#   - image policy rules for designators other than root/usr
#   - loop devices with autoclear, released once unmerged
#   - block device images (no extra loop device unless partitions are
#     needed but not scanned, sidecars not looked up, the device never
#     detached)
. /work/test/e2e/lib.sh
allow_skip fs:xfs fs:btrfs arch:secondary

echo "=== Installing build dependencies ==="
pkg_add e2fsprogs e2fsprogs-extra kmod squashfs-tools device-mapper
pkg_try xfsprogs btrfs-progs
install_sysext
need_overlayfs
fs_supported ext4 || fatal "kernel lacks ext4"
[ -n "$ROOT_GUID" ] || fatal "no partition types known for $HOST_ARCH"

mkdir -p /var/lib/extensions
W=$WORKDIR

# Partition types: the host's, its secondary architecture's and a foreign one.
NATIVE_ROOT=$ROOT_GUID
NATIVE_USR=$USR_GUID
SECONDARY_ROOT=$(part_type "${SECONDARY_ARCH:-none}" root)
FOREIGN_ROOT=$(part_type "$FOREIGN_ARCH" root)
ESP=$ESP_GUID
GENERIC=$GENERIC_GUID

# tree NAME DIR [PREFIX] — sysext payload; PREFIX "" puts usr/ content at
# the top (for usr partitions).
tree() {
    if [ "${3-usr/}" = "" ]; then mk_tree "$2" "$1" "" usr; else mk_tree "$2" "$1"; fi
}

# ext4img DIR FILE MIB [mkfs args...]
ext4img() {
    dir=$1 file=$2 mib=$3
    shift 3
    img_ext4 "$dir" "$file" "$mib" -b 4096 "$@"
}

put() { img_put "$@"; }
gpt() { part_image "$@"; }
loops_bound() { our_loops /var/lib/extensions/ | wc -l; }

# use IMG NAME — make IMG the only extension, as NAME.raw.
use() {
    rm -rf /var/lib/extensions/*
    cp "$1" "/var/lib/extensions/$2.raw"
}

# check NAME CTX — merge, check the payload, unmerge, check cleanup.
check() {
    name=$1 ctx=$2
    if out=$(sysext merge 2>&1); then
        pass "$ctx: merge"
    else
        fail "$ctx: merge: $out"
        sysext unmerge >/dev/null 2>&1
        return 1
    fi
    if [ "$(cat "/usr/share/$name/hello.txt" 2>/dev/null)" = "hello from $name" ]; then
        pass "$ctx: payload visible"
    else
        fail "$ctx: payload missing"
    fi
    if out=$(sysext unmerge 2>&1); then
        pass "$ctx: unmerge"
    else
        fail "$ctx: unmerge: $out"
    fi
    if released; then
        pass "$ctx: loop devices and mounts released"
    else
        fail "$ctx: leaked: $(leaked)"
    fi
}

# refuse CTX ERROR REASON — merge must fail with ERROR, the debug log
# giving REASON, and leave nothing behind.
refuse() {
    ctx=$1 error=$2 reason=$3
    if out=$(SYSTEMD_LOG_LEVEL=debug sysext merge 2>&1); then
        fail "$ctx: merge succeeded"
        sysext unmerge >/dev/null 2>&1
    elif [ "$(echo "$out" | tail -n 1)" = "$error" ] && echo "$out" | grep -q -- "$reason"; then
        pass "$ctx: refused ($reason)"
    else
        fail "$ctx: unexpected error: $out"
    fi
    if released; then
        pass "$ctx: nothing left attached"
    else
        fail "$ctx: leaked: $(leaked)"
    fi
}

echo "=== Running tests ==="

# --- 4096-byte sectors -------------------------------------------------------
tree s4k "$W/s4k"
ext4img "$W/s4k" "$W/s4k.ext4" 8
printf 'label: gpt\nstart=256, size=2048, type=%s\n' "$NATIVE_ROOT" | gpt "$W/s4k.raw" 4096 12
put "$W/s4k.ext4" "$W/s4k.raw" 4096 256
use "$W/s4k.raw" s4k
if sysext merge; then
    pass "4K-sector DDI: merge"
    if [ "$(cat /usr/share/s4k/hello.txt 2>/dev/null)" = "hello from s4k" ]; then
        pass "4K-sector DDI: payload visible"
    else
        fail "4K-sector DDI: payload missing"
    fi
    l=$(loop_of /var/lib/extensions/s4k.raw)
    if [ "$(cat "/sys/block/$l/queue/logical_block_size" 2>/dev/null)" = 4096 ]; then
        pass "4K-sector DDI: loop device uses 4096-byte blocks"
    else
        fail "4K-sector DDI: loop block size $(cat "/sys/block/$l/queue/logical_block_size" 2>/dev/null)"
    fi
    if [ "$(cat "/sys/block/$l/loop/autoclear" 2>/dev/null)" = 1 ]; then
        pass "loop device has autoclear set while merged"
    else
        fail "loop device autoclear: $(cat "/sys/block/$l/loop/autoclear" 2>/dev/null)"
    fi
    sysext unmerge || fail "4K-sector DDI: unmerge"
    released && pass "4K-sector DDI: released" || fail "4K-sector DDI: leaked: $(leaked)"
else
    fail "4K-sector DDI: merge"
fi

# --- usr-only DDI ------------------------------------------------------------
tree usronly "$W/usronly" ""
ext4img "$W/usronly" "$W/usronly.ext4" 8
printf 'label: gpt\nstart=2048, size=16384, type=%s\n' "$NATIVE_USR" | gpt "$W/usronly.raw" 512 12
put "$W/usronly.ext4" "$W/usronly.raw" 512 2048
use "$W/usronly.raw" usronly
check usronly "usr-only DDI"
check usronly "usr-only DDI (again)"
[ ! -e /run/systemd/sysext ] && pass "usr-only DDI: workspace removed" || fail "usr-only DDI: workspace left: $(find /run/systemd/sysext | head)"

# --- root + usr DDI ----------------------------------------------------------
rm -rf "$W/ru-root"
mkdir -p "$W/ru-root/opt/rutest" "$W/ru-root/usr"
echo "opt from rutest" > "$W/ru-root/opt/rutest/hello"
tree rutest "$W/ru-usr" ""
ext4img "$W/ru-root" "$W/ru-root.ext4" 4
ext4img "$W/ru-usr" "$W/ru-usr.ext4" 4
printf 'label: gpt\nstart=2048, size=8192, type=%s\nstart=10240, size=8192, type=%s\n' "$NATIVE_ROOT" "$NATIVE_USR" \
    | gpt "$W/rutest.raw" 512 12
put "$W/ru-root.ext4" "$W/rutest.raw" 512 2048
put "$W/ru-usr.ext4" "$W/rutest.raw" 512 10240
use "$W/rutest.raw" rutest
if sysext merge; then
    pass "root+usr DDI: merge"
    [ "$(cat /opt/rutest/hello 2>/dev/null)" = "opt from rutest" ] && pass "root+usr DDI: /opt from root partition" || fail "root+usr DDI: /opt payload missing"
    [ "$(cat /usr/share/rutest/hello.txt 2>/dev/null)" = "hello from rutest" ] && pass "root+usr DDI: /usr from usr partition" || fail "root+usr DDI: /usr payload missing"
    sysext unmerge || fail "root+usr DDI: unmerge"
    released && pass "root+usr DDI: released" || fail "root+usr DDI: leaked: $(leaked)"
else
    fail "root+usr DDI: merge"
fi

# --- partition selection -----------------------------------------------------
tree gen "$W/gen"
ext4img "$W/gen" "$W/gen.ext4" 8
printf 'label: gpt\nstart=2048, size=16384, type=%s\n' "$GENERIC" | gpt "$W/gen.raw" 512 12
put "$W/gen.ext4" "$W/gen.raw" 512 2048
use "$W/gen.raw" gen
check gen "generic Linux partition as root"

printf 'label: gpt\nstart=2048, size=16384, type=%s\nstart=18432, size=16384, type=%s\n' "$GENERIC" "$GENERIC" \
    | gpt "$W/gen2.raw" 512 20
put "$W/gen.ext4" "$W/gen2.raw" 512 2048
use "$W/gen2.raw" gen
refuse "two generic Linux partitions" "Failed to read metadata for image gen: Name not unique on network" "multiple generic"

tree pick "$W/pick"
ext4img "$W/pick" "$W/pick.ext4" 4
rm -rf "$W/decoy"
mkdir -p "$W/decoy"
ext4img "$W/decoy" "$W/decoy.ext4" 4
printf 'label: gpt\nstart=2048, size=8192, type=%s, name="_empty"\nstart=10240, size=8192, type=%s, attrs="GUID:63"\nstart=18432, size=8192, type=%s\n' \
    "$NATIVE_ROOT" "$NATIVE_ROOT" "$NATIVE_ROOT" | gpt "$W/pick.raw" 512 14
put "$W/decoy.ext4" "$W/pick.raw" 512 2048
put "$W/decoy.ext4" "$W/pick.raw" 512 10240
put "$W/pick.ext4" "$W/pick.raw" 512 18432
use "$W/pick.raw" pick
check pick "_empty and no-auto partitions skipped"

printf 'label: gpt\nstart=2048, size=8192, type=%s, name="pick_1.10"\nstart=10240, size=8192, type=%s, name="pick_1.9"\n' \
    "$NATIVE_ROOT" "$NATIVE_ROOT" | gpt "$W/ver.raw" 512 10
put "$W/pick.ext4" "$W/ver.raw" 512 2048
put "$W/decoy.ext4" "$W/ver.raw" 512 10240
use "$W/ver.raw" pick
check pick "newest partition label version wins"

if [ -n "$SECONDARY_ROOT" ]; then
    printf 'label: gpt\nstart=2048, size=8192, type=%s\nstart=10240, size=8192, type=%s\n' \
        "$SECONDARY_ROOT" "$NATIVE_ROOT" | gpt "$W/arch.raw" 512 10
    put "$W/decoy.ext4" "$W/arch.raw" 512 2048
    put "$W/pick.ext4" "$W/arch.raw" 512 10240
    use "$W/arch.raw" pick
    check pick "native architecture ($HOST_ARCH) preferred over secondary ($SECONDARY_ARCH)"
else
    skip arch:secondary "$HOST_ARCH has no secondary architecture"
fi

printf 'label: gpt\nstart=2048, size=8192, type=%s\n' "$FOREIGN_ROOT" | gpt "$W/foreign.raw" 512 6
put "$W/pick.ext4" "$W/foreign.raw" 512 2048
use "$W/foreign.raw" pick
check pick "foreign architecture ($FOREIGN_ARCH) root partition used when it is the only one"

# --- corrupted primary GPT header --------------------------------------------
printf 'label: gpt\nstart=2048, size=16384, type=%s\n' "$NATIVE_ROOT" | gpt "$W/crc.raw" 512 12
put "$W/gen.ext4" "$W/crc.raw" 512 2048
printf '\377' | dd of="$W/crc.raw" bs=1 seek=528 conv=notrunc status=none
use "$W/crc.raw" gen
check gen "corrupted primary GPT header (alternate header used)"

# --- MBR ---------------------------------------------------------------------
printf 'label: dos\nstart=2048, size=16384, type=83, bootable\n' | gpt "$W/mbr.raw" 512 12
put "$W/gen.ext4" "$W/mbr.raw" 512 2048
use "$W/mbr.raw" gen
check gen "MBR image with a bootable Linux partition"

printf 'label: dos\nstart=2048, size=16384, type=83\n' | gpt "$W/mbr2.raw" 512 12
put "$W/gen.ext4" "$W/mbr2.raw" 512 2048
use "$W/mbr2.raw" gen
refuse "MBR image without a bootable partition" "Failed to read metadata for image gen: No such device or address" "found neither"

# --- filesystems -------------------------------------------------------------
tree rec "$W/rec"
ext4img "$W/rec" "$W/rec.raw" 8
if debugfs -w -R "feature needs_recovery" "$W/rec.raw" >/dev/null 2>&1; then
    use "$W/rec.raw" rec
    check rec "ext4 image needing journal recovery (norecovery)"
    if ! dumpe2fs -h "/var/lib/extensions/rec.raw" 2>/dev/null | grep -q needs_recovery; then
        fail "ext4 image was modified by the read-only mount"
    else
        pass "ext4 image left untouched"
    fi
else
    skip tool:debugfs "debugfs cannot set needs_recovery"
fi

tree old "$W/old"
rm -f "$W/old.raw"
dd if=/dev/zero of="$W/old.raw" bs=1M count=8 status=none
mkfs.ext2 -q -F -d "$W/old" "$W/old.raw"
use "$W/old.raw" old
refuse "ext2 image" "Failed to read metadata for image old: Identifier removed" "not allowed"

if command -v mkfs.xfs >/dev/null 2>&1 && fs_supported xfs; then
    tree xfsimg "$W/xfsimg"
    rm -f "$W/xfsimg.raw"
    truncate -s 320M "$W/xfsimg.raw"
    if mkfs.xfs -q -f -p "file=$W/xfsimg" "$W/xfsimg.raw" 2>/dev/null; then
        use "$W/xfsimg.raw" xfsimg
        check xfsimg "xfs image"
    else
        skip fs:xfs "mkfs.xfs cannot populate from a directory"
    fi
else
    skip fs:xfs "xfs unavailable"
fi

if command -v mkfs.btrfs >/dev/null 2>&1 && fs_supported btrfs; then
    tree btrfsimg "$W/btrfsimg"
    rm -f "$W/btrfsimg.raw"
    truncate -s 128M "$W/btrfsimg.raw"
    if mkfs.btrfs -q --rootdir "$W/btrfsimg" "$W/btrfsimg.raw" >/dev/null 2>&1; then
        use "$W/btrfsimg.raw" btrfsimg
        check btrfsimg "btrfs image"
    else
        skip fs:btrfs "mkfs.btrfs --rootdir failed"
    fi
else
    skip fs:btrfs "btrfs unavailable"
fi

# --- image policy beyond root/usr --------------------------------------------
printf 'label: gpt\nstart=2048, size=16384, type=%s\nstart=18432, size=2048, type=%s\n' "$NATIVE_ROOT" "$ESP" \
    | gpt "$W/esp.raw" 512 12
put "$W/gen.ext4" "$W/esp.raw" 512 2048
use "$W/esp.raw" gen
if out=$(sysext --image-policy=root=open:esp=absent merge 2>&1); then
    fail "esp=absent with an ESP partition: merge succeeded"
    sysext unmerge >/dev/null 2>&1
else
    pass "esp=absent with an ESP partition refused"
fi
if sysext --image-policy=root=open merge >/dev/null 2>&1; then
    pass "ESP partition ignored by root=open"
    sysext unmerge || fail "unmerge after ESP image"
else
    fail "ESP partition must be ignored by root=open"
fi

printf 'label: gpt\nstart=2048, size=16384, type=%s\n' "$NATIVE_ROOT" | gpt "$W/ro.raw" 512 12
put "$W/gen.ext4" "$W/ro.raw" 512 2048
use "$W/ro.raw" gen
if sysext --image-policy=root=read-only-on merge >/dev/null 2>&1; then
    fail "root=read-only-on accepted a partition without the read-only flag"
    sysext unmerge >/dev/null 2>&1
else
    pass "root=read-only-on refuses a partition without the read-only flag"
fi
printf 'label: gpt\nstart=2048, size=16384, type=%s, attrs="GUID:60"\n' "$NATIVE_ROOT" | gpt "$W/ro.raw" 512 12
put "$W/gen.ext4" "$W/ro.raw" 512 2048
use "$W/ro.raw" gen
if sysext --image-policy=root=read-only-on merge >/dev/null 2>&1; then
    pass "root=read-only-on accepts a partition with the read-only flag"
    sysext unmerge || fail "unmerge after read-only image"
else
    fail "root=read-only-on refused a partition with the read-only flag"
fi

# --- block devices -----------------------------------------------------------
nloops() { our_loops | wc -l; }
# settle N — wait (up to 5 s) until N loop devices of this suite are left.
settle() {
    i=0
    while [ "$(nloops)" -ne "$1" ] && [ $i -lt 50 ]; do
        sleep 0.1
        i=$((i + 1))
    done
    [ "$(nloops)" -eq "$1" ]
}
source_of() { mounts | awk -v m="/run/systemd/sysext/extensions/$1" '$1 == m {print $3}'; }

rm -rf /var/lib/extensions/*
released || fail "block devices: leftovers of the previous tests: $(leaked)"
tree blk "$W/blk"
ext4img "$W/blk" "$W/blk.ext4" 8
dev=$(loop_attach "$W/blk.ext4") || fatal "losetup $W/blk.ext4"
ln -s "$dev" /var/lib/extensions/blk
echo garbage > "$dev.roothash"
expect_eq "block device image listed as such" "$(sysext list --json=short | grep -o '"type":"[a-z]*"')" '"type":"block"'
before=$(nloops)
if out=$(sysext merge 2>&1); then pass "block device image: merge (no sidecars next to devices)"; else fail "block device image: merge: $out"; fi
expect_eq "block device image: payload visible" "$(cat /usr/share/blk/hello.txt 2>&1)" "hello from blk"
expect_eq "block device image: mounted without another loop device" "$(source_of blk) $(nloops)" "$dev $before"
sysext unmerge >/dev/null 2>&1
loop_attached "$dev" && pass "block device image: still attached after unmerge" || fail "block device image: detached by unmerge"
rm -f "$dev.roothash"
if out=$(sysext --image-policy=root=verity merge 2>&1); then
    fail "block device image: merged against root=verity"
    sysext unmerge >/dev/null 2>&1
else
    pass "block device image: refused by root=verity"
fi
loop_attached "$dev" && pass "block device image: still attached after a failed merge" || fail "block device image: detached by a failed merge"
rm -f /var/lib/extensions/blk
losetup -d "$dev"

printf 'label: gpt\nstart=2048, size=16384, type=%s\n' "$NATIVE_ROOT" | gpt "$W/blkgpt.raw" 512 12
put "$W/blk.ext4" "$W/blkgpt.raw" 512 2048
dev=$(loop_attach "$W/blkgpt.raw") || fatal "losetup $W/blkgpt.raw"
ln -s "$dev" /var/lib/extensions/blk
before=$(nloops)
if out=$(sysext merge 2>&1); then pass "GPT block device without partition scanning: merge"; else fail "GPT block device without partition scanning: merge: $out"; fi
expect_eq "GPT block device without partition scanning: payload visible" "$(cat /usr/share/blk/hello.txt 2>&1)" "hello from blk"
src=$(source_of blk)
case $src in
    "$dev"*) fail "GPT block device without partition scanning: mounted from $src" ;;
    /dev/loop*p1) [ "$(nloops)" -gt "$before" ] && pass "GPT block device without partition scanning: a loop device on top ($src)" ||
        fail "GPT block device without partition scanning: no loop device on top" ;;
    *) fail "GPT block device without partition scanning: mounted from $src" ;;
esac
sysext unmerge >/dev/null 2>&1
settle "$before" && pass "GPT block device without partition scanning: loop device on top released" ||
    fail "GPT block device without partition scanning: leaked: $(leaked)"
loop_attached "$dev" && pass "GPT block device without partition scanning: still attached" || fail "GPT block device without partition scanning: detached"
rm -f /var/lib/extensions/blk
losetup -d "$dev"

dev=$(loop_attach "$W/blkgpt.raw" -P) || fatal "losetup -P $W/blkgpt.raw"
ln -s "$dev" /var/lib/extensions/blk
before=$(nloops)
if out=$(sysext merge 2>&1); then pass "GPT block device with partition scanning: merge"; else fail "GPT block device with partition scanning: merge: $out"; fi
expect_eq "GPT block device with partition scanning: payload visible" "$(cat /usr/share/blk/hello.txt 2>&1)" "hello from blk"
expect_eq "GPT block device with partition scanning: its own partition mounted" "$(source_of blk) $(nloops)" "${dev}p1 $before"
sysext unmerge >/dev/null 2>&1
loop_attached "$dev" && pass "GPT block device with partition scanning: still attached after unmerge" ||
    fail "GPT block device with partition scanning: detached by unmerge"
rm -f /var/lib/extensions/blk
losetup -d "$dev"

rm -rf /var/lib/extensions/*
finish

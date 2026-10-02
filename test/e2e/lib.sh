# shellcheck shell=sh disable=SC2034,SC3043
# SC3043: busybox ash, dash and bash all have local. SC2034: the variables
# are for the suites.
#
# Shared helpers for the e2e suites. Every suite sources this file inside its
# privileged container (see run.sh):
#
#   . /work/test/e2e/lib.sh
#
# Results: pass, fail, skip TAG MESSAGE, expect_eq, expect_err; finish prints
# the summary and exits. A skip names the missing capability as TAG. With
# E2E_STRICT=1 (CI) a skip is a failure unless the suite accepted TAG with
# allow_skip or E2E_ALLOW_SKIP (space separated) lists it.
#
# Cleanup: an EXIT trap unmerges, unmounts what the suite registered with
# track_mount, removes device-mapper devices stacked on the suite's loop
# devices and detaches loop devices backed by the suite's files. finish first
# asserts that the binary under test left none of these behind.
set -u

E2E_SUITE=${E2E_SUITE:-$(basename "$0" .sh)}
WORKDIR=/tmp/$E2E_SUITE
PASSES=0
FAILS=0
SKIPS=0
E2E_ALLOWED=" ${E2E_ALLOW_SKIP:-} "
E2E_MOUNTS=""
E2E_ROOTS=""
E2E_FINISHED=0
# Binaries the end-of-suite checks unmerge with; suites installing the
# binary under another name override this.
E2E_TOOLS="sysext confext"
mkdir -p "$WORKDIR"

pass() {
    PASSES=$((PASSES + 1))
    echo "PASS: $*"
}

fail() {
    FAILS=$((FAILS + 1))
    echo "FAIL: $*"
}

# skip TAG MESSAGE
skip() {
    local tag
    tag=$1
    shift
    case $E2E_ALLOWED in
        *" $tag "*) ;;
        *)
            if [ "${E2E_STRICT:-0}" = 1 ]; then
                fail "unexpected skip [$tag] under E2E_STRICT=1: $*"
                return
            fi
            ;;
    esac
    SKIPS=$((SKIPS + 1))
    echo "SKIP: [$tag] $*"
}

# allow_skip TAG... — skips with these tags are acceptable under E2E_STRICT=1.
allow_skip() {
    local t
    for t in "$@"; do E2E_ALLOWED="$E2E_ALLOWED$t "; done
}

# expect_eq CONTEXT GOT WANT
expect_eq() {
    if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got [$2], want [$3]"; fi
}

# expect_err CONTEXT RC OUTPUT MESSAGE — a non-zero exit whose output ends
# with MESSAGE.
expect_err() {
    case "$3" in
        *"$4") if [ "$2" -ne 0 ]; then pass "$1"; else fail "$1: exit code 0"; fi ;;
        *) fail "$1: got [$3], want [$4]" ;;
    esac
}

fatal() {
    echo "FATAL: $*"
    FAILS=$((FAILS + 1))
    finish
}

# ---------------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------------

# pkg_add PACKAGE... — install packages (apk or pacman); fatal on failure.
# util-linux is always added: the device helpers need its losetup.
pkg_add() {
    local out
    if command -v apk >/dev/null 2>&1; then
        out=$(apk add --no-cache util-linux "$@" 2>&1) || { echo "$out"; fatal "apk add $*"; }
    else
        out=$(pacman -Sy --noconfirm --needed "$@" 2>&1) || { echo "$out"; fatal "pacman -S $*"; }
    fi
}

# pkg_try PACKAGE... — install optional packages.
pkg_try() {
    if command -v apk >/dev/null 2>&1; then
        apk add --no-cache "$@" >/dev/null 2>&1
    else
        pacman -Sy --noconfirm --needed "$@" >/dev/null 2>&1
    fi
}

# install_sysext [DIR] — install the binary under test as DIR/sysext with a
# confext link next to it (default DIR: /usr/bin).
install_sysext() {
    [ -x /work/bin/sysext ] || fatal "/work/bin/sysext missing (run 'make build' on the host)"
    install -m 0755 /work/bin/sysext "${1:-/usr/bin}/sysext"
    ln -sf sysext "${1:-/usr/bin}/confext"
}

# fs_supported FS — FS is registered with the kernel or can be loaded (run.sh
# bind-mounts the host's /lib/modules).
fs_supported() {
    grep -qw "$1" /proc/filesystems && return 0
    modprobe "$1" 2>/dev/null || true
    grep -qw "$1" /proc/filesystems
}

need_overlayfs() {
    fs_supported overlay || fatal "kernel does not support overlayfs"
}

# verity_supported — veritysetup and the dm-verity target are usable
# (E2E_FORCE_NO_VERITY=1 pretends they are not, to check the skip policy).
verity_supported() {
    [ "${E2E_FORCE_NO_VERITY:-0}" = 1 ] && return 1
    command -v veritysetup >/dev/null 2>&1 || return 1
    modprobe dm-verity 2>/dev/null || modprobe dm_verity 2>/dev/null || true
    dmsetup targets 2>/dev/null | grep -qw verity
}

# ---------------------------------------------------------------------------
# Architecture (systemd names) and UAPI DPS partition types
# ---------------------------------------------------------------------------

case $(uname -m) in
    x86_64) HOST_ARCH=x86-64 SECONDARY_ARCH=x86 ;;
    i?86) HOST_ARCH=x86 SECONDARY_ARCH= ;;
    aarch64) HOST_ARCH=arm64 SECONDARY_ARCH=arm ;;
    armv*l) HOST_ARCH=arm SECONDARY_ARCH= ;;
    riscv64) HOST_ARCH=riscv64 SECONDARY_ARCH= ;;
    loongarch64) HOST_ARCH=loongarch64 SECONDARY_ARCH= ;;
    ppc64le) HOST_ARCH=ppc64-le SECONDARY_ARCH= ;;
    s390x) HOST_ARCH=s390x SECONDARY_ARCH=s390 ;;
    *) HOST_ARCH=$(uname -m) SECONDARY_ARCH= ;;
esac
# An architecture the host cannot execute.
if [ "$HOST_ARCH" = arm64 ]; then FOREIGN_ARCH=x86-64; else FOREIGN_ARCH=arm64; fi

# part_type ARCH DESIGNATOR — partition type GUID; DESIGNATOR is root,
# root-verity, root-verity-sig, usr, usr-verity or usr-verity-sig. Prints
# nothing for unknown architectures.
part_type() {
    case $1 in
        x86-64) set -- "$2" 4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709 2C7357ED-EBD2-46D9-AEC1-23D437EC2BF5 41092B05-9FC8-4523-994F-2DEF0408B176 8484680C-9521-48C6-9C11-B0720656F69E 77FF5F63-E7B6-4633-ACF4-1565B864C0E6 E7BB33FB-06CF-4E81-8273-E543B413E2E2 ;;
        x86) set -- "$2" 44479540-F297-41B2-9AF7-D131D5F0458A D13C5D3B-B5D1-422A-B29F-9454FDC89D76 5996FC05-109C-48DE-808B-23FA0830B676 75250D76-8CC6-458E-BD66-BD47CC81A812 8F461B0D-14EE-4E81-9AA9-049B6FB97ABD 974A71C0-DE41-43C3-BE5D-5C5CCD1AD2C0 ;;
        arm64) set -- "$2" B921B045-1DF0-41C3-AF44-4C6F280D3FAE DF3300CE-D69F-4C92-978C-9BFB0F38D820 6DB69DE6-29F4-4758-A7A5-962190F00CE3 B0E01050-EE5F-4390-949A-9101B17104E9 6E11A4E7-FBCA-4DED-B9E9-E1A512BB664E C23CE4FF-44BD-4B00-B2D4-B41B3419E02A ;;
        arm) set -- "$2" 69DAD710-2CE4-4E3C-B16C-21A1D49ABED3 7386CDF2-203C-47A9-A498-F2ECCE45A2D6 42B0455F-EB11-491D-98D3-56145BA9D037 7D0359A3-02B3-4F0A-865C-654403E70625 C215D751-7BCD-4649-BE90-6627490A4C05 D7FF812F-37D1-4902-A810-D76BA57B975A ;;
        riscv64) set -- "$2" 72EC70A6-CF74-40E6-BD49-4BDA08E8F224 B6ED5582-440B-4209-B8DA-5FF7C419EA3D EFE0F087-EA8D-4469-821A-4C2A96A8386A BEAEC34B-8442-439B-A40B-984381ED097D 8F1056BE-9B05-47C4-81D6-BE53128E5B54 D2F9000A-7A18-453F-B5CD-4D32F77A7B32 ;;
        loongarch64) set -- "$2" 77055800-792C-4F94-B39A-98C91B762BB6 F3393B22-E9AF-4613-A948-9D3BFBD0C535 5AFB67EB-ECC8-4F85-AE8E-AC1E7C50E7D0 E611C702-575C-4CBE-9A46-434FA0BF7E3F F46B2C26-59AE-48F0-9106-C50ED47F673D B024F315-D330-444C-8461-44BBDE524E99 ;;
        ppc64-le) set -- "$2" C31C45E6-3F39-412E-80FB-4809C4980599 906BD944-4589-4AAE-A4E4-DD983917446A D4A236E7-E873-4C07-BF1D-BF6CF7F1C3C6 15BB03AF-77E7-4D4A-B12B-C0D084F7491C EE2B9983-21E8-4153-86D9-B6901A54D1CE C8BFBD1E-268E-4521-8BBA-BF314C399557 ;;
        s390x) set -- "$2" 5EEAD9A9-FE09-4A1E-A1D7-520D00531306 B325BFBE-C7BE-4AB8-8357-139E652D2F6B C80187A5-73A3-491A-901A-017C3FA953E9 8A4F5770-50AA-4ED3-874A-99B710DB6FEA 31741CC4-1A2A-4111-A581-E00B447D2D06 3F324816-667B-46AE-86EE-9B0C0C6C11B4 ;;
        s390) set -- "$2" 08A7ACEA-624C-4A20-91E8-6E0FA67D23F9 7AC63B47-B25C-463B-8DF8-B4A94E6C90E1 3482388E-4254-435A-A241-766A065F9960 CD0F869B-D0FB-4CA0-B141-9EA87CC78D66 B663C618-E7BC-4D6D-90AA-11B756BB1797 17440E4F-A8D0-467F-A46E-3912AE6EF2C5 ;;
        *) return 0 ;;
    esac
    case $1 in
        root) echo "$2" ;;
        root-verity) echo "$3" ;;
        root-verity-sig) echo "$4" ;;
        usr) echo "$5" ;;
        usr-verity) echo "$6" ;;
        usr-verity-sig) echo "$7" ;;
    esac
}

ROOT_GUID=$(part_type "$HOST_ARCH" root)
ROOT_VERITY_GUID=$(part_type "$HOST_ARCH" root-verity)
ROOT_VERITY_SIG_GUID=$(part_type "$HOST_ARCH" root-verity-sig)
USR_GUID=$(part_type "$HOST_ARCH" usr)
USR_VERITY_GUID=$(part_type "$HOST_ARCH" usr-verity)
USR_VERITY_SIG_GUID=$(part_type "$HOST_ARCH" usr-verity-sig)
GENERIC_GUID=0FC63DAF-8483-4772-8E79-3D69D8477DE4
ESP_GUID=C12A7328-F81F-11D2-BA4B-00A0C93EC93B

# ---------------------------------------------------------------------------
# Image builders
# ---------------------------------------------------------------------------

# mk_tree DIR NAME [RELEASE] [KIND] — extension payload for NAME in DIR
# (replaced). RELEASE is a printf format for the extension-release file
# (default "ID=_any\nARCHITECTURE=_any\n"). KIND:
#   sysext   usr/lib/extension-release.d/extension-release.NAME,
#            usr/share/NAME/hello.txt ("hello from NAME"), usr/bin/NAME-tool
#   usr      the same without the usr/ prefix (content of a usr partition)
#   confext  etc/extension-release.d/extension-release.NAME,
#            etc/NAME/hello.conf ("conf from NAME")
mk_tree() {
    local dir name rel p
    dir=$1 name=$2 rel=${3:-'ID=_any\nARCHITECTURE=_any\n'}
    rm -rf "$dir"
    case ${4:-sysext} in
        confext)
            mkdir -p "$dir/etc/extension-release.d" "$dir/etc/$name"
            # shellcheck disable=SC2059
            printf "$rel" > "$dir/etc/extension-release.d/extension-release.$name"
            echo "conf from $name" > "$dir/etc/$name/hello.conf"
            return
            ;;
        usr) p=$dir ;;
        *) p=$dir/usr ;;
    esac
    mkdir -p "$p/lib/extension-release.d" "$p/share/$name" "$p/bin"
    # shellcheck disable=SC2059
    printf "$rel" > "$p/lib/extension-release.d/extension-release.$name"
    echo "hello from $name" > "$p/share/$name/hello.txt"
    printf '#!/bin/sh\necho %s-tool\n' "$name" > "$p/bin/$name-tool"
    chmod 0755 "$p/bin/$name-tool"
}

# img_squashfs TREE OUT [mksquashfs args]
img_squashfs() {
    local t o
    t=$1 o=$2
    shift 2
    rm -f "$o"
    mksquashfs "$t" "$o" -noappend -quiet "$@" >/dev/null
}

# img_erofs TREE OUT
img_erofs() {
    rm -f "$2"
    mkfs.erofs "$2" "$1" >/dev/null 2>&1
}

# img_ext4 TREE OUT MIB [mkfs.ext4 args]
img_ext4() {
    local t o m
    t=$1 o=$2 m=$3
    shift 3
    rm -f "$o"
    dd if=/dev/zero of="$o" bs=1M count="$m" status=none
    mkfs.ext4 -q -F "$@" -d "$t" "$o"
}

# part_image IMG SECTOR_SIZE MIB — create IMG and partition it with the
# sfdisk script read from stdin.
part_image() {
    rm -f "$1"
    dd if=/dev/zero of="$1" bs=1M count="$3" status=none
    sfdisk -q --sector-size "$2" "$1" >/dev/null
}

# img_put SRC IMG SECTOR_SIZE START — copy SRC into IMG at sector START.
img_put() {
    dd if="$1" of="$2" bs="$3" seek="$4" conv=notrunc status=none
}

# uuid_of HEX — the first 32 hex digits of HEX as a UUID.
uuid_of() {
    echo "$1" | sed -E 's/^(.{8})(.{4})(.{4})(.{4})(.{12}).*/\1-\2-\3-\4-\5/'
}

# ddi_build OUT TREE [-s SECTOR_SIZE] [-t root|usr] [-a ARCH] [-v] [-g]
#           [-u hash|random] [-V "veritysetup format args"]
# GPT DDI with an 8 MiB ext4 data partition (4096-byte blocks) at 1 MiB for
# ARCH (default: the host), a 4 MiB verity partition at 9 MiB with -v, and
# an empty 1 MiB verity signature partition at 13 MiB with -g (fill it with
# ddi_write_sig). With -u hash (the default) the data and verity partition
# UUIDs carry the root hash. Sets DDI_DATA, DDI_HASH and DDI_ROOTHASH.
ddi_build() {
    local out tree ss kind arch verity sig uuids vargs mib du vu script total o
    out=$1 tree=$2
    shift 2
    ss=512 kind=root arch=$HOST_ARCH verity=0 sig=0 uuids=hash vargs=""
    OPTIND=1
    while getopts s:t:a:vgu:V: o; do
        case $o in
            s) ss=$OPTARG ;;
            t) kind=$OPTARG ;;
            a) arch=$OPTARG ;;
            v) verity=1 ;;
            g) verity=1 sig=1 ;;
            u) uuids=$OPTARG ;;
            V) vargs=$OPTARG ;;
            *) return 1 ;;
        esac
    done
    DDI_DATA=$out.data DDI_HASH=$out.hash DDI_ROOTHASH=""
    img_ext4 "$tree" "$DDI_DATA" 8 -b 4096 || return 1
    mib=$((1024 * 1024 / ss))
    du="" vu=""
    if [ "$verity" = 1 ]; then
        rm -f "$DDI_HASH"
        # shellcheck disable=SC2086
        DDI_ROOTHASH=$(veritysetup format $vargs "$DDI_DATA" "$DDI_HASH" | awk '/^Root hash/{print $3}')
        [ -n "$DDI_ROOTHASH" ] || return 1
        if [ "$uuids" = hash ]; then
            du=", uuid=$(uuid_of "$(echo "$DDI_ROOTHASH" | cut -c1-32)")"
            vu=", uuid=$(uuid_of "$(echo "$DDI_ROOTHASH" | cut -c33-64)")"
        fi
    fi
    script=$(printf 'label: gpt\nstart=%d, size=%d, type=%s%s\n' "$mib" $((8 * mib)) "$(part_type "$arch" "$kind")" "$du")
    total=12
    if [ "$verity" = 1 ]; then
        script=$(printf '%s\nstart=%d, size=%d, type=%s%s\n' "$script" $((9 * mib)) $((4 * mib)) "$(part_type "$arch" "$kind-verity")" "$vu")
        total=14
    fi
    if [ "$sig" = 1 ]; then
        script=$(printf '%s\nstart=%d, size=%d, type=%s\n' "$script" $((13 * mib)) "$mib" "$(part_type "$arch" "$kind-verity-sig")")
        total=16
    fi
    echo "$script" | part_image "$out" "$ss" "$total" || return 1
    dd if="$DDI_DATA" of="$out" bs=1M seek=1 conv=notrunc status=none
    if [ "$verity" = 1 ]; then
        dd if="$DDI_HASH" of="$out" bs=1M seek=9 conv=notrunc status=none
    fi
}

# new_cert NAME [openssl req args] — throwaway self-signed signing
# certificate: $WORKDIR/NAME.key and $WORKDIR/NAME.pem.
new_cert() {
    local n
    n=$1
    shift
    [ $# -gt 0 ] || set -- -days 2
    openssl req -x509 -newkey rsa:2048 -nodes -subj "/CN=$n" \
        -keyout "$WORKDIR/$n.key" -out "$WORKDIR/$n.pem" "$@" 2>/dev/null
}

# smime_sign KEY CERT CONTENT OUT [openssl smime args] — detached DER PKCS#7
# signature over CONTENT (no trailing newline), as the DPS mandates.
smime_sign() {
    local key cert content out
    key=$1 cert=$2 content=$3 out=$4
    shift 4
    printf %s "$content" > "$WORKDIR/.smime-content"
    openssl smime -sign -in "$WORKDIR/.smime-content" -signer "$cert" -inkey "$key" \
        -binary -outform der -noattr "$@" > "$out"
}

# sig_json ROOTHASH DER [CERT_FINGERPRINT] — the signature partition JSON.
sig_json() {
    if [ -n "${3:-}" ]; then
        printf '{"rootHash":"%s","signature":"%s","certificateFingerprint":"%s"}' "$1" "$(openssl base64 -A -in "$2")" "$3"
    else
        printf '{"rootHash":"%s","signature":"%s"}' "$1" "$(openssl base64 -A -in "$2")"
    fi
}

# ddi_write_sig IMG JSONFILE — write JSONFILE into the zeroed signature
# partition of a ddi_build -g image.
ddi_write_sig() {
    dd if=/dev/zero of="$1" bs=1M seek=13 count=1 conv=notrunc status=none
    dd if="$2" of="$1" bs=1M seek=13 conv=notrunc status=none
}

# ---------------------------------------------------------------------------
# Mounts and devices
# ---------------------------------------------------------------------------

# mounts — "MOUNTPOINT FSTYPE SOURCE OPTIONS" per mount, in mount order.
mounts() {
    awk '{for (i = 7; i <= NF; i++) if ($i == "-") { print $5, $(i + 1), $(i + 2), $6; break }}' /proc/self/mountinfo
}

# track_mount PATH — unmount PATH (lazily) when the suite ends.
track_mount() {
    E2E_MOUNTS="$1 $E2E_MOUNTS"
}

# track_root ROOT — unmerge --root=ROOT when the suite ends.
track_root() {
    E2E_ROOTS="$E2E_ROOTS $1"
}

# devnum_of PATH — st_dev of PATH as MAJOR:MINOR.
devnum_of() {
    local d
    d=$(stat -Lc %d "$1") || return 1
    echo "$(((d >> 8) & 0xfff | (d >> 32) & ~0xfff)):$((d & 0xff | (d >> 12) & ~0xff))"
}

E2E_ROOTDEV=$(devnum_of /)

# loop_backing LOOP — "INODE MAJOR:MINOR" of the file backing /dev/LOOP.
loop_backing() {
    local node
    node=/dev/$1
    if [ ! -b "$node" ]; then
        node=$WORKDIR/.node-$1
        rm -f "$node"
        # shellcheck disable=SC2046
        mknod "$node" b $(tr : ' ' < "/sys/block/$1/dev") 2>/dev/null || return 1
    fi
    losetup -nl -O BACK-INO,BACK-MAJ:MIN "$node" 2>/dev/null | awk 'NF == 2 {print $1, $2}'
}

# loop_is_ours LOOP — LOOP is backed by a file of this container: its path
# resolves here to the same inode, or it lived on the container's root
# filesystem (deleted files). Loop devices are host-global; this keeps the
# helpers away from the host's and other containers' devices.
loop_is_ours() {
    local id f
    [ -r "/sys/block/$1/loop/backing_file" ] || return 1
    id=$(loop_backing "$1")
    [ -n "$id" ] || return 1
    f=$(cat "/sys/block/$1/loop/backing_file")
    if [ -e "$f" ] && [ "$id" = "$(stat -Lc %i "$f") $(devnum_of "$f")" ]; then
        return 0
    fi
    [ "${id#* }" = "$E2E_ROOTDEV" ]
}

# our_loops [PREFIX...] — loop devices backed by this container's files
# (below one of the PREFIXes when given), one per line.
# shellcheck disable=SC2120
our_loops() {
    local d n f m p
    for d in /sys/block/loop*; do
        [ -f "$d/loop/backing_file" ] || continue
        n=${d##*/}
        if [ $# -gt 0 ]; then
            f=$(cat "$d/loop/backing_file")
            m=0
            for p in "$@"; do
                case $f in "$p"*) m=1 ;; esac
            done
            [ "$m" = 1 ] || continue
        fi
        loop_is_ours "$n" && echo "$n"
    done
}

# loop_of FILE — the loop device (name) backed by FILE.
loop_of() {
    local d
    for d in /sys/block/loop*; do
        [ -f "$d/loop/backing_file" ] || continue
        case "$(cat "$d/loop/backing_file")" in
            "$1" | "$1 (deleted)") loop_is_ours "${d##*/}" && { echo "${d##*/}"; return 0; } ;;
        esac
    done
    return 1
}

# loop_attach FILE [losetup args] — attach a loop device to FILE (a block
# device image for the binary under test), making its node when the
# container lacks it; prints the node.
loop_attach() {
    local f n
    f=$1
    shift
    n=$(losetup -f) || return 1
    # shellcheck disable=SC2046
    [ -b "$n" ] || mknod "$n" b $(tr : ' ' < "/sys/block/${n#/dev/}/dev") || return 1
    losetup "$@" "$n" "$f" || return 1
    echo "$n"
}

# loop_attached NODE — the loop device NODE is attached.
loop_attached() {
    [ -f "/sys/block/${1#/dev/}/loop/backing_file" ]
}

# our_dm [TARGET] — device-mapper devices (names) stacked on this suite's
# loop devices, optionally only those of TARGET type.
our_dm() {
    local loops d s name
    loops=" $(our_loops | tr '\n' ' ') "
    for d in /sys/block/dm-*; do
        [ -d "$d/slaves" ] || continue
        for s in "$d"/slaves/*; do
            s=${s##*/}
            case $loops in
                *" ${s%p[0-9]*} "*)
                    name=$(cat "$d/dm/name")
                    if [ -z "${1:-}" ] || [ "$(dmsetup table "$name" 2>/dev/null | awk '{print $3; exit}')" = "$1" ]; then
                        echo "$name"
                    fi
                    break
                    ;;
            esac
        done
    done
}

# verity_devs — dm-verity devices of this suite, one per line.
verity_devs() {
    our_dm verity
}

# released — wait (up to 5 s) until the kernel released the loop and dm
# devices of this suite (autoclear, deferred remove) and nothing is mounted
# in the workspace.
released() {
    local i
    i=0
    while [ $i -lt 50 ]; do
        if [ -z "$(our_loops)$(our_dm)" ] && ! mounts | grep -q '^/run/systemd/'; then
            return 0
        fi
        sleep 0.1
        i=$((i + 1))
    done
    return 1
}

# leaked — what released waits for, for failure messages.
leaked() {
    echo "loops=[$(our_loops | tr '\n' ' ')] dm=[$(our_dm | tr '\n' ' ')] mounts=[$(mounts | awk '$1 ~ "^/run/systemd/" {print $1}' | tr '\n' ' ')]"
}

# ---------------------------------------------------------------------------
# Cleanup and summary
# ---------------------------------------------------------------------------

e2e_cleanup() {
    local t r m n node
    for t in $E2E_TOOLS; do
        command -v "$t" >/dev/null 2>&1 || continue
        "$t" unmerge >/dev/null 2>&1
        for r in $E2E_ROOTS; do "$t" --root="$r" unmerge >/dev/null 2>&1; done
    done
    mounts | awk '($2 == "overlay" && ($3 == "sysext" || $3 == "confext")) || $1 ~ "^/run/systemd/" {print $1}' |
        sort -r | while read -r m; do umount -l "$m" 2>/dev/null; done
    for m in $E2E_MOUNTS; do umount -l "$m" 2>/dev/null; done
    for n in $(our_dm); do
        dmsetup remove "$n" >/dev/null 2>&1 || dmsetup remove --deferred "$n" >/dev/null 2>&1
    done
    for n in $(our_loops); do
        node=/dev/$n
        [ -b "$node" ] || node=$WORKDIR/.node-$n
        losetup -d "$node" 2>/dev/null
    done
}

# e2e_check_end_state — the final unmerge works and is idempotent, and the
# binary under test left no overlay, workspace, loop or dm device behind.
e2e_check_end_state() {
    local t out r left
    for t in $E2E_TOOLS; do
        command -v "$t" >/dev/null 2>&1 || continue
        out=$("$t" unmerge 2>&1)
        expect_eq "end state: final $t unmerge" "$?" 0
        out=$("$t" unmerge 2>&1)
        expect_eq "end state: $t unmerge is idempotent" "$?:$out" "0:"
        for r in $E2E_ROOTS; do
            [ -d "$r" ] || continue
            out=$("$t" --root="$r" unmerge 2>&1)
            expect_eq "end state: final $t --root=$r unmerge" "$?" 0
        done
    done
    left=$(mounts | awk '($2 == "overlay" && ($3 == "sysext" || $3 == "confext")) || $1 ~ "^/run/systemd/" {print $1}' | tr '\n' ' ')
    expect_eq "end state: no merged overlays or workspace mounts" "$left" ""
    left=$(find /run/systemd -mindepth 1 -maxdepth 1 \( -name 'sysext*' -o -name 'confext*' \) ! -name '*.lock' 2>/dev/null | tr '\n' ' ')
    expect_eq "end state: no workspace directories in /run/systemd" "$left" ""
    released
    expect_eq "end state: no loop devices backed by suite files" "$(our_loops | tr '\n' ' ')" ""
    expect_eq "end state: no dm devices on suite loop devices" "$(our_dm | tr '\n' ' ')" ""
}

# finish — end-of-suite checks, cleanup, summary; exits.
finish() {
    if [ "$E2E_FINISHED" = 0 ]; then
        E2E_FINISHED=1
        e2e_check_end_state
        e2e_cleanup
    fi
    echo "==========================================="
    echo "Passes: $PASSES  Failures: $FAILS  Skips: $SKIPS"
    echo "E2E-SUMMARY suite=$E2E_SUITE pass=$PASSES fail=$FAILS skip=$SKIPS"
    if [ "$FAILS" -gt 0 ]; then
        echo "RESULT: FAIL"
        exit 1
    fi
    echo "RESULT: PASS"
    exit 0
}

e2e_on_exit() {
    rc=$?
    [ "$E2E_FINISHED" = 1 ] && return
    E2E_FINISHED=1
    trap - EXIT
    echo "FAIL: suite aborted (exit status $rc)"
    FAILS=$((FAILS + 1))
    e2e_cleanup
    echo "==========================================="
    echo "Passes: $PASSES  Failures: $FAILS  Skips: $SKIPS"
    echo "E2E-SUMMARY suite=$E2E_SUITE pass=$PASSES fail=$FAILS skip=$SKIPS"
    echo "RESULT: FAIL"
    exit 1
}

trap e2e_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "=== $E2E_SUITE on $HOST_ARCH ($(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"'), / $(grep -q ' / / [^ ]* shared:' /proc/self/mountinfo && echo shared || echo private)) ==="

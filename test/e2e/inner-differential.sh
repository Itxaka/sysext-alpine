#!/bin/sh
# shellcheck disable=SC3043  # sh is bash on archlinux
# e2e-image: archlinux:latest
# e2e-propagation: shared
# e2e-fixtures: repart
#
# Differential suite against the real systemd-sysext/systemd-confext 262 of
# archlinux:latest. Every scenario runs twice from the same initial state,
# once with systemd's binaries and once with the binary under test installed
# as systemd-sysext/systemd-confext, and the transcripts are compared step by
# step: stdout, stderr and exit status of each command, and the merge state
# (mounts at and below each hierarchy, metadata markers, hierarchy modes,
# routing directories).
#
# Normalised before comparing: timestamps (since, time, crtime, mtime and
# the table form), mount IDs, file handles, JSON key order of the origin
# marker (systemd writes a hash map), dev/backing markers (compared with
# stat), the order in which mountinfo lists preserved submounts, and the
# workspace paths in overlay options and messages: systemd stages in
# /run/systemd/sysext of a private namespace, the binary under test in the
# caller's namespace, one workspace per class and --root
# (/run/systemd/<class>.<hash>) with refreshes staged in <workspace>/next.*.
# Divergences the port intends are listed in ALLOWED with the reason; any
# other difference fails, and so does an ALLOWED entry whose step no longer
# differs.
. /work/test/e2e/lib.sh

echo "=== Installing dependencies ==="
pkg_add squashfs-tools erofs-utils jq diffutils
need_overlayfs

ver=$(pacman -Q systemd 2>/dev/null | awk '{print $2}')
case $ver in
    262 | 262[.-]*) pass "archlinux:latest ships systemd $ver" ;;
    *)
        skip systemd262 "archlinux:latest ships systemd '${ver:-none}', this suite compares against 262"
        finish
        ;;
esac

# systemd-sysext marks /run MS_SLAVE in its private namespace and binds the
# merged trees back, which reaches this namespace only when / is shared.
mountpoint -q /run || mount -t tmpfs -o mode=0755 tmpfs /run || fatal "cannot mount a tmpfs on /run"
# Without udev, partition nodes of loop devices appear in /dev only with a
# devtmpfs there; systemd needs them to dissect GPT images.
[ "$(stat -f -c %T /dev)" = devtmpfs ] || mount -t devtmpfs devtmpfs /dev || fatal "cannot mount devtmpfs on /dev"
mount --make-rshared / || fatal "cannot make / rshared"

GO=/e2e/go
mkdir -p "$GO"
install -m 0755 /work/bin/sysext "$GO/systemd-sysext"
ln -sf systemd-sysext "$GO/systemd-confext"
# shellcheck disable=SC2034  # read by lib.sh
E2E_TOOLS="$GO/systemd-sysext $GO/systemd-confext"
BASEPATH=$PATH
HOST_VERSION_ID=$(. /etc/os-release && echo "$VERSION_ID")
fs_supported squashfs && HAVE_SQUASHFS=1 || HAVE_SQUASHFS=0
fs_supported erofs && HAVE_EROFS=1 || HAVE_EROFS=0
[ "$HAVE_SQUASHFS" = 1 ] || skip fs:squashfs "squashfs unavailable; raw image steps left out"
[ "$HAVE_EROFS" = 1 ] || skip fs:erofs "erofs unavailable; erofs steps left out"
verity_supported && HAVE_VERITY=1 || HAVE_VERITY=0
[ "$HAVE_VERITY" = 1 ] || skip dm-verity "dm-verity unavailable; verity steps left out"

# Verity DDIs get a random salt: build the one both sides use once.
if [ "$HAVE_VERITY" = 1 ]; then
    mk_tree "$WORKDIR/vt" vt 'ID=_any\n'
    ddi_build "$WORKDIR/vt.raw" "$WORKDIR/vt" -v || fatal "building the verity DDI failed"
    VERITY_DDI=$WORKDIR/vt.raw
fi

T=$WORKDIR/transcripts
mkdir -p "$T"
TR=/dev/null
HIERS="/usr /opt"

ALLOWED='
cli/--help|the help text names OpenRC as the service manager reloaded and lists --confext
cli/-h|the help text names OpenRC as the service manager reloaded and lists --confext
cli/help verb|the help text names OpenRC as the service manager reloaded and lists --confext
cli/confext --help|the help text names OpenRC as the service manager reloaded and lists --confext
cli/--version|the version string names this implementation and its feature set
reload/merge with EXTENSION_RELOAD_MANAGER=1|systemd reloads the manager over D-Bus (no bus here: it fails); this implementation uses OpenRC (not running: nothing to do)
reload/unmerge with EXTENSION_RELOAD_MANAGER=1|systemd reloads the manager over D-Bus (no bus here: it fails); this implementation uses OpenRC (not running: nothing to do)
reload/refresh with EXTENSION_RELOAD_MANAGER=1|systemd reloads the manager over D-Bus (no bus here: it fails); this implementation uses OpenRC (not running: nothing to do)
confext/sysext --confext status|--confext selects the confext class without a confext link; systemd only looks at the program name
cli/--confext list|--confext selects the confext class without a confext link; systemd only looks at the program name
precedence/directory and raw image of the same name|systemd keeps whichever entry readdir returns first (file system dependent); this implementation sorts the names
precedence/merge with a directory and a raw image of the same name|systemd keeps whichever entry readdir returns first (file system dependent); this implementation sorts the names
stale/state over stale metadata|systemd lets the copied-up markers shadow the new ones (dev is "1"); this implementation ignores metadata in the routing directory
stale/status over stale metadata|systemd cannot parse the shadowed dev marker; this implementation reports the merge
stale/unmerge over stale metadata|systemd cannot parse the shadowed dev marker and keeps the merge; this implementation unmerges
env/state after a failed merge|systemd loses the submounts of /usr (the /lib/modules volume) when the overlay mount fails; this implementation keeps them
'

allowed() {
    echo "$ALLOWED" | awk -F'|' -v k="$1" '$1 == k {print $2; exit}'
}

# norm — normalise a transcript. libcryptsetup first tries in-kernel
# signature verification, which logs device-mapper errors without a kernel
# keyring entry; the in-kernel keyring is out of scope here, so those lines
# are dropped.
norm() {
    sed -E \
        -e '/^device-mapper: reload ioctl on .* failed: Required key not available$/d' \
        -e 's/("(since|time|crtime|mtime|onMountId)" ?: ?)[1-9][0-9]*/\1N/g' \
        -e 's/("handle" ?: ?)"[0-9a-f]+"/\1"H"/g' \
        -e 's/(Mon|Tue|Wed|Thu|Fri|Sat|Sun) [0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} [A-Z]+/TIMESTAMP/g' \
        -e 's#/run/systemd/(sysext|confext)(\.[0-9a-f]{16})?(/next\.[0-9]+)?/meta/#WORKSPACE/meta/#g' \
        -e 's#/run/systemd/(sysext|confext)(\.[0-9a-f]{16})?(/next\.[0-9]+)?/(extensions|confexts)/#WORKSPACE/images/#g' \
        -e 's#/run/systemd/(sysext|confext)\.[0-9a-f]{16}#/run/systemd/\1#g'
}

# run STEP CMD... — record CMD's stdout, stderr and exit status.
run() {
    local st rc
    st=$1
    shift
    "$@" > "$WORKDIR/.out" 2> "$WORKDIR/.err" < /dev/null
    rc=$?
    {
        echo "### $st"
        echo "\$ $*"
        cat "$WORKDIR/.out"
        echo "--- stderr"
        cat "$WORKDIR/.err"
        echo "--- exit status $rc"
    } | norm >> "$TR"
    return $rc
}

# origin_shape FILE — the origin marker's layout with strings and numbers
# blanked: indentation, separators and nesting must match systemd's.
origin_shape() {
    tr '\n' '|' < "$1" | sed -E 's/"[^"]*"/"s"/g; s/[0-9]+/0/g'
}

# mounts_at PATH — the mounts at and below PATH: mount point, type, source,
# mount flags and superblock options. Sorted by mount point (mounts stacked
# on one mount point keep their order): the implementations may list
# preserved submounts before or after the overlay.
mounts_at() {
    awk -v m="$1" '{
        for (i = 7; i <= NF; i++) if ($i == "-") break
        if ($5 == m || index($5, m "/") == 1) print "  mount " $5 ": " $(i + 1) " " $(i + 2) " " $6 " " $(i + 3)
    }' /proc/self/mountinfo | sort -s -k2,2
}

# state STEP [ROOT] — record the merge state of $HIERS below ROOT.
state() {
    local st root h p c d f v
    st=$1 root=${2:-}
    {
        echo "### $st"
        for h in $HIERS; do
            p=$root$h
            if [ ! -e "$p" ] && [ ! -L "$p" ]; then
                echo "$h: missing"
                continue
            fi
            if [ -L "$p" ]; then
                echo "$h: symlink to $(readlink "$p")"
                continue
            fi
            echo "$h: mode $(stat -c %a "$p")"
            mounts_at "$p"
            for c in sysext confext; do
                d=$p/.systemd-$c
                [ -d "$d" ] || continue
                for f in $(ls -A "$d" | sort); do
                    case $f in
                        dev)
                            v=$(cat "$d/dev")
                            [ "$v" = "$(devnum_of "$p")" ] && v="st_dev of the merged $h"
                            ;;
                        backing)
                            v=$(cat "$d/backing")
                            [ -n "$root" ] && [ "$v" = "$(devnum_of "$root")" ] && v="device of the root file system"
                            ;;
                        origin) v="$(origin_shape "$d/origin") $(jq -S -c . "$d/origin" 2>&1)" ;;
                        *) v=$(tr '\n' ' ' < "$d/$f") ;;
                    esac
                    echo "  $c/$f: $v"
                done
                if touch "$d/.e2e-probe" 2>/dev/null; then
                    rm -f "$d/.e2e-probe"
                    echo "  $c metadata writable"
                else
                    echo "  $c metadata read-only"
                fi
            done
        done
        if [ -d "$root/var/lib/extensions.mutable" ]; then
            echo "routing directory:"
            (cd "$root/var/lib/extensions.mutable" && find . -mindepth 1 | sort | sed 's/^/  /')
        fi
    } | norm >> "$TR"
}

# expect_mounted PATH — sanity check on each side: a merge that should have
# worked is visible here (otherwise both sides could agree on nothing).
expect_mounted() {
    if mountpoint -q "$1"; then pass "$SIDE: $1 merged"; else fail "$SIDE: $1 not merged"; fi
}

# dext [-d DIR] [-r RELNAME] NAME [RELEASE [PATH...]] — directory sysext
# NAME in DIR (default /var/lib/extensions) with its release file named
# after RELNAME (default NAME); each PATH (default usr/share/NAME/f) contains
# NAME. RELEASE is a printf format (default "ID=_any\n").
dext() {
    local xdir xrel xcls o n rel d rd f
    xdir=/var/lib/extensions xrel="" xcls=sysext
    OPTIND=1
    while getopts d:r:c o; do
        case $o in
            d) xdir=$OPTARG ;;
            r) xrel=$OPTARG ;;
            c) xcls=confext ;;
            *) return 1 ;;
        esac
    done
    shift $((OPTIND - 1))
    n=$1 rel=${2:-'ID=_any\n'}
    shift
    [ $# -gt 0 ] && shift
    d=$xdir/$n
    rm -rf "$d"
    if [ "$xcls" = confext ]; then rd=$d/etc/extension-release.d; else rd=$d/usr/lib/extension-release.d; fi
    mkdir -p "$rd"
    # shellcheck disable=SC2059
    printf "$rel" > "$rd/extension-release.${xrel:-$n}"
    if [ $# -eq 0 ]; then
        if [ "$xcls" = confext ]; then set -- "etc/$n/f"; else set -- "usr/share/$n/f"; fi
    fi
    for f in "$@"; do
        mkdir -p "$d/$(dirname "$f")"
        echo "$n" > "$d/$f"
    done
}

# cext [-d DIR] [-r RELNAME] NAME [RELEASE [PATH...]] — directory confext,
# like dext (default DIR /var/lib/confexts, PATH etc/NAME/f).
cext() {
    local xd xr o
    OPTIND=1
    xd=/var/lib/confexts xr=""
    while getopts d:r: o; do
        case $o in
            d) xd=$OPTARG ;;
            r) xr=$OPTARG ;;
            *) return 1 ;;
        esac
    done
    shift $((OPTIND - 1))
    if [ -n "$xr" ]; then dext -c -d "$xd" -r "$xr" -- "$@"; else dext -c -d "$xd" -- "$@"; fi
}

# rext FILE NAME [RELEASE [FS]] — raw sysext image FILE (squashfs or erofs)
# whose release file is named after NAME; the tree is $WORKDIR/trees/NAME.
rext() {
    local file rn rrel fs
    file=$1 rn=$2 rrel=${3:-'ID=_any\n'} fs=${4:-squashfs}
    dext -d "$WORKDIR/trees" -- "$rn" "$rrel"
    mkdir -p "$(dirname "$file")"
    if [ "$fs" = erofs ]; then
        img_erofs "$WORKDIR/trees/$rn" "$file"
    else
        img_squashfs "$WORKDIR/trees/$rn" "$file"
    fi
}

base_mounts() {
    mounts | awk '$1 ~ "^/(usr|opt|etc)(/|$)" {print $1, $2}' | sort
}
BASE_MOUNTS=$(base_mounts)

# Keep a private bind of every mount below the hierarchies (docker's
# /etc/hostname, the /lib/modules volume): systemd 262 loses submounts when a
# merge fails after moving them, and the steps after it and the next run must
# start from the same state. restore_mounts (called by reset and after such a
# step) binds lost ones back.
KEEP=/e2e/keep
mkdir -p "$KEEP"
: > "$KEEP/list"
i=0
for m in $(mounts | awk '$1 ~ "^/(usr|opt|etc)/" {print $1}'); do
    i=$((i + 1))
    if [ -d "$m" ]; then mkdir -p "$KEEP/$i"; else touch "$KEEP/$i"; fi
    mount --bind "$m" "$KEEP/$i" && mount --make-private "$KEEP/$i" && echo "$m $KEEP/$i" >> "$KEEP/list"
done

restore_mounts() {
    while read -r m k; do
        mounts | awk -v m="$m" '$1 == m {f = 1} END {exit !f}' || mount --bind "$k" "$m"
    done < "$KEEP/list"
}

# reset — the initial state of every scenario run.
reset() {
    local t m
    PATH=$BASEPATH
    for t in systemd-sysext systemd-confext "$GO/systemd-sysext" "$GO/systemd-confext"; do
        "$t" unmerge >/dev/null 2>&1
        [ -d /x ] && "$t" --root=/x unmerge >/dev/null 2>&1
    done
    mounts | awk '$2 == "overlay" && ($3 == "sysext" || $3 == "confext") {print $1}' | sort -r |
        while read -r m; do umount -l "$m"; done
    while mountpoint -q /x 2>/dev/null; do umount -l /x; done
    while mountpoint -q /var/lib 2>/dev/null; do umount -l /var/lib; done
    if [ -L /opt ]; then
        rm -f /opt
        mv /opt.saved /opt
        rm -f /var/opt/hostfile
    fi
    rm -rf /x /var/lib/extensions /var/lib/confexts /var/lib/extensions.mutable /run/extensions /etc/extensions \
        /run/confexts /etc/systemd/sysext.conf /etc/systemd/sysext.conf.d /etc/systemd/confext.conf \
        /etc/systemd/confext.conf.d /etc/verity.d "$WORKDIR/trees" /srv/* /opt/* 2>/dev/null
    mkdir -p /var/lib/extensions /var/lib/confexts
    restore_mounts
    if [ "$(base_mounts)" != "$BASE_MOUNTS" ]; then
        fail "reset: mounts below the hierarchies changed: [$(base_mounts | tr '\n' ' ')], want [$(echo "$BASE_MOUNTS" | tr '\n' ' ')]"
    fi
    HIERS="/usr /opt"
}

# mkroot [ext4] — a fresh root at /x (tmpfs, or ext4 on a loop device).
mkroot() {
    mkdir -p /x
    if [ "${1:-}" = ext4 ]; then
        truncate -s 64M "$WORKDIR/root.img"
        mkfs.ext4 -qF "$WORKDIR/root.img"
        mount -o loop "$WORKDIR/root.img" /x || fail "mount the ext4 root"
    else
        mount -t tmpfs -o mode=0755 tmpfs /x || fail "mount the tmpfs root"
    fi
    mkdir -p /x/usr/lib /x/usr/share /x/etc /x/opt /x/var/lib/extensions /x/var/lib/confexts
    printf 'ID=testos\nVERSION_ID=1\n' > /x/etc/os-release
}
track_root /x
track_mount /x

split_steps() {
    rm -rf "$2"
    mkdir -p "$2"
    awk -v d="$2" '/^### / {if (f) close(f); n++; f = sprintf("%s/%03d", d, n)} f {print > f}' "$1"
}

# compare SCENARIO — compare both transcripts step by step.
compare() {
    local sd go f st key reason
    sd=$T/$1.sd.d go=$T/$1.go.d
    split_steps "$T/$1.sd" "$sd"
    split_steps "$T/$1.go" "$go"
    for f in $( (ls "$sd"; ls "$go") | sort -u); do
        st=$(head -n 1 "$sd/$f" 2>/dev/null || head -n 1 "$go/$f")
        key="$1/${st#\#\#\# }"
        reason=$(allowed "$key")
        if [ -f "$sd/$f" ] && [ -f "$go/$f" ] && cmp -s "$sd/$f" "$go/$f"; then
            if [ -n "$reason" ]; then
                fail "$key: identical to systemd 262, drop its ALLOWED entry"
            else
                pass "$key"
            fi
            continue
        fi
        if [ -n "$reason" ]; then
            pass "$key: intended divergence: $reason"
        else
            fail "$key differs from systemd 262 (- systemd, + this implementation):"
        fi
        diff -u --label systemd --label sysext "$sd/$f" "$go/$f" 2>/dev/null | tail -n +3 | sed 's/^/    /'
    done
}

# scenario NAME — run sc_NAME with systemd and with the binary under test.
scenario() {
    echo "--- scenario $1 ---"
    for SIDE in sd go; do
        reset
        TR=$T/$1.$SIDE
        : > "$TR"
        if [ "$SIDE" = go ]; then PATH=$GO:$BASEPATH; else PATH=$BASEPATH; fi
        export PATH
        "sc_$1"
        PATH=$BASEPATH
    done
    reset
    TR=/dev/null
    compare "$1"
}

# ---------------------------------------------------------------------------
# Scenarios
# ---------------------------------------------------------------------------

sc_basic() {
    run "list without images" systemd-sysext list
    run "list --json=short without images" systemd-sysext list --json=short
    run "status without a merge" systemd-sysext status
    run "status --json=short without a merge" systemd-sysext status --json=short
    run "status --json=pretty without a merge" systemd-sysext status --json=pretty
    run "unmerge without a merge" systemd-sysext unmerge
    dext foo 'ID=_any\n' usr/share/foo/f opt/foo/f
    dext bar
    [ "$HAVE_SQUASHFS" = 1 ] && rext /var/lib/extensions/sq.raw sq
    [ "$HAVE_EROFS" = 1 ] && rext /var/lib/extensions/ero.raw ero 'ID=_any\n' erofs
    run "list" systemd-sysext list
    run "list --no-legend" systemd-sysext list --no-legend
    run "list --json=short" systemd-sysext list --json=short
    run "list --json=pretty" systemd-sysext list --json=pretty
    run "merge" systemd-sysext merge
    expect_mounted /usr
    state "state after merge"
    run "merged payload" cat /usr/share/foo/f /opt/foo/f /usr/share/bar/f
    run "status" systemd-sysext status
    run "status --no-legend" systemd-sysext status --no-legend
    run "status --json=short" systemd-sysext status --json=short
    run "status --json=pretty" systemd-sysext status --json=pretty
    run "no verb is status" systemd-sysext
    run "merge while merged" systemd-sysext merge
    run "refresh without changes" systemd-sysext refresh
    touch /var/lib/extensions/bar
    run "refresh after touching a directory image" systemd-sysext refresh
    dext bar 'ID=_any\n' usr/share/bar/f usr/share/bar/g
    run "refresh after replacing an image" systemd-sysext refresh
    state "state after refresh"
    run "refresh --always-refresh=yes" systemd-sysext refresh --always-refresh=yes
    run "unmerge" systemd-sysext unmerge
    state "state after unmerge"
    run "payload gone" ls /usr/share/foo
    run "unmerge again" systemd-sysext unmerge
    rm -rf /var/lib/extensions/*
    run "refresh without images" systemd-sysext refresh
}

sc_incompatible() {
    HIERS=/usr
    dext good
    dext fedora 'ID=fedora\nVERSION_ID=40\n'
    dext oldver 'ID=arch\nVERSION_ID=1\n'
    dext samever "ID=arch\nVERSION_ID=$HOST_VERSION_ID\n"
    dext initrd 'ID=_any\nSYSEXT_SCOPE=initrd\n'
    dext portable 'ID=_any\nSYSEXT_SCOPE=portable\n'
    dext sysscope 'ID=_any\nSYSEXT_SCOPE=system portable\n'
    dext foreign "ID=_any\nARCHITECTURE=$FOREIGN_ARCH\n"
    dext native "ID=_any\nARCHITECTURE=$HOST_ARCH\n"
    dext badarch 'ID=_any\nARCHITECTURE=pdp11\n'
    dext noid 'VERSION_ID=1\n'
    mkdir -p /var/lib/extensions/norel/usr/share/norel
    run "merge skips incompatible images" systemd-sysext merge
    expect_mounted /usr
    state "state with incompatible images"
    run "unmerge" systemd-sysext unmerge
    rm -rf /var/lib/extensions/good /var/lib/extensions/native /var/lib/extensions/samever /var/lib/extensions/sysscope
    run "merge with only incompatible images" systemd-sysext merge
    run "refresh with only incompatible images" systemd-sysext refresh
    run "--force merge" systemd-sysext --force merge
    state "state after --force merge"
    run "unmerge after --force" systemd-sysext unmerge
    rm -rf /var/lib/extensions/*
    dext osr
    echo ID=evil > /var/lib/extensions/osr/usr/lib/os-release
    run "directory image shipping os-release" systemd-sysext merge
    run "--force with a directory image shipping os-release" systemd-sysext --force merge
    systemd-sysext unmerge >/dev/null 2>&1
    rm -rf /var/lib/extensions/osr
    if [ "$HAVE_SQUASHFS" = 1 ]; then
        dext ok
        rext /var/lib/extensions/ros.raw ros
        t=$WORKDIR/trees/ros
        echo ID=evil > "$t/usr/lib/os-release"
        img_squashfs "$t" /var/lib/extensions/ros.raw
        run "raw image shipping os-release" systemd-sysext merge
        run "unmerge after a raw image shipping os-release" systemd-sysext unmerge
        run "--force with a raw image shipping os-release" systemd-sysext --force merge
        run "host os-release after --force" grep ^ID= /usr/lib/os-release
        run "unmerge after --force with os-release" systemd-sysext unmerge
        rm -f /var/lib/extensions/ros.raw
        mkdir -p "$WORKDIR/trees/nr/usr/share/nr"
        img_squashfs "$WORKDIR/trees/nr" /var/lib/extensions/nr.raw
        run "raw image without release data" systemd-sysext merge
        run "--force with a raw image without release data" systemd-sysext --force merge
        systemd-sysext unmerge >/dev/null 2>&1
        rm -f /var/lib/extensions/nr.raw
        head -c 1048576 /dev/zero > /var/lib/extensions/zero.raw
        run "raw image without a file system" systemd-sysext merge
        systemd-sysext unmerge >/dev/null 2>&1
    fi
}

# ccase STEP HOST_OS_RELEASE EXT_RELEASE [NAME [RELNAME]] — merge one
# directory sysext into /x and record whether it was merged.
ccase() {
    printf "$2" > /x/etc/os-release
    rm -rf /x/var/lib/extensions/*
    dext -d /x/var/lib/extensions -r "${5:-${4:-c}}" -- "${4:-c}" "$3"
    run "$1" systemd-sysext --root=/x merge
    run "$1: merged trees" ls /x/usr/share
    systemd-sysext --root=/x unmerge >/dev/null 2>&1
}

sc_compat() {
    mkroot
    H='ID=testos\nVERSION_ID=1\n'
    ccase "same ID and VERSION_ID" "$H" 'ID=testos\nVERSION_ID=1\n'
    ccase "other VERSION_ID" "$H" 'ID=testos\nVERSION_ID=2\n'
    ccase "other ID" "$H" 'ID=other\nVERSION_ID=1\n'
    ccase "ID=_any" "$H" 'ID=_any\n'
    ccase "no ID" "$H" 'VERSION_ID=1\n'
    ccase "no VERSION_ID" "$H" 'ID=testos\n'
    ccase "quoted values" "$H" 'ID="testos"\nVERSION_ID='"'"'1'"'"'\n'
    ccase "ID_LIKE match" 'ID=testos\nID_LIKE="debian alpine"\nVERSION_ID=1\n' 'ID=alpine\nVERSION_ID=1\n'
    ccase "ID_LIKE without match" 'ID=testos\nID_LIKE="debian alpine"\nVERSION_ID=1\n' 'ID=fedora\nVERSION_ID=1\n'
    ccase "rolling host, extension without version" 'ID=rolling\n' 'ID=rolling\n'
    ccase "rolling host, extension with VERSION_ID" 'ID=rolling\n' 'ID=rolling\nVERSION_ID=5\n'
    ccase "rolling host, extension with SYSEXT_LEVEL" 'ID=rolling\n' 'ID=rolling\nSYSEXT_LEVEL=1\n'
    L='ID=testos\nVERSION_ID=1\nSYSEXT_LEVEL=2\n'
    ccase "same SYSEXT_LEVEL" "$L" 'ID=testos\nSYSEXT_LEVEL=2\n'
    ccase "other SYSEXT_LEVEL" "$L" 'ID=testos\nSYSEXT_LEVEL=3\n'
    ccase "host level, extension VERSION_ID only" "$L" 'ID=testos\nVERSION_ID=1\n'
    ccase "host level, other VERSION_ID only" "$L" 'ID=testos\nVERSION_ID=9\n'
    ccase "extension level, host without level" "$H" 'ID=testos\nSYSEXT_LEVEL=2\nVERSION_ID=1\n'
    ccase "extension level only, host without level" "$H" 'ID=testos\nSYSEXT_LEVEL=2\n'
    ccase "host level without VERSION_ID" 'ID=testos\nSYSEXT_LEVEL=2\n' 'ID=testos\nSYSEXT_LEVEL=2\n'
    ccase "ARCHITECTURE=_any" "$H" 'ID=_any\nARCHITECTURE=_any\n'
    ccase "host ARCHITECTURE" "$H" "ID=_any\nARCHITECTURE=$HOST_ARCH\n"
    ccase "foreign ARCHITECTURE" "$H" "ID=_any\nARCHITECTURE=$FOREIGN_ARCH\n"
    ccase "invalid ARCHITECTURE" "$H" 'ID=_any\nARCHITECTURE=pdp11\n'
    ccase "SYSEXT_SCOPE=system" "$H" 'ID=_any\nSYSEXT_SCOPE=system\n'
    ccase "SYSEXT_SCOPE=initrd" "$H" 'ID=_any\nSYSEXT_SCOPE=initrd\n'
    ccase "SYSEXT_SCOPE=portable" "$H" 'ID=_any\nSYSEXT_SCOPE=portable\n'
    ccase "SYSEXT_SCOPE with several scopes" "$H" 'ID=_any\nSYSEXT_SCOPE=initrd system\n'
    touch /x/etc/initrd-release
    ccase "initrd root: SYSEXT_SCOPE=initrd" "$H" 'ID=_any\nSYSEXT_SCOPE=initrd\n'
    ccase "initrd root: SYSEXT_SCOPE=system" "$H" 'ID=_any\nSYSEXT_SCOPE=system\n'
    rm -f /x/etc/initrd-release
    ccase "versioned name, base name release file" "$H" 'ID=_any\n' c_1.2 c
    ccase "versioned name, full name release file" "$H" 'ID=_any\n' c_1.2 c_1.2
    ccase "release file of another name" "$H" 'ID=_any\n' c other
    rm -rf /x/var/lib/extensions/*
    dext -d /x/var/lib/extensions -r other x
    setfattr -n user.extension-release.strict -v false /x/var/lib/extensions/x/usr/lib/extension-release.d/extension-release.other
    run "strict xattr false" systemd-sysext --root=/x merge
    run "strict xattr false: merged trees" ls /x/usr/share
    systemd-sysext --root=/x unmerge >/dev/null 2>&1
    setfattr -n user.extension-release.strict -v true /x/var/lib/extensions/x/usr/lib/extension-release.d/extension-release.other
    run "strict xattr true" systemd-sysext --root=/x merge
    systemd-sysext --root=/x unmerge >/dev/null 2>&1
    setfattr -n user.extension-release.strict -v false /x/var/lib/extensions/x/usr/lib/extension-release.d/extension-release.other
    cp -a /x/var/lib/extensions/x/usr/lib/extension-release.d/extension-release.other /x/var/lib/extensions/x/usr/lib/extension-release.d/extension-release.another
    run "two release files with strict xattr false" systemd-sysext --root=/x merge
    systemd-sysext --root=/x unmerge >/dev/null 2>&1
    if [ "$HAVE_SQUASHFS" = 1 ]; then
        rm -rf /x/var/lib/extensions/*
        rext /x/var/lib/extensions/v_2.raw v
        run "raw versioned name, base name release file" systemd-sysext --root=/x merge
        systemd-sysext --root=/x unmerge >/dev/null 2>&1
        rm -f /x/var/lib/extensions/v_2.raw
        rext /x/var/lib/extensions/w.raw other
        t=$WORKDIR/trees/other
        setfattr -n user.extension-release.strict -v false "$t/usr/lib/extension-release.d/extension-release.other"
        img_squashfs "$t" /x/var/lib/extensions/w.raw -xattrs
        run "raw image with strict xattr false" systemd-sysext --root=/x merge
        run "raw image with strict xattr false: merged trees" ls /x/usr/share
        systemd-sysext --root=/x unmerge >/dev/null 2>&1
    fi
    rm -rf /x/var/lib/extensions/*
    dext -d /x/var/lib/extensions c
    printf 'VERSION_ID=1\n' > /x/etc/os-release
    run "host os-release without ID" systemd-sysext --root=/x merge
    rm -f /x/etc/os-release
    run "no host os-release" systemd-sysext --root=/x merge
    printf 'ID=testos\n' > /x/usr/lib/os-release
    run "host os-release in usr/lib" systemd-sysext --root=/x merge
    systemd-sysext --root=/x unmerge >/dev/null 2>&1
    ln -s /usr/lib/os-release /x/etc/os-release
    dext -d /x/var/lib/extensions c 'ID=testos\n'
    run "absolute os-release symlink resolved in the root" systemd-sysext --root=/x merge
    systemd-sysext --root=/x unmerge >/dev/null 2>&1
    printf 'ID=elsewhere\n' > /x/etc/alt-os-release
    dext -d /x/var/lib/extensions c 'ID=elsewhere\n'
    run "SYSTEMD_OS_RELEASE" env SYSTEMD_OS_RELEASE=/etc/alt-os-release systemd-sysext --root=/x merge
    systemd-sysext --root=/x unmerge >/dev/null 2>&1
    rm -rf /x/var/lib/extensions/*
    printf 'ID=testos\nVERSION_ID=1\nCONFEXT_LEVEL=3\nSYSEXT_LEVEL=2\n' > /x/usr/lib/os-release
    rm -f /x/etc/os-release
    cext -d /x/var/lib/confexts lv 'ID=testos\nCONFEXT_LEVEL=3\n'
    run "confext: same CONFEXT_LEVEL" systemd-confext --root=/x merge
    systemd-confext --root=/x unmerge >/dev/null 2>&1
    cext -d /x/var/lib/confexts lv 'ID=testos\nCONFEXT_LEVEL=2\n'
    run "confext: other CONFEXT_LEVEL" systemd-confext --root=/x merge
    systemd-confext --root=/x unmerge >/dev/null 2>&1
    cext -d /x/var/lib/confexts lv 'ID=testos\nCONFEXT_SCOPE=initrd\n'
    run "confext: CONFEXT_SCOPE=initrd without a level" systemd-confext --root=/x merge
    systemd-confext --root=/x unmerge >/dev/null 2>&1
    cext -d /x/var/lib/confexts lv 'ID=testos\nCONFEXT_LEVEL=3\nCONFEXT_SCOPE=initrd\n'
    run "confext: CONFEXT_SCOPE=initrd" systemd-confext --root=/x merge
    systemd-confext --root=/x unmerge >/dev/null 2>&1
    cext -d /x/var/lib/confexts lv 'ID=testos\nCONFEXT_LEVEL=3\nCONFEXT_SCOPE=system\n'
    run "confext: CONFEXT_SCOPE=system" systemd-confext --root=/x merge
    run "confext: CONFEXT_SCOPE=system: merged trees" ls /x/etc
    systemd-confext --root=/x unmerge >/dev/null 2>&1
}

sc_mutable() {
    mount -t tmpfs -o mode=0755 tmpfs /var/lib
    mkdir -p /var/lib/extensions
    mkdir -p /opt/host
    echo host > /opt/host/f
    dext m
    for mode in no auto yes import ephemeral ephemeral-import; do
        rm -rf /var/lib/extensions.mutable
        mkdir -p /var/lib/extensions.mutable/usr
        echo seeded > /var/lib/extensions.mutable/usr/seeded
        run "merge --mutable=$mode" systemd-sysext --mutable=$mode merge
        state "state with --mutable=$mode"
        run "seeded file with --mutable=$mode" cat /usr/seeded
        run "write /usr with --mutable=$mode" sh -c 'echo w > /usr/written && echo written'
        run "write /opt with --mutable=$mode" sh -c 'echo w > /opt/written && echo written'
        run "status with --mutable=$mode" systemd-sysext status --json=short
        run "refresh with --mutable=$mode" systemd-sysext --mutable=$mode refresh
        run "refresh with another mode than --mutable=$mode" systemd-sysext --mutable=no refresh
        run "unmerge after --mutable=$mode" systemd-sysext unmerge
        state "state after unmerging --mutable=$mode"
    done
    rm -rf /var/lib/extensions.mutable
    run "merge --mutable=auto without a routing directory" systemd-sysext --mutable=auto merge
    state "state with --mutable=auto without a routing directory"
    systemd-sysext unmerge >/dev/null 2>&1
    run "merge --mutable=yes without a routing directory" systemd-sysext --mutable=yes merge
    state "state with --mutable=yes without a routing directory"
    systemd-sysext unmerge >/dev/null 2>&1
    rm -rf /var/lib/extensions.mutable
    mkdir -p /var/lib/extensions.mutable/usr
    chmod 0700 /var/lib/extensions.mutable/usr
    run "routing directory with another mode" systemd-sysext --mutable=auto merge
    systemd-sysext unmerge >/dev/null 2>&1
    rm -rf /var/lib/extensions.mutable
    (umask 077 && run "merge --mutable=yes under umask 077" systemd-sysext --mutable=yes merge)
    state "state under umask 077"
    systemd-sysext unmerge >/dev/null 2>&1
    rm -rf /var/lib/extensions/m
    run "merge --mutable=yes without extensions" systemd-sysext --mutable=yes merge
    state "state of a mutable merge without extensions"
    run "status of a mutable merge without extensions" systemd-sysext status --json=short
    run "refresh of a mutable merge without extensions" systemd-sysext --mutable=yes refresh
    run "unmerge of a mutable merge without extensions" systemd-sysext unmerge
    run "merge --mutable=no without extensions" systemd-sysext --mutable=no merge
    run "--mutable=bogus" systemd-sysext --mutable=bogus merge
}

# A metadata directory copied up into the routing directory by an older
# merge. In a tmpfs root: systemd cannot unmerge the result, and forcing the
# overlay away must not take the container's submounts with it.
sc_stale() {
    mkroot
    printf 'ID=testos\n' > /x/etc/os-release
    dext -d /x/var/lib/extensions m
    mkdir -p /x/var/lib/extensions.mutable/usr/.systemd-sysext
    echo 1 > /x/var/lib/extensions.mutable/usr/.systemd-sysext/dev
    run "merge --mutable=yes over stale metadata" systemd-sysext --root=/x --mutable=yes merge
    state "state over stale metadata" /x
    run "status over stale metadata" systemd-sysext --root=/x status --json=short
    run "unmerge over stale metadata" systemd-sysext --root=/x unmerge
}

sc_env() {
    dext foo 'ID=_any\n' usr/share/foo/f opt/foo/f srv/h1/x srv/a/b/y
    run "empty SYSTEMD_SYSEXT_HIERARCHIES" env SYSTEMD_SYSEXT_HIERARCHIES= systemd-sysext merge
    run "relative SYSTEMD_SYSEXT_HIERARCHIES" env SYSTEMD_SYSEXT_HIERARCHIES=usr systemd-sysext merge
    run "SYSTEMD_SYSEXT_HIERARCHIES with a relative entry" env SYSTEMD_SYSEXT_HIERARCHIES=/usr:opt systemd-sysext status
    run "duplicate hierarchies" env SYSTEMD_SYSEXT_HIERARCHIES=/usr:/usr systemd-sysext status --json=short
    run "missing hierarchy" env SYSTEMD_SYSEXT_HIERARCHIES=/usr:/nonexistent systemd-sysext status --json=short
    run "unnormalised hierarchy" env SYSTEMD_SYSEXT_HIERARCHIES=/usr//./ systemd-sysext status --json=short
    HIERS="/usr /srv/h1 /srv/none /srv/a/b"
    run "custom hierarchies" env SYSTEMD_SYSEXT_HIERARCHIES=/usr:/srv/h1:/srv/none:/srv/a/b systemd-sysext merge
    state "state with custom hierarchies"
    run "status with custom hierarchies" env SYSTEMD_SYSEXT_HIERARCHIES=/usr:/srv/h1:/srv/none:/srv/a/b systemd-sysext status --json=short
    run "partial unmerge" env SYSTEMD_SYSEXT_HIERARCHIES=/srv/h1 systemd-sysext unmerge
    state "state after a partial unmerge"
    run "unmerge with custom hierarchies" env SYSTEMD_SYSEXT_HIERARCHIES=/usr:/srv/h1:/srv/none:/srv/a/b systemd-sysext unmerge
    state "state after unmerging custom hierarchies"
    HIERS="/usr /opt"
    mkdir -p /etc/systemd
    printf '[SysExt]\nMutable=ephemeral\n' > /etc/systemd/sysext.conf
    run "configuration Mutable=ephemeral" systemd-sysext merge
    state "state with configuration Mutable=ephemeral"
    systemd-sysext unmerge >/dev/null 2>&1
    run "SYSTEMD_SYSEXT_MUTABLE_MODE=no beats the configuration" env SYSTEMD_SYSEXT_MUTABLE_MODE=no systemd-sysext merge
    state "state with SYSTEMD_SYSEXT_MUTABLE_MODE=no"
    systemd-sysext unmerge >/dev/null 2>&1
    mkdir -p /etc/systemd/sysext.conf.d
    printf '[SysExt]\nMutable=no\n' > /etc/systemd/sysext.conf.d/50-no.conf
    run "drop-in beats the main configuration" systemd-sysext merge
    state "state with a drop-in"
    systemd-sysext unmerge >/dev/null 2>&1
    rm -rf /etc/systemd/sysext.conf /etc/systemd/sysext.conf.d
    run "SYSTEMD_SYSEXT_MUTABLE_MODE=ephemeral" env SYSTEMD_SYSEXT_MUTABLE_MODE=ephemeral systemd-sysext merge
    state "state with SYSTEMD_SYSEXT_MUTABLE_MODE=ephemeral"
    systemd-sysext unmerge >/dev/null 2>&1
    run "--mutable= beats SYSTEMD_SYSEXT_MUTABLE_MODE" env SYSTEMD_SYSEXT_MUTABLE_MODE=ephemeral systemd-sysext --mutable=no merge
    state "state with --mutable=no over the environment"
    systemd-sysext unmerge >/dev/null 2>&1
    run "invalid SYSTEMD_SYSEXT_MUTABLE_MODE" env SYSTEMD_SYSEXT_MUTABLE_MODE=bogus systemd-sysext merge
    systemd-sysext unmerge >/dev/null 2>&1
    run "SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS" env SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS=noatime,xino=off systemd-sysext merge
    state "state with overlayfs mount options"
    run "refresh with other mount options" systemd-sysext refresh
    state "state after refreshing with other mount options"
    systemd-sysext unmerge >/dev/null 2>&1
    run "invalid overlayfs mount option" env SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS=bogus=1 systemd-sysext merge
    state "state after a failed merge"
    systemd-sysext unmerge >/dev/null 2>&1
    restore_mounts
    run "empty SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS with --mutable=ephemeral" env SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS= systemd-sysext --mutable=ephemeral merge
    state "state with empty overlayfs mount options"
    systemd-sysext unmerge >/dev/null 2>&1
    mkdir -p /etc/systemd
    printf '[SysExt]\nMutable=bogus\nImagePolicy=nonsense\nUnknown=1\n[Other]\nX=1\n' > /etc/systemd/sysext.conf
    run "invalid configuration" systemd-sysext status
    rm -f /etc/systemd/sysext.conf
    run "invalid --image-policy" systemd-sysext --image-policy=bogus merge
    run "--image-policy=root=absent" systemd-sysext --image-policy=root=absent merge
    systemd-sysext unmerge >/dev/null 2>&1
    cext c 'ID=_any\n' srv/conf/c
    HIERS=/srv/conf
    run "SYSTEMD_CONFEXT_HIERARCHIES" env SYSTEMD_CONFEXT_HIERARCHIES=/srv/conf systemd-confext merge
    state "state with SYSTEMD_CONFEXT_HIERARCHIES"
    run "confext unmerge with SYSTEMD_CONFEXT_HIERARCHIES" env SYSTEMD_CONFEXT_HIERARCHIES=/srv/conf systemd-confext unmerge
}

sc_noexec() {
    dext x 'ID=_any\n' opt/x/run usr/share/x/f
    chmod 0755 /var/lib/extensions/x/opt/x/run
    printf '#!/bin/sh\necho run\n' > /var/lib/extensions/x/opt/x/run
    HIERS=/opt
    run "--noexec=yes" env SYSTEMD_SYSEXT_HIERARCHIES=/opt systemd-sysext --noexec=yes merge
    state "state with --noexec=yes"
    run "exec from the noexec hierarchy" /opt/x/run
    env SYSTEMD_SYSEXT_HIERARCHIES=/opt systemd-sysext unmerge >/dev/null 2>&1
    run "default exec" env SYSTEMD_SYSEXT_HIERARCHIES=/opt systemd-sysext merge
    state "state by default"
    run "exec from the merged hierarchy" /opt/x/run
    env SYSTEMD_SYSEXT_HIERARCHIES=/opt systemd-sysext unmerge >/dev/null 2>&1
    run "--noexec=no" env SYSTEMD_SYSEXT_HIERARCHIES=/opt systemd-sysext --noexec=no merge
    state "state with --noexec=no"
    env SYSTEMD_SYSEXT_HIERARCHIES=/opt systemd-sysext unmerge >/dev/null 2>&1
    run "--noexec=bogus" systemd-sysext --noexec=bogus merge
    HIERS="/usr /opt"
    mode=$(stat -c %a /usr)
    (umask 077 && run "merge under umask 077" systemd-sysext merge)
    state "state under umask 077"
    run "mode kept" sh -c "[ \"\$(stat -c %a /usr)\" = $mode ] && echo kept"
    systemd-sysext unmerge >/dev/null 2>&1
    cext c 'ID=_any\n' etc/c/run
    printf '#!/bin/sh\necho run\n' > /var/lib/confexts/c/etc/c/run
    chmod 0755 /var/lib/confexts/c/etc/c/run
    HIERS=/etc
    run "confext default" systemd-confext merge
    state "state of a confext merge"
    run "exec from the confext" /etc/c/run
    systemd-confext unmerge >/dev/null 2>&1
    run "confext --noexec=no" systemd-confext --noexec=no merge
    state "state with confext --noexec=no"
    run "exec from the confext with --noexec=no" /etc/c/run
    systemd-confext unmerge >/dev/null 2>&1
}

sc_confext() {
    HIERS=/etc
    cext c1
    cext c2 'ID=_any\n' etc/c2/f etc/c2/sub/g
    run "confext list" systemd-confext list
    run "confext list --json=short" systemd-confext list --json=short
    run "confext merge" systemd-confext merge
    expect_mounted /etc
    state "state after the confext merge"
    run "confext payload and the hostname bind mount" cat /etc/c1/f /etc/c2/sub/g /etc/hostname
    run "confext status" systemd-confext status
    run "confext status --json=short" systemd-confext status --json=short
    run "sysext --confext status" systemd-sysext --confext status --json=short
    run "confext refresh without changes" systemd-confext refresh
    run "confext merge while merged" systemd-confext merge
    run "confext unmerge" systemd-confext unmerge
    state "state after the confext unmerge"
    mount -t tmpfs -o mode=0755 tmpfs /var/lib
    mkdir -p /var/lib/confexts /var/lib/extensions.mutable/etc
    cext c1
    run "confext merge --mutable=auto" systemd-confext --mutable=auto merge
    state "state of a mutable confext merge"
    run "write /etc in a mutable confext merge" sh -c 'echo w > /etc/written && cat /var/lib/extensions.mutable/etc/written'
    run "confext unmerge after --mutable=auto" systemd-confext unmerge
    rm -f /etc/written
}

sc_root() {
    mkroot ext4
    rm -f /x/etc/os-release
    printf 'ID=testos\nVERSION_ID=1\n' > /x/usr/lib/os-release
    ln -s /usr/lib/os-release /x/etc/os-release
    dext -d /x/var/lib/extensions r 'ID=testos\nVERSION_ID=1\n' usr/share/r/f opt/r/f
    if [ "$HAVE_SQUASHFS" = 1 ]; then
        rext /x/store/lnk.raw lnk
        ln -s /store/lnk.raw /x/var/lib/extensions/lnk.raw
    fi
    mkdir -p /x/store/d
    cp -a /x/var/lib/extensions/r/. /x/store/d/
    mv /x/store/d/usr/lib/extension-release.d/extension-release.r /x/store/d/usr/lib/extension-release.d/extension-release.ld
    ln -s /store/d /x/var/lib/extensions/ld
    mv /x/opt /x/var/opt
    ln -s /var/opt /x/opt
    HIERS="/usr /opt /var/opt"
    run "list --root" systemd-sysext --root=/x list
    run "list --root --json=short" systemd-sysext --root=/x list --json=short
    run "merge --root" systemd-sysext --root=/x merge
    expect_mounted /x/usr
    state "state after merge --root" /x
    run "payload below the root" cat /x/usr/share/r/f /x/var/opt/r/f
    run "host /usr untouched" sh -c 'mountpoint /usr; ls /usr/share/r 2>&1'
    run "status --root" systemd-sysext --root=/x status
    run "status --root --json=short" systemd-sysext --root=/x status --json=short
    run "merge --root while merged" systemd-sysext --root=/x merge
    run "refresh --root without changes" systemd-sysext --root=/x refresh
    run "status of the host" systemd-sysext status --json=short
    run "unmerge --root" systemd-sysext --root=/x unmerge
    state "state after unmerge --root" /x
    run "relative --root" sh -c 'cd / && systemd-sysext --root=x merge'
    run "status with a relative --root" sh -c 'cd /x && systemd-sysext --root=. status --json=short'
    run "unmerge with a relative --root" sh -c 'cd / && systemd-sysext --root=x/ unmerge'
    mkdir -p /x/var/lib/extensions.mutable/usr
    run "merge --root --mutable=auto" systemd-sysext --root=/x --mutable=auto merge
    state "state after merge --root --mutable=auto" /x
    run "unmerge --root after --mutable=auto" systemd-sysext --root=/x unmerge
    state "state after unmerging --root --mutable=auto" /x
    run "--root to a missing directory" systemd-sysext --root=/nonexistent merge
}

sc_vpick() {
    if [ "$HAVE_SQUASHFS" != 1 ]; then return; fi
    V=/var/lib/extensions/app.raw.v
    mkdir -p "$V"
    for v in 1 2 10; do
        rext "$V/app_$v.raw" app
        t=$WORKDIR/trees/app
        mkdir -p "$t/usr/share/app"
        echo "$v" > "$t/usr/share/app/version"
        img_squashfs "$t" "$V/app_$v.raw"
    done
    cp "$V/app_10.raw" "$V/app_11_$FOREIGN_ARCH.raw"
    cp "$V/app_10.raw" "$V/app_12+0-3.raw"
    cp "$V/app_10.raw" "$V/other_99.raw"
    cp "$V/app_1.raw" "$V/app_0_$HOST_ARCH.raw"
    mkdir -p /var/lib/extensions/raw-in-dir.v
    cp "$V/app_1.raw" /var/lib/extensions/raw-in-dir.v/raw-in-dir_5.raw
    mkdir -p /var/lib/extensions/dir.v
    for v in 1.0 1.1; do
        dext -d /var/lib/extensions/dir.v -r dir "dir_$v" 'ID=_any\n' usr/share/dir/version
        echo "$v" > "/var/lib/extensions/dir.v/dir_$v/usr/share/dir/version"
    done
    mkdir -p /var/lib/extensions/empty.v
    run "list with versioned directories" systemd-sysext list
    run "list --json=short with versioned directories" systemd-sysext list --json=short
    run "merge versioned images" systemd-sysext merge
    run "picked versions" cat /usr/share/app/version /usr/share/dir/version
    state "state with versioned images"
    run "status with versioned images" systemd-sysext status --json=short
    run "refresh with versioned images" systemd-sysext refresh
    systemd-sysext unmerge >/dev/null 2>&1
    rm -f "$V/app_10.raw"
    run "merge picks the next version" systemd-sysext merge
    run "picked version after removal" cat /usr/share/app/version
    systemd-sysext unmerge >/dev/null 2>&1
}

sc_order() {
    for v in 1 2 10; do
        dext -r app "app_$v" 'ID=_any\n' usr/share/app/version "usr/share/app/only-$v"
    done
    dext zz 'ID=_any\n' usr/share/app/version
    dext 0a 'ID=_any\n' usr/share/app/version
    run "merge several versions" systemd-sysext merge
    run "top layer" cat /usr/share/app/version
    run "every layer" ls /usr/share/app
    state "state with several versions"
    systemd-sysext unmerge >/dev/null 2>&1
}

sc_precedence() {
    mkdir -p /etc/extensions /run/extensions
    dext -d /etc/extensions p 'ID=_any\n' usr/share/p/where
    echo etc > /etc/extensions/p/usr/share/p/where
    dext -d /run/extensions p 'ID=_any\n' usr/share/p/where
    echo run > /run/extensions/p/usr/share/p/where
    dext p 'ID=_any\n' usr/share/p/where
    echo var > /var/lib/extensions/p/usr/share/p/where
    dext -d /run/extensions onlyrun
    run "list across search directories" systemd-sysext list
    run "merge across search directories" systemd-sysext merge
    run "highest priority copy wins" cat /usr/share/p/where
    systemd-sysext unmerge >/dev/null 2>&1
    rm -rf /etc/extensions/p
    run "next directory after removal" systemd-sysext list --json=short
    mkdir -p /etc/extensions/onlyrun
    run "empty directory masks an image" systemd-sysext list
    run "merge with a masked image" systemd-sysext merge
    systemd-sysext unmerge >/dev/null 2>&1
    rm -rf /etc/extensions/onlyrun
    ln -s /dev/null /etc/extensions/onlyrun.raw
    run "/dev/null symlink" systemd-sysext list
    rm -f /etc/extensions/onlyrun.raw
    if [ "$HAVE_SQUASHFS" = 1 ]; then
        rext /var/lib/extensions/q.raw q
        dext q
        run "directory and raw image of the same name" systemd-sysext list
        run "merge with a directory and a raw image of the same name" systemd-sysext merge
        systemd-sysext unmerge >/dev/null 2>&1
        rm -rf /var/lib/extensions/q
        mkdir -p /srv/store
        rext /srv/store/img.raw lnk
        ln -s /srv/store/img.raw /var/lib/extensions/lnk.raw
        rext /var/lib/extensions/ok.sysext.raw ok
        rext /var/lib/extensions/conf.confext.raw conf
    fi
    mkdir -p /srv/store/d
    cp -a /var/lib/extensions/p/. /srv/store/d/
    mv /srv/store/d/usr/lib/extension-release.d/extension-release.p /srv/store/d/usr/lib/extension-release.d/extension-release.ld
    ln -s /srv/store/d /var/lib/extensions/ld
    dext .hidden
    dext 'bad name'
    dext -- -dash
    dext 'tilde~'
    touch /var/lib/extensions/file.txt
    run "list with symlinks, suffixes and invalid names" systemd-sysext list
    run "list --json=short with symlinks, suffixes and invalid names" systemd-sysext list --json=short
    run "merge with a confext image among the sysexts" systemd-sysext merge
    rm -f /var/lib/extensions/conf.confext.raw
    run "merge with symlinks, suffixes and invalid names" systemd-sysext merge
    state "state with symlinked images"
    systemd-sysext unmerge >/dev/null 2>&1
    mv /opt /opt.saved
    mkdir -p /var/opt
    echo host > /var/opt/hostfile
    ln -s var/opt /opt
    dext so 'ID=_any\n' opt/so/f
    run "merge into a symlinked hierarchy" systemd-sysext merge
    HIERS="/usr /opt /var/opt"
    state "state with a symlinked hierarchy"
    run "content of the symlinked hierarchy" ls /opt/
    run "status with a symlinked hierarchy" systemd-sysext status --json=short
    run "merge while a symlinked hierarchy is merged" systemd-sysext merge
    run "unmerge a symlinked hierarchy" systemd-sysext unmerge
}

sc_cli() {
    dext m
    run "--help" systemd-sysext --help
    run "-h" systemd-sysext -h
    run "help verb" systemd-sysext help
    run "confext --help" systemd-confext --help
    run "--version" systemd-sysext --version
    run "--mutable=help" systemd-sysext --mutable=help
    run "--no-legend --mutable=help" systemd-sysext --no-legend --mutable=help
    run "--json=help" systemd-sysext --json=help
    run "unknown verb" systemd-sysext frob
    run "unknown verb close to one" systemd-sysext merg
    run "unknown option" systemd-sysext --bogus
    run "unknown short option" systemd-sysext -x
    run "ambiguous option" systemd-sysext --no
    run "abbreviated options" systemd-sysext --js=short --no-leg list
    run "option after the verb" systemd-sysext list --json=short
    run "too many arguments" systemd-sysext merge unmerge
    run "--json=bogus" systemd-sysext --json=bogus status
    run "--mutable=bogus" systemd-sysext --mutable=bogus status
    run "--noexec=bogus" systemd-sysext --noexec=bogus status
    run "--always-refresh=bogus" systemd-sysext --always-refresh=bogus status
    run "--root without a value" systemd-sysext --root
    run "empty --root" systemd-sysext --root= status
    run "--json=off" systemd-sysext --json=off list
    run "--no-pager status" systemd-sysext --no-pager status
    run "--confext list" systemd-sysext --confext list
    run "SYSTEMD_LOG_LEVEL=warning" env SYSTEMD_LOG_LEVEL=warning systemd-sysext merge
    systemd-sysext unmerge >/dev/null 2>&1
    run "SYSTEMD_LOG_LEVEL=bogus" env SYSTEMD_LOG_LEVEL=bogus systemd-sysext status
}

sc_reload() {
    dext rl 'ID=_any\nEXTENSION_RELOAD_MANAGER=1\n'
    run "merge with EXTENSION_RELOAD_MANAGER=1" systemd-sysext merge
    run "refresh with EXTENSION_RELOAD_MANAGER=1" systemd-sysext refresh --always-refresh=yes
    run "unmerge with EXTENSION_RELOAD_MANAGER=1" systemd-sysext unmerge
    run "--no-reload merge" systemd-sysext --no-reload merge
    run "--no-reload unmerge" systemd-sysext --no-reload unmerge
    dext rl 'ID=_any\nEXTENSION_RELOAD_MANAGER=banana\nEXTENSION_RESTART_UNITS="foo.service bad/x"\n'
    run "--no-reload with invalid reload fields" systemd-sysext --no-reload merge
    systemd-sysext --no-reload unmerge >/dev/null 2>&1
    mkroot
    dext -d /x/var/lib/extensions rl 'ID=_any\nEXTENSION_RELOAD_MANAGER=1\n'
    run "--root implies --no-reload" systemd-sysext --root=/x merge
    run "--root unmerge without reload" systemd-sysext --root=/x unmerge
}

# interop: one implementation merges, the side's implementation inspects
# and unmerges. Equal transcripts mean this implementation reads systemd's
# merges (and systemd reads this implementation's) like the original does.
interop_steps() {
    mount -t tmpfs -o mode=0755 tmpfs /var/lib
    mkdir -p /var/lib/extensions /var/lib/extensions.mutable
    dext foo 'ID=_any\n' usr/share/foo/f opt/foo/f
    dext bar
    for mode in no yes; do
        run "merge --mutable=$mode by $1" "$MERGER" --mutable=$mode merge
        state "state after merge --mutable=$mode by $1"
        run "status of the merge --mutable=$mode by $1" systemd-sysext status --json=short
        run "merge over the merge --mutable=$mode by $1" systemd-sysext --mutable=$mode merge
        run "refresh of the merge --mutable=$mode by $1" systemd-sysext --mutable=$mode refresh
        run "unmerge of the merge --mutable=$mode by $1" systemd-sysext unmerge
        state "state after unmerging the merge --mutable=$mode by $1"
    done
    run "confext merge by $1" "$CMERGER" merge
    run "confext status of the merge by $1" systemd-confext status --json=short
    run "confext unmerge of the merge by $1" systemd-confext unmerge
}

sc_interop_systemd() {
    MERGER=/usr/bin/systemd-sysext CMERGER=/usr/bin/systemd-confext
    cext c
    interop_steps systemd
}

sc_interop_go() {
    MERGER=$GO/systemd-sysext CMERGER=$GO/systemd-confext
    cext c
    interop_steps sysext
}

sc_images() {
    HIERS=/usr
    if [ "$HAVE_EROFS" = 1 ]; then rext /var/lib/extensions/e.raw e 'ID=_any\n' erofs; fi
    dext -d "$WORKDIR/trees" x4
    img_ext4 "$WORKDIR/trees/x4" /var/lib/extensions/x4.raw 8
    dext -d "$WORKDIR/trees" gpt
    ddi_build "$WORKDIR/gpt.raw" "$WORKDIR/trees/gpt" && cp "$WORKDIR/gpt.raw" /var/lib/extensions/gpt.raw
    run "list of raw images" systemd-sysext list --json=short
    run "merge raw images" systemd-sysext merge
    state "state with raw images"
    run "payload of raw images" ls /usr/share
    run "status with raw images" systemd-sysext status --json=short
    run "refresh with raw images" systemd-sysext refresh
    systemd-sysext unmerge >/dev/null 2>&1
    rm -f /var/lib/extensions/e.raw /var/lib/extensions/x4.raw
    run "image policy that forbids the GPT image" systemd-sysext --image-policy=root=absent:usr=absent merge
    systemd-sysext unmerge >/dev/null 2>&1
    rm -f /var/lib/extensions/*
    if [ "$HAVE_VERITY" = 1 ]; then
        cp "$VERITY_DDI" /var/lib/extensions/vt.raw
        run "merge a verity DDI with root=verity" systemd-sysext --image-policy=root=verity merge
        state "state with a verity DDI"
        systemd-sysext unmerge >/dev/null 2>&1
        run "verity DDI with root=signed" systemd-sysext --image-policy=root=signed merge
        systemd-sysext unmerge >/dev/null 2>&1
        rm -f /var/lib/extensions/vt.raw
        if [ -f /work/examples/signed-example.raw ]; then
            cp /work/examples/signed-example.raw /var/lib/extensions/
            run "signed DDI without a trusted certificate" systemd-sysext --image-policy=root=signed merge
            systemd-sysext unmerge >/dev/null 2>&1
            mkdir -p /etc/verity.d
            cp /work/examples/keys/db.pem /etc/verity.d/db.crt
            run "signed DDI with root=signed" systemd-sysext --image-policy=root=signed merge
            run "signed DDI payload" cat /usr/share/signed-example/hello.txt
            state "state with a signed DDI"
            systemd-sysext unmerge >/dev/null 2>&1
            rm -rf /etc/verity.d /var/lib/extensions/signed-example.raw
        fi
    fi
}

echo "=== Running scenarios ==="
# E2E_DIFF_SCENARIOS selects scenarios (debugging); the transcripts land in
# /artifacts/differential when run.sh mounts E2E_ARTIFACTS there.
for s in ${E2E_DIFF_SCENARIOS:-basic incompatible compat mutable stale env noexec confext root vpick order precedence cli reload interop_systemd interop_go images}; do
    scenario "$s"
done
if [ -d /artifacts ]; then
    mkdir -p /artifacts/differential
    cp "$T"/*.sd "$T"/*.go /artifacts/differential/ 2>/dev/null
    chown -R "$(stat -c %u:%g /artifacts)" /artifacts/differential
fi

finish

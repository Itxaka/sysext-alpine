#!/bin/sh
# e2e suite for how merged overlays sit in the mount table, with / made
# rshared like on hosts running k3s, kubelet or containerd.
#
# Exercises, against systemd-sysext 262 semantics:
#   - merge/refresh/unmerge below shared mounts
#   - mounts below a hierarchy (tmpfs, nested mounts, docker's /etc/hostname
#     bind) kept across merge, refresh and unmerge, also when they fail,
#     mounts made while merged restored on unmerge
#   - metadata markers: dev as MAJOR:MINOR, the full extension list in every
#     hierarchy, the confext list name, the origin object, the mount source
#   - backing marker and root-relative origin paths for --root on an ext4 root
#   - symlinked, missing and custom hierarchies
#   - mount flags (--noexec=) and the hierarchy mode under a strict umask
#   - workspace kept outside --root, stale workspace mounts, images that
#     contribute to no hierarchy, partial unmerge of the hierarchy list
. /work/test/e2e/lib.sh

echo "=== Installing build dependencies ==="
pkg_add e2fsprogs kmod squashfs-tools jq
# Installed outside /usr so that it still runs while /usr is merged noexec.
install_sysext /sbin
need_overlayfs

E=/var/lib/extensions
C=/var/lib/confexts
W=$WORKDIR
mkdir -p "$E" "$C"
WS=/run/systemd/sysext

# ext NAME FILE... — directory sysext NAME shipping FILEs (paths below /).
ext() {
    name=$1
    shift
    mkdir -p "$E/$name/usr/lib/extension-release.d"
    printf 'ID=_any\n' > "$E/$name/usr/lib/extension-release.d/extension-release.$name"
    for f in "$@"; do
        mkdir -p "$E/$name/$(dirname "$f")"
        echo "$name" > "$E/$name/$f"
    done
}

devnum() { devnum_of "$1"; }
# opts MOUNTPOINT — per-mount options of the topmost mount at MOUNTPOINT.
opts() { awk -v m="$1" '$5 == m {o = $6} END {print o}' /proc/self/mountinfo; }
has() { case ",$1," in *",$2,"*) return 0 ;; esac; return 1; }
mounts_below() { awk -v p="$1" 'index($5, p) == 1' /proc/self/mountinfo | wc -l; }
loops_bound() { our_loops "$E/" | wc -l; }
# clean CTX — nothing of ours may be left mounted or attached.
clean() {
    i=0
    while [ $i -lt 50 ]; do
        [ "$(mounts_below /run/systemd)" = 0 ] && [ "$(loops_bound)" = 0 ] && break
        sleep 0.1
        i=$((i + 1))
    done
    expect_eq "$1: no workspace mounts left" "$(mounts_below /run/systemd)" 0
    expect_eq "$1: no loop devices left" "$(loops_bound)" 0
    if [ -e "$WS" ]; then fail "$1: workspace $WS left behind"; else pass "$1: workspace removed"; fi
}

echo "=== Running tests ==="

mount --make-rshared / || fatal "cannot make / rshared"
expect_eq "/ is shared" "$(findmnt -no PROPAGATION /)" shared

# --- shared propagation, markers --------------------------------------------
ext foo usr/share/foo/f opt/foo/f usr/bin/foo-tool
chmod 0755 "$E/foo/usr/bin/foo-tool"
printf '#!/bin/sh\necho foo-tool\n' > "$E/foo/usr/bin/foo-tool"
ext bar usr/share/bar/f

if out=$(sysext merge 2>&1); then pass "merge below a shared /"; else fail "merge below a shared /: $out"; fi
expect_eq "merge messages" "$out" "Using extensions 'bar', 'foo'.
Merged extensions into '/usr'.
Merged extensions into '/opt'."
expect_eq "payload merged" "$(cat /usr/share/foo/f 2>&1)" foo
dev=$(cat /usr/.systemd-sysext/dev)
expect_eq "dev marker is MAJOR:MINOR of /usr" "$dev" "$(devnum /usr)"
expect_eq "dev marker of /opt" "$(cat /opt/.systemd-sysext/dev)" "$(devnum /opt)"
expect_eq "/usr lists every merged extension" "$(cat /usr/.systemd-sysext/extensions)" "bar
foo"
expect_eq "/opt lists every merged extension" "$(cat /opt/.systemd-sysext/extensions)" "bar
foo"
cmp -s /usr/.systemd-sysext/origin /opt/.systemd-sysext/origin && pass "origin identical in every hierarchy" || fail "origin differs between hierarchies"
if jq -e '.mutable.mode == "no" and (.extensions.foo.path == "/var/lib/extensions/foo") and (.extensions.foo.mtime == 0) and (.extensions.foo.onMountId > 0) and (.extensions.foo | has("crtime"))' /usr/.systemd-sysext/origin >/dev/null; then
    pass "origin object shape"
else
    fail "origin: $(cat /usr/.systemd-sysext/origin)"
fi
[ -e /usr/.systemd-sysext/backing ] && fail "backing marker for an overlayfs host /usr" || pass "no backing marker for a major-0 host /usr"
[ -e /usr/.systemd-sysext/work_dir ] && fail "work_dir marker in read-only mode" || pass "no work_dir marker in read-only mode"
expect_eq "/usr mount source" "$(findmnt -no SOURCE /usr)" sysext
expect_eq "merged /usr propagates" "$(findmnt -no PROPAGATION /usr)" shared
o=$(opts /usr)
has "$o" ro && has "$o" nodev && pass "/usr mounted ro,nodev" || fail "/usr options: $o"
has "$o" noexec && fail "sysext defaults to noexec: $o" || pass "sysext allows exec by default"
expect_eq "merged tool runs" "$(/usr/bin/foo-tool 2>&1)" foo-tool
expect_eq "image tree in the workspace" "$(cat "$WS/extensions/foo/usr/share/foo/f" 2>&1)" foo
[ "$(findmnt -no PROPAGATION "$WS")" = private ] && pass "workspace is private" || fail "workspace propagation: $(findmnt -no PROPAGATION "$WS")"
out=$(sysext status --json=short)
echo "$out" | jq -e '.[] | select(.hierarchy == "/usr") | .extensions == ["bar", "foo"] and .since > 0' >/dev/null \
    && pass "status lists the merged extensions" || fail "status: $out"
out=$(sysext refresh 2>&1)
expect_eq "unchanged refresh is skipped" "$out" "Skipping extension refresh because no change was found, use --always-refresh=yes to always do a refresh."
expect_eq "skipped refresh keeps the mount" "$(cat /usr/.systemd-sysext/dev)" "$dev"
if out=$(sysext unmerge 2>&1); then pass "unmerge below a shared /"; else fail "unmerge: $out"; fi
expect_eq "unmerge messages" "$out" "Unmerged '/usr'.
Unmerged '/opt'."
clean "shared unmerge"

# --- mounts below the hierarchies -------------------------------------------
mkdir -p /usr/local/sub /usr/src/late
mount -t tmpfs tmpfs /usr/local/sub
echo sub > /usr/local/sub/marker
mkdir /usr/local/sub/nested
mount -t tmpfs tmpfs /usr/local/sub/nested
echo nested > /usr/local/sub/nested/n
sysext merge >/dev/null 2>&1 || fail "merge with submounts"
expect_eq "tmpfs below /usr visible while merged" "$(cat /usr/local/sub/marker 2>&1)" sub
expect_eq "nested mount visible while merged" "$(cat /usr/local/sub/nested/n 2>&1)" nested
expect_eq "payload merged next to submounts" "$(cat /usr/share/bar/f 2>&1)" bar
sysext refresh --always-refresh=yes >/dev/null 2>&1 || fail "refresh --always-refresh=yes with submounts"
expect_eq "submount kept by refresh" "$(cat /usr/local/sub/nested/n 2>&1)" nested
mount -t tmpfs tmpfs /usr/src/late && echo late > /usr/src/late/f
sysext unmerge >/dev/null 2>&1 || fail "unmerge with submounts"
expect_eq "submount back on the host" "$(cat /usr/local/sub/nested/n 2>&1)" nested
expect_eq "mount made while merged kept" "$(cat /usr/src/late/f 2>&1)" late
umount /usr/src/late /usr/local/sub/nested /usr/local/sub
clean "submounts"

# A merge or refresh failing on /opt (importing it into itself) after /usr
# was staged with the mounts below it puts them back onto the host.
mount -t tmpfs tmpfs /usr/local/sub && track_mount /usr/local/sub
echo sub > /usr/local/sub/marker
mkdir -p /var/lib/extensions.mutable
ln -sfn /opt /var/lib/extensions.mutable/opt
out=$(sysext --mutable=import merge 2>&1) && fail "import of /opt into itself merged: $out"
expect_eq "failed merge keeps the /usr submount" "$(mountpoint -q /usr/local/sub && cat /usr/local/sub/marker 2>&1)" sub
rm /var/lib/extensions.mutable/opt
sysext --mutable=import merge >/dev/null 2>&1 || fail "import merge"
ln -sfn /opt /var/lib/extensions.mutable/opt
out=$(sysext --mutable=import --always-refresh=yes refresh 2>&1) && fail "refresh importing /opt into itself: $out"
expect_eq "failed refresh keeps the /usr submount" "$(mountpoint -q /usr/local/sub && cat /usr/local/sub/marker 2>&1)" sub
sysext unmerge >/dev/null 2>&1
rm -rf /var/lib/extensions.mutable
umount /usr/local/sub
clean "failed merges with submounts"

mkdir -p "$C/conf/etc/extension-release.d" "$C/conf/etc/conf"
printf 'ID=_any\n' > "$C/conf/etc/extension-release.d/extension-release.conf"
echo c > "$C/conf/etc/conf/f"
hostname_before=$(cat /etc/hostname)
if confext merge >/dev/null 2>&1; then pass "confext merge"; else fail "confext merge"; fi
expect_eq "confext payload" "$(cat /etc/conf/f 2>&1)" c
expect_eq "/etc/hostname bind mount kept" "$(cat /etc/hostname)" "$hostname_before"
[ -s /etc/resolv.conf ] && pass "/etc/resolv.conf kept" || fail "/etc/resolv.conf hidden"
expect_eq "confext list marker" "$(cat /etc/.systemd-confext/confexts 2>&1)" conf
[ -e /etc/.systemd-confext/extensions ] && fail "confext wrote an extensions marker" || pass "no extensions marker for confext"
expect_eq "/etc mount source" "$(findmnt -no SOURCE /etc)" confext
o=$(opts /etc)
has "$o" ro && has "$o" nodev && has "$o" nosuid && has "$o" noexec && pass "/etc mounted ro,nodev,nosuid,noexec" || fail "/etc options: $o"
confext unmerge >/dev/null 2>&1 || fail "confext unmerge"
expect_eq "/etc/hostname after unmerge" "$(cat /etc/hostname)" "$hostname_before"
confext --noexec=no merge >/dev/null 2>&1 || fail "confext --noexec=no merge"
has "$(opts /etc)" noexec && fail "confext --noexec=no keeps noexec" || pass "confext --noexec=no allows exec"
confext unmerge >/dev/null 2>&1

# --- mount flags and mode ------------------------------------------------------
sysext --noexec=yes merge >/dev/null 2>&1 || fail "merge --noexec=yes"
has "$(opts /usr)" noexec && pass "sysext --noexec=yes mounts noexec" || fail "--noexec=yes ignored: $(opts /usr)"
/usr/bin/foo-tool >/dev/null 2>&1 && fail "tool runs under noexec" || pass "exec blocked under noexec"
sysext unmerge >/dev/null 2>&1
mode=$(stat -c %a /usr)
(umask 077; sysext merge >/dev/null 2>&1) || fail "merge under umask 077"
expect_eq "/usr mode kept under umask 077" "$(stat -c %a /usr)" "$mode"
su -s /bin/sh nobody -c 'cat /usr/share/foo/f' >/dev/null 2>&1 && pass "non-root can read the merged /usr" || fail "non-root cannot read the merged /usr"
sysext unmerge >/dev/null 2>&1
clean "flags"

# --- hierarchy resolution -------------------------------------------------------
mv /opt /var/opt && ln -s var/opt /opt
out=$(sysext merge 2>&1) || fail "merge into a symlinked /opt: $out"
echo "$out" | grep -q "Merged extensions into '/var/opt'." && pass "symlinked hierarchy merged at its target" || fail "messages: $out"
sysext status --json=short | jq -e '.[] | select(.hierarchy == "/opt") | .extensions != "none" and .extensions != []' >/dev/null \
    && pass "status sees the symlinked hierarchy merged" || fail "status: $(sysext status --json=short)"
sysext merge >/dev/null 2>&1 && fail "second merge over a symlinked hierarchy" || pass "symlinked hierarchy detected as merged"
sysext unmerge >/dev/null 2>&1
mountpoint -q /var/opt && fail "/var/opt still mounted after unmerge" || pass "symlinked hierarchy unmerged"
rm /opt && mv /var/opt /opt
clean "symlinked hierarchy"

rmdir /opt
sysext merge >/dev/null 2>&1 || fail "merge with /opt missing"
expect_eq "missing /opt created and merged" "$(cat /opt/foo/f 2>&1)" foo
sysext unmerge >/dev/null 2>&1
[ -d /opt ] || mkdir /opt
ext custom srv/new/x
SYSTEMD_SYSEXT_HIERARCHIES=/usr:/srv/new:/srv/none sysext merge >/dev/null 2>&1 || fail "merge with custom hierarchies"
expect_eq "custom hierarchy created and merged" "$(cat /srv/new/x 2>&1)" custom
[ -e /srv/none ] && fail "hierarchy nobody ships was created" || pass "hierarchy nobody ships not created"
SYSTEMD_SYSEXT_HIERARCHIES=/usr:/srv/new:/srv/none sysext unmerge >/dev/null 2>&1
rm -rf "$E/custom"
clean "custom hierarchies"

# --- --root on a block device -----------------------------------------------------
if fs_supported ext4; then
    truncate -s 64M "$W/root.img"
    mkfs.ext4 -qF "$W/root.img"
    mkdir -p /x
    mount -o loop "$W/root.img" /x
    track_mount /x
    track_root /x
    mkdir -p /x/usr/lib /x/etc /x/var/lib/extensions
    printf 'ID=test\n' > /x/usr/lib/os-release
    cp -a "$E/foo" /x/var/lib/extensions/r
    mv /x/var/lib/extensions/r/usr/lib/extension-release.d/extension-release.foo /x/var/lib/extensions/r/usr/lib/extension-release.d/extension-release.r
    rootdev=$(devnum /x/usr)
    if sysext --root=/x merge >/dev/null 2>&1; then pass "merge --root=/x"; else fail "merge --root=/x"; fi
    expect_eq "backing marker is the root's block device" "$(cat /x/usr/.systemd-sysext/backing 2>&1)" "$rootdev"
    expect_eq "origin path without the root" "$(jq -r '.extensions.r.path' /x/usr/.systemd-sysext/origin)" /var/lib/extensions/r
    [ -e /x/run ] && fail "--root merge wrote into the root's /run" || pass "workspace kept outside the root"
    expect_eq "/opt created inside the root" "$(cat /x/opt/foo/f 2>&1)" foo
    sysext --root=/x unmerge >/dev/null 2>&1 || fail "unmerge --root=/x"
    mountpoint -q /x/usr && fail "/x/usr still mounted" || pass "--root unmerged"
    umount /x
    loops=$(losetup -j "$W/root.img" | wc -l)
    expect_eq "loop root detached" "$loops" 0
else
    skip fs:ext4 "ext4 not supported; skipping --root on a block device"
fi
clean "--root"

# --- workspace safety ---------------------------------------------------------
sysext merge >/dev/null 2>&1 || fail "host merge"
mkdir -p /T/usr /T/etc
cp /usr/lib/os-release /T/etc/os-release
ln -s /run /T/run
sysext --root=/T unmerge >/dev/null 2>&1 || fail "unmerge of an unmerged --root"
mountpoint -q /usr && [ -d "$WS/extensions/foo" ] && pass "--root unmerge through a /run symlink left the host alone" || fail "--root unmerge touched the host merge"
sysext unmerge >/dev/null 2>&1
rm -rf /T

mkdir -p "$WS/extensions/stale"
mount -t tmpfs tmpfs "$WS/extensions/stale"
sysext merge >/dev/null 2>&1 || fail "merge over a stale workspace"
sysext unmerge >/dev/null 2>&1 || fail "unmerge after a stale workspace"
clean "stale workspace"

if fs_supported squashfs; then
    mkdir -p "$W/usronly/usr/lib/extension-release.d" "$W/usronly/usr/share/usronly"
    printf 'ID=_any\n' > "$W/usronly/usr/lib/extension-release.d/extension-release.usronly"
    echo u > "$W/usronly/usr/share/usronly/f"
    mksquashfs "$W/usronly" "$E/usronly.raw" -quiet -noappend
    out=$(SYSTEMD_SYSEXT_HIERARCHIES=/opt sysext merge 2>&1) || fail "merge with an image shipping no /opt: $out"
    mountpoint -q /opt && pass "/opt merged from the directory image" || fail "/opt not merged"
    [ -f "$WS/extensions/usronly/usr/share/usronly/f" ] && pass "image without /opt content reachable in the workspace" || fail "usronly not in the workspace"
    SYSTEMD_SYSEXT_HIERARCHIES=/opt sysext unmerge >/dev/null 2>&1
    clean "image without content for one hierarchy"
    mv "$E/foo" "$E/bar" "$W/"
    for i in 1 2; do
        out=$(SYSTEMD_SYSEXT_HIERARCHIES=/opt sysext merge 2>&1)
        expect_eq "nothing merged from an image without /opt content ($i)" "$out" "Using extensions 'usronly.raw'."
        clean "image without content for the hierarchy ($i)"
    done
    rm -f "$E/usronly.raw"
    mv "$W/foo" "$W/bar" "$E/"
else
    skip fs:squashfs "squashfs not supported; skipping images without hierarchy content"
fi

sysext merge >/dev/null 2>&1 || fail "merge of /usr and /opt"
SYSTEMD_SYSEXT_HIERARCHIES=/usr sysext unmerge >/dev/null 2>&1 || fail "partial unmerge"
mountpoint -q /usr && fail "/usr still merged after a partial unmerge" || pass "partial unmerge unmerged /usr"
expect_eq "/opt still merged and recognizable" "$(cat /opt/.systemd-sysext/dev 2>&1)" "$(devnum /opt)"
expect_eq "/opt content still served" "$(cat /opt/foo/f 2>&1)" foo
sysext unmerge >/dev/null 2>&1 || fail "unmerge of the rest"
mountpoint -q /opt && fail "/opt still merged" || pass "remaining hierarchy unmerged"
clean "partial unmerge"

finish

#!/bin/sh
# e2e suite for extension compatibility and discovery, with the outcomes
# systemd-sysext 262 gives (inner-differential.sh runs the same cases against
# it):
#   1. extension-release validation in a --root with a controlled os-release:
#      ID, ID_LIKE, VERSION_ID, rolling releases, SYSEXT_LEVEL with and
#      without a host level, ARCHITECTURE (_any, host, foreign, invalid),
#      SYSEXT_SCOPE (also in an initrd root), CONFEXT_LEVEL/CONFEXT_SCOPE
#   2. release file lookup: versioned image names, the base name, the
#      user.extension-release.strict xattr escape hatch (directory and
#      squashfs images), ambiguous candidates
#   3. host os-release: missing ID, missing file, usr/lib fallback,
#      SYSTEMD_OS_RELEASE
#   4. --root with absolute symlinks (os-release, images, hierarchies) that
#      must resolve inside the root and never reach the host's files
#   5. version ordering of extensions overriding the same file
#   6. search directory precedence, masking, symlinked images, image suffixes
#   7. versioned image directories (.v, vpick)
. /work/test/e2e/lib.sh

echo "=== Installing dependencies ==="
pkg_add squashfs-tools attr jq
install_sysext
need_overlayfs
fs_supported squashfs || fatal "kernel lacks squashfs"

E=/var/lib/extensions
R=/x
mkdir -p "$E" "$R"
mount -t tmpfs -o mode=0755 tmpfs "$R" || fatal "cannot mount a tmpfs on $R"
track_mount "$R"
track_root "$R"
mkdir -p "$R/usr/lib" "$R/usr/share" "$R/etc" "$R/opt" "$R/var/lib/extensions" "$R/var/lib/confexts"

# ext DIR NAME RELEASE [RELNAME] — directory sysext DIR/NAME with RELEASE as
# extension-release.RELNAME (default NAME), shipping usr/share/NAME/f.
ext() {
    rm -rf "${1:?}/$2"
    mkdir -p "$1/$2/usr/lib/extension-release.d" "$1/$2/usr/share/$2"
    printf "$3" > "$1/$2/usr/lib/extension-release.d/extension-release.${4:-$2}"
    echo "$2" > "$1/$2/usr/share/$2/f"
}

# rootcase CTX HOST_OS_RELEASE EXT_RELEASE WANT [NAME [RELNAME]] — merge one
# directory sysext into the root; WANT is yes (merged) or no (ignored).
rootcase() {
    n=${5:-c}
    printf "$2" > "$R/etc/os-release"
    rm -rf "${R:?}"/var/lib/extensions/*
    ext "$R/var/lib/extensions" "$n" "$3" "${6:-$n}"
    out=$(sysext --root="$R" merge 2>&1)
    rc=$?
    got=no
    [ -f "$R/usr/share/$n/f" ] && got=yes
    expect_eq "$1" "$got" "$4"
    case $4:$rc in
        yes:0) ;;
        no:0) [ "$out" = "No suitable extensions found (1 ignored due to incompatible image(s))." ] || fail "$1: output [$out]" ;;
        *) fail "$1: exit status $rc: $out" ;;
    esac
    sysext --root="$R" unmerge >/dev/null 2>&1
}

echo "=== Running tests ==="

echo "--- 1. extension-release validation ---"
H='ID=testos\nVERSION_ID=1\n'
rootcase "same ID and VERSION_ID" "$H" 'ID=testos\nVERSION_ID=1\n' yes
rootcase "other VERSION_ID" "$H" 'ID=testos\nVERSION_ID=2\n' no
rootcase "other ID" "$H" 'ID=other\nVERSION_ID=1\n' no
rootcase "ID=_any" "$H" 'ID=_any\n' yes
rootcase "no ID" "$H" 'VERSION_ID=1\n' no
rootcase "no VERSION_ID on a versioned host" "$H" 'ID=testos\n' no
rootcase "quoted values" "$H" 'ID="testos"\nVERSION_ID='"'"'1'"'"'\n' yes
rootcase "ID_LIKE match" 'ID=testos\nID_LIKE="debian alpine"\nVERSION_ID=1\n' 'ID=alpine\nVERSION_ID=1\n' yes
rootcase "ID_LIKE without match" 'ID=testos\nID_LIKE="debian alpine"\nVERSION_ID=1\n' 'ID=fedora\nVERSION_ID=1\n' no
rootcase "rolling host, extension without version" 'ID=rolling\n' 'ID=rolling\n' yes
rootcase "rolling host, extension with VERSION_ID" 'ID=rolling\n' 'ID=rolling\nVERSION_ID=5\n' yes
rootcase "rolling host, extension with SYSEXT_LEVEL" 'ID=rolling\n' 'ID=rolling\nSYSEXT_LEVEL=1\n' yes
L='ID=testos\nVERSION_ID=1\nSYSEXT_LEVEL=2\n'
rootcase "same SYSEXT_LEVEL" "$L" 'ID=testos\nSYSEXT_LEVEL=2\n' yes
rootcase "other SYSEXT_LEVEL" "$L" 'ID=testos\nSYSEXT_LEVEL=3\n' no
rootcase "host level, extension VERSION_ID only" "$L" 'ID=testos\nVERSION_ID=1\n' yes
rootcase "host level, other VERSION_ID only" "$L" 'ID=testos\nVERSION_ID=9\n' no
rootcase "extension level, host without level: VERSION_ID decides" "$H" 'ID=testos\nSYSEXT_LEVEL=2\nVERSION_ID=1\n' yes
rootcase "extension level only, host without level" "$H" 'ID=testos\nSYSEXT_LEVEL=2\n' no
rootcase "host level without VERSION_ID" 'ID=testos\nSYSEXT_LEVEL=2\n' 'ID=testos\nSYSEXT_LEVEL=2\n' yes
rootcase "ARCHITECTURE=_any" "$H" 'ID=_any\nARCHITECTURE=_any\n' yes
rootcase "ARCHITECTURE=$HOST_ARCH (host)" "$H" "ID=_any\nARCHITECTURE=$HOST_ARCH\n" yes
rootcase "ARCHITECTURE=$FOREIGN_ARCH (foreign)" "$H" "ID=_any\nARCHITECTURE=$FOREIGN_ARCH\n" no
rootcase "invalid ARCHITECTURE" "$H" 'ID=_any\nARCHITECTURE=pdp11\n' no
rootcase "SYSEXT_SCOPE=system" "$H" 'ID=_any\nSYSEXT_SCOPE=system\n' yes
rootcase "SYSEXT_SCOPE=initrd" "$H" 'ID=_any\nSYSEXT_SCOPE=initrd\n' no
rootcase "SYSEXT_SCOPE=portable" "$H" 'ID=_any\nSYSEXT_SCOPE=portable\n' no
rootcase "SYSEXT_SCOPE with several scopes" "$H" 'ID=_any\nSYSEXT_SCOPE=initrd system\n' yes
touch "$R/etc/initrd-release"
rootcase "initrd root: SYSEXT_SCOPE=initrd" "$H" 'ID=_any\nSYSEXT_SCOPE=initrd\n' yes
rootcase "initrd root: SYSEXT_SCOPE=system" "$H" 'ID=_any\nSYSEXT_SCOPE=system\n' no
rm -f "$R/etc/initrd-release"

printf 'ID=testos\nVERSION_ID=1\nSYSEXT_LEVEL=2\nCONFEXT_LEVEL=3\n' > "$R/etc/os-release"
cx=$R/var/lib/confexts/lv
for c in "CONFEXT_LEVEL=3:yes" "CONFEXT_LEVEL=2:no" "CONFEXT_LEVEL=3\nCONFEXT_SCOPE=initrd:no" "CONFEXT_LEVEL=3\nCONFEXT_SCOPE=system:yes"; do
    rm -rf "$cx"
    mkdir -p "$cx/etc/extension-release.d" "$cx/etc/lv"
    printf "ID=testos\n${c%:*}\n" > "$cx/etc/extension-release.d/extension-release.lv"
    echo lv > "$cx/etc/lv/f"
    confext --root="$R" merge >/dev/null 2>&1
    got=no
    [ -f "$R/etc/lv/f" ] && got=yes
    expect_eq "confext $(printf "${c%:*}" | tr '\n' ' ')" "$got" "${c#*:}"
    confext --root="$R" unmerge >/dev/null 2>&1
done
rm -rf "$cx"

echo "--- 2. release file lookup ---"
rootcase "versioned name, base name release file" "$H" 'ID=_any\n' yes c_1.2 c
rootcase "versioned name, full name release file" "$H" 'ID=_any\n' yes c_1.2 c_1.2
rootcase "release file of another name" "$H" 'ID=_any\n' no c other
rel=$R/var/lib/extensions/c/usr/lib/extension-release.d
rm -rf "${R:?}"/var/lib/extensions/*
ext "$R/var/lib/extensions" c 'ID=_any\n' other
if setfattr -n user.extension-release.strict -v false "$rel/extension-release.other" 2>/dev/null; then
    sysext --root="$R" merge >/dev/null 2>&1
    expect_eq "strict xattr false accepts another release file name" "$(cat "$R/usr/share/c/f" 2>/dev/null)" c
    sysext --root="$R" unmerge >/dev/null 2>&1
    setfattr -n user.extension-release.strict -v true "$rel/extension-release.other"
    out=$(sysext --root="$R" merge 2>&1)
    expect_eq "strict xattr true" "$out" "No suitable extensions found (1 ignored due to incompatible image(s))."
    sysext --root="$R" unmerge >/dev/null 2>&1
    setfattr -n user.extension-release.strict -v 0 "$rel/extension-release.other"
    cp "$rel/extension-release.other" "$rel/extension-release.another"
    setfattr -n user.extension-release.strict -v false "$rel/extension-release.another"
    out=$(sysext --root="$R" merge 2>&1)
    expect_eq "two candidates with strict xattr false are ambiguous" "$out" "No suitable extensions found (1 ignored due to incompatible image(s))."
    sysext --root="$R" unmerge >/dev/null 2>&1
    rm -rf "${R:?}"/var/lib/extensions/*
    ext "$WORKDIR" w 'ID=_any\n' other
    setfattr -n user.extension-release.strict -v false "$WORKDIR/w/usr/lib/extension-release.d/extension-release.other"
    img_squashfs "$WORKDIR/w" "$R/var/lib/extensions/w.raw" -xattrs
    sysext --root="$R" merge >/dev/null 2>&1
    expect_eq "squashfs image with strict xattr false" "$(cat "$R/usr/share/w/f" 2>/dev/null)" w
    sysext --root="$R" unmerge >/dev/null 2>&1
else
    skip xattr "user xattrs unsupported on the tmpfs root"
fi
rm -rf "${R:?}"/var/lib/extensions/*
ext "$WORKDIR" v 'ID=_any\n'
img_squashfs "$WORKDIR/v" "$R/var/lib/extensions/v_2.raw"
sysext --root="$R" merge >/dev/null 2>&1
expect_eq "raw versioned name, base name release file" "$(cat "$R/usr/share/v/f" 2>/dev/null)" v
sysext --root="$R" unmerge >/dev/null 2>&1

echo "--- 3. host os-release ---"
rm -rf "${R:?}"/var/lib/extensions/*
ext "$R/var/lib/extensions" c 'ID=testos\n'
printf 'VERSION_ID=1\n' > "$R/etc/os-release"
out=$(sysext --root="$R" merge 2>&1)
expect_eq "host os-release without ID is fatal" "$?:$(echo "$out" | head -n 1)" "1:'ID' field not found or empty in 'os-release' data of OS tree '$R'."
rm -f "$R/etc/os-release"
out=$(sysext --root="$R" merge 2>&1)
rc=$?
case $rc:$out in
    1:"Failed to acquire 'os-release' data of OS tree '$R': "*) pass "missing host os-release is fatal" ;;
    *) fail "missing host os-release: $rc: $out" ;;
esac
printf 'ID=testos\n' > "$R/usr/lib/os-release"
sysext --root="$R" merge >/dev/null 2>&1
expect_eq "host os-release in usr/lib" "$(cat "$R/usr/share/c/f" 2>/dev/null)" c
sysext --root="$R" unmerge >/dev/null 2>&1
printf 'ID=elsewhere\n' > "$R/etc/alt-os-release"
ext "$R/var/lib/extensions" c 'ID=elsewhere\n'
SYSTEMD_OS_RELEASE=/etc/alt-os-release sysext --root="$R" merge >/dev/null 2>&1
expect_eq "SYSTEMD_OS_RELEASE names the os-release inside the root" "$(cat "$R/usr/share/c/f" 2>/dev/null)" c
sysext --root="$R" unmerge >/dev/null 2>&1
rm -f "$R/etc/alt-os-release"

echo "--- 4. --root with absolute symlinks ---"
# Decoys on the host: anything resolved against the host instead of the
# root shows up as "host".
mkdir -p /store/d/usr/lib/extension-release.d /store/d/usr/share/ld
printf 'ID=_any\n' > /store/d/usr/lib/extension-release.d/extension-release.ld
echo host > /store/d/usr/share/ld/f
ext "$WORKDIR" lnk 'ID=_any\n'
echo host > "$WORKDIR/lnk/usr/share/lnk/f"
img_squashfs "$WORKDIR/lnk" /store/lnk.raw
mkdir -p /var/opt
rm -rf "${R:?}"/var/lib/extensions/*
ln -sf /usr/lib/os-release "$R/etc/os-release"
printf 'ID=testos\nVERSION_ID=1\n' > "$R/usr/lib/os-release"
ext "$R/var/lib/extensions" r 'ID=testos\nVERSION_ID=1\n'
mkdir -p "$R/var/lib/extensions/r/opt/r"
echo r > "$R/var/lib/extensions/r/opt/r/f"
mkdir -p "$R/store"
ext "$R/store" d 'ID=_any\n' ld
mv "$R/store/d/usr/share/d" "$R/store/d/usr/share/ld"
echo root > "$R/store/d/usr/share/ld/f"
ln -s /store/d "$R/var/lib/extensions/ld"
ext "$WORKDIR" lnk 'ID=_any\n'
echo root > "$WORKDIR/lnk/usr/share/lnk/f"
img_squashfs "$WORKDIR/lnk" "$R/store/lnk.raw"
ln -s /store/lnk.raw "$R/var/lib/extensions/lnk.raw"
rm -rf "$R/opt"
mkdir -p "$R/var/opt"
ln -s /var/opt "$R/opt"
l=$(sysext --root="$R" list --json=short)
expect_eq "list --root resolves image symlinks inside the root" \
    "$(echo "$l" | jq -r '[.[] | .name + "=" + .path] | join(" ")')" "ld=$R/store/d lnk=$R/store/lnk.raw r=$R/var/lib/extensions/r"
out=$(sysext --root="$R" merge 2>&1)
expect_eq "merge --root with absolute symlinks" "$?:$out" "0:Using extensions 'd', 'lnk.raw', 'r'.
Merged extensions into '$R/usr'.
Merged extensions into '$R/var/opt'."
expect_eq "absolute os-release symlink read inside the root" "$(cat "$R/usr/share/r/f" 2>/dev/null)" r
expect_eq "symlinked directory image from the root" "$(cat "$R/usr/share/ld/f" 2>/dev/null)" root
expect_eq "symlinked raw image from the root" "$(cat "$R/usr/share/lnk/f" 2>/dev/null)" root
expect_eq "absolute hierarchy symlink merged inside the root" "$(cat "$R/var/opt/r/f" 2>/dev/null)" r
mountpoint -q /var/opt && fail "the host's /var/opt was merged" || pass "the host's /var/opt untouched"
mountpoint -q /usr && fail "the host's /usr was merged" || pass "the host's /usr untouched"
expect_eq "status --root follows the hierarchy symlink" \
    "$(sysext --root="$R" status --json=short | jq -c '[.[] | {(.hierarchy): .extensions}] | add')" '{"/opt":["ld","lnk","r"],"/usr":["ld","lnk","r"]}'
sysext --root="$R" unmerge >/dev/null 2>&1
mountpoint -q "$R/var/opt" && fail "symlinked hierarchy still merged" || pass "symlinked hierarchy unmerged"
rm -rf /store "$R/opt" "${R:?}"/var/lib/extensions/* "$R/store"
mkdir -p "$R/opt"

echo "--- 5. version ordering ---"
for v in 1 2 10; do
    ext "$E" "app_$v" 'ID=_any\n' app
    mkdir -p "$E/app_$v/usr/share/app"
    echo "$v" > "$E/app_$v/usr/share/app/version"
    touch "$E/app_$v/usr/share/app/only-$v"
done
for n in zz 0a; do
    ext "$E" "$n" 'ID=_any\n'
    mkdir -p "$E/$n/usr/share/app"
    echo "$n" > "$E/$n/usr/share/app/version"
done
out=$(sysext merge 2>&1)
expect_eq "extensions are ordered like strverscmp_improved" "$out" "Using extensions 'app_1', 'app_2', 'app_10', 'zz', '0a'.
Merged extensions into '/usr'."
expect_eq "the last extension is the top layer" "$(cat /usr/share/app/version 2>/dev/null)" 0a
expect_eq "every layer contributes" "$(ls /usr/share/app | tr '\n' ' ')" "only-1 only-10 only-2 version "
expect_eq "extensions marker in merge order" "$(tr '\n' ' ' < /usr/.systemd-sysext/extensions)" "app_1 app_2 app_10 zz 0a "
sysext unmerge >/dev/null 2>&1
rm -rf "${E:?}"/*

echo "--- 6. search directories ---"
mkdir -p /etc/extensions /run/extensions
for d in /etc/extensions /run/extensions /var/lib/extensions; do
    ext "$d" p 'ID=_any\n'
    echo "${d%/extensions}" > "$d/p/usr/share/p/f"
done
ext /run/extensions onlyrun 'ID=_any\n'
expect_eq "list takes /etc over /run over /var/lib" \
    "$(sysext list --json=short | jq -r '[.[] | .name + "=" + .path] | join(" ")')" "onlyrun=/run/extensions/onlyrun p=/etc/extensions/p"
sysext merge >/dev/null 2>&1
expect_eq "merged copy from /etc" "$(cat /usr/share/p/f 2>/dev/null)" /etc
sysext unmerge >/dev/null 2>&1
rm -rf /etc/extensions/p
expect_eq "next search directory once removed" "$(sysext list --json=short | jq -r '.[] | select(.name == "p") | .path')" /run/extensions/p
mkdir -p /etc/extensions/onlyrun
expect_eq "an empty directory masks an image" "$(sysext list --json=short | jq -r '.[] | select(.name == "onlyrun") | .path')" /etc/extensions/onlyrun
out=$(sysext merge 2>&1)
expect_eq "a masked image is not merged" "$out" "Using extensions 'p'.
Merged extensions into '/usr'."
sysext unmerge >/dev/null 2>&1
rm -rf /etc/extensions/onlyrun
ln -s /dev/null /etc/extensions/onlyrun.raw
expect_eq "a /dev/null symlink is no image and masks nothing" "$(sysext list --json=short | jq -r '.[] | select(.name == "onlyrun") | .path')" /run/extensions/onlyrun
rm -f /etc/extensions/onlyrun.raw
mkdir -p /srv/store
ext "$WORKDIR" lnk 'ID=_any\n'
img_squashfs "$WORKDIR/lnk" /srv/store/img.raw
ln -s /srv/store/img.raw "$E/lnk.raw"
ext "$WORKDIR" ok 'ID=_any\n'
img_squashfs "$WORKDIR/ok" "$E/ok.sysext.raw"
ext "$E" .hidden 'ID=_any\n'
expect_eq "symlinked image listed with its resolved path" "$(sysext list --json=short | jq -r '.[] | select(.name == "lnk") | .path')" /srv/store/img.raw
expect_eq ".sysext.raw suffix" "$(sysext list --json=short | jq -r '.[] | select(.name == "ok") | .type + " " + .path')" "raw $E/ok.sysext.raw"
expect_eq "hidden directories are images too" "$(sysext list --json=short | jq -r '.[] | select(.name == ".hidden") | .path')" "$E/.hidden"
out=$(sysext merge 2>&1)
expect_eq "merge with symlinked and suffixed images" "$out" "Using extensions '.hidden', 'img.raw', 'ok.sysext.raw', 'onlyrun', 'p'.
Merged extensions into '/usr'."
expect_eq "symlinked image payload" "$(cat /usr/share/lnk/f 2>/dev/null)" lnk
sysext unmerge >/dev/null 2>&1
rm -rf /etc/extensions /run/extensions /srv/store "${E:?}"/* "${E:?}"/.hidden

echo "--- 7. versioned image directories ---"
V=$E/app.raw.v
mkdir -p "$V"
for v in 1 2 10; do
    ext "$WORKDIR" app 'ID=_any\n'
    echo "$v" > "$WORKDIR/app/usr/share/app/f"
    img_squashfs "$WORKDIR/app" "$V/app_$v.raw"
done
cp "$V/app_10.raw" "$V/app_11_$FOREIGN_ARCH.raw"
cp "$V/app_10.raw" "$V/app_12+0-3.raw"
cp "$V/app_10.raw" "$V/other_99.raw"
mkdir -p "$E/dir.v"
for v in 1.0 1.1; do
    ext "$E/dir.v" "dir_$v" 'ID=_any\n' dir
    mkdir -p "$E/dir.v/dir_$v/usr/share/dir"
    echo "$v" > "$E/dir.v/dir_$v/usr/share/dir/f"
done
mkdir -p "$E/empty.v"
l=$(sysext list --json=short)
expect_eq "vpick picks the newest native version" \
    "$(echo "$l" | jq -r '[.[] | .name + "=" + .path] | join(" ")')" "app=$V/app_10.raw dir=$E/dir.v/dir_1.1"
sysext merge >/dev/null 2>&1
expect_eq "picked versions merged" "$(cat /usr/share/app/f /usr/share/dir/f 2>/dev/null | tr '\n' ' ')" "10 1.1 "
sysext unmerge >/dev/null 2>&1
rm -f "$V/app_10.raw"
sysext merge >/dev/null 2>&1
expect_eq "the next version once the newest is gone" "$(cat /usr/share/app/f 2>/dev/null)" 2
sysext unmerge >/dev/null 2>&1
rm -rf "${E:?}"/*

finish

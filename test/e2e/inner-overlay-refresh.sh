#!/bin/sh
# e2e suite for image validation and refresh.
#
# Exercises, against systemd-sysext 262 semantics:
#   - incompatible images (ID, VERSION_ID, scope, architecture, no release
#     data) ignored with systemd's summary, compatible ones merged
#   - images shipping /usr/lib/os-release: fatal for directories (also with
#     --force), ignored raw images unless --force; raw images without any
#     release file fatal even with --force
#   - refresh change detection from the origin identity: unchanged sets are
#     skipped, replaced images and mutable mode changes remerge, --always-refresh
#   - a failing refresh leaves the previous merge in place; one that fails
#     cleaning up after the new merge went live keeps that merge
#   - refresh without qualifying images unmerges; merge over a merged
#     hierarchy fails even without images
. /work/test/e2e/lib.sh

echo "=== Installing build dependencies ==="
pkg_add kmod squashfs-tools
install_sysext
need_overlayfs
fs_supported squashfs || fatal "kernel lacks squashfs"

E=/var/lib/extensions
W=$WORKDIR
mkdir -p "$E"
HOST_ID=$(. /etc/os-release && echo "$ID")
OTHER_ARCH=$FOREIGN_ARCH

# rel NAME RELEASE [DIR] — directory sysext NAME with RELEASE as its
# extension-release, shipping /usr/share/NAME/f.
rel() {
    d=${3:-$E/$1}
    rm -rf "$d"
    mkdir -p "$d/usr/lib/extension-release.d" "$d/usr/share/$1"
    printf "$2" > "$d/usr/lib/extension-release.d/extension-release.$1"
    echo "$1" > "$d/usr/share/$1/f"
}
# raw NAME RELEASE [CONTENT] — squashfs sysext NAME built in place via rename.
raw() {
    rel "$1" "$2" "$W/$1"
    echo "${3:-$1}" > "$W/$1/usr/share/$1/f"
    mksquashfs "$W/$1" "$E/$1.raw.new" -quiet -noappend && mv -f "$E/$1.raw.new" "$E/$1.raw"
}
merged() { [ "$(cat "/usr/share/$1/f" 2>/dev/null)" != "" ]; }
dev() { cat /usr/.systemd-sysext/dev 2>/dev/null; }
SKIPMSG="Skipping extension refresh because no change was found, use --always-refresh=yes to always do a refresh."

echo "=== Running tests ==="

# --- incompatible images ------------------------------------------------------
rel good 'ID=_any\n'
rel fedora 'ID=fedora\nVERSION_ID=40\n'
rel stale "ID=$HOST_ID\nVERSION_ID=0.0.1\n"
rel scope 'ID=_any\nSYSEXT_SCOPE=initrd\n'
rel arch "ID=_any\nARCHITECTURE=$OTHER_ARCH\n"
mkdir -p "$E/norel/usr/share/norel"
out=$(sysext merge 2>&1)
rc=$?
expect_eq "merge with incompatible images exits 0" "$rc" 0
expect_eq "only the compatible image is used" "$out" "Using extensions 'good'.
Merged extensions into '/usr'."
expect_eq "extensions marker" "$(cat /usr/.systemd-sysext/extensions)" good
merged fedora && fail "incompatible image merged" || pass "incompatible images not merged"
sysext unmerge >/dev/null 2>&1
rm -rf "$E/good"
out=$(sysext merge 2>&1)
rc=$?
expect_eq "merge with only incompatible images exits 0" "$rc" 0
expect_eq "ignored summary" "$out" "No suitable extensions found (5 ignored due to incompatible image(s))."
mountpoint -q /usr && fail "/usr merged without suitable images" || pass "nothing merged"
[ -e /run/systemd/sysext ] && fail "workspace left after nothing was merged" || pass "no workspace left"
if sysext --force merge >/dev/null 2>&1 && merged fedora && merged arch; then
    pass "--force merges incompatible images"
else
    fail "--force merge"
fi
sysext unmerge >/dev/null 2>&1
rm -rf "${E:?}"/*
out=$(sysext merge 2>&1)
expect_eq "merge without images" "$out" "No extensions found."

# --- forbidden content -------------------------------------------------------------
osid=$(grep '^ID=' /usr/lib/os-release)
rel osr 'ID=_any\n'
echo ID=evil > "$E/osr/usr/lib/os-release"
out=$(sysext merge 2>&1)
rc=$?
expect_err "directory image shipping /usr/lib/os-release refused" "$rc" "$out" "Failed to read metadata for image osr: No medium found"
mountpoint -q /usr && sysext unmerge >/dev/null 2>&1
sysext --force merge >/dev/null 2>&1 && { fail "--force merged a directory image with os-release"; sysext unmerge >/dev/null 2>&1; } || pass "--force still refuses a directory image with os-release"
expect_eq "host os-release untouched" "$(grep '^ID=' /usr/lib/os-release)" "$osid"
mksquashfs "$E/osr" "$E/osr2.raw" -quiet -noappend
rm -rf "$E/osr"
mv "$E/osr2.raw" "$E/osr.raw"
rel ok 'ID=_any\n'
out=$(sysext merge 2>&1)
rc=$?
expect_eq "raw image with os-release ignored" "$rc:$out" "0:Failed to mount image: No medium found
Using extensions 'ok'.
Merged extensions into '/usr'."
expect_eq "host os-release untouched (raw)" "$(grep '^ID=' /usr/lib/os-release)" "$osid"
sysext unmerge >/dev/null 2>&1
sysext --force merge >/dev/null 2>&1 && merged osr && pass "--force merges a raw image with os-release" || fail "--force raw os-release"
sysext unmerge >/dev/null 2>&1
rm -f "$E/osr.raw"
mkdir -p "$W/nr/usr/share/nr"
mksquashfs "$W/nr" "$E/nr.raw" -quiet -noappend
for f in "" --force; do
    out=$(sysext $f merge 2>&1)
    rc=$?
    expect_err "raw image without release data is fatal ${f:-without --force}" "$rc" "$out" "Failed to read metadata for image nr: No medium found"
    mountpoint -q /usr && sysext unmerge >/dev/null 2>&1
done
rm -f "$E/nr.raw"
[ -e /run/systemd/sysext ] && fail "workspace left after a failed merge" || pass "failed merge cleaned up"

# --- refresh change detection ---------------------------------------------------
rm -rf "${E:?}"/*
raw a 'ID=_any\n' v1
rel b 'ID=_any\n'
mkdir -p "$E/b/opt/b" && echo b > "$E/b/opt/b/f"
sysext merge >/dev/null 2>&1 || fail "merge before refresh"
d0=$(dev)
out=$(sysext refresh 2>&1)
expect_eq "unchanged refresh skipped although only one image ships /opt" "$out" "$SKIPMSG"
expect_eq "skipped refresh keeps the mount" "$(dev)" "$d0"
touch "$E/b"
expect_eq "touching a directory image is no change" "$(sysext refresh 2>&1)" "$SKIPMSG"
sleep 1
raw a 'ID=_any\n' v2
out=$(sysext refresh 2>&1)
expect_eq "replaced raw image remerged" "$out" "Using extensions 'a.raw', 'b'.
Unmerged '/usr'.
Unmerged '/opt'.
Merged extensions into '/usr'.
Merged extensions into '/opt'."
expect_eq "new content visible" "$(cat /usr/share/a/f)" v2
d1=$(dev)
[ "$d1" != "$d0" ] && pass "remerge mounted a new overlay" || fail "dev marker unchanged after remerge"
touch "$E/a.raw"
out=$(sysext refresh 2>&1)
echo "$out" | grep -q "^Using extensions" && pass "touching a raw image remerges" || fail "touch raw: $out"
out=$(sysext --always-refresh=yes refresh 2>&1)
echo "$out" | grep -q "^Merged extensions into '/usr'" && pass "--always-refresh=yes remerges" || fail "--always-refresh: $out"
mkdir -p /var/lib/extensions.mutable
mount -t tmpfs tmpfs /var/lib/extensions.mutable
track_mount /var/lib/extensions.mutable
out=$(sysext --mutable=ephemeral refresh 2>&1)
echo "$out" | grep -q "^Merged extensions" && pass "mutable mode change remerges" || fail "mutable change: $out"
expect_eq "same mutable mode is no change" "$(sysext --mutable=ephemeral refresh 2>&1)" "$SKIPMSG"
sysext refresh >/dev/null 2>&1

# --- failing refresh keeps the merge --------------------------------------------
d3=$(dev)
rel fedora 'ID=fedora\nVERSION_ID=40\n'
out=$(sysext refresh 2>&1)
rc=$?
expect_eq "refresh with a new incompatible image" "$rc:$out" "0:$SKIPMSG"
head -c 1048576 /dev/urandom > "$E/junk.raw"
if sysext refresh >/dev/null 2>&1; then fail "refresh with a corrupt image succeeded"; else pass "refresh with a corrupt image fails"; fi
expect_eq "failed refresh kept the merge" "$(dev)" "$d3"
expect_eq "failed refresh kept the content" "$(cat /usr/share/a/f 2>&1)" v2
ls -d /run/systemd/sysext/next.* >/dev/null 2>&1 && fail "failed refresh left its staging area" || pass "failed refresh cleaned its staging area"
rm -f "$E/junk.raw"
rm -rf "$E/fedora"

# --- failed clean up after a refresh ----------------------------------------------
# A mount below the layers of the replaced merge makes cleaning them up fail
# once the new overlays are live: the refresh reports the merge and fails,
# and the next one finds the live merge unchanged.
B=/run/systemd/sysext/meta/usr/blocker
mkdir -p $B
mount -t tmpfs tmpfs $B && track_mount $B
out=$(sysext --always-refresh=yes refresh 2>&1)
rc=$?
case $rc:$out in
    1:*"Merged extensions into '/usr'."*"Extensions merged, but failed to clean up /run/systemd/sysext: "*)
        pass "refresh reports the merge and the failed clean up" ;;
    *) fail "refresh with a busy old generation: rc=$rc $out" ;;
esac
echo "$out" | grep -q "Failed to merge hierarchies" && fail "a refresh that merged reported as failed" ||
    pass "a refresh that merged is not reported as failed"
umount $B
expect_eq "refresh after a failed clean up finds no change" "$(sysext refresh 2>&1)" "$SKIPMSG"
expect_eq "live merge intact" "$(cat /usr/share/a/f 2>&1) $(tr '\n' ' ' < /usr/.systemd-sysext/extensions)" "v2 a b "
sysext --always-refresh=yes refresh >/dev/null 2>&1 && [ "$(cat /usr/share/a/f 2>&1)" = v2 ] &&
    pass "forced refresh after a failed clean up" || fail "forced refresh after a failed clean up"

# --- nothing left / already merged -----------------------------------------------
rm -rf "$E/b"
mv "$E/a.raw" "$W/a.raw"
out=$(sysext merge 2>&1)
expect_err "merge over a merged hierarchy without images" $? "$out" "Hierarchy '/usr' is already merged."
out=$(sysext refresh 2>&1)
expect_eq "refresh without images unmerges" "$out" "No extensions found.
Unmerged '/usr'.
Unmerged '/opt'."
mountpoint -q /usr && fail "/usr still merged" || pass "/usr unmerged by refresh"
[ -e /run/systemd/sysext ] && fail "workspace left after refresh unmerged" || pass "workspace removed"
umount /var/lib/extensions.mutable

finish

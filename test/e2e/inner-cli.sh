#!/bin/sh
# e2e suite for the command line interface, checked against systemd-sysext
# 262:
#   1. list output (sorting, time in usec, JSON shapes, empty list message)
#   2. message texts and streams, error wording
#   3. help values (--mutable=help, --json=help, help verb)
#   4. environment: *_MUTABLE_MODE, *_OVERLAYFS_MOUNT_OPTIONS (also set but
#      empty), *_HIERARCHIES
#   5. --noexec= as a tristate for both classes
#   6. configuration errors are warnings, --image-policy= is checked early
#   7. the CAP_SYS_ADMIN check
#   8. an interrupted merge leaves a consistent state
. /work/test/e2e/lib.sh
allow_skip birthtime

echo "=== Installing dependencies ==="
pkg_add squashfs-tools coreutils jq
install_sysext
need_overlayfs
fs_supported squashfs >/dev/null

E=/var/lib/extensions
C=/var/lib/confexts
W=$WORKDIR
mkdir -p $E $C

# tree DIR NAME [CLASS] — a payload with release data for NAME.
tree() {
    rm -rf "$1"
    if [ "${3:-sysext}" = confext ]; then
        mkdir -p "$1/etc/extension-release.d" "$1/etc/$2"
        echo 'ID=_any' > "$1/etc/extension-release.d/extension-release.$2"
        echo "$2" > "$1/etc/$2/f"
    else
        mkdir -p "$1/usr/lib/extension-release.d" "$1/usr/share/$2" "$1/opt/$2"
        echo 'ID=_any' > "$1/usr/lib/extension-release.d/extension-release.$2"
        echo "$2" > "$1/usr/share/$2/f"
        printf '#!/bin/sh\necho %s\n' "$2" > "$1/opt/$2/run"
        chmod 0755 "$1/opt/$2/run"
    fi
}
opts_of() { awk -v m="$1" '$5 == m {print $6 " " $NF}' /proc/self/mountinfo | tail -n 1; }

echo "=== Running tests ==="

echo "--- 1. list ---"
out=$(sysext list 2>&1 >/dev/null)
expect_eq "empty list message on stderr" "$out" "No OS extensions found."
expect_eq "empty list prints nothing" "$(sysext list 2>/dev/null)" ""
expect_eq "empty list json" "$(sysext list --json=short)" "[]"
tree $E/foo-9 foo-9
tree $E/foo-10 foo-10
tree $W/sq sq
if grep -qw squashfs /proc/filesystems && mksquashfs $W/sq $E/sq.raw -quiet -noappend; then
    l=$(sysext list --json=short)
    expect_eq "list sorts by name like strcmp" "$(echo "$l" | jq -r '[.[].name] | join(" ")')" "foo-10 foo-9 sq"
    expect_eq "list keys" "$(echo "$l" | jq -c '[.[] | keys] | unique')" '[["name","path","time","type"]]'
    expect_eq "list types" "$(echo "$l" | jq -r '[.[].type] | join(",")')" "directory,directory,raw"
    expect_eq "raw image time is the mtime in usec" "$(echo "$l" | jq '.[2].time')" \
        "$(stat -c '%.6Y' $E/sq.raw | tr -d .)"
    btime=$(stat -c '%.6W' $E/foo-9 | tr -d .)
    if [ "$btime" != 0 ] && [ "$btime" != "-" ]; then
        expect_eq "directory time is the birth time in usec" "$(echo "$l" | jq '.[1].time')" "$btime"
    else
        skip birthtime "no birth time on this filesystem"
    fi
else
    skip fs:squashfs "squashfs unavailable"
    rm -f $E/sq.raw
fi
sysext list --json=pretty | jq -e . >/dev/null && pass "pretty json parses" || fail "pretty json"
expect_eq "pretty json indents with tabs" "$(sysext list --json=pretty | sed -n 2p | od -c | head -1 | awk '{print $2}')" '\t'
expect_eq "--no-legend drops the header" "$(sysext list --no-legend | grep -c '^NAME')" 0
expect_eq "table header" "$(sysext list | head -1 | awk '{print $1, $2, $3, $4}')" "NAME TYPE PATH TIME"
rm -rf $E/foo-9 $E/foo-10

echo "--- 2. messages ---"
tree $E/m m
expect_eq "merge prints nothing on stdout" "$(sysext merge 2>/dev/null)" ""
sysext unmerge >/dev/null 2>&1
out=$(sysext merge 2>&1 >/dev/null)
case $out in
    "Using extensions 'm'"*".
Merged extensions into '/usr'.
Merged extensions into '/opt'.") pass "merge messages on stderr" ;;
    *) fail "merge messages: $out" ;;
esac
out=$(sysext merge 2>&1)
expect_eq "second merge" "$? $out" "1 Hierarchy '/usr' is already merged."
mv $E $E.off
mkdir $E
out=$(sysext merge 2>&1)
expect_eq "merge without images while merged" "$? $out" "1 Hierarchy '/usr' is already merged."
rmdir $E
mv $E.off $E
out=$(sysext unmerge 2>&1 >/dev/null)
expect_eq "unmerge messages on stderr" "$out" "Unmerged '/usr'.
Unmerged '/opt'."
expect_eq "unknown verb" "$(sysext frob 2>&1; echo $?)" "Unknown command verb 'frob', did you mean 'merge'?
1"
expect_eq "option errors name the program" "$(confext --bogus 2>&1)" "confext: unrecognized option '--bogus'"
expect_eq "ambiguous option" "$(sysext --no 2>&1)" "sysext: option '--no' is ambiguous; possibilities: --noexec, --no-reload, --no-pager, --no-legend"
expect_eq "option prefixes" "$(sysext --js=short --no-leg list | jq -r '.[0].name')" "m"
expect_eq "too many arguments" "$(sysext merge unmerge 2>&1)" "Too many arguments."
expect_eq "no Debug lines by default" "$(sysext --no-reload merge 2>&1 | grep -c Debug)" 0
sysext unmerge >/dev/null 2>&1

echo "--- 3. help ---"
expect_eq "--mutable=help" "$(sysext --mutable=help)" "Known mutability modes:
no
yes
auto
import
ephemeral
ephemeral-import"
expect_eq "--no-legend --mutable=help" "$(sysext --no-legend --mutable=help | head -1)" "no"
expect_eq "--json=help" "$(sysext --json=help | tr '\n' ' ')" "pretty short off "
expect_eq "help verb" "$(sysext help | head -1)" "> sysext [OPTION…] COMMAND …"
expect_eq "confext help" "$(confext --help | sed -n 3p)" "Merge configuration extension images into /etc/."

echo "--- 4. environment ---"
mkdir -p /etc/systemd
printf '[SysExt]\nMutable=ephemeral\n' > /etc/systemd/sysext.conf
SYSTEMD_SYSEXT_MUTABLE_MODE=no sysext merge >/dev/null 2>&1
touch /usr/e2e-w 2>/dev/null && fail "environment must beat the configuration" || pass "environment beats the configuration"
sysext unmerge >/dev/null 2>&1
rm -f /etc/systemd/sysext.conf
SYSTEMD_SYSEXT_MUTABLE_MODE=ephemeral sysext merge >/dev/null 2>&1
touch /usr/e2e-w 2>/dev/null && pass "SYSTEMD_SYSEXT_MUTABLE_MODE=ephemeral makes /usr writable" || fail "mode environment ignored"
sysext unmerge >/dev/null 2>&1
[ -e /usr/e2e-w ] && fail "ephemeral write survived" || pass "ephemeral write gone after unmerge"
SYSTEMD_SYSEXT_MUTABLE_MODE=ephemeral sysext --mutable=no merge >/dev/null 2>&1
touch /usr/e2e-w 2>/dev/null && fail "--mutable= must beat the environment" || pass "--mutable= beats the environment"
sysext unmerge >/dev/null 2>&1
out=$(SYSTEMD_SYSEXT_MUTABLE_MODE=bogus sysext merge 2>&1)
case $out in "Failed to parse SYSTEMD_SYSEXT_MUTABLE_MODE environment variable value 'bogus'. Ignoring."*) pass "invalid mode environment warned about" ;;
    *) fail "invalid mode environment: $out" ;; esac
sysext unmerge >/dev/null 2>&1
SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS=noatime,xino=off sysext merge >/dev/null 2>&1
o=$(opts_of /usr)
case $o in *noatime*xino=off*) pass "overlayfs mount options from the environment: $o" ;; *) fail "mount options: $o" ;; esac
out=$(sysext refresh 2>&1)
case $out in Skipping*) fail "changed mount options must not skip the refresh" ;; *) pass "refresh notices changed mount options" ;; esac
case $(opts_of /usr) in *xino=off*) fail "options of the old merge kept" ;; *) pass "refresh drops the old options" ;; esac
sysext unmerge >/dev/null 2>&1
sysext --mutable=ephemeral merge >/dev/null 2>&1 || fail "ephemeral merge"
case $(opts_of /usr) in *noatime*) pass "a mutable merge uses the default overlayfs options" ;; *) fail "mutable defaults missing: $(opts_of /usr)" ;; esac
sysext unmerge >/dev/null 2>&1
SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS='' sysext --mutable=ephemeral merge >/dev/null 2>&1 || fail "ephemeral merge with empty overlayfs mount options"
case $(opts_of /usr) in *noatime*) fail "empty overlayfs mount options kept the mutable defaults: $(opts_of /usr)" ;; *) pass "empty overlayfs mount options replace the mutable defaults" ;; esac
sysext unmerge >/dev/null 2>&1
expect_eq "empty hierarchy list is an error" "$(SYSTEMD_SYSEXT_HIERARCHIES='' sysext status 2>&1; echo $?)" "Failed to determine sysext hierarchies: Invalid argument
1"
expect_eq "relative hierarchy is an error" "$(SYSTEMD_SYSEXT_HIERARCHIES=usr sysext merge 2>&1; echo $?)" "Failed to determine sysext hierarchies: Invalid argument
1"
mountpoint -q /usr && fail "merged despite invalid hierarchies" || pass "nothing merged with invalid hierarchies"
expect_eq "duplicate hierarchies are accepted" "$(SYSTEMD_SYSEXT_HIERARCHIES=/usr:/usr sysext status --json=short | jq length)" 2
expect_eq "status leaves out missing hierarchies" "$(SYSTEMD_SYSEXT_HIERARCHIES=/usr:/nonexistent sysext status --json=short | jq -r '.[].hierarchy')" "/usr"

echo "--- 5. --noexec= ---"
SYSTEMD_SYSEXT_HIERARCHIES=/opt sysext --noexec=yes merge >/dev/null 2>&1
case $(opts_of /opt) in *noexec*) pass "sysext --noexec=yes mounts noexec" ;; *) fail "--noexec=yes ignored: $(opts_of /opt)" ;; esac
/opt/m/run >/dev/null 2>&1 && fail "executed from a noexec /opt" || pass "nothing executes from the noexec /opt"
SYSTEMD_SYSEXT_HIERARCHIES=/opt sysext unmerge >/dev/null 2>&1
SYSTEMD_SYSEXT_HIERARCHIES=/opt sysext merge >/dev/null 2>&1
case $(opts_of /opt) in *noexec*) fail "sysext defaults to noexec" ;; *) pass "sysext defaults to exec" ;; esac
expect_eq "sysext payload executes" "$(/opt/m/run 2>&1)" m
SYSTEMD_SYSEXT_HIERARCHIES=/opt sysext unmerge >/dev/null 2>&1
tree $C/c c confext
confext merge >/dev/null 2>&1
case $(opts_of /etc) in *noexec*) pass "confext defaults to noexec" ;; *) fail "confext default: $(opts_of /etc)" ;; esac
confext unmerge >/dev/null 2>&1
confext --noexec=no merge >/dev/null 2>&1
case $(opts_of /etc) in *noexec*) fail "confext --noexec=no ignored" ;; *) pass "confext --noexec=no clears noexec" ;; esac
confext unmerge >/dev/null 2>&1
rm -rf $C/c

echo "--- 6. configuration and image policy ---"
printf '[SysExt]\nMutable=bogus\n' > /etc/systemd/sysext.conf
out=$(sysext status 2>&1 >/dev/null)
expect_eq "invalid Mutable= is a warning" "$?:$out" "0:/etc/systemd/sysext.conf:2: Failed to parse Mutable=bogus, ignoring: Invalid argument"
sysext merge >/dev/null 2>&1
touch /usr/e2e-w 2>/dev/null && fail "/usr writable after an invalid Mutable=" || pass "invalid Mutable= falls back to read-only"
sysext unmerge >/dev/null 2>&1
rm -f /etc/systemd/sysext.conf
out=$(sysext --image-policy=bogus merge 2>&1)
expect_eq "invalid --image-policy= fails early" "$?:$out" "1:Failed to parse image policy: bogus"
mountpoint -q /usr && fail "merged with an invalid policy" || pass "nothing merged with an invalid policy"

echo "--- 7. privileges ---"
if setpriv --inh-caps=-sys_admin --bounding-set=-sys_admin true 2>/dev/null; then
    sysext merge >/dev/null 2>&1
    out=$(setpriv --inh-caps=-sys_admin --bounding-set=-sys_admin sysext refresh --always-refresh=yes 2>&1)
    expect_eq "refresh without CAP_SYS_ADMIN" "$?:$out" "1:Need to be privileged."
    [ -f /usr/share/m/f ] && pass "the merge is intact after a refused refresh" || fail "merge damaged"
    expect_eq "status needs no privileges" "$(setpriv --inh-caps=-sys_admin --bounding-set=-sys_admin sysext status --json=short | jq -r '.[] | select(.hierarchy == "/usr") | .extensions[0]')" m
    sysext unmerge >/dev/null 2>&1
else
    skip capdrop "setpriv cannot drop CAP_SYS_ADMIN"
fi

echo "--- 8. interrupted merge ---"
i=0
while [ $i -lt 40 ]; do
    tree $W/s$i s$i
    if [ -f $E/sq.raw ]; then
        mksquashfs $W/s$i $E/s$i.raw -quiet -noappend
    else
        cp -a $W/s$i $E/s$i
    fi
    i=$((i + 1))
done
total=$(sysext list --json=short | jq length)
for delay in 0.01 0.03 0.05 0.08; do
    sysext merge >/dev/null 2>&1 &
    p=$!
    sleep $delay
    kill -TERM $p 2>/dev/null
    wait $p
    rc=$?
    if mountpoint -q /usr; then
        n=$(sysext status --json=short | jq '.[] | select(.hierarchy == "/usr") | .extensions | length')
        expect_eq "merge interrupted after ${delay}s (rc $rc) is complete" "$n" "$total"
        sysext unmerge >/dev/null 2>&1
    else
        left=$(awk '$5 ~ "^/run/systemd/sysext" {print $5}' /proc/self/mountinfo)
        expect_eq "merge interrupted after ${delay}s (rc $rc) leaves no mounts" "$left" ""
    fi
    case $rc in 0|143) pass "exit status $rc" ;; *) fail "unexpected exit status $rc" ;; esac
done
mkdir -p /run/systemd
(flock -x 9; sleep 2) 9>/run/systemd/sysext.lock &
holder=$!
sleep 0.3
sysext merge >/dev/null 2>&1 &
p=$!
sleep 0.3
kill -TERM $p
wait $p
expect_eq "a merge waiting for the lock ends on SIGTERM" "$?" 143
wait $holder
mountpoint -q /usr && fail "the terminated merge went ahead" || pass "the terminated merge did nothing"
sysext merge >/dev/null 2>&1 && pass "merge after the interruptions" || fail "merge after the interruptions"
sysext unmerge >/dev/null 2>&1

echo "--- cleanup ---"
left=$(awk '$5 ~ "^/run/systemd" || $5 == "/usr" || $5 == "/etc" || $5 == "/opt" {print $5}' /proc/self/mountinfo)
expect_eq "no merge left behind" "$left" ""
released
expect_eq "no loop devices left" "$(our_loops | tr '\n' ' ')" ""

finish

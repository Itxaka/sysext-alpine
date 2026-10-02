#!/bin/sh
# e2e suite for the OpenRC integration, with OpenRC installed and marked as
# running (/run/openrc/softlevel):
#   1. EXTENSION_RELOAD_MANAGER= refreshes the dependency cache on merge and
#      unmerge
#   2. EXTENSION_RESTART_UNITS= on merge, refresh and unmerge, including a
#      service shipped by a confext that is stopped before its script goes
#   3. EXTENSION_RELOAD_OR_RESTART_UNITS= reloads or restarts
#   4. --no-reload and --root= leave OpenRC alone
#   5. the init scripts: start (refresh), reload, stop (restarting nothing
#      while the system goes down), status, conf.d options, confext noexec
#      handling
#   6. the kernel command line switch when started by OpenRC, also from the
#      arguments of PID 1 in a container
#   7. the services' position in the boot runlevel
# Daemons stopped by OpenRC must be reaped, which the shell running as PID 1
# of the container does not do while it waits for a command substitution.
set -u
if [ $$ -eq 1 ]; then
    apk add --no-cache tini >/dev/null || { echo "FATAL: apk add tini failed"; exit 1; }
    exec /sbin/tini -s -- /bin/sh "$0" "$@"
fi

. /work/test/e2e/lib.sh

echo "=== Installing dependencies ==="
pkg_add openrc jq
install_sysext
need_overlayfs

mkdir -p /run/openrc
touch /run/openrc/softlevel
rc-update -u >/dev/null 2>&1

E=/var/lib/extensions
C=/var/lib/confexts
mkdir -p $E $C

# service NAME [BODY] — write an init script running sleep in the background.
service() {
    cat > "${3:-/etc/init.d}/$1" <<EOS
#!/sbin/openrc-run
command=/bin/sleep
command_args=1000
command_background=yes
pidfile=/run/$1.pid
${2:-}
EOS
    chmod 0755 "${3:-/etc/init.d}/$1"
}
pid() { cat "/run/$1.pid" 2>/dev/null; }
# start NAME — start a service and wait until its daemon runs: stopping it
# while start-stop-daemon has not exec'd sleep yet fails.
start() {
    rc-service "$1" start >/dev/null 2>&1
    i=0
    while [ $i -lt 30 ] && [ "$(readlink "/proc/$(pid "$1")/exe" 2>/dev/null)" != /bin/busybox ]; do
        sleep 0.1
        i=$((i + 1))
    done
}
running() { [ -n "$(pid "$1")" ] && kill -0 "$(pid "$1")" 2>/dev/null; }

# sysext_dir NAME RELEASE — a directory sysext with the given release data.
sysext_dir() {
    rm -rf "${E:?}/$1"
    mkdir -p "$E/$1/usr/lib/extension-release.d" "$E/$1/usr/share/$1"
    printf "$2" > "$E/$1/usr/lib/extension-release.d/extension-release.$1"
    echo "$1" > "$E/$1/usr/share/$1/f"
}
# confext_dir NAME RELEASE — a directory confext with the given release data.
confext_dir() {
    rm -rf "${C:?}/$1"
    mkdir -p "$C/$1/etc/extension-release.d" "$C/$1/etc/init.d"
    printf "$2" > "$C/$1/etc/extension-release.d/extension-release.$1"
}
in_deptree() { grep -q "$1" /run/openrc/deptree 2>/dev/null; }

echo "=== Running tests ==="

echo "--- 1. EXTENSION_RELOAD_MANAGER ---"
confext_dir svc 'ID=_any\nEXTENSION_RELOAD_MANAGER=yes\n'
service extsvc "" "$C/svc/etc/init.d"
in_deptree extsvc && fail "extsvc known before the merge"
out=$(confext --noexec=no merge 2>&1) || fail "confext merge: $out"
in_deptree extsvc && pass "dependency cache refreshed after merge (EXTENSION_RELOAD_MANAGER=yes)" \
    || fail "dependency cache stale after merge: $out"
rc-service extsvc start >/dev/null 2>&1 && running extsvc && pass "service shipped by the confext starts" \
    || fail "extsvc does not start"
rc-service extsvc stop >/dev/null 2>&1
confext unmerge >/dev/null 2>&1 || fail "confext unmerge"
in_deptree extsvc && fail "dependency cache still lists extsvc after unmerge" \
    || pass "dependency cache refreshed after unmerge"
rm -rf "${C:?}/svc"

echo "--- 2. EXTENSION_RESTART_UNITS ---"
service hostsvc
start hostsvc
p0=$(pid hostsvc)
sysext_dir rs 'ID=_any\nEXTENSION_RESTART_UNITS="hostsvc.service bogus/name"\n'
out=$(sysext merge 2>&1) || fail "merge: $out"
p1=$(pid hostsvc)
[ -n "$p1" ] && [ "$p1" != "$p0" ] && running hostsvc && pass "merge restarts hostsvc ($p0 -> $p1)" \
    || fail "hostsvc not restarted on merge: $p0 -> $p1: $out"
echo "$out" | grep -qx "Invalid unit name 'bogus/name' in EXTENSION_RESTART_UNITS= of rs, ignoring." \
    && pass "invalid unit names are warned about" || fail "no warning for bogus/name: $out"
sysext refresh >/dev/null 2>&1
expect_eq "unchanged refresh restarts nothing" "$(pid hostsvc)" "$p1"
sysext refresh --always-refresh=yes >/dev/null 2>&1
p2=$(pid hostsvc)
[ "$p2" != "$p1" ] && pass "refresh restarts hostsvc" || fail "hostsvc not restarted on refresh"
sysext unmerge >/dev/null 2>&1
p3=$(pid hostsvc)
[ -n "$p3" ] && [ "$p3" != "$p2" ] && running hostsvc && pass "unmerge restarts hostsvc" \
    || fail "hostsvc not restarted on unmerge: $p2 -> $p3"
rc-service hostsvc stop >/dev/null 2>&1
sysext merge >/dev/null 2>&1
running hostsvc && pass "a stopped unit is started (RestartUnit)" || fail "stopped hostsvc not started"
sysext unmerge >/dev/null 2>&1
rm -rf "${E:?}/rs"

confext_dir shipped 'ID=_any\nEXTENSION_RESTART_UNITS=shipsvc.service\n'
service shipsvc "" "$C/shipped/etc/init.d"
confext --noexec=no merge >/dev/null 2>&1
ps=$(pid shipsvc)
running shipsvc && pass "merge starts the service the confext ships" || fail "shipsvc not started on merge"
confext unmerge >/dev/null 2>&1
if [ -n "$ps" ] && ! kill -0 "$ps" 2>/dev/null; then
    pass "the shipped service is stopped before its script goes away"
else
    fail "shipsvc leaked after unmerge (pid $ps)"
fi
[ -e /etc/init.d/shipsvc ] && fail "shipsvc script still present" || pass "shipsvc script gone with the confext"
confext --noexec=no merge >/dev/null 2>&1
ps=$(pid shipsvc)
mv "$C/shipped" /tmp/shipped
out=$(confext refresh 2>&1)
if [ -n "$ps" ] && ! kill -0 "$ps" 2>/dev/null && ! mountpoint -q /etc; then
    pass "refresh without extensions stops the shipped service, then unmerges"
else
    fail "refresh without extensions: pid $ps, $out"
fi
rm -rf /tmp/shipped

echo "--- 3. EXTENSION_RELOAD_OR_RESTART_UNITS ---"
service relsvc 'extra_started_commands="reload"
reload() { touch /run/relsvc.reloaded; }'
service plainsvc
service stoppedsvc
start relsvc
start plainsvc
pr=$(pid relsvc)
pp=$(pid plainsvc)
sysext_dir ror 'ID=_any\nEXTENSION_RELOAD_OR_RESTART_UNITS="relsvc.service plainsvc.service stoppedsvc.service gone.service"\n'
sysext merge >/dev/null 2>&1
[ -e /run/relsvc.reloaded ] && [ "$(pid relsvc)" = "$pr" ] && pass "a service implementing reload is reloaded" \
    || fail "relsvc: reloaded=$([ -e /run/relsvc.reloaded ] && echo y) pid $pr -> $(pid relsvc)"
[ "$(pid plainsvc)" != "$pp" ] && running plainsvc && pass "a service without reload is restarted" \
    || fail "plainsvc not restarted"
running stoppedsvc && pass "a stopped service is started" || fail "stoppedsvc not started"
sysext unmerge >/dev/null 2>&1
for s in relsvc plainsvc stoppedsvc; do rc-service $s stop >/dev/null 2>&1; done
rm -rf "${E:?}/ror"

sysext_dir both 'ID=_any\nEXTENSION_RESTART_UNITS=relsvc.service\nEXTENSION_RELOAD_OR_RESTART_UNITS=relsvc.service\n'
rm -f /run/relsvc.reloaded
start relsvc
pr=$(pid relsvc)
sysext merge >/dev/null 2>&1
[ ! -e /run/relsvc.reloaded ] && [ "$(pid relsvc)" != "$pr" ] && pass "restart wins over reload-or-restart" \
    || fail "relsvc listed in both fields was not just restarted"
sysext --no-reload unmerge >/dev/null 2>&1
rc-service relsvc stop >/dev/null 2>&1
rm -rf "${E:?}/both"

echo "--- 4. --no-reload and --root= ---"
sysext_dir quiet 'ID=_any\nEXTENSION_RELOAD_MANAGER=1\nEXTENSION_RESTART_UNITS=hostsvc.service\n'
start hostsvc
p=$(pid hostsvc)
rc-update -u >/dev/null 2>&1
m0=$(stat -c %Y /run/openrc/deptree)
sleep 1.1
sysext --no-reload merge >/dev/null 2>&1
mountpoint -q /usr && pass "merge --no-reload" || fail "merge --no-reload"
sysext --no-reload unmerge >/dev/null 2>&1
expect_eq "--no-reload restarts nothing" "$(pid hostsvc)" "$p"
expect_eq "--no-reload leaves the dependency cache alone" "$(stat -c %Y /run/openrc/deptree)" "$m0"

R=/tmp/root
track_root $R
mkdir -p $R/usr/lib $R/etc $R/opt $R/var/lib/extensions $R/run/openrc
printf 'ID=alpine\n' > $R/usr/lib/os-release
cp -a "$E/quiet" $R/var/lib/extensions/
(cd / && sysext --root=tmp/root merge >/dev/null 2>&1) && mountpoint -q $R/usr && ! mountpoint -q /usr \
    && pass "merge with a relative --root" || fail "merge --root"
expect_eq "status --root" "$(sysext --root=$R status --json=short | jq -c '[.[] | select(.hierarchy == "/usr") | .extensions]')" '[["quiet"]]'
(cd /tmp && sysext --root=/tmp/root unmerge >/dev/null 2>&1) && ! mountpoint -q $R/usr \
    && pass "unmerge with an absolute --root" || fail "unmerge --root"
expect_eq "--root restarts nothing" "$(pid hostsvc)" "$p"
expect_eq "--root leaves the host dependency cache alone" "$(stat -c %Y /run/openrc/deptree)" "$m0"
rc-service hostsvc stop >/dev/null 2>&1
rm -rf "${E:?}/quiet"

echo "--- 5. init scripts ---"
install -m 0755 /work/packaging/openrc/sysext.initd /etc/init.d/sysext
install -m 0755 /work/packaging/openrc/confext.initd /etc/init.d/confext
install -m 0644 /work/packaging/openrc/sysext.confd /etc/conf.d/sysext
install -m 0644 /work/packaging/openrc/confext.confd /etc/conf.d/confext
rc-update -u >/dev/null 2>&1
sysext_dir one 'ID=_any\n'
rc-service sysext status >/dev/null 2>&1
expect_eq "status of the stopped service" "$?" 3
sysext merge >/dev/null 2>&1
rc-service sysext start >/dev/null 2>&1 && pass "start works while already merged (refresh)" || fail "start after a manual merge"
rc-service sysext status >/dev/null 2>&1
expect_eq "status of the started service" "$?" 0
sysext_dir two 'ID=_any\n'
rc-service sysext reload >/dev/null 2>&1 && [ -f /usr/share/two/f ] && pass "reload merges a new image" \
    || fail "reload: $(sysext status)"
rc-service sysext zap >/dev/null 2>&1
rc-service sysext start >/dev/null 2>&1 && pass "start after zap" || fail "start after zap"
rc-service sysext stop >/dev/null 2>&1 && ! mountpoint -q /usr && pass "stop unmerges" || fail "stop"
rc-service sysext status >/dev/null 2>&1
expect_eq "status after stop" "$?" 3
echo 'SYSEXT_OPTS="--mutable=ephemeral"' > /etc/conf.d/sysext
rc-service sysext start >/dev/null 2>&1
touch /usr/share/written 2>/dev/null && pass "SYSEXT_OPTS from conf.d reach sysext" || fail "SYSEXT_OPTS ignored"
rc-service sysext stop >/dev/null 2>&1
[ -e /usr/share/written ] && fail "ephemeral write survived the unmerge" || pass "ephemeral write gone after stop"
install -m 0644 /work/packaging/openrc/sysext.confd /etc/conf.d/sysext
rm -rf "${E:?}/one" "${E:?}/two"

# Stopping while the system goes down restarts nothing. openrc-run passes on
# RC_GOINGDOWN, which openrc sets then, only when openrc started it.
sysext_dir down 'ID=_any\nEXTENSION_RESTART_UNITS=hostsvc.service\n'
echo 'rc_env_allow="RC_GOINGDOWN"' >> /etc/rc.conf
rc-service sysext start >/dev/null 2>&1
rc-service hostsvc stop >/dev/null 2>&1
RC_GOINGDOWN=YES rc-service sysext stop >/dev/null 2>&1
! mountpoint -q /usr && pass "stop while going down unmerges" || fail "stop while going down"
running hostsvc && fail "stop while going down restarted hostsvc" || pass "stop while going down restarts nothing"
rc-service sysext start >/dev/null 2>&1
rc-service hostsvc stop >/dev/null 2>&1
rc-service sysext stop >/dev/null 2>&1
i=0
while [ $i -lt 30 ] && ! running hostsvc; do
    sleep 0.1
    i=$((i + 1))
done
running hostsvc && pass "any other stop restarts the units" || fail "stop did not restart hostsvc"
rc-service hostsvc stop >/dev/null 2>&1
rm -rf "${E:?}/down"

confext_dir conf 'ID=_any\n'
echo conf > "$C/conf/etc/conf-marker"
rc-service confext start >/dev/null 2>&1 || fail "rc-service confext start"
[ "$(cat /etc/conf-marker 2>/dev/null)" = conf ] && pass "confext service merges /etc" || fail "confext not merged"
opts=$(awk '$2 == "/etc" {print $4}' /proc/mounts | tail -n 1)
case ",$opts," in *,noexec,*) fail "confext service mounted /etc noexec: $opts" ;; *) pass "confext service keeps /etc executable" ;; esac
rc-service local describe >/dev/null 2>&1 && pass "OpenRC scripts in /etc/init.d still run" || fail "/etc/init.d not executable"
touch /etc/e2e-write 2>/dev/null && fail "merged /etc is writable" || pass "merged /etc is read-only"
rc-service confext stop >/dev/null 2>&1 && ! mountpoint -q /etc && pass "confext service stop" || fail "confext stop"
echo 'CONFEXT_OPTS="--noexec=false --mutable=ephemeral"' > /etc/conf.d/confext
rc-service confext start >/dev/null 2>&1
touch /etc/e2e-write 2>/dev/null && pass "CONFEXT_OPTS=--mutable=ephemeral makes /etc writable" || fail "ephemeral /etc not writable"
rc-service confext stop >/dev/null 2>&1
[ -e /etc/e2e-write ] && fail "ephemeral /etc write survived" || pass "ephemeral /etc write gone after stop"
install -m 0644 /work/packaging/openrc/confext.confd /etc/conf.d/confext
out=$(confext merge 2>&1)
opts=$(awk '$2 == "/etc" {print $4}' /proc/mounts | tail -n 1)
case ",$opts," in *,noexec,*) pass "plain confext merge is noexec, like systemd" ;; *) fail "confext default lost noexec: $opts" ;; esac
echo "$out" | grep -q "OpenRC cannot execute the init scripts in /etc/init.d/" && pass "noexec /etc is warned about" \
    || fail "no noexec warning: $out"
confext unmerge >/dev/null 2>&1
rm -rf "${C:?}/conf"

echo "--- 6. kernel command line switch ---"
sysext_dir ks 'ID=_any\n'
echo 'rc_env_allow="SYSTEMD_PROC_CMDLINE"' >> /etc/rc.conf
out=$(SYSTEMD_PROC_CMDLINE='quiet systemd.sysext=0' rc-service sysext start 2>&1)
rc=$?
if [ $rc -eq 0 ] && ! mountpoint -q /usr && echo "$out" | grep -q "Disabled by the kernel command line option 'systemd.sysext='"; then
    pass "systemd.sysext=0 turns the service into a no-op"
else
    fail "kill switch: rc=$rc $out"
fi
SYSTEMD_PROC_CMDLINE='quiet systemd.sysext=0' rc-service sysext stop >/dev/null 2>&1
SYSTEMD_PROC_CMDLINE='systemd.sysext=0 systemd.sysext=1' rc-service sysext start >/dev/null 2>&1
mountpoint -q /usr && pass "the last systemd.sysext= wins" || fail "systemd.sysext=1 after =0 ignored"
rc-service sysext stop >/dev/null 2>&1
out=$(SYSTEMD_PROC_CMDLINE=systemd.sysext=0 RC_SVCNAME=sysext sysext merge 2>&1)
! mountpoint -q /usr && pass "RC_SVCNAME marks a service invocation" || fail "kill switch with RC_SVCNAME: $out"
SYSTEMD_PROC_CMDLINE=systemd.sysext=0 sysext merge >/dev/null 2>&1
mountpoint -q /usr && pass "manual invocation ignores the switch" || fail "manual merge with systemd.sysext=0"
sysext unmerge >/dev/null 2>&1
# In a container the kernel command line words are the arguments of PID 1,
# without the options PID 1 takes and their values.
out=$(env -u SYSTEMD_PROC_CMDLINE unshare -pf --mount-proc /bin/sh -c 'RC_SVCNAME=sysext sysext merge 2>&1; :' sh systemd.sysext=0)
case $out in
    *"Disabled by the kernel command line option 'systemd.sysext='"*) pass "systemd.sysext=0 among the arguments of PID 1 disables a service merge" ;;
    *) fail "kill switch from the arguments of PID 1: $out" ;;
esac
out=$(env -u SYSTEMD_PROC_CMDLINE unshare -pf --mount-proc /bin/sh -c 'RC_SVCNAME=sysext sysext merge 2>&1; sysext unmerge >/dev/null 2>&1' sh --log-level systemd.sysext=0)
case $out in
    *"Using extensions "*) pass "the value of an option of PID 1 is no kernel command line word" ;;
    *) fail "option value of PID 1 taken as a kernel command line word: $out" ;;
esac
out=$(SYSTEMD_PROC_CMDLINE=systemd.confext=0 rc-service confext start 2>&1)
echo "$out" | grep -q "'systemd.confext='" && ! mountpoint -q /etc && pass "systemd.confext=0 for the confext service" \
    || fail "confext kill switch: $out"
rc-service confext stop >/dev/null 2>&1
rm -rf "${E:?}/ks"

echo "--- 7. boot runlevel order ---"
sysext_dir boot 'ID=_any\n'
for s in sysext confext bootmisc sysctl modules; do rc-update add $s boot >/dev/null 2>&1; done
out=$(openrc boot 2>&1)
line() { echo "$out" | grep -n "$1" | head -n 1 | cut -d: -f1; }
mods=$(line "Loading modules")
sys=$(line "Merging system extensions")
conf=$(line "Merging configuration extensions")
sysctl=$(line "Configuring kernel parameters")
misc=$(line "Cleaning /tmp")
if [ -n "$mods" ] && [ -n "$sys" ] && [ -n "$conf" ] && [ -n "$sysctl" ] && [ -n "$misc" ] &&
    [ "$mods" -lt "$sys" ] && [ "$mods" -lt "$conf" ] && [ "$sys" -lt "$sysctl" ] && [ "$conf" -lt "$sysctl" ] &&
    [ "$sysctl" -lt "$misc" ]; then
    pass "boot runlevel: modules, then the extensions, then sysctl and bootmisc"
else
    fail "boot order: $out"
fi
[ -f /usr/share/boot/f ] && pass "extensions merged at boot" || fail "boot runlevel did not merge"
rc-service sysext stop >/dev/null 2>&1
rc-service confext stop >/dev/null 2>&1
rm -rf "${E:?}/boot"

echo "--- cleanup ---"
sysext unmerge >/dev/null 2>&1
confext unmerge >/dev/null 2>&1
left=$(awk '$5 ~ "^/run/systemd" || $5 == "/usr" || $5 == "/etc" || $5 == "/opt" {print $5}' /proc/self/mountinfo)
expect_eq "no merge left behind" "$left" ""
released
expect_eq "no loop devices left" "$(our_loops | tr '\n' ' ')" ""

finish

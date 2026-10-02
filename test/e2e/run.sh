#!/bin/sh
# Host-side e2e runner: runs each suite (test/e2e/inner*.sh) in a fresh
# privileged container and prints a summary. Exits non-zero when a suite
# fails, aborts or times out.
#
# Usage: test/e2e/run.sh [SUITE...]
#   SUITE is a file name (inner-cli.sh) or a short name (cli, inner-cli);
#   default: every suite.
#
# Environment:
#   IMAGE            image for suites without an e2e-image header
#   E2E_PROPAGATION  private (default), shared, or "private shared": with
#                    shared each suite runs after mount --make-rshared /
#   E2E_STRICT=1     a skip the suite does not accept is a failure (CI)
#   E2E_ALLOW_SKIP   additional acceptable skip tags (space separated)
#   E2E_EXCLUDE      suites to leave out (same names as SUITE)
#   E2E_TIMEOUT      per-suite timeout in seconds (default 1200)
#   E2E_LOGDIR       keep each suite's output there
#   E2E_ARTIFACTS    directory mounted at /artifacts in the containers; the
#                    differential suite leaves its transcripts there
#   E2E_FIXTURES=0   do not build the systemd-repart fixtures
#   COVDIR           collect binary coverage of every run (make e2e-cover)
#   DOCKER           container runtime (default docker; podman works)
#
# Suite headers:
#   # e2e-image: IMAGE        run the suite in IMAGE instead of $IMAGE
#   # e2e-propagation: MODE   the suite sets up propagation itself; it runs
#                             once per invocation, whatever E2E_PROPAGATION is
#   # e2e-fixtures: repart    the suite uses the systemd-repart fixtures
#                             (fixtures.sh builds missing ones first)
#
# Requires: docker (or podman), a static bin/sysext (make build).
set -eu

REPO=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
DOCKER=${DOCKER:-docker}
IMAGE=${IMAGE:-alpine:3.24}
FIXTURE_IMAGE=archlinux:latest
PROPAGATIONS=${E2E_PROPAGATION:-private}
TIMEOUT=${E2E_TIMEOUT:-1200}
RUN_ID=$(date +%s)-$$

if [ ! -x "$REPO/bin/sysext" ]; then
    echo "ERROR: $REPO/bin/sysext not found. Run 'make build' first." >&2
    exit 1
fi
# Suites run the binary on Alpine (musl) and Arch (glibc): it must be static.
if command -v readelf >/dev/null 2>&1 && readelf -lW "$REPO/bin/sysext" | grep -q 'program interpreter'; then
    echo "ERROR: $REPO/bin/sysext is dynamically linked; build it with CGO_ENABLED=0 (make build)." >&2
    exit 1
fi

for p in $PROPAGATIONS; do
    case $p in
        private | shared) ;;
        *) echo "ERROR: E2E_PROPAGATION: unknown mode '$p'" >&2; exit 1 ;;
    esac
done

# suite_file NAME — the suite's file name for a SUITE argument.
suite_file() {
    for f in "$1" "$1.sh" "inner-$1.sh" "inner-$1"; do
        if [ -f "$REPO/test/e2e/$f" ]; then
            echo "$f"
            return 0
        fi
    done
    [ "$1" = inner ] && { echo inner.sh; return 0; }
    echo "ERROR: unknown suite '$1'" >&2
    return 1
}

# header SUITE KEY — value of a "# e2e-KEY: value" line in SUITE.
header() {
    sed -n "s/^# e2e-$2: *//p" "$REPO/test/e2e/$1" | head -n 1
}

SUITES=""
if [ $# -gt 0 ]; then
    for a in "$@"; do SUITES="$SUITES $(suite_file "$a")"; done
else
    SUITES=$(cd "$REPO/test/e2e" && ls inner*.sh)
fi
EXCLUDED=" "
for a in ${E2E_EXCLUDE:-}; do EXCLUDED="$EXCLUDED$(suite_file "$a") "; done

# Coverage mode: when COVDIR is set (see `make e2e-cover`), mount it into the
# container and point the instrumented binary's GOCOVERDIR at it, so every
# sysext/confext invocation in the suites emits binary coverage data.
MOUNTARGS=""
if [ -n "${COVDIR:-}" ]; then
    mkdir -p "$COVDIR"
    MOUNTARGS="-v $COVDIR:/covdata -e GOCOVERDIR=/covdata"
fi
if [ -n "${E2E_ARTIFACTS:-}" ]; then
    mkdir -p "$E2E_ARTIFACTS"
    MOUNTARGS="$MOUNTARGS -v $(CDPATH='' cd -- "$E2E_ARTIFACTS" && pwd):/artifacts"
fi

# Every E2E_* variable reaches the suites (E2E_STRICT, E2E_ALLOW_SKIP,
# suite-specific knobs).
ENVARGS=""
for v in $(env | sed -n 's/^\(E2E_[A-Za-z0-9_]*\)=.*/\1/p'); do
    [ "$v" = E2E_PROPAGATION ] || ENVARGS="$ENVARGS -e $v"
done

TMP=$(mktemp -d)
CURRENT=""
cleanup() {
    [ -n "$CURRENT" ] && "$DOCKER" rm -f "$CURRENT" >/dev/null 2>&1
    # The suites run as root (some under umask 077): hand coverage data and
    # artifacts back to the caller.
    for d in ${COVDIR:-} ${E2E_ARTIFACTS:-}; do
        [ -d "$d" ] && "$DOCKER" run --rm -v "$(CDPATH='' cd -- "$d" && pwd):/out" "$IMAGE" \
            chown -R "$(id -u):$(id -g)" /out >/dev/null 2>&1
    done
    rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "${E2E_LOGDIR:-}" ]; then
    mkdir -p "$E2E_LOGDIR"
fi

# Fixtures built by systemd-repart (archlinux ships it); suites skip what is
# missing.
need_fixtures=0
for suite in $SUITES; do
    case $EXCLUDED in *" $suite "*) continue ;; esac
    [ -n "$(header "$suite" fixtures)" ] && need_fixtures=1
done
if [ "$need_fixtures" = 1 ] && [ "${E2E_FIXTURES:-1}" != 0 ] &&
    { [ ! -f "$REPO/examples/signed-example.raw" ] || [ ! -f "$REPO/test/fixtures/repart-4k/repart4k.raw" ]; }; then
    echo "=== building systemd-repart fixtures in $FIXTURE_IMAGE ==="
    "$DOCKER" run --rm -v "$REPO:/work" -e FIXTURE_OWNER="$(id -u):$(id -g)" "$FIXTURE_IMAGE" \
        /bin/sh /work/test/e2e/fixtures.sh ||
        echo "WARNING: building the fixtures failed; the suites using them will skip" >&2
fi

# run_suite SUITE IMAGE PROPAGATION LOG — run one suite, tee its output to
# LOG; returns the container's exit status. On timeout, timeout(1) signals
# docker run, which forwards SIGTERM to the suite (its trap cleans up).
run_suite() {
    name=e2e-$RUN_ID-$(echo "${1%.sh}" | tr -c 'a-zA-Z0-9\n' -)-$3
    case $3 in
        shared) cmd='mount --make-rshared / && exec /bin/sh "$0"' ;;
        *) cmd='exec /bin/sh "$0"' ;;
    esac
    CURRENT=$name
    {
        set +e
        # shellcheck disable=SC2086  # ENVARGS and MOUNTARGS are word-split
        timeout "$TIMEOUT" "$DOCKER" run --privileged --rm --name "$name" \
            -v "$REPO:/work:ro" \
            -v /lib/modules:/lib/modules:ro \
            -e E2E_PROPAGATION="$3" \
            $ENVARGS $MOUNTARGS \
            "$2" /bin/sh -c "$cmd" "/work/test/e2e/$1" 2>&1
        echo $? > "$TMP/rc"
    } | tee "$4"
    rc=$(cat "$TMP/rc")
    if [ "$rc" = 124 ]; then
        echo "FAIL: suite timed out after ${TIMEOUT}s" | tee -a "$4"
        "$DOCKER" rm -f "$name" >/dev/null 2>&1 || true
    fi
    CURRENT=""
    return "$rc"
}

SUMMARY=$TMP/summary
: > "$SUMMARY"
status=0
ran_once=" "
for prop in $PROPAGATIONS; do
    for suite in $SUITES; do
        case $EXCLUDED in *" $suite "*) continue ;; esac
        image=$(header "$suite" image)
        image=${image:-$IMAGE}
        sprop=$(header "$suite" propagation)
        if [ -n "$sprop" ]; then
            case $ran_once in *" $suite "*) continue ;; esac
            ran_once="$ran_once$suite "
            mode=$sprop
        else
            mode=$prop
        fi
        log=$TMP/$suite.$mode.log
        echo "=== e2e suite: $suite (image $image, propagation $mode) ==="
        start=$(date +%s)
        if run_suite "$suite" "$image" "$mode" "$log"; then rc=0; else rc=$?; fi
        secs=$(($(date +%s) - start))
        line=$(grep '^E2E-SUMMARY ' "$log" | tail -n 1)
        p=$(echo "$line" | sed -n 's/.* pass=\([0-9]*\).*/\1/p')
        f=$(echo "$line" | sed -n 's/.* fail=\([0-9]*\).*/\1/p')
        s=$(echo "$line" | sed -n 's/.* skip=\([0-9]*\).*/\1/p')
        if [ -z "$line" ]; then
            result=ABORTED
        elif [ "$rc" -ne 0 ] || [ "${f:-1}" -ne 0 ]; then
            result=FAIL
        else
            result=PASS
        fi
        [ "$result" = PASS ] || status=1
        printf '%-28s %-18s %-8s %5s %5s %5s %5ss  %s\n' "$suite" "$image" "$mode" "${p:--}" "${f:--}" "${s:--}" "$secs" "$result" >> "$SUMMARY"
        if [ -n "${E2E_LOGDIR:-}" ]; then
            cp "$log" "$E2E_LOGDIR/${suite%.sh}.$mode.log"
        fi
    done
done

echo
echo "=== e2e summary ==="
printf '%-28s %-18s %-8s %5s %5s %5s %6s  %s\n' SUITE IMAGE MODE PASS FAIL SKIP TIME RESULT
cat "$SUMMARY"
awk '{p += $4; f += $5; s += $6; n++; if ($8 != "PASS") bad++}
    END {printf "%d suite runs, %d passed checks, %d failed, %d skipped, %d suite runs not passing\n", n, p, f, s, bad}' "$SUMMARY"
if [ $status -ne 0 ]; then
    echo "RESULT: FAIL"
else
    echo "RESULT: PASS"
fi
exit $status

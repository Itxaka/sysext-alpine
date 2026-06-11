#!/bin/sh
# Host-side e2e wrapper: launches a privileged Alpine container and runs
# the actual test suite (inner.sh) inside it.
#
# Requires: docker (or podman via DOCKER=podman), bin/sysext built
# (make build-static).
set -eu

REPO=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
DOCKER=${DOCKER:-docker}
IMAGE=${IMAGE:-alpine:3.21}

if [ ! -x "$REPO/bin/sysext" ]; then
    echo "ERROR: $REPO/bin/sysext not found. Run 'make build-static' first." >&2
    exit 1
fi

# Optional argument: which inner suite(s) to run (basenames relative to
# test/e2e/). Default: every inner*.sh suite, each in a fresh container.
SUITES=${1:-}
if [ -z "$SUITES" ]; then
    SUITES=$(cd "$REPO/test/e2e" && ls inner*.sh)
fi

# Host kernel modules are mounted read-only so the (privileged) container can
# modprobe squashfs/erofs/etc. for the host kernel.
rc=0
for suite in $SUITES; do
    echo "=== e2e suite: $suite ==="
    "$DOCKER" run --privileged --rm \
        -v "$REPO:/work" \
        -v /lib/modules:/lib/modules:ro \
        "$IMAGE" \
        /bin/sh "/work/test/e2e/$suite" || rc=1
done
exit $rc

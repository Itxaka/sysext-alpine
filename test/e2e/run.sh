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

# Host kernel modules are mounted read-only so the (privileged) container can
# modprobe squashfs/erofs/etc. for the host kernel.
exec "$DOCKER" run --privileged --rm \
    -v "$REPO:/work" \
    -v /lib/modules:/lib/modules:ro \
    "$IMAGE" \
    /bin/sh /work/test/e2e/inner.sh

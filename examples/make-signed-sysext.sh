#!/bin/sh
# Regenerates the committed example: test signing keys and a signed sysext
# DDI (root + root-verity + root-verity-sig) built with systemd-repart.
#
# The generated keys are intentionally PUBLIC test keys — they are committed
# to the repository so the example image can be verified by anyone. Never
# use them to sign anything real.
#
# The DDI holds partitions for one architecture, the build host's unless
# ARCH is set to a systemd architecture name (x86-64, arm64, riscv64, ...);
# the extension-release file names the same one. Builds are reproducible:
# UUIDs derive from a fixed seed and file timestamps from SOURCE_DATE_EPOCH.
#
# Requires: openssl, systemd-repart (systemd >= 255), mkfs.erofs or
# mksquashfs. Runs unprivileged.
set -eu

cd "$(dirname -- "$0")"

NAME=signed-example
SEED=7d1c3a52-9b0e-4f6a-8d2e-5c4b3a291807
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-1767225600}"

if [ -z "${ARCH:-}" ]; then
    case $(uname -m) in
        x86_64) ARCH=x86-64 ;;
        i?86) ARCH=x86 ;;
        aarch64) ARCH=arm64 ;;
        armv7*|armv6*) ARCH=arm ;;
        riscv64) ARCH=riscv64 ;;
        ppc64le) ARCH=ppc64-le ;;
        s390x) ARCH=s390x ;;
        loongarch64) ARCH=loongarch64 ;;
        *) echo "unknown architecture $(uname -m), set ARCH" >&2; exit 1 ;;
    esac
fi

SRC=$(mktemp -d)
trap 'rm -rf "$SRC"' EXIT
mkdir -p keys "$SRC/usr/lib/extension-release.d" \
    "$SRC/usr/share/$NAME" "$SRC/usr/bin"

# 10-year self-signed test certificate.
if [ ! -f keys/db.key ] || [ ! -f keys/db.pem ]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -subj "/CN=sysext-alpine-test-db" -keyout keys/db.key -out keys/db.pem
fi
# Git checkouts do not keep the mode; systemd-repart refuses lax ones.
chmod 0600 keys/db.key

# Minimal but valid sysext payload: the extension-release file MUST be named
# after the image (extension-release.<name>).
printf 'ID=_any\nARCHITECTURE=%s\n' "$ARCH" \
    > "$SRC/usr/lib/extension-release.d/extension-release.$NAME"
echo "hello from $NAME" > "$SRC/usr/share/$NAME/hello.txt"
printf '#!/bin/sh\necho %s-tool\n' "$NAME" > "$SRC/usr/bin/$NAME-tool"
chmod 0755 "$SRC/usr/bin/$NAME-tool"
find "$SRC" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

rm -f "$NAME.raw"
systemd-repart --make-ddi=sysext --copy-source="$SRC" --architecture="$ARCH" \
    --seed="$SEED" --private-key=keys/db.key --certificate=keys/db.pem "$NAME.raw"

echo "Built $NAME.raw"

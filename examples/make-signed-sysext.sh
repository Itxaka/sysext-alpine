#!/bin/sh
# Regenerates the committed example: test signing keys and a signed sysext
# DDI (root + root-verity + root-verity-sig) built with systemd-repart.
#
# The generated keys are intentionally PUBLIC test keys — they are committed
# to the repository so the example image can be verified by anyone. Never
# use them to sign anything real.
#
# Requires: openssl, systemd-repart (systemd >= 255), mkfs.erofs or
# mksquashfs. Runs unprivileged.
set -eu

cd "$(dirname -- "$0")"

NAME=signed-example

mkdir -p keys "source/usr/lib/extension-release.d" \
    "source/usr/share/$NAME" "source/usr/bin"

# 10-year self-signed test certificate.
[ -f keys/db.key ] || openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -subj "/CN=sysext-alpine-test-db" -keyout keys/db.key -out keys/db.pem

# Minimal but valid sysext payload: the extension-release file MUST be named
# after the image (extension-release.<name>).
printf 'ID=_any\nARCHITECTURE=_any\n' \
    > "source/usr/lib/extension-release.d/extension-release.$NAME"
echo "hello from $NAME" > "source/usr/share/$NAME/hello.txt"
printf '#!/bin/sh\necho %s-tool\n' "$NAME" > "source/usr/bin/$NAME-tool"
chmod 0755 "source/usr/bin/$NAME-tool"

rm -f "$NAME.raw"
systemd-repart --make-ddi=sysext --copy-source=source \
    --private-key=keys/db.key --certificate=keys/db.pem "$NAME.raw"

echo "Built $NAME.raw"

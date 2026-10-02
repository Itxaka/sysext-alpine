#!/bin/sh
# Builds the e2e fixtures that need systemd-repart: the signed example
# (examples/signed-example.raw) and a 4096-byte-sector signed DDI
# (test/fixtures/repart-4k/). run.sh runs this in archlinux:latest with the
# repository mounted at /work; FIXTURE_OWNER (uid:gid) owns the results.
set -eu

pacman -Sy --noconfirm --needed erofs-utils squashfs-tools openssl >/dev/null

cd /work
if [ ! -f examples/signed-example.raw ]; then
    ./examples/make-signed-sysext.sh >/dev/null
fi

F=test/fixtures/repart-4k
if [ ! -f "$F/repart4k.raw" ] || [ ! -f "$F/cert.pem" ]; then
    rm -rf "$F"
    mkdir -p "$F/src/usr/lib/extension-release.d" "$F/src/usr/share/repart4k"
    printf 'ID=_any\nARCHITECTURE=_any\n' > "$F/src/usr/lib/extension-release.d/extension-release.repart4k"
    echo "hello from repart4k" > "$F/src/usr/share/repart4k/hello.txt"
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj /CN=sysext-e2e-repart4k \
        -keyout "$F/key.pem" -out "$F/cert.pem" 2>/dev/null
    if ! out=$(systemd-repart --make-ddi=sysext --sector-size=4096 --copy-source="$F/src" \
        --private-key="$F/key.pem" --certificate="$F/cert.pem" "$F/repart4k.raw" 2>&1); then
        echo "$out" >&2
        exit 1
    fi
fi

if [ -n "${FIXTURE_OWNER:-}" ]; then
    chown -R "$FIXTURE_OWNER" examples/signed-example.raw examples/keys test/fixtures
fi
echo "fixtures: examples/signed-example.raw $F/repart4k.raw"

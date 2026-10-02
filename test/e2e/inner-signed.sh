#!/bin/sh
# e2e-fixtures: repart
#
# e2e suite for SIGNED dm-verity protected GPT DDIs. Builds a GPT image with
# a root data partition for the host architecture, a root-verity partition
# (veritysetup) and a root-verity-sig partition carrying the UAPI DPS
# signature JSON: the verity root hash plus a detached PKCS#7 signature over
# the ASCII hex root hash, made with a throwaway openssl key/cert. The
# certificate is installed to /etc/verity.d/ as the trust anchor.
#
# Exercises:
#   (a) merge with --image-policy=root=signed (trusted cert installed)
#   (b) trust anchor removed -> root=signed merge must fail
#   (c) root=signed+verity with an untrusted cert -> merge succeeds with a
#       degradation warning (plain verity still enforced)
#   (d) corrupted signature partition JSON -> root=signed merge must fail
#   (e) unsigned image (no sig partition) -> root=signed merge must fail
#   (f) a real signed DDI built by systemd-repart: examples/signed-example.raw
#       (run.sh builds it with archlinux:latest) and any test/fixtures/
#       signed-*.raw with its certificate
. /work/test/e2e/lib.sh

echo "=== Installing build dependencies ==="
pkg_add openssl cryptsetup device-mapper e2fsprogs kmod erofs-utils squashfs-tools
install_sysext
need_overlayfs
fs_supported ext4 || fatal "kernel lacks ext4"
[ -n "$ROOT_GUID" ] || fatal "no partition types known for $HOST_ARCH"
if ! verity_supported; then
    skip dm-verity "dm-verity target or veritysetup unavailable; skipping the signed-verity suite"
    finish
fi
echo "dm-verity target available"

verity_gone() { released; }

mkdir -p /var/lib/extensions /etc/verity.d

echo "=== Generating signing key and certificate ==="
new_cert cert -days 1 || fatal "openssl key/cert generation failed"
# A second, unrelated cert: the "untrusted signer" trust anchor for (c).
new_cert other-cert -days 1 || fatal "openssl second key/cert generation failed"

echo "=== Building signed verity test image ==="
NAME=test-signed
IMG=/var/lib/extensions/$NAME.raw
# Partition layout of ddi_build -g in 512-byte sectors: data 8 MiB at 2048,
# verity 4 MiB at 18432, signature 1 MiB at 26624, image 16 MiB.
DATA_START=2048
DATA_SECTORS=16384
VERITY_START=18432
VERITY_SECTORS=8192
SIG_START=26624
IMG_MIB=16

mk_tree "$WORKDIR/tree" "$NAME"
ddi_build "$WORKDIR/$NAME.raw" "$WORKDIR/tree" -g || fatal "building $NAME.raw failed"
DATA=$DDI_DATA
HASH=$DDI_HASH
ROOTHASH=$DDI_ROOTHASH
if [ "${#ROOTHASH}" != 64 ]; then
    fail "veritysetup format did not yield a 64-hex root hash (got '$ROOTHASH')"
    finish
fi
echo "verity root hash: $ROOTHASH"
DATA_UUID=$(uuid_of "$(echo "$ROOTHASH" | cut -c1-32)")
VERITY_UUID=$(uuid_of "$(echo "$ROOTHASH" | cut -c33-64)")

# Signature JSON (UAPI DPS): rootHash + base64 DER PKCS#7 detached signature
# over the exact ASCII hex root hash, NUL-padded to a multiple of 4096 bytes.
smime_sign "$WORKDIR/cert.key" "$WORKDIR/cert.pem" "$ROOTHASH" "$WORKDIR/sig.der" \
    || { fail "openssl smime signing failed"; finish; }
CERT_FP=$(openssl x509 -in "$WORKDIR/cert.pem" -outform der | sha256sum | cut -d' ' -f1)
sig_json "$ROOTHASH" "$WORKDIR/sig.der" "$CERT_FP" > "$WORKDIR/sig.json"
size=$(wc -c < "$WORKDIR/sig.json")
padding=$(( (4096 - size % 4096) % 4096 ))
if [ "$padding" -gt 0 ]; then
    head -c "$padding" /dev/zero >> "$WORKDIR/sig.json"
fi
echo "signature blob: $(wc -c < "$WORKDIR/sig.json") bytes (cert sha256 $CERT_FP)"
ddi_write_sig "$WORKDIR/$NAME.raw" "$WORKDIR/sig.json"
cp "$WORKDIR/$NAME.raw" "$IMG"
echo "built: $NAME.raw (GPT: root + root-verity + root-verity-sig)"

# Trust anchor in place for (a).
cp "$WORKDIR/cert.pem" /etc/verity.d/test.crt

# ---------------------------------------------------------------------------
# (a) merge with root=signed, trusted cert installed
# ---------------------------------------------------------------------------
echo "=== Running tests ==="

if out=$(sysext --image-policy=root=signed merge 2>&1); then
    pass "merge with --image-policy=root=signed (trusted cert)"
else
    fail "merge with --image-policy=root=signed: $out"
fi

if [ -f "/usr/share/$NAME/hello.txt" ] \
   && [ "$(cat "/usr/share/$NAME/hello.txt")" = "hello from $NAME" ]; then
    pass "payload readable through signed verity device"
else
    fail "payload missing/corrupt: /usr/share/$NAME/hello.txt"
fi

if [ -n "$(verity_devs)" ]; then
    pass "dm-verity device active: $(verity_devs)"
else
    fail "no dm-verity device active"
fi

if sysext unmerge; then
    pass "unmerge after signed merge"
else
    fail "unmerge after signed merge"
fi

if verity_gone; then
    pass "dm-verity device removed after unmerge"
else
    fail "dm-verity device still present after unmerge: $(verity_devs)"
fi

# ---------------------------------------------------------------------------
# (b) trust anchor removed -> root=signed must fail
# ---------------------------------------------------------------------------
rm -f /etc/verity.d/*.crt

if sysext --image-policy=root=signed merge 2>/dev/null; then
    fail "merge with root=signed succeeded without any trust anchor"
    sysext unmerge
else
    pass "merge with root=signed rejected without trust anchor"
fi

# ---------------------------------------------------------------------------
# (c) root=signed+verity with an untrusted cert -> degrade to verity + warn
# ---------------------------------------------------------------------------
cp "$WORKDIR/other-cert.pem" /etc/verity.d/test.crt # wrong cert installed

if out=$(sysext --image-policy=root=signed+verity merge 2>&1); then
    pass "merge with root=signed+verity and untrusted cert (degraded)"
    if echo "$out" | grep -qi "signature verification failed"; then
        pass "degradation warning printed"
    else
        fail "no degradation warning in output: $out"
    fi
    if [ -n "$(verity_devs)" ]; then
        pass "dm-verity still enforced after degradation"
    else
        fail "dm-verity device missing after degradation"
    fi
    if [ -f "/usr/share/$NAME/hello.txt" ]; then
        pass "payload visible after degraded merge"
    else
        fail "payload missing after degraded merge"
    fi
    sysext unmerge || fail "unmerge after degraded merge"
else
    fail "merge with root=signed+verity and untrusted cert should degrade, got: $out"
fi

# Untrusted cert + signed-only policy must fail outright.
if sysext --image-policy=root=signed merge 2>/dev/null; then
    fail "merge with root=signed succeeded with untrusted cert"
    sysext unmerge
else
    pass "merge with root=signed rejected with untrusted cert"
fi

# Restore the good trust anchor for the remaining image-side tests.
cp "$WORKDIR/cert.pem" /etc/verity.d/test.crt

# ---------------------------------------------------------------------------
# (d) corrupted signature partition JSON -> root=signed must fail
# ---------------------------------------------------------------------------
dd if=/dev/urandom of="$IMG" bs=512 seek=$SIG_START count=1 conv=notrunc status=none

if sysext --image-policy=root=signed merge 2>/dev/null; then
    fail "merge with root=signed succeeded despite corrupted signature JSON"
    sysext unmerge
else
    pass "merge with root=signed rejected with corrupted signature JSON"
fi

# Restore the good signature blob (image stays valid for later use).
dd if="$WORKDIR/sig.json" of="$IMG" bs=512 seek=$SIG_START conv=notrunc status=none

# ---------------------------------------------------------------------------
# (e) unsigned image (no sig partition) -> root=signed must fail
# ---------------------------------------------------------------------------
mv "$IMG" "$WORKDIR/$NAME.raw.signed"

UNSIGNED=/var/lib/extensions/$NAME.raw
printf 'label: gpt\nstart=%d, size=%d, type=%s, uuid=%s\nstart=%d, size=%d, type=%s, uuid=%s\n' \
    $DATA_START $DATA_SECTORS "$ROOT_GUID" "$DATA_UUID" $VERITY_START $VERITY_SECTORS "$ROOT_VERITY_GUID" "$VERITY_UUID" \
    | part_image "$UNSIGNED" 512 $IMG_MIB || { fail "sfdisk failed building unsigned image"; finish; }
img_put "$DATA" "$UNSIGNED" 512 $DATA_START
img_put "$HASH" "$UNSIGNED" 512 $VERITY_START

if sysext --image-policy=root=signed merge 2>/dev/null; then
    fail "merge with root=signed succeeded on unsigned image"
    sysext unmerge
else
    pass "merge with root=signed rejected on unsigned image"
fi

# Sanity: the unsigned image still merges as plain verity.
if sysext --image-policy=root=verity merge >/dev/null 2>&1; then
    pass "unsigned image still merges with root=verity"
    sysext unmerge
else
    fail "unsigned image no longer merges with root=verity"
fi

rm -f "$UNSIGNED"

# ---------------------------------------------------------------------------
# (f) real signed DDIs built by systemd-repart. Like systemd, the
#     certificate's validity period is not checked.
# ---------------------------------------------------------------------------
# fixture IMG CERT [PAYLOAD-PATH CONTENT]
fixture() {
    fimg=$1 fcrt=$2
    echo "=== Real signed fixture: $fimg (cert: $fcrt) ==="
    rm -f /etc/verity.d/*.crt /var/lib/extensions/*.raw
    cp "$fcrt" /etc/verity.d/fixture.crt
    cp "$fimg" "/var/lib/extensions/$(basename "$fimg")"
    # --force skips the host/version (and release-file) validation: signing
    # fixtures may carry no extension-release payload. Signature
    # verification is never skipped.
    if out=$(sysext --image-policy=root=signed --force merge 2>&1); then
        pass "real signed fixture $(basename "$fimg") merged with root=signed"
        if [ -n "${3:-}" ]; then
            expect_eq "fixture payload readable through the signed merge" "$(cat "$3" 2>/dev/null)" "$4"
        fi
        if [ -n "$(verity_devs)" ]; then
            pass "dm-verity active for real fixture"
        else
            fail "no dm-verity device for real fixture"
        fi
        sysext unmerge || fail "unmerge after fixture merge"
    else
        fail "real signed fixture rejected: $out"
    fi
    rm -f "/var/lib/extensions/$(basename "$fimg")" /etc/verity.d/fixture.crt
}

if [ -f /work/examples/signed-example.raw ]; then
    fixture /work/examples/signed-example.raw /work/examples/keys/db.pem \
        /usr/share/signed-example/hello.txt "hello from signed-example"
else
    skip fixture:signed-example "examples/signed-example.raw missing (run.sh builds it with archlinux:latest, or run 'make example')"
fi
# Additional local fixtures: test/fixtures/signed-*.raw signed by
# test/fixtures/db.pem (systemd-repart --certificate= convention).
for f in /work/test/fixtures/signed-*.raw; do
    [ -f "$f" ] && [ -f /work/test/fixtures/db.pem ] && fixture "$f" /work/test/fixtures/db.pem
done

rm -f /etc/verity.d/test.crt
finish

#!/bin/sh
# e2e test suite for SIGNED dm-verity protected GPT DDIs. Runs INSIDE a
# privileged Alpine container (launched by run.sh, which auto-runs every
# inner*.sh).
#
# Builds a GPT image with a root (x86-64) data partition, a root-verity
# partition (veritysetup) and a root-verity-sig partition carrying the UAPI
# DPS signature JSON: the verity root hash plus a detached PKCS#7 signature
# over the ASCII hex root hash, made with a throwaway openssl key/cert.
# The certificate is installed to /etc/verity.d/ as the trust anchor.
#
# Exercises:
#   (a) merge with --image-policy=root=signed (trusted cert installed)
#   (b) trust anchor removed -> root=signed merge must fail
#   (c) root=signed+verity with an untrusted cert -> merge succeeds with a
#       degradation warning (plain verity still enforced)
#   (d) corrupted signature partition JSON -> root=signed merge must fail
#   (e) unsigned image (no sig partition) -> root=signed merge must fail
#   (f) optional: a real systemd-built signed DDI dropped into
#       /work/test/fixtures (signed-*.raw + *.crt) is merged for compat
#       validation.
#
# The whole suite SKIPs when openssl, veritysetup or the dm-verity target
# is unavailable. PASS/FAIL/SKIP accounting as in inner-verity.sh.
set -u

FAILS=0
SKIPS=0

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
skip() { echo "SKIP: $*"; SKIPS=$((SKIPS + 1)); }

finish() {
    echo "==========================================="
    echo "Failures: $FAILS  Skips: $SKIPS"
    if [ "$FAILS" -gt 0 ]; then
        echo "RESULT: FAIL"
        exit 1
    fi
    echo "RESULT: PASS"
    exit 0
}

fs_supported() {
    grep -qw "$1" /proc/filesystems && return 0
    modprobe "$1" 2>/dev/null || true
    grep -qw "$1" /proc/filesystems
}

# ---------------------------------------------------------------------------
# Environment setup
# ---------------------------------------------------------------------------
echo "=== Installing build dependencies ==="
apk add --no-cache openssl cryptsetup device-mapper e2fsprogs \
    util-linux kmod \
    || { echo "FATAL: apk add failed"; exit 1; }

if [ ! -x /work/bin/sysext ]; then
    echo "FATAL: /work/bin/sysext missing (run 'make build-static' on the host)"
    exit 1
fi
install -m 0755 /work/bin/sysext /usr/bin/sysext

if ! fs_supported overlay; then
    echo "FATAL: kernel does not support overlayfs; cannot test anything"
    exit 1
fi

# ---------------------------------------------------------------------------
# Capability checks — SKIP the whole suite if signing/verity cannot work here
# ---------------------------------------------------------------------------
if ! command -v openssl >/dev/null 2>&1; then
    skip "openssl not available; skipping signed-verity suite"
    finish
fi

if ! command -v veritysetup >/dev/null 2>&1; then
    skip "veritysetup not available; skipping signed-verity suite"
    finish
fi

if ! fs_supported ext4; then
    skip "ext4 not supported by host kernel; skipping signed-verity suite"
    finish
fi

modprobe dm-verity 2>/dev/null || modprobe dm_verity 2>/dev/null || true
if ! dmsetup targets 2>/dev/null | grep -qw verity; then
    skip "dm-verity target unavailable (modprobe failed?); skipping signed-verity suite"
    finish
fi
echo "dm-verity target available"

mkdir -p /var/lib/extensions /etc/verity.d
WORKDIR=/tmp/e2e-signed
mkdir -p "$WORKDIR"

# ---------------------------------------------------------------------------
# Signing key + certificate (throwaway, self-signed)
# ---------------------------------------------------------------------------
echo "=== Generating signing key and certificate ==="
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -subj /CN=sysext-test \
    -keyout "$WORKDIR/key.pem" -out "$WORKDIR/cert.pem" 2>/dev/null \
    || { echo "FATAL: openssl key/cert generation failed"; exit 1; }

# A second, unrelated cert: the "untrusted signer" trust anchor for (c).
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -subj /CN=sysext-other \
    -keyout "$WORKDIR/other-key.pem" -out "$WORKDIR/other-cert.pem" 2>/dev/null \
    || { echo "FATAL: openssl second key/cert generation failed"; exit 1; }

# ---------------------------------------------------------------------------
# Test image construction
# ---------------------------------------------------------------------------
echo "=== Building signed verity test image ==="

NAME=test-signed
IMG=/var/lib/extensions/$NAME.raw
DATA=$WORKDIR/data.img
HASH=$WORKDIR/hash.img
DM_NODE=/dev/mapper/sysext-$NAME-verity

# UAPI Discoverable Partitions Spec type GUIDs (x86-64).
ROOT_GUID=4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709
ROOT_VERITY_GUID=2C7357ED-EBD2-46D9-AEC1-23D437EC2BF5
ROOT_VERITY_SIG_GUID=41092B05-9FC8-4523-994F-2DEF0408B176

# Partition layout (512-byte sectors): data 8 MiB @ 2048, verity 4 MiB
# @ 18432, signature 1 MiB @ 26624, image 16 MiB total.
DATA_START=2048
DATA_SECTORS=16384
VERITY_START=18432
VERITY_SECTORS=8192
SIG_START=26624
SIG_SECTORS=2048
IMG_MIB=16

# Payload tree (same conventions as inner-verity.sh).
tree=$WORKDIR/tree
mkdir -p "$tree/usr/lib/extension-release.d" \
         "$tree/usr/share/$NAME" \
         "$tree/usr/bin"
printf 'ID=_any\nARCHITECTURE=_any\n' \
    > "$tree/usr/lib/extension-release.d/extension-release.$NAME"
echo "hello from $NAME" > "$tree/usr/share/$NAME/hello.txt"
printf '#!/bin/sh\necho %s-tool\n' "$NAME" > "$tree/usr/bin/$NAME-tool"
chmod 0755 "$tree/usr/bin/$NAME-tool"

dd if=/dev/zero of="$DATA" bs=512 count=$DATA_SECTORS status=none
if ! mkfs.ext4 -q -F -b 4096 -d "$tree" "$DATA" 2>/dev/null; then
    mkfs.ext4 -q -F -b 4096 "$DATA" || { echo "FATAL: mkfs.ext4 failed"; exit 1; }
    mnt=$WORKDIR/mnt
    mkdir -p "$mnt"
    mount -o loop "$DATA" "$mnt" || { echo "FATAL: loop mount failed"; exit 1; }
    cp -a "$tree"/. "$mnt"/
    umount "$mnt"
fi

ROOTHASH=$(veritysetup format "$DATA" "$HASH" | awk '/^Root hash/{print $3}')
if [ "${#ROOTHASH}" != 64 ]; then
    fail "veritysetup format did not yield a 64-hex root hash (got '$ROOTHASH')"
    finish
fi
echo "verity root hash: $ROOTHASH"

# ---------------------------------------------------------------------------
# Signature JSON (UAPI DPS): rootHash + base64 DER PKCS#7 detached signature
# over the exact ASCII hex root hash (no trailing newline!).
# ---------------------------------------------------------------------------
printf %s "$ROOTHASH" > "$WORKDIR/roothash.txt"
openssl smime -sign -in "$WORKDIR/roothash.txt" \
    -signer "$WORKDIR/cert.pem" -inkey "$WORKDIR/key.pem" \
    -binary -outform der -noattr > "$WORKDIR/sig.der" \
    || { fail "openssl smime signing failed"; finish; }

SIG_B64=$(openssl base64 -A -in "$WORKDIR/sig.der")
CERT_FP=$(openssl x509 -in "$WORKDIR/cert.pem" -outform der \
          | sha256sum | cut -d' ' -f1)

printf '{"rootHash":"%s","signature":"%s","certificateFingerprint":"%s"}' \
    "$ROOTHASH" "$SIG_B64" "$CERT_FP" > "$WORKDIR/sig.json"

# NUL-pad to a multiple of 4096 bytes, as the spec mandates.
size=$(wc -c < "$WORKDIR/sig.json")
padding=$(( (4096 - size % 4096) % 4096 ))
if [ "$padding" -gt 0 ]; then
    head -c "$padding" /dev/zero >> "$WORKDIR/sig.json"
fi
echo "signature blob: $(wc -c < "$WORKDIR/sig.json") bytes (cert sha256 $CERT_FP)"

# Root-hash discovery rule: data partition unique UUID = first 128 bits of
# the root hash, verity partition unique UUID = last 128 bits.
uuid_of() {
    h=$1
    printf '%s-%s-%s-%s-%s' \
        "$(echo "$h" | cut -c1-8)" \
        "$(echo "$h" | cut -c9-12)" \
        "$(echo "$h" | cut -c13-16)" \
        "$(echo "$h" | cut -c17-20)" \
        "$(echo "$h" | cut -c21-32)"
}
DATA_UUID=$(uuid_of "$(echo "$ROOTHASH" | cut -c1-32)")
VERITY_UUID=$(uuid_of "$(echo "$ROOTHASH" | cut -c33-64)")

dd if=/dev/zero of="$IMG" bs=1M count=$IMG_MIB status=none
sfdisk -q "$IMG" <<EOF || { fail "sfdisk failed building $IMG"; finish; }
label: gpt
start=$DATA_START, size=$DATA_SECTORS, type=$ROOT_GUID, uuid=$DATA_UUID
start=$VERITY_START, size=$VERITY_SECTORS, type=$ROOT_VERITY_GUID, uuid=$VERITY_UUID
start=$SIG_START, size=$SIG_SECTORS, type=$ROOT_VERITY_SIG_GUID
EOF

dd if="$DATA" of="$IMG" bs=512 seek=$DATA_START conv=notrunc status=none
dd if="$HASH" of="$IMG" bs=512 seek=$VERITY_START conv=notrunc status=none
dd if="$WORKDIR/sig.json" of="$IMG" bs=512 seek=$SIG_START conv=notrunc status=none
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

if [ -b "$DM_NODE" ]; then
    pass "dm-verity device node exists: $DM_NODE"
else
    fail "dm-verity device node missing: $DM_NODE"
fi

if sysext unmerge; then
    pass "unmerge after signed merge"
else
    fail "unmerge after signed merge"
fi

if [ -e "$DM_NODE" ]; then
    fail "dm-verity device still present after unmerge: $DM_NODE"
else
    pass "dm-verity device removed after unmerge"
fi

# ---------------------------------------------------------------------------
# (b) trust anchor removed -> root=signed must fail
# ---------------------------------------------------------------------------
rm -f /etc/verity.d/*.crt

if sysext --image-policy=root=signed merge 2>/dev/null; then
    fail "merge with root=signed succeeded without any trust anchor"
    sysext unmerge || true
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
    if [ -b "$DM_NODE" ]; then
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
    sysext unmerge || true
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
    sysext unmerge || true
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
dd if=/dev/zero of="$UNSIGNED" bs=1M count=$IMG_MIB status=none
sfdisk -q "$UNSIGNED" <<EOF || { fail "sfdisk failed building unsigned image"; finish; }
label: gpt
start=$DATA_START, size=$DATA_SECTORS, type=$ROOT_GUID, uuid=$DATA_UUID
start=$VERITY_START, size=$VERITY_SECTORS, type=$ROOT_VERITY_GUID, uuid=$VERITY_UUID
EOF
dd if="$DATA" of="$UNSIGNED" bs=512 seek=$DATA_START conv=notrunc status=none
dd if="$HASH" of="$UNSIGNED" bs=512 seek=$VERITY_START conv=notrunc status=none

if sysext --image-policy=root=signed merge 2>/dev/null; then
    fail "merge with root=signed succeeded on unsigned image"
    sysext unmerge || true
else
    pass "merge with root=signed rejected on unsigned image"
fi

# Sanity: the unsigned image still merges as plain verity.
if sysext --image-policy=root=verity merge >/dev/null 2>&1; then
    pass "unsigned image still merges with root=verity"
    sysext unmerge || true
else
    fail "unsigned image no longer merges with root=verity"
fi

rm -f "$UNSIGNED"

# ---------------------------------------------------------------------------
# (f) optional real-fixture compat test: drop a systemd-built signed DDI as
#     /work/test/fixtures/signed-*.raw plus its certificate as
#     /work/test/fixtures/*.crt to validate interoperability.
# ---------------------------------------------------------------------------
FIXTURE_IMG=$(ls /work/test/fixtures/signed-*.raw 2>/dev/null | head -n1)
FIXTURE_CRT=$(ls /work/test/fixtures/*.crt 2>/dev/null | head -n1)
if [ -n "${FIXTURE_IMG:-}" ] && [ -n "${FIXTURE_CRT:-}" ]; then
    echo "=== Real signed fixture: $FIXTURE_IMG ==="
    rm -f /etc/verity.d/*.crt /var/lib/extensions/*.raw
    cp "$FIXTURE_CRT" /etc/verity.d/fixture.crt
    cp "$FIXTURE_IMG" "/var/lib/extensions/$(basename "$FIXTURE_IMG")"
    # --force only skips the version/host match, not signature verification.
    if out=$(sysext --image-policy=root=signed --force merge 2>&1); then
        pass "real signed fixture merged with root=signed"
        sysext unmerge || fail "unmerge after fixture merge"
    else
        fail "real signed fixture rejected: $out"
    fi
    rm -f "/var/lib/extensions/$(basename "$FIXTURE_IMG")"
else
    skip "no real signed fixture (drop signed-*.raw + *.crt into test/fixtures to enable)"
fi

# Final cleanup so the container exits with nothing mounted.
sysext unmerge >/dev/null 2>&1 || true
rm -f /etc/verity.d/test.crt

finish

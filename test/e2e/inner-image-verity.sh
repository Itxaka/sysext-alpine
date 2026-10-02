#!/bin/sh
# e2e-fixtures: repart
#
# e2e suite for dm-verity setup details.
#
# Exercises, against systemd v262 semantics:
#   - dm device naming per loop attachment and kernel-side cleanup
#     (deferred remove, loop autoclear), verity DDIs on block devices with
#     and without partition scanning
#   - root hash taken from the signature JSON (partition UUIDs unrelated),
#     sha512 verity, 4096-byte sector DDIs with verity
#   - the signature trust model: signer must be an installed certificate,
#     embedded certificates are ignored, no validity period checks, the
#     verity.d directory cascade with /dev/null masking
#   - malformed and hostile (deeply nested) signatures fail cleanly
#   - sidecar verity data for bare filesystem images
#   - a sysext and a confext of the same name, both verity protected
. /work/test/e2e/lib.sh
allow_skip dm-deferred-info

echo "=== Installing build dependencies ==="
pkg_add openssl cryptsetup device-mapper e2fsprogs kmod squashfs-tools
install_sysext
need_overlayfs
fs_supported ext4 || fatal "kernel lacks ext4"
[ -n "$ROOT_GUID" ] || fatal "no partition types known for $HOST_ARCH"
if ! verity_supported; then
    skip dm-verity "dm-verity target or veritysetup unavailable"
    finish
fi

mkdir -p /var/lib/extensions /var/lib/confexts /etc/verity.d
W=$WORKDIR

expect_released() {
    if released; then
        pass "$1: dm-verity and loop devices released"
    else
        fail "$1: leaked: $(leaked)"
    fi
}

# payload NAME DIR — sysext payload.
payload() { mk_tree "$2" "$1"; }

# confpayload NAME DIR — confext payload.
confpayload() { mk_tree "$2" "$1" "" confext; }

# ddi NAME SS OUT [veritysetup args...] — GPT image with an ext4 root and a
# verity partition, plus an empty signature partition when SIG=1. Sets
# ROOTHASH. Partition UUIDs carry the root hash when UUIDS=hash (sha256
# only), otherwise they are random.
ddi() {
    name=$1 ss=$2 out=$3
    shift 3
    payload "$name" "$W/$name.tree"
    if [ "$SIG" = 1 ]; then g=-g; else g=-v; fi
    ddi_build "$out" "$W/$name.tree" -s "$ss" "$g" -u "$UUIDS" -V "$*" || fail "build $out"
    ROOTHASH=$DDI_ROOTHASH
}
UUIDS=random
SIG=0

# sigjson ROOTHASH DERFILE OUT IMG — write the signature JSON into the
# signature partition of IMG.
sigjson() {
    sig_json "$1" "$2" > "$3"
    ddi_write_sig "$4" "$3"
}

# smime KEY CERT ROOTHASH OUT [extra args]
smime() { smime_sign "$@"; }

newcert() { new_cert "$1"; }

only() {
    rm -rf /var/lib/extensions/* /var/lib/confexts/*
    cp "$1" "/var/lib/extensions/$2.raw"
}

echo "=== Running tests ==="
newcert signer
newcert other

# --- naming and lifetime ------------------------------------------------------
UUIDS='hash'
ddi life 512 "$W/life.raw"
only "$W/life.raw" life
if sysext --image-policy=root=verity merge; then
    pass "verity DDI merged"
    devs=$(verity_devs)
    case "$devs" in
        loop*p1-*-verity | loop*p1-verity) pass "dm device named after the partition and disk sequence: $devs" ;;
        *) fail "unexpected dm device name(s): $devs" ;;
    esac
    if dmsetup table "$devs" 2>/dev/null | grep -q "$ROOTHASH"; then
        pass "dm table carries the GUID-derived root hash"
    else
        fail "dm table: $(dmsetup table "$devs" 2>&1)"
    fi
    if dmsetup info "$devs" 2>/dev/null | grep -qi "deferred remove"; then
        pass "dm device is marked for deferred removal"
    else
        skip dm-deferred-info "dmsetup info does not report deferred removal: $(dmsetup info "$devs" 2>&1 | tr '\n' ' ')"
    fi
    sysext unmerge || fail "unmerge"
    expect_released "unmerge"
else
    fail "verity DDI merge"
fi

# Unmerge without origin data still releases everything: the kernel owns it.
if sysext --image-policy=root=verity merge >/dev/null 2>&1; then
    rm -f /run/systemd/sysext/meta/*/.systemd-sysext/origin 2>/dev/null
    sysext unmerge >/dev/null 2>&1
    expect_released "unmerge without origin markers"
fi

# --- 4K sectors with verity ---------------------------------------------------
ddi v4k 4096 "$W/v4k.raw"
only "$W/v4k.raw" v4k
if sysext --image-policy=root=verity merge >/dev/null 2>&1 \
    && [ "$(cat /usr/share/v4k/hello.txt 2>/dev/null)" = "hello from v4k" ]; then
    pass "4K-sector verity DDI merged"
else
    fail "4K-sector verity DDI"
fi
sysext unmerge >/dev/null 2>&1
expect_released "4K-sector verity DDI"

# --- verity DDIs on block devices ----------------------------------------------
# dm_slaves — the devices below the dm devices of this suite.
dm_slaves() {
    for n in $(our_dm); do
        for d in /sys/block/dm-*; do
            [ "$(cat "$d/dm/name")" = "$n" ] && ls "$d/slaves"
        done
    done | tr '\n' ' '
}
# dm_settle — wait (up to 5 s) until no dm device of this suite is left.
dm_settle() {
    i=0
    while [ -n "$(our_dm)" ] && [ $i -lt 50 ]; do
        sleep 0.1
        i=$((i + 1))
    done
    [ -z "$(our_dm)" ]
}
ddi blkv 512 "$W/blkv.raw"
rm -rf /var/lib/extensions/* /var/lib/confexts/*
for scan in -P ""; do
    if [ -n "$scan" ]; then w=with; else w=without; fi
    ctx="verity DDI on a block device $w partition scanning"
    # shellcheck disable=SC2086
    dev=$(loop_attach "$W/blkv.raw" $scan) || fatal "losetup $W/blkv.raw"
    ln -sf "$dev" /var/lib/extensions/blkv
    if sysext --image-policy=root=verity merge >/dev/null 2>&1 &&
        [ "$(cat /usr/share/blkv/hello.txt 2>/dev/null)" = "hello from blkv" ]; then
        pass "$ctx: merged"
    else
        fail "$ctx: merge"
    fi
    slaves=$(dm_slaves)
    case $slaves in
        "") fail "$ctx: no dm-verity device" ;;
        *"${dev#/dev/}p1 "*)
            [ -n "$scan" ] && pass "$ctx: dm-verity on the device's own partition" ||
                fail "$ctx: dm-verity on a partition of a device scanning none" ;;
        *)
            [ -z "$scan" ] && pass "$ctx: dm-verity on a loop device on top ($slaves)" ||
                fail "$ctx: dm-verity below $slaves, not on the device's partition" ;;
    esac
    sysext unmerge >/dev/null 2>&1
    dm_settle && pass "$ctx: dm-verity released" || fail "$ctx: leaked: $(leaked)"
    loop_attached "$dev" && pass "$ctx: device still attached" || fail "$ctx: device detached"
    rm -f /var/lib/extensions/blkv
    losetup -d "$dev"
done
expect_released "verity DDIs on block devices"

# --- root hash from the signature, unrelated partition UUIDs ------------------
UUIDS=random
SIG=1
ddi sig 512 "$W/sig.raw"
smime "$W/signer.key" "$W/signer.pem" "$ROOTHASH" "$W/sig.der"
sigjson "$ROOTHASH" "$W/sig.der" "$W/sig.json" "$W/sig.raw"
cp "$W/signer.pem" /etc/verity.d/signer.crt
only "$W/sig.raw" sig
if out=$(sysext --image-policy=root=signed merge 2>&1); then
    pass "signed DDI with random partition UUIDs merged (root hash from signature JSON)"
    dmsetup table "$(verity_devs)" | grep -q "$ROOTHASH" && pass "dm table carries the signed root hash" || fail "dm table lacks the signed root hash"
    sysext unmerge >/dev/null 2>&1
else
    fail "signed DDI with random UUIDs: $out"
fi
expect_released "signed DDI"

if sysext --image-policy=root=verity merge >/dev/null 2>&1; then
    fail "root=verity must fall back to the (wrong) GUID root hash"
    sysext unmerge >/dev/null 2>&1
else
    pass "root=verity ignores the signature and the GUID root hash does not verify"
fi
expect_released "GUID fallback"

# --- sha512 -------------------------------------------------------------------
ddi s512 512 "$W/s512.raw" --hash sha512
smime "$W/signer.key" "$W/signer.pem" "$ROOTHASH" "$W/s512.der"
sigjson "$ROOTHASH" "$W/s512.der" "$W/s512.json" "$W/s512.raw"
only "$W/s512.raw" s512
if [ "${#ROOTHASH}" = 128 ] && sysext --image-policy=root=signed merge >/dev/null 2>&1; then
    pass "sha512 signed DDI merged"
    sysext unmerge >/dev/null 2>&1
else
    fail "sha512 signed DDI (root hash length ${#ROOTHASH})"
fi
expect_released "sha512"

# --- trust model --------------------------------------------------------------
only "$W/sig.raw" sig
ROOTHASH_SIG=$(sed -E 's/.*"rootHash":"([0-9a-f]+)".*/\1/' "$W/sig.json")
smime "$W/signer.key" "$W/signer.pem" "$ROOTHASH_SIG" "$W/nocerts.der" -nocerts
sigjson "$ROOTHASH_SIG" "$W/nocerts.der" "$W/nocerts.json" "/var/lib/extensions/sig.raw"
if sysext --image-policy=root=signed merge >/dev/null 2>&1; then
    pass "signature without embedded certificate accepted (signer installed)"
    sysext unmerge >/dev/null 2>&1
else
    fail "signature without embedded certificate rejected"
fi

# Only the issuing CA installed: the leaf is not trusted.
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=ca -keyout "$W/ca.key" -out "$W/ca.pem" \
    -addext basicConstraints=critical,CA:TRUE 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj /CN=leaf -keyout "$W/leaf.key" -out "$W/leaf.csr" 2>/dev/null
openssl x509 -req -in "$W/leaf.csr" -CA "$W/ca.pem" -CAkey "$W/ca.key" -CAcreateserial -days 2 -out "$W/leaf.pem" 2>/dev/null
smime "$W/leaf.key" "$W/leaf.pem" "$ROOTHASH_SIG" "$W/leaf.der" -certfile "$W/ca.pem"
sigjson "$ROOTHASH_SIG" "$W/leaf.der" "$W/leaf.json" "/var/lib/extensions/sig.raw"
rm -f /etc/verity.d/*
cp "$W/ca.pem" /etc/verity.d/ca.crt
if sysext --image-policy=root=signed merge >/dev/null 2>&1; then
    fail "leaf signature accepted with only its CA installed"
    sysext unmerge >/dev/null 2>&1
else
    pass "leaf signature rejected with only its CA installed"
fi
expect_released "CA-only trust"

# Vendor directory and masking.
sigjson "$ROOTHASH_SIG" "$W/sig.der" "$W/sig.json" "/var/lib/extensions/sig.raw"
rm -f /etc/verity.d/*
mkdir -p /usr/lib/verity.d
cp "$W/signer.pem" /usr/lib/verity.d/vendor.crt
if sysext --image-policy=root=signed merge >/dev/null 2>&1; then
    pass "certificate in /usr/lib/verity.d trusted"
    sysext unmerge >/dev/null 2>&1
else
    fail "certificate in /usr/lib/verity.d not trusted"
fi
ln -sf /dev/null /etc/verity.d/vendor.crt
if sysext --image-policy=root=signed merge >/dev/null 2>&1; then
    fail "/etc/verity.d/vendor.crt -> /dev/null does not mask /usr/lib/verity.d/vendor.crt"
    sysext unmerge >/dev/null 2>&1
else
    pass "/dev/null symlink masks the vendor certificate"
fi
rm -f /etc/verity.d/vendor.crt /usr/lib/verity.d/vendor.crt
expect_released "verity.d cascade"

# Expired signer certificate: no validity checks, like OpenSSL's NOVERIFY.
if openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=expired -keyout "$W/expired.key" -out "$W/expired.pem" \
    -not_before 20000101000000Z -not_after 20010101000000Z 2>/dev/null; then
    smime "$W/expired.key" "$W/expired.pem" "$ROOTHASH_SIG" "$W/expired.der"
    sigjson "$ROOTHASH_SIG" "$W/expired.der" "$W/expired.json" "/var/lib/extensions/sig.raw"
    cp "$W/expired.pem" /etc/verity.d/expired.crt
    if sysext --image-policy=root=signed merge >/dev/null 2>&1; then
        pass "expired signer certificate accepted"
        sysext unmerge >/dev/null 2>&1
    else
        fail "expired signer certificate rejected"
    fi
    rm -f /etc/verity.d/expired.crt
else
    skip tool:openssl-dates "openssl cannot create certificates with explicit validity dates"
fi

# Malformed signature data: clean failure, nothing left behind.
cp "$W/signer.pem" /etc/verity.d/signer.crt
printf '{"rootHash":"%s","signature":"MIQB"}' "$ROOTHASH_SIG" > "$W/bad.json"
dd if=/dev/zero of=/var/lib/extensions/sig.raw bs=1M seek=13 count=1 conv=notrunc status=none
dd if="$W/bad.json" of=/var/lib/extensions/sig.raw bs=1M seek=13 conv=notrunc status=none
out=$(sysext --image-policy=root=signed merge 2>&1)
rc=$?
if [ $rc -ne 0 ] && ! echo "$out" | grep -q "panic:"; then
    pass "malformed PKCS#7 signature fails cleanly"
else
    fail "malformed PKCS#7 signature: rc=$rc $out"
    sysext unmerge >/dev/null 2>&1
fi
expect_released "malformed signature"
grep -q " /run/systemd/sysext" /proc/self/mountinfo && fail "mounts left after malformed signature" || pass "no mounts left after malformed signature"

# Hostile nesting: UNITS nested indefinite-length SEQUENCEs ("30 80") are
# refused by the BER depth and size limits before anything parses them.
for units in 16384 262144; do
    printf '0\200' > "$W/deep.der"
    n=1
    while [ $n -lt $units ]; do
        cat "$W/deep.der" "$W/deep.der" > "$W/deep.tmp" && mv "$W/deep.tmp" "$W/deep.der"
        n=$((n * 2))
    done
    sigjson "$ROOTHASH_SIG" "$W/deep.der" "$W/deep.json" /var/lib/extensions/sig.raw
    out=$(SYSTEMD_LOG_LEVEL=debug sysext --image-policy=root=signed merge 2>&1)
    rc=$?
    case $rc:$out in
        0:*)
            fail "signature nested $units levels deep accepted"
            sysext unmerge >/dev/null 2>&1
            ;;
        *"deeper than 32"* | *"more than the 65536 allowed"*) pass "signature nested $units levels deep refused" ;;
        *) fail "signature nested $units levels deep: rc=$rc $(echo "$out" | tail -n 3)" ;;
    esac
    expect_released "signature nested $units levels deep"
done

# --- sidecar verity data for a bare filesystem --------------------------------
if fs_supported squashfs; then
    payload side "$W/side.tree"
    rm -rf /var/lib/extensions/* "$W/side.raw"
    mksquashfs "$W/side.tree" "$W/side.raw" -noappend -quiet
    cp "$W/side.raw" /var/lib/extensions/side.raw
    SIDEHASH=$(veritysetup format /var/lib/extensions/side.raw /var/lib/extensions/side.verity | awk '/^Root hash/{print $3}')
    printf '%s\n' "$SIDEHASH" > /var/lib/extensions/side.roothash
    if sysext --image-policy=root=verity merge >/dev/null 2>&1; then
        pass "bare squashfs with .verity/.roothash sidecars merged under root=verity"
        src=$(awk '$5 == "/run/systemd/sysext/extensions/side" {print $0}' /proc/self/mountinfo)
        case "$src" in *mapper*) pass "sidecar image mounted through dm-verity" ;; *) fail "sidecar image not mounted through dm-verity: $src" ;; esac
        sysext unmerge >/dev/null 2>&1
    else
        fail "bare squashfs with verity sidecars"
    fi
    expect_released "sidecar verity"

    smime "$W/signer.key" "$W/signer.pem" "$SIDEHASH" /var/lib/extensions/side.roothash.p7s
    if sysext --image-policy=root=signed merge >/dev/null 2>&1; then
        pass "sidecar signature verified under root=signed"
        sysext unmerge >/dev/null 2>&1
    else
        fail "sidecar signature rejected"
    fi
    : > /var/lib/extensions/side.roothash.p7s
    if sysext merge >/dev/null 2>&1; then
        fail "empty .roothash.p7s accepted"
        sysext unmerge >/dev/null 2>&1
    else
        pass "empty .roothash.p7s refused"
    fi
    rm -f /var/lib/extensions/side.roothash.p7s
    printf '\377\377\377\377' | dd of=/var/lib/extensions/side.raw bs=1 seek=200 conv=notrunc status=none
    if sysext --image-policy=root=verity merge >/dev/null 2>&1 && cat /usr/share/side/hello.txt >/dev/null 2>&1 \
        && find /usr/share/side /usr/lib/extension-release.d -type f -exec cat {} + >/dev/null 2>&1; then
        fail "tampered sidecar-protected image read without errors"
    else
        pass "tampered sidecar-protected image detected"
    fi
    sysext unmerge >/dev/null 2>&1
    expect_released "sidecar tamper"
else
    skip fs:squashfs "squashfs unavailable; sidecar tests skipped"
fi

# --- systemd-repart built 4K-sector signed DDI ---------------------------------
# Built by test/e2e/fixtures.sh (run.sh runs it in archlinux:latest):
#   systemd-repart --make-ddi=sysext --sector-size=4096 --copy-source=SRC \
#       --private-key=KEY --certificate=test/fixtures/repart-4k/cert.pem \
#       test/fixtures/repart-4k/repart4k.raw
# where SRC carries usr/share/repart4k/hello.txt and the extension-release.
F=/work/test/fixtures/repart-4k
if [ -f "$F/repart4k.raw" ] && [ -f "$F/cert.pem" ]; then
    rm -f /etc/verity.d/*
    cp "$F/cert.pem" /etc/verity.d/repart.crt
    only "$F/repart4k.raw" repart4k
    if out=$(sysext --image-policy=root=signed merge 2>&1) \
        && [ "$(cat /usr/share/repart4k/hello.txt 2>/dev/null)" = "hello from repart4k" ]; then
        pass "systemd-repart 4K-sector signed DDI merged under root=signed"
    else
        fail "systemd-repart 4K-sector signed DDI: $out"
    fi
    sysext unmerge >/dev/null 2>&1
    expect_released "systemd-repart 4K-sector DDI"
    rm -f /etc/verity.d/repart.crt
else
    skip fixture:repart-4k "no systemd-repart 4K fixture in test/fixtures/repart-4k (run.sh builds it with archlinux:latest)"
fi

# --- sysext and confext of the same name --------------------------------------
UUIDS='hash'
SIG=0
ddi dup 512 "$W/dup-sys.raw"
confpayload dup "$W/dupc.tree"
ddi_build "$W/dup-conf.raw" "$W/dupc.tree" -v || fail "build dup-conf.raw"
only "$W/dup-sys.raw" dup
cp "$W/dup-conf.raw" /var/lib/confexts/dup.raw
if sysext merge >/dev/null 2>&1 && confext merge >/dev/null 2>&1; then
    pass "verity sysext and confext with the same name merged"
    [ "$(cat /etc/dup/hello.conf 2>/dev/null)" = "conf from dup" ] || fail "confext payload missing"
    [ "$(verity_devs | wc -l)" = 2 ] && pass "two distinct dm devices" || fail "dm devices: $(verity_devs)"
    confext unmerge >/dev/null 2>&1
    sleep 0.5
    if [ "$(cat /usr/share/dup/hello.txt 2>/dev/null)" = "hello from dup" ] && [ "$(verity_devs | wc -l)" = 1 ]; then
        pass "confext unmerge leaves the sysext device alone"
    else
        fail "confext unmerge disturbed the sysext: dm=[$(verity_devs)]"
    fi
    sysext unmerge >/dev/null 2>&1
else
    fail "verity sysext + confext with the same name"
    sysext unmerge >/dev/null 2>&1
    confext unmerge >/dev/null 2>&1
fi
expect_released "sysext + confext"

# --- concurrent merges ----------------------------------------------------------
rm -rf /var/lib/confexts/*
only "$W/life.raw" life
sysext --image-policy=root=verity merge > "$W/m1.log" 2>&1 &
p1=$!
sysext --image-policy=root=verity merge > "$W/m2.log" 2>&1 &
p2=$!
wait $p1
r1=$?
wait $p2
r2=$?
if [ $r1 = 0 ] || [ $r2 = 0 ]; then
    pass "concurrent merges: one succeeded"
else
    fail "concurrent merges both failed: $(cat "$W/m1.log" "$W/m2.log")"
fi
if grep -q "DM_DEV\|device-mapper" "$W/m1.log" "$W/m2.log"; then
    fail "concurrent merges hit device-mapper errors: $(cat "$W/m1.log" "$W/m2.log")"
else
    pass "concurrent merges: no device-mapper collisions"
fi
sysext unmerge >/dev/null 2>&1
expect_released "concurrent merges"

rm -rf /var/lib/extensions/* /var/lib/confexts/* /etc/verity.d/*
finish

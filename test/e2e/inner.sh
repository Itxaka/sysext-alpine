#!/bin/sh
# e2e suite for the basic verbs and image formats. Builds extension images in
# every supported format (squashfs, ext4, erofs, GPT DDI for the host
# architecture, directory, confext squashfs) and exercises
# merge/unmerge/refresh/status/list and masking.
. /work/test/e2e/lib.sh

echo "=== Installing build dependencies ==="
pkg_add squashfs-tools e2fsprogs erofs-utils kmod jq
install_sysext
need_overlayfs
mkdir -p /var/lib/extensions /var/lib/confexts /etc/extensions

# Names of raw sysext images that were built (fs supported).
BUILT=""
built() {
    case " $BUILT " in
        *" $1 "*) return 0 ;;
        *) return 1 ;;
    esac
}

echo "=== Building test images ==="

if fs_supported squashfs; then
    mk_tree "$WORKDIR/test-squashfs" test-squashfs
    if img_squashfs "$WORKDIR/test-squashfs" /var/lib/extensions/test-squashfs.raw; then
        BUILT="$BUILT test-squashfs"
    else
        fail "build test-squashfs.raw"
    fi
else
    skip fs:squashfs "squashfs not supported by the host kernel; skipping test-squashfs"
fi

if fs_supported ext4; then
    mk_tree "$WORKDIR/test-ext4" test-ext4
    if img_ext4 "$WORKDIR/test-ext4" /var/lib/extensions/test-ext4.raw 8; then
        BUILT="$BUILT test-ext4"
    else
        fail "build test-ext4.raw"
    fi
else
    skip fs:ext4 "ext4 not supported by the host kernel; skipping test-ext4"
fi

if fs_supported erofs; then
    mk_tree "$WORKDIR/test-erofs" test-erofs
    if img_erofs "$WORKDIR/test-erofs" /var/lib/extensions/test-erofs.raw; then
        BUILT="$BUILT test-erofs"
    else
        fail "build test-erofs.raw"
    fi
else
    skip fs:erofs "erofs not supported by the host kernel; skipping test-erofs"
fi

# GPT DDI with a root partition for the host architecture.
if [ -z "$ROOT_GUID" ]; then
    skip arch "no partition types known for $HOST_ARCH; skipping test-gpt"
elif fs_supported ext4; then
    mk_tree "$WORKDIR/test-gpt" test-gpt
    if ddi_build "$WORKDIR/test-gpt.raw" "$WORKDIR/test-gpt" && cp "$WORKDIR/test-gpt.raw" /var/lib/extensions/; then
        BUILT="$BUILT test-gpt"
    else
        fail "build test-gpt.raw (GPT + ext4 root partition)"
    fi
fi

mk_tree /var/lib/extensions/test-dir test-dir

CONFEXT_OK=0
if fs_supported squashfs; then
    mk_tree "$WORKDIR/conf-test" conf-test 'ID=_any\nARCHITECTURE=_any\n' confext
    if img_squashfs "$WORKDIR/conf-test" /var/lib/confexts/conf-test.raw; then
        CONFEXT_OK=1
    else
        fail "build conf-test.raw"
    fi
else
    skip fs:squashfs "squashfs not supported; skipping the confext test image"
fi

ALL_EXTS="$BUILT test-dir"
echo "=== Test extensions: $ALL_EXTS ==="

echo "=== Running tests ==="

# 1. list shows all extensions
out=$(sysext list 2>&1)
expect_eq "sysext list exits 0" "$?" 0
for name in $ALL_EXTS; do
    if echo "$out" | grep -q "$name"; then
        pass "sysext list shows $name"
    else
        fail "sysext list missing $name (output: $out)"
    fi
done
l=$(sysext list --json=short)
for name in $BUILT; do
    expect_eq "list type of $name" "$(echo "$l" | jq -r ".[] | select(.name == \"$name\") | .type")" raw
done
expect_eq "list type of test-dir" "$(echo "$l" | jq -r '.[] | select(.name == "test-dir") | .type')" directory

# 2. merge succeeds
if sysext merge; then pass "sysext merge"; else fail "sysext merge"; fi

# 3. payload files present for every extension
for name in $ALL_EXTS; do
    if [ -f "/usr/share/$name/hello.txt" ]; then
        pass "merged payload present: /usr/share/$name/hello.txt"
    else
        fail "merged payload missing: /usr/share/$name/hello.txt"
    fi
done

# 4. merged tools execute
for name in $ALL_EXTS; do
    expect_eq "tool executes: $name-tool" "$("/usr/bin/$name-tool" 2>&1)" "$name-tool"
done

# 5. status shows merged extensions
out=$(sysext status 2>&1)
if [ "$?" -eq 0 ] && echo "$out" | grep -q "test-dir"; then
    pass "sysext status shows merged extensions"
else
    fail "sysext status does not show merged extensions (output: $out)"
fi
expect_eq "status lists every merged extension" \
    "$(sysext status --json=short | jq -r '.[] | select(.hierarchy == "/usr") | .extensions | join(" ")')" \
    "$(for n in $ALL_EXTS; do echo "$n"; done | sort | tr '\n' ' ' | sed 's/ $//')"

# 6. second merge must fail (already merged)
if sysext merge 2>/dev/null; then
    fail "second sysext merge succeeded but must fail when already merged"
else
    pass "second sysext merge fails as expected"
fi

# 7. unmerge and verify files gone
if sysext unmerge; then pass "sysext unmerge"; else fail "sysext unmerge"; fi
for name in $ALL_EXTS; do
    if [ -e "/usr/share/$name/hello.txt" ]; then
        fail "file still present after unmerge: /usr/share/$name/hello.txt"
    else
        pass "file gone after unmerge: /usr/share/$name/hello.txt"
    fi
done
released && pass "loop devices released after unmerge" || fail "leaked after unmerge: $(leaked)"

# 8. refresh merges again
if sysext refresh; then pass "sysext refresh"; else fail "sysext refresh"; fi
for name in $ALL_EXTS; do
    if [ -f "/usr/share/$name/hello.txt" ]; then
        pass "payload back after refresh: $name"
    else
        fail "payload missing after refresh: $name"
    fi
done

# 9. confext merge / verify / unmerge
if [ "$CONFEXT_OK" = 1 ]; then
    if confext merge; then pass "confext merge"; else fail "confext merge"; fi
    if [ "$(cat /etc/conf-test/hello.conf 2>/dev/null)" = "conf from conf-test" ]; then
        pass "confext payload present: /etc/conf-test/hello.conf"
    else
        fail "confext payload missing: /etc/conf-test/hello.conf"
    fi
    if confext unmerge; then pass "confext unmerge"; else fail "confext unmerge"; fi
    if [ -e /etc/conf-test/hello.conf ]; then
        fail "confext payload still present after unmerge"
    else
        pass "confext payload gone after unmerge"
    fi
fi

# 10. masking: an empty dir in /etc/extensions masks the same-named extension
MASK_NAME=""
if built test-ext4; then
    MASK_NAME=test-ext4
else
    for name in $BUILT; do
        MASK_NAME=$name
        break
    done
fi
if [ -n "$MASK_NAME" ]; then
    mkdir -p "/etc/extensions/$MASK_NAME"
    if sysext refresh; then
        pass "sysext refresh with $MASK_NAME masked"
    else
        fail "sysext refresh with $MASK_NAME masked"
    fi
    if [ -e "/usr/share/$MASK_NAME/hello.txt" ]; then
        fail "masked extension $MASK_NAME still merged"
    else
        pass "masked extension $MASK_NAME not merged"
    fi
    for name in $ALL_EXTS; do
        [ "$name" = "$MASK_NAME" ] && continue
        if [ -f "/usr/share/$name/hello.txt" ]; then
            pass "unmasked extension still merged: $name"
        else
            fail "unmasked extension missing while $MASK_NAME masked: $name"
        fi
    done
    rmdir "/etc/extensions/$MASK_NAME"
    if sysext refresh; then
        pass "sysext refresh after unmasking"
    else
        fail "sysext refresh after unmasking"
    fi
    if [ -f "/usr/share/$MASK_NAME/hello.txt" ]; then
        pass "extension $MASK_NAME merged again after unmasking"
    else
        fail "extension $MASK_NAME missing after unmasking"
    fi
else
    skip fs:any "masking test needs a raw image"
fi

finish

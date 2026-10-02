package image

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAuxiliaryPath(t *testing.T) {
	for in, want := range map[string]string{
		"/x/foo.raw":     "/x/foo.roothash",
		"/x/foo":         "/x/foo.roothash",
		"/x/foo.raw.raw": "/x/foo.raw.roothash",
		"/x/foo.img":     "/x/foo.img.roothash",
	} {
		if got := auxiliaryPath(in, ".roothash"); got != want {
			t.Errorf("auxiliaryPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDevicePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/dev/vdb":                 true,
		"/dev/disk/by-label/x":     true,
		"/sys/block/vdb":           true,
		"/dev":                     false,
		"/dev/":                    false,
		"/sys":                     false,
		"/devices/x":               false,
		"/var/lib/extensions/x":    false,
		"/root/dev/vdb":            false,
		"/var/lib/extensions/dev/": false,
	} {
		if got := isDevicePath(p); got != want {
			t.Errorf("isDevicePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadVeritySidecars(t *testing.T) {
	hash := strings.Repeat("ab", 32)

	t.Run("none", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "foo.raw")
		writeFile(t, img, "x")
		vs, err := loadVeritySidecars(img)
		if err != nil || vs.rootHash != nil || vs.sig != nil || vs.dataPath != "" || vs.designator != partInvalid {
			t.Fatalf("got %+v, %v", vs, err)
		}
	})
	t.Run("root hash, signature and hash tree", func(t *testing.T) {
		dir := t.TempDir()
		img := filepath.Join(dir, "foo.raw")
		writeFile(t, img, "x")
		writeFile(t, filepath.Join(dir, "foo.roothash"), hash+"\n")
		writeFile(t, filepath.Join(dir, "foo.roothash.p7s"), "sig")
		writeFile(t, filepath.Join(dir, "foo.verity"), "tree")
		writeFile(t, filepath.Join(dir, "foo.usrhash.p7s"), "other")
		vs, err := loadVeritySidecars(img)
		if err != nil {
			t.Fatal(err)
		}
		if len(vs.rootHash) != 32 || string(vs.sig) != "sig" || vs.dataPath != filepath.Join(dir, "foo.verity") || vs.designator != partRoot {
			t.Fatalf("got %+v", vs)
		}
		if !vs.covers(partRoot) || vs.covers(partUsr) {
			t.Error("covers() mismatch")
		}
	})
	t.Run("usr hash", func(t *testing.T) {
		dir := t.TempDir()
		img := filepath.Join(dir, "foo.raw")
		writeFile(t, img, "x")
		writeFile(t, filepath.Join(dir, "foo.usrhash"), hash)
		writeFile(t, filepath.Join(dir, "foo.roothash.p7s"), "ignored")
		writeFile(t, filepath.Join(dir, "foo.usrhash.p7s"), "sig")
		vs, err := loadVeritySidecars(img)
		if err != nil || vs.designator != partUsr || string(vs.sig) != "sig" || vs.covers(partRoot) {
			t.Fatalf("got %+v, %v", vs, err)
		}
	})
	t.Run("signature without root hash is ignored", func(t *testing.T) {
		dir := t.TempDir()
		img := filepath.Join(dir, "foo.raw")
		writeFile(t, img, "x")
		writeFile(t, filepath.Join(dir, "foo.roothash.p7s"), "sig")
		vs, err := loadVeritySidecars(img)
		if err != nil || vs.sig != nil {
			t.Fatalf("got %+v, %v", vs, err)
		}
	})
	t.Run("oversized signature", func(t *testing.T) {
		dir := t.TempDir()
		img := filepath.Join(dir, "foo.raw")
		writeFile(t, img, "x")
		writeFile(t, filepath.Join(dir, "foo.roothash"), hash)
		writeFile(t, filepath.Join(dir, "foo.roothash.p7s"), "")
		if err := os.Truncate(filepath.Join(dir, "foo.roothash.p7s"), maxVeritySigSize+1); err != nil {
			t.Fatal(err)
		}
		if _, err := loadVeritySidecars(img); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("err = %v", err)
		}
		if err := os.Truncate(filepath.Join(dir, "foo.roothash.p7s"), maxVeritySigSize); err != nil {
			t.Fatal(err)
		}
		if vs, err := loadVeritySidecars(img); err != nil || len(vs.sig) != maxVeritySigSize {
			t.Fatalf("signature of the maximum size: %v", err)
		}
	})
	for name, files := range map[string]map[string]string{
		"empty signature": {"foo.roothash": hash, "foo.roothash.p7s": ""},
		"bad hex":         {"foo.roothash": "xyz"},
		"short hash":      {"foo.roothash": "abcd"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			img := filepath.Join(dir, "foo.raw")
			writeFile(t, img, "x")
			for n, c := range files {
				writeFile(t, filepath.Join(dir, n), c)
			}
			if _, err := loadVeritySidecars(img); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("disabled", func(t *testing.T) {
		dir := t.TempDir()
		img := filepath.Join(dir, "foo.raw")
		writeFile(t, img, "x")
		writeFile(t, filepath.Join(dir, "foo.roothash"), hash)
		t.Setenv("SYSTEMD_DISSECT_VERITY_SIDECAR", "0")
		vs, err := loadVeritySidecars(img)
		if err != nil || vs.rootHash != nil {
			t.Fatalf("got %+v, %v", vs, err)
		}
	})
	t.Run("xattr wins over file", func(t *testing.T) {
		dir := t.TempDir()
		img := filepath.Join(dir, "foo.raw")
		writeFile(t, img, "x")
		writeFile(t, filepath.Join(dir, "foo.roothash"), strings.Repeat("cd", 32))
		if err := unix.Setxattr(img, "user.verity.roothash", []byte(hash), 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
				t.Skip("user xattrs unsupported here")
			}
			t.Fatal(err)
		}
		vs, err := loadVeritySidecars(img)
		if err != nil || !bytes.Equal(vs.rootHash, bytes.Repeat([]byte{0xab}, 32)) {
			t.Fatalf("got %+v, %v", vs, err)
		}
	})
}

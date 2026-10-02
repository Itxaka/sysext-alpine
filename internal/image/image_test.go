package image

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/release"
)

func writeImage(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "img.raw")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMountWithOptsDirectory(t *testing.T) {
	dir := t.TempDir()
	m, err := MountWithOpts(discover.Image{Name: "d", Path: dir, Type: discover.TypeDirectory}, t.TempDir(), MountOpts{})
	if err != nil || m.Root != dir || m.RootHash != "" {
		t.Fatalf("got %+v, %v", m, err)
	}
	if err := m.Unmount(); err != nil {
		t.Errorf("Unmount of a directory image: %v", err)
	}
	var nilMounted *Mounted
	if err := nilMounted.Unmount(); err != nil {
		t.Errorf("Unmount(nil): %v", err)
	}
}

// TestMountWithOptsRejectsEarly covers failures that happen before any
// loop device is attached, so they run unprivileged.
func TestMountWithOptsRejectsEarly(t *testing.T) {
	x := archTypesOf(t, "x86-64")
	cases := []struct {
		name  string
		data  []byte
		opts  MountOpts
		want  string
		errno unix.Errno
	}{
		{"junk", make([]byte, 64<<10), MountOpts{}, "no suitable partition table or file system", unix.ENOPKG},
		{"bad policy", make([]byte, 4096), MountOpts{Policy: "root=banana"}, "unknown partition policy flag", unix.EBADRQC},
		{"no usable partition", buildGPT(t, 512, testPart{typ: gptTypeSwap}), MountOpts{}, "found neither", unix.ENXIO},
		{"usr ignored for confext", buildGPT(t, 512, testPart{typ: x[3]}), MountOpts{Class: release.Confext}, "found neither", unix.ENXIO},
		{"policy wants verity", buildGPT(t, 4096, testPart{typ: x[0]}), MountOpts{Policy: "root=verity"}, "does not satisfy image policy", unix.ERFKILL},
		{"policy forbids root", buildGPT(t, 512, testPart{typ: x[0]}), MountOpts{Policy: "root=absent"}, "the policy requires it to be absent", unix.ERFKILL},
		{"bare fs ignored by policy", func() []byte {
			b := make([]byte, 8192)
			copy(b, "hsqs")
			b[28] = 4
			return b
		}(), MountOpts{Policy: "root=ignore"}, "ignores the root partition", unix.ENOPKG},
		{"bare fs needs verity", func() []byte {
			b := make([]byte, 8192)
			copy(b, "hsqs")
			b[28] = 4
			return b
		}(), MountOpts{Policy: "root=verity"}, "does not satisfy image policy", unix.ERFKILL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := writeImage(t, tc.data)
			_, err := MountWithOpts(discover.Image{Name: "img", Path: img, Type: discover.TypeRaw}, t.TempDir(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if errno, _ := errors.AsType[unix.Errno](err); errno != tc.errno {
				t.Errorf("errno = %v, want %v", errno, tc.errno)
			}
		})
	}
}

func TestMountWithOptsExternalHashTreeNeedsFilesystem(t *testing.T) {
	img := writeImage(t, buildGPT(t, 512, testPart{typ: archTypesOf(t, "x86-64")[0]}))
	writeFile(t, strings.TrimSuffix(img, ".raw")+".verity", "tree")
	_, err := MountWithOpts(discover.Image{Name: "img", Path: img, Type: discover.TypeRaw}, t.TempDir(), MountOpts{})
	if err == nil || !strings.Contains(err.Error(), "not a single filesystem") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopConfig(t *testing.T) {
	f, err := os.Open(writeImage(t, []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg := loopConfig(f, "/var/lib/extensions/foo.raw", 4096, true)
	if cfg.Size != 4096 || cfg.Fd != uint32(f.Fd()) {
		t.Errorf("size/fd = %d/%d", cfg.Size, cfg.Fd)
	}
	want := uint32(unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR | unix.LO_FLAGS_DIRECT_IO)
	if cfg.Info.Flags != want {
		t.Errorf("flags = %#x, want %#x (partscan is enabled separately)", cfg.Info.Flags, want)
	}
	if name := string(cfg.Info.File_name[:len("/var/lib/extensions/foo.raw")]); name != "/var/lib/extensions/foo.raw" {
		t.Errorf("file name = %q", name)
	}
	cfg = loopConfig(f, strings.Repeat("a", 100), 512, false)
	if cfg.Info.Flags&unix.LO_FLAGS_DIRECT_IO != 0 || cfg.Info.File_name[63] != 0 {
		t.Errorf("non-direct config: flags %#x, name not terminated", cfg.Info.Flags)
	}
}

func TestParseDevT(t *testing.T) {
	maj, mnr, err := parseDevT("259:3")
	if err != nil || maj != 259 || mnr != 3 {
		t.Errorf("parseDevT(259:3) = %d, %d, %v", maj, mnr, err)
	}
	for _, s := range []string{"garbage", "1:x", "x:1"} {
		if _, _, err := parseDevT(s); err == nil {
			t.Errorf("parseDevT(%q) accepted", s)
		}
	}
}

func TestParseBoolean(t *testing.T) {
	for s, want := range map[string]bool{"1": true, "YES": true, "on": true, "t": true, "0": false, "No": false, "off": false, "f": false} {
		if got, ok := parseBoolean(s); !ok || got != want {
			t.Errorf("parseBoolean(%q) = %v, %v", s, got, ok)
		}
	}
	if _, ok := parseBoolean("maybe"); ok {
		t.Error("parseBoolean(maybe) accepted")
	}
	t.Setenv("X_TEST_BOOL", "maybe")
	if envDisabled("X_TEST_BOOL") || envStrictlyEnabled("X_TEST_BOOL") {
		t.Error("unparsable value handling")
	}
	t.Setenv("X_TEST_BOOL", "0")
	if !envDisabled("X_TEST_BOOL") {
		t.Error("envDisabled(0) = false")
	}
}

func TestMountWithOptsRejectsCharDevice(t *testing.T) {
	_, err := MountWithOpts(discover.Image{Name: "null", Path: "/dev/null", Type: discover.TypeBlock}, t.TempDir(), MountOpts{})
	if err == nil || !strings.Contains(err.Error(), "neither a regular file nor a block device") {
		t.Errorf("err = %v", err)
	}
}

func TestUseDeviceDirectly(t *testing.T) {
	f, err := os.Open(writeImage(t, []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, tc := range []struct {
		block      bool
		sectorSize int64
		blockSize  uint32
		partscan   bool
		want       bool
	}{
		{false, 512, 0, false, false},
		{true, 512, 0, false, true},
		{true, 4096, 0, false, true},
		{true, 512, 512, false, true},
		{true, 512, 4096, false, false},
		{true, 4096, 512, true, false},
		// No sysfs entry to tell whether the kernel scans partitions.
		{true, 512, 512, true, false},
	} {
		m := &mounter{file: f, block: tc.block, sectorSize: tc.sectorSize}
		if got := m.useDeviceDirectly(tc.blockSize, tc.partscan); got != tc.want {
			t.Errorf("%+v: useDeviceDirectly = %v", tc, got)
		}
	}
}

func TestPartscanEnabled(t *testing.T) {
	for _, tc := range []struct {
		files map[string]string
		want  bool
	}{
		{map[string]string{"partscan": "1\n", "ext_range": "1\n"}, true},
		{map[string]string{"partscan": "0\n", "ext_range": "256\n"}, false},
		{map[string]string{"partition": "1\n", "ext_range": "256\n"}, false},
		{map[string]string{"loop/partscan": "0\n", "ext_range": "256\n"}, false},
		{map[string]string{"loop/partscan": "1\n", "ext_range": "256\n"}, true},
		{map[string]string{"ext_range": "1\n"}, false},
		{map[string]string{"ext_range": "16\n"}, true},
		{map[string]string{}, false},
	} {
		dir := t.TempDir()
		for name, data := range tc.files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, name), data)
		}
		if got := partscanEnabled(dir); got != tc.want {
			t.Errorf("partscanEnabled(%v) = %v, want %v", tc.files, got, tc.want)
		}
	}
}

func TestPartitionName(t *testing.T) {
	for _, tc := range []struct {
		disk string
		n    int
		want string
	}{
		{"loop3", 1, "loop3p1"},
		{"nvme0n1", 2, "nvme0n1p2"},
		{"mmcblk0", 1, "mmcblk0p1"},
		{"vdb", 3, "vdb3"},
		{"sda", 12, "sda12"},
	} {
		if got := partitionName(tc.disk, tc.n); got != tc.want {
			t.Errorf("partitionName(%q, %d) = %q, want %q", tc.disk, tc.n, got, tc.want)
		}
	}
}

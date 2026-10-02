package discover

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// --- helpers ---------------------------------------------------------------

// mkdirImage creates a non-empty directory image at dir/name.
func mkdirImage(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(p, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// mkemptyDir creates an empty directory at dir/name.
func mkemptyDir(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// mkrawImage creates a regular file at dir/name.
func mkrawImage(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mksymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func names(images []Image) []string {
	out := make([]string, len(images))
	for i, img := range images {
		out[i] = img.Name
	}
	return out
}

func discover(t *testing.T, class release.Class, root string) []Image {
	t.Helper()
	got, err := Discover(class, root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	return got
}

// --- SearchDirs ------------------------------------------------------------

func TestSearchDirs(t *testing.T) {
	tests := []struct {
		name  string
		class release.Class
		root  string
		want  []string
	}{
		{"sysext empty root", release.Sysext, "",
			[]string{"/etc/extensions", "/run/extensions", "/var/lib/extensions"}},
		{"sysext slash root", release.Sysext, "/",
			[]string{"/etc/extensions", "/run/extensions", "/var/lib/extensions"}},
		{"sysext alternate root", release.Sysext, "/mnt/target",
			[]string{"/mnt/target/etc/extensions", "/mnt/target/run/extensions", "/mnt/target/var/lib/extensions"}},
		{"confext empty root", release.Confext, "",
			[]string{"/run/confexts", "/var/lib/confexts", "/usr/local/lib/confexts", "/usr/lib/confexts"}},
		{"confext alternate root", release.Confext, "/sysroot",
			[]string{"/sysroot/run/confexts", "/sysroot/var/lib/confexts", "/sysroot/usr/local/lib/confexts", "/sysroot/usr/lib/confexts"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SearchDirs(tc.class, tc.root); !slices.Equal(got, tc.want) {
				t.Errorf("SearchDirs(%v, %q) = %v, want %v", tc.class, tc.root, got, tc.want)
			}
		})
	}
}

func TestImageTypeString(t *testing.T) {
	for typ, want := range map[ImageType]string{
		TypeDirectory: "directory", TypeRaw: "raw", TypeBlock: "block", ImageType(42): "ImageType(42)",
	} {
		if got := typ.String(); got != want {
			t.Errorf("ImageType(%d).String() = %q, want %q", int(typ), got, want)
		}
	}
}

func TestImageTime(t *testing.T) {
	if got := (Image{MTime: 5, CrTime: 3}).Time(); got != 5 {
		t.Errorf("Time() = %d, want MTime", got)
	}
	if got := (Image{CrTime: 3}).Time(); got != 3 {
		t.Errorf("Time() = %d, want CrTime fallback", got)
	}
}

// --- Discover --------------------------------------------------------------

func TestDiscover(t *testing.T) {
	type want struct {
		name string
		typ  ImageType
	}
	tests := []struct {
		name  string
		class release.Class
		setup func(t *testing.T, root string)
		want  []want
	}{
		{
			name:  "raw vs dir typing",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), "dirext")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "rawext.raw")
			},
			want: []want{{"dirext", TypeDirectory}, {"rawext", TypeRaw}},
		},
		{
			name:  "priority shadowing across dirs",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "etc/extensions"), "foo")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "foo.raw")
				mkrawImage(t, filepath.Join(root, "run/extensions"), "bar.raw")
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), "bar")
			},
			want: []want{{"bar", TypeRaw}, {"foo", TypeDirectory}},
		},
		{
			name:  "empty dir shadows a lower-priority extension",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkemptyDir(t, filepath.Join(root, "etc/extensions"), "masked")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "masked.raw")
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), "kept")
			},
			want: []want{{"kept", TypeDirectory}, {"masked", TypeDirectory}},
		},
		{
			name:  "symlinks resolved to dir and raw",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "images"), "realdir")
				mkrawImage(t, filepath.Join(root, "images"), "real.raw")
				mksymlink(t, "../../../images/realdir", filepath.Join(root, "var/lib/extensions/linkdir"))
				mksymlink(t, "/images/real.raw", filepath.Join(root, "var/lib/extensions/linkraw.raw"))
			},
			want: []want{{"linkdir", TypeDirectory}, {"linkraw", TypeRaw}},
		},
		{
			name:  "broken symlink skipped",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mksymlink(t, "/nonexistent", filepath.Join(root, "var/lib/extensions/dangling.raw"))
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "ok.raw")
			},
			want: []want{{"ok", TypeRaw}},
		},
		{
			name:  "hidden entries are images",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), ".hiddendir")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), ".hidden.raw")
			},
			want: []want{{".hidden", TypeRaw}, {".hiddendir", TypeDirectory}},
		},
		{
			name:  "sysupdate temporaries, invalid and atomic-write names skipped",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions")
				mkrawImage(t, dir, ".sysupdate.x.raw")
				mkdirImage(t, dir, ".sysupdate.dir")
				mkrawImage(t, dir, ".#x.raw")
				mkrawImage(t, dir, "ctl\x01.raw")
				mkdirImage(t, dir, "nl\nx")
				mkrawImage(t, dir, "bad\xff.raw")
				mkrawImage(t, dir, ".raw")
				mkrawImage(t, dir, "visible.raw")
			},
			want: []want{{"visible", TypeRaw}},
		},
		{
			name:  "non-raw regular files and other inodes ignored",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions")
				mkrawImage(t, dir, "notes.txt")
				mkrawImage(t, dir, "good.raw")
				if err := unix.Mkfifo(filepath.Join(dir, "fifo.raw"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []want{{"good", TypeRaw}},
		},
		{
			name:  "directory with a .raw name keeps it",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), "odd.raw")
			},
			want: []want{{"odd.raw", TypeDirectory}},
		},
		{
			name:  "class suffix stripped",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions")
				mkrawImage(t, dir, "bar.sysext.raw")
				mkdirImage(t, dir, "baz.sysext")
				mkrawImage(t, dir, "qux.confext.raw")
			},
			want: []want{{"bar", TypeRaw}, {"baz", TypeDirectory}, {"qux.confext", TypeRaw}},
		},
		{
			name:  "confext class suffix stripped",
			class: release.Confext,
			setup: func(t *testing.T, root string) {
				mkrawImage(t, filepath.Join(root, "var/lib/confexts"), "qux.confext.raw")
				mkrawImage(t, filepath.Join(root, "var/lib/confexts"), "sx.sysext.raw")
			},
			want: []want{{"qux", TypeRaw}, {"sx.sysext", TypeRaw}},
		},
		{
			name:  "class suffix name shadowed by plain name",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkrawImage(t, filepath.Join(root, "etc/extensions"), "bar.raw")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "bar.sysext.raw")
			},
			want: []want{{"bar", TypeRaw}},
		},
		{
			name:  "mstack directories are not merged and shadow nothing",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "etc/extensions"), "ms.mstack")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "ms.raw")
			},
			want: []want{{"ms", TypeRaw}},
		},
		{
			name:  "all search dirs missing",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {},
			want:  nil,
		},
		{
			name:  "confext search dirs used",
			class: release.Confext,
			setup: func(t *testing.T, root string) {
				mkrawImage(t, filepath.Join(root, "run/confexts"), "conf.raw")
				mkdirImage(t, filepath.Join(root, "usr/local/lib/confexts"), "localconf")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "wrongclass.raw")
			},
			want: []want{{"conf", TypeRaw}, {"localconf", TypeDirectory}},
		},
		{
			name:  "result is version sorted",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions")
				mkrawImage(t, dir, "foo-10.raw")
				mkrawImage(t, dir, "foo-2.raw")
				mkrawImage(t, dir, "foo-1.raw")
			},
			want: []want{{"foo-1", TypeRaw}, {"foo-2", TypeRaw}, {"foo-10", TypeRaw}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got := discover(t, tc.class, root)
			if len(got) != len(tc.want) {
				t.Fatalf("Discover() = %v, want %v", got, tc.want)
			}
			for i, w := range tc.want {
				if got[i].Name != w.name || got[i].Type != w.typ {
					t.Errorf("image[%d] = {%q %s}, want {%q %s}", i, got[i].Name, got[i].Type, w.name, w.typ)
				}
				if got[i].Path == "" {
					t.Errorf("image[%d] %q has empty Path", i, got[i].Name)
				}
			}
		})
	}
}

// The /usr/local copy of a confext shadows the /usr/lib one, as in
// systemd's image_search_path.
func TestDiscoverConfextSearchOrder(t *testing.T) {
	root := t.TempDir()
	mkdirImage(t, filepath.Join(root, "usr/lib/confexts"), "foo")
	local := mkdirImage(t, filepath.Join(root, "usr/local/lib/confexts"), "foo")
	got := discover(t, release.Confext, root)
	if len(got) != 1 || got[0].Path != local {
		t.Errorf("Discover() = %v, want %s", got, local)
	}
}

func TestDiscoverChaseInRoot(t *testing.T) {
	root := t.TempDir()
	target := mkrawImage(t, filepath.Join(root, "srv/images"), "foo.raw")
	mksymlink(t, "/srv/images/foo.raw", filepath.Join(root, "etc/extensions/foo.raw"))
	mksymlink(t, "../../../../../../../../srv/images/foo.raw", filepath.Join(root, "run/extensions/esc.raw"))
	mksymlink(t, "/etc/passwd", filepath.Join(root, "run/extensions/host.raw"))
	extdir := mkdirImage(t, filepath.Join(root, "srv/ext"), "inlinked")
	mksymlink(t, "/srv/ext", filepath.Join(root, "var/lib/extensions"))

	got := discover(t, release.Sysext, root)
	want := map[string]string{"foo": target, "esc": target, "inlinked": extdir}
	if len(got) != len(want) {
		t.Fatalf("Discover() = %v, want %v", got, want)
	}
	for _, img := range got {
		if want[img.Name] != img.Path {
			t.Errorf("%s: Path = %q, want %q", img.Name, img.Path, want[img.Name])
		}
	}
}

func TestDiscoverSymlinkLoopFails(t *testing.T) {
	root := t.TempDir()
	mksymlink(t, "loop.raw", filepath.Join(root, "var/lib/extensions/loop.raw"))
	if _, err := Discover(release.Sysext, root); !errors.Is(err, unix.ELOOP) {
		t.Errorf("err = %v, want ELOOP", err)
	}
}

func TestDiscoverSearchDirNotADirectory(t *testing.T) {
	root := t.TempDir()
	mkrawImage(t, filepath.Join(root, "etc"), "extensions")
	if _, err := Discover(release.Sysext, root); !errors.Is(err, unix.ENOTDIR) {
		t.Errorf("err = %v, want ENOTDIR", err)
	}
}

func TestDiscoverTimes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "var/lib/extensions")
	raw := mkrawImage(t, dir, "r.raw")
	d := mkdirImage(t, dir, "d")
	mtime := time.Unix(1_700_000_000, 123_456_789)
	if err := os.Chtimes(raw, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(d, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	got := discover(t, release.Sysext, root)
	if len(got) != 2 {
		t.Fatalf("Discover() = %v", got)
	}
	byName := map[string]Image{got[0].Name: got[0], got[1].Name: got[1]}
	if r := byName["r"]; r.MTime != 1_700_000_000_123_456 {
		t.Errorf("raw MTime = %d, want microsecond precision", r.MTime)
	}
	if byName["d"].MTime != 0 {
		t.Errorf("directory MTime = %d, want 0 like systemd", byName["d"].MTime)
	}

	var le [8]byte
	binary.LittleEndian.PutUint64(le[:], 1_000_000)
	if err := unix.Setxattr(d, "user.crtime_usec", le[:], 0); err != nil {
		t.Skipf("user xattrs unsupported: %v", err)
	}
	got = discover(t, release.Sysext, root)
	for _, img := range got {
		if img.Name == "d" && (img.CrTime != 1_000_000 || img.Time() != 1_000_000) {
			t.Errorf("directory CrTime = %d, want the older user.crtime_usec", img.CrTime)
		}
	}
}

func TestDiscoverBlockDevice(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mknod needs root")
	}
	root := t.TempDir()
	dev := filepath.Join(root, "dev/loop-test")
	if err := os.MkdirAll(filepath.Dir(dev), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(dev, unix.S_IFBLK|0o600, int(unix.Mkdev(7, 200))); err != nil {
		t.Skipf("mknod: %v", err)
	}
	mksymlink(t, "/dev/loop-test", filepath.Join(root, "run/extensions/blk.sysext.raw"))
	got := discover(t, release.Sysext, root)
	if len(got) != 1 || got[0].Name != "blk.sysext.raw" || got[0].Type != TypeBlock || got[0].Path != dev {
		t.Errorf("Discover() = %+v, want block image named after the entry", got)
	}
}

// --- versioned directories ---------------------------------------------------

func foreignArch() string {
	for _, a := range []string{"sparc64", "tilegx", "alpha"} {
		if a != release.NativeArchitecture() && a != release.SecondaryArchitecture() {
			return a
		}
	}
	return "cris"
}

func TestDiscoverVersioned(t *testing.T) {
	native := release.NativeArchitecture()
	tests := []struct {
		name     string
		class    release.Class
		setup    func(t *testing.T, root string)
		wantName string
		wantFile string // base name of the picked entry, "" = no image
		wantType ImageType
	}{
		{
			name: "newest version, exhausted and foreign entries skipped",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.raw.v")
				for _, n := range []string{"foo_1.2.raw", "foo_1.10.raw", "foo_1.11+0.raw",
					"foo_2.0_" + foreignArch() + ".raw", "foo_1.9_" + native + ".raw", "bar_9.raw", "foo_1.12.img"} {
					mkrawImage(t, dir, n)
				}
			},
			wantName: "foo", wantFile: "foo_1.10.raw", wantType: TypeRaw,
		},
		{
			name: "directory entries",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.v")
				mkdirImage(t, dir, "foo_1")
				mkdirImage(t, dir, "foo_2")
				mkrawImage(t, dir, "foo_3.raw")
			},
			wantName: "foo", wantFile: "foo_2", wantType: TypeDirectory,
		},
		{
			name: "class suffix",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/bar.sysext.raw.v")
				mkrawImage(t, dir, "bar_3.sysext.raw")
				mkrawImage(t, dir, "bar_4.raw")
			},
			wantName: "bar", wantFile: "bar_3.sysext.raw", wantType: TypeRaw,
		},
		{
			name: "native preferred at equal version",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.raw.v")
				mkrawImage(t, dir, "foo_5.raw")
				mkrawImage(t, dir, "foo_5_"+native+".raw")
			},
			wantName: "foo", wantFile: "foo_5_" + native + ".raw", wantType: TypeRaw,
		},
		{
			name: "more tries left, then fewer tries done",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.raw.v")
				mkrawImage(t, dir, "foo_1+3-0.raw")
				mkrawImage(t, dir, "foo_1+5-4.raw")
				mkrawImage(t, dir, "foo_1+5-2.raw")
			},
			wantName: "foo", wantFile: "foo_1+5-2.raw", wantType: TypeRaw,
		},
		{
			name: "entry without counter beats counted ones",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.raw.v")
				mkrawImage(t, dir, "foo_1+5.raw")
				mkrawImage(t, dir, "foo_1.raw")
			},
			wantName: "foo", wantFile: "foo_1.raw", wantType: TypeRaw,
		},
		{
			name: "only exhausted entries still pick the newest",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.raw.v")
				mkrawImage(t, dir, "foo_1+0.raw")
				mkrawImage(t, dir, "foo_2+0-3.raw")
			},
			wantName: "foo", wantFile: "foo_2+0-3.raw", wantType: TypeRaw,
		},
		{
			name: "unknown architecture suffix is chopped",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.raw.v")
				mkrawImage(t, dir, "foo_7_bogus.raw")
				mkrawImage(t, dir, "foo_6.raw")
			},
			wantName: "foo", wantFile: "foo_7_bogus.raw", wantType: TypeRaw,
		},
		{
			name: "invalid versions and counters ignored",
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/extensions/foo.raw.v")
				mkrawImage(t, dir, "foo_9+x.raw")
				mkrawImage(t, dir, "foo_8+08.raw")
				mkrawImage(t, dir, "foo_.raw")
				mkrawImage(t, dir, "foo_7@.raw")
				mkrawImage(t, dir, "foo_1.raw")
			},
			wantName: "foo", wantFile: "foo_1.raw", wantType: TypeRaw,
		},
		{
			name: "symlinked entry resolved inside root",
			setup: func(t *testing.T, root string) {
				mkrawImage(t, filepath.Join(root, "store"), "blob")
				mksymlink(t, "/store/blob", filepath.Join(root, "var/lib/extensions/foo.raw.v/foo_3.raw"))
				mkrawImage(t, filepath.Join(root, "var/lib/extensions/foo.raw.v"), "foo_2.raw")
			},
			wantName: "foo", wantFile: "blob", wantType: TypeRaw,
		},
		{
			name: "dangling matching entry skips the whole directory",
			setup: func(t *testing.T, root string) {
				mksymlink(t, "/nonexistent", filepath.Join(root, "var/lib/extensions/foo.raw.v/foo_3.raw"))
				mkrawImage(t, filepath.Join(root, "var/lib/extensions/foo.raw.v"), "foo_2.raw")
			},
		},
		{
			name: "empty versioned dir does not mask",
			setup: func(t *testing.T, root string) {
				mkemptyDir(t, filepath.Join(root, "etc/extensions"), "baz.raw.v")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "baz.raw")
			},
			wantName: "baz", wantFile: "baz.raw", wantType: TypeRaw,
		},
		{
			name: "plain image shadows a versioned dir",
			setup: func(t *testing.T, root string) {
				mkrawImage(t, filepath.Join(root, "etc/extensions"), "foo.raw")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions/foo.raw.v"), "foo_9.raw")
			},
			wantName: "foo", wantFile: "foo.raw", wantType: TypeRaw,
		},
		{
			name:  "confext versioned dir",
			class: release.Confext,
			setup: func(t *testing.T, root string) {
				dir := filepath.Join(root, "var/lib/confexts/c.confext.v")
				mkdirImage(t, dir, "c_1.confext")
				mkdirImage(t, dir, "c_2")
			},
			wantName: "c", wantFile: "c_1.confext", wantType: TypeDirectory,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got := discover(t, tc.class, root)
			if tc.wantFile == "" {
				if len(got) != 0 {
					t.Errorf("Discover() = %v, want nothing", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("Discover() = %v, want one image", got)
			}
			if got[0].Name != tc.wantName || filepath.Base(got[0].Path) != tc.wantFile || got[0].Type != tc.wantType {
				t.Errorf("Discover() = %+v, want %s -> %s (%s)", got[0], tc.wantName, tc.wantFile, tc.wantType)
			}
		})
	}
}

// The vectors of systemd's test-vpick.c path_pick test, for x86 builds.
func TestPickVersionSystemdVectors(t *testing.T) {
	if n := release.NativeArchitecture(); n != "x86-64" {
		t.Skipf("vectors assume x86-64, native is %s", n)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "foo.raw.v")
	for _, n := range []string{"foo_5.5.raw", "foo_55.raw", "foo_5.raw", "foo_5_ia64.raw", "foo_7.raw",
		"foo_7_x86-64.raw", "foo_55_x86-64.raw", "foo_55_x86.raw", "foo_99_x86.raw", "foo_100_sparc.raw",
		"quux_1_s390.raw", "quux_2_s390+4-6.raw", "quux_3_s390+0-10.raw"} {
		mkrawImage(t, dir, n)
	}
	pick, err := pickVersion(root, "/foo.raw.v", "foo", ".raw", typeReg|typeBlk)
	if err != nil || pick == nil || filepath.Base(pick.path) != "foo_99_x86.raw" || pick.version != "99" || pick.arch != "x86" {
		t.Fatalf("pickVersion() = %+v, %v; want foo_99_x86.raw", pick, err)
	}
	if err := os.Remove(filepath.Join(dir, "foo_99_x86.raw")); err != nil {
		t.Fatal(err)
	}
	pick, err = pickVersion(root, "/foo.raw.v", "foo", ".raw", typeReg|typeBlk)
	if err != nil || pick == nil || filepath.Base(pick.path) != "foo_55_x86-64.raw" || pick.arch != "x86-64" {
		t.Fatalf("pickVersion() = %+v, %v; want foo_55_x86-64.raw", pick, err)
	}
	if pick, err := pickVersion(root, "/foo.raw.v", "foo", ".raw", typeDir); err != nil || pick != nil {
		t.Errorf("directory filter: %+v, %v; want no match", pick, err)
	}
}

func TestParseTries(t *testing.T) {
	tests := []struct {
		in         string
		left, done uint32
		ok         bool
	}{
		{"+3", 3, 0, true},
		{"+3-1", 3, 1, true},
		{"+0", 0, 0, true},
		{"+0-10", 0, 10, true},
		{"+010", 8, 0, true},
		{"+4294967295", 4294967295, 0, true},
		{"+4294967296", 0, 0, false},
		{"+08", 0, 0, false},
		{"+", 0, 0, false},
		{"+-1", 0, 0, false},
		{"+3-", 0, 0, false},
		{"+3-x", 0, 0, false},
		{"+3x", 0, 0, false},
		{"3", 0, 0, false},
	}
	for _, tc := range tests {
		left, done, ok := parseTries(tc.in)
		if ok != tc.ok || (ok && (left != tc.left || done != tc.done)) {
			t.Errorf("parseTries(%q) = %d, %d, %v; want %d, %d, %v", tc.in, left, done, ok, tc.left, tc.done, tc.ok)
		}
	}
}

func TestExtractImageBasename(t *testing.T) {
	tests := []struct {
		fname, class string
		formats      []string
		name, suffix string
		ok           bool
	}{
		{"foo.raw", ".sysext", []string{".raw"}, "foo", ".raw", true},
		{"foo.sysext.raw", ".sysext", []string{".raw"}, "foo", ".sysext.raw", true},
		{"foo.confext.raw", ".sysext", []string{".raw"}, "foo.confext", ".raw", true},
		{"foo", ".sysext", []string{".raw"}, "", "", false},
		{"foo.sysext", ".sysext", []string{".mstack", ""}, "foo", ".sysext", true},
		{"foo", ".sysext", []string{".raw", ".mstack", ""}, "foo", "", true},
		{"foo.mstack", ".sysext", []string{".mstack", ""}, "foo", ".mstack", true},
		{".sysext.raw", ".sysext", []string{".raw"}, "", "", false},
		{"blk", "", nil, "blk", "", true},
	}
	for _, tc := range tests {
		name, suffix, ok := extractImageBasename(tc.fname, tc.class, tc.formats)
		if name != tc.name || suffix != tc.suffix || ok != tc.ok {
			t.Errorf("extractImageBasename(%q) = %q, %q, %v; want %q, %q, %v", tc.fname, name, suffix, ok, tc.name, tc.suffix, tc.ok)
		}
	}
}

// --- CompareVersions / Sort --------------------------------------------------

func sgn(v int) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	default:
		return 0
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"1.0", "1.0", 0},
		{"007", "7", 0},
		{"1!.0", "1.0", 0},
		{"foo-1", "foo-2", -1},
		{"foo-2", "foo-10", -1},
		{"9", "10", -1},
		{"2.1", "2.10", -1},
		{"123", "123.1", -1},
		{"1.0~rc1", "1.0", -1},
		{"1.0~~", "1.0~", 1},
		{"~", "", -1},
		{"A", "a", -1},
		{"1-2", "1^2", -1},
		{"1^2", "1.2", -1},
		{"1.0.1", "1.0a", -1},
		{"foo1", "fooa", -1},

		// a run of zeros is a numeric segment and beats letters
		{"0", "a", 1},
		{"app-0", "app-beta", 1},
		{"a0-", "0", -1},
		{"-", "-^0_b", 1},
		// separators are handled in sequence, not by restarting
		{"~_1", "~1", -1},
		{"1-^2", "1-2", -1},

		// systemd test-string-util.c corner cases
		{"123_aa2-67.89", "123aa+2-67.89", 0},
		{"123.", "123", 1},
		{"12_3", "123", -1},
		{"12_3", "12", 1},
		{"12_3", "12.3", 1},
		{"123.0", "123", 1},
		{"123_0", "123", 1},
		{"123..0", "123.0", -1},
		{"0_", "0", 0},
		{"_0_", "0", 0},
		{"_0", "0", 0},
		{"0", "0___", 0},
		{"", "_", 0},
		{"_", "_", 0},
		{"", "~", 1},
		{"~", "~", 0},
		{"1٠١٢٣٤٥٦٧٨٩", "1", 0},

		// rpm rpmvercmp.at vectors kept by systemd
		{"1.0", "2.0", -1},
		{"2.0.1a", "2.0.1", 1},
		{"5.5p1", "5.5p2", -1},
		{"5.5p10", "5.5p1", 1},
		{"10xyz", "10.1xyz", 1},
		{"xyz10", "xyz10.1", -1},
		{"xyz.4", "8", -1},
		{"xyz.4", "2", -1},
		{"5.5p2", "5.6p1", -1},
		{"6.0", "6.0.rc1", -1},
		{"10a2", "10b2", -1},
		{"1.0aa", "1.0a", 1},
		{"10.0001", "10.1", 0},
		{"10.0001", "10.0039", -1},
		{"4.999.9", "5.0", -1},
		{"20101121", "20101122", -1},
		{"2_0", "2_0", 0},
		{"2.0", "2_0", -1},
		{"a", "a", 0},
		{"a+", "a_", 0},
		{"+a", "_a", 0},
		{"1.0~rc1", "1.0~rc1", 0},
		{"1.0~rc1", "1.0~rc2", -1},
		{"1.0~rc1~git123", "1.0~rc1", -1},
		{"1.0^", "1.0", 1},
		{"1.0^git1", "1.0^git2", -1},
		{"1.0^git1", "1.01", -1},
		{"1.0^20160101", "1.0.1", -1},
		{"1.0~rc1^git1", "1.0~rc1", 1},
		{"1.0^git1~pre", "1.0^git1", -1},
	}
	for _, tc := range tests {
		if got := sgn(CompareVersions(tc.a, tc.b)); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if rev := sgn(CompareVersions(tc.b, tc.a)); rev != -tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.b, tc.a, rev, -tc.want)
		}
	}
}

// The ordered list from systemd's TEST(strverscmp_improved).
func TestCompareVersionsSystemdOrder(t *testing.T) {
	versions := []string{"~1", "", "ab", "abb", "abc", "0001", "002", "12", "122", "122.9", "123~rc1", "123",
		"123-a", "123-a.1", "123-a1", "123-a1.1", "123-3", "123-3.1", "123^patch1", "123^1", "123.a-1",
		"123.1-1", "123a-1", "124"}
	for i, older := range versions {
		for _, newer := range versions[i+1:] {
			if CompareVersions(older, newer) >= 0 || CompareVersions(newer, older) <= 0 {
				t.Errorf("%q must sort before %q", older, newer)
			}
		}
	}
	pairs := [][2]string{
		{"123.45-67.88", "123.45-67.89"}, {"123.45-67.89", "123.45-67.89a"}, {"123.45-67.ab", "123.45-67.89"},
		{"123.45-67.9", "123.45-67.89"}, {"123.45-67", "123.45-67.89"}, {"123.45-66.89", "123.45-67.89"},
		{"123.45-9.99", "123.45-67.89"}, {"123.42-99.99", "123.45-67.89"}, {"123-99.99", "123.45-67.89"},
		{"123~rc1-99.99", "123.45-67.89"}, {"123~rc1-99.99", "123-45.67.89"}, {"123~rc1-99.99", "123~rc2-67.89"},
		{"123~rc1-99.99", "123^aa2-67.89"}, {"123~rc1-99.99", "123aa2-67.89"}, {"123-99.99", "123^aa2-67.89"},
		{"123-99.99", "123aa2-67.89"}, {"123^45-67.89", "123.45-67.89"}, {"123^aa1-99.99", "123^aa2-67.89"},
		{"123^aa2-67.89", "123aa2-67.89"}, {"123.aa2-67.89", "123aa2-67.89"}, {"123.aa2-67.89", "123.ab2-67.89"},
	}
	for _, p := range pairs {
		if CompareVersions(p[0], p[1]) >= 0 || CompareVersions(p[1], p[0]) <= 0 {
			t.Errorf("%q must sort before %q", p[0], p[1])
		}
	}
}

func FuzzCompareVersions(f *testing.F) {
	for _, s := range [][2]string{{"1.0", "1.0~rc1"}, {"app-0", "app-beta"}, {"~_1", "~1"}, {"-", "-^0_b"}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := sgn(CompareVersions(a, b)), sgn(CompareVersions(b, a))
		if ab != -ba {
			t.Fatalf("not antisymmetric: (%q, %q) = %d, reverse %d", a, b, ab, ba)
		}
		if CompareVersions(a, a) != 0 {
			t.Fatalf("CompareVersions(%q, %q) != 0", a, a)
		}
	})
}

func TestSort(t *testing.T) {
	imgs := []Image{{Name: "foo-10"}, {Name: "bar"}, {Name: "foo-2"}, {Name: "foo-1.0~rc1"}, {Name: "foo-1"}, {Name: "foo-1.0"}}
	Sort(imgs)
	want := []string{"bar", "foo-1", "foo-1.0~rc1", "foo-1.0", "foo-2", "foo-10"}
	if got := names(imgs); !slices.Equal(got, want) {
		t.Errorf("Sort() order = %v, want %v", got, want)
	}
}

func TestDiscoverRoots(t *testing.T) {
	if _, err := Discover(release.Sysext, filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing root: err = %v, want ErrNotExist", err)
	}

	dir := t.TempDir()
	raw := mkrawImage(t, filepath.Join(dir, "r/var/lib/extensions"), "r.raw")
	mksymlink(t, "/var/lib/extensions/r.raw", filepath.Join(dir, "r/etc/extensions/l.raw"))
	t.Chdir(dir)
	got := discover(t, release.Sysext, "r")
	if len(got) != 2 || got[0].Path != raw || got[1].Path != raw {
		t.Errorf("relative root: Discover() = %+v, want absolute paths %s", got, raw)
	}
}

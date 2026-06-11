package discover

import (
	"os"
	"path/filepath"
	"testing"

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

// mkemptyDir creates an empty directory (a mask) at dir/name.
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

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- SearchDirs ------------------------------------------------------------

func TestSearchDirs(t *testing.T) {
	tests := []struct {
		name  string
		class release.Class
		root  string
		want  []string
	}{
		{
			name:  "sysext empty root",
			class: release.Sysext,
			root:  "",
			want:  []string{"/etc/extensions", "/run/extensions", "/var/lib/extensions"},
		},
		{
			name:  "sysext slash root",
			class: release.Sysext,
			root:  "/",
			want:  []string{"/etc/extensions", "/run/extensions", "/var/lib/extensions"},
		},
		{
			name:  "sysext alternate root",
			class: release.Sysext,
			root:  "/mnt/target",
			want: []string{
				"/mnt/target/etc/extensions",
				"/mnt/target/run/extensions",
				"/mnt/target/var/lib/extensions",
			},
		},
		{
			name:  "confext empty root",
			class: release.Confext,
			root:  "",
			want: []string{
				"/run/confexts", "/var/lib/confexts",
				"/usr/lib/confexts", "/usr/local/lib/confexts",
			},
		},
		{
			name:  "confext alternate root",
			class: release.Confext,
			root:  "/sysroot",
			want: []string{
				"/sysroot/run/confexts", "/sysroot/var/lib/confexts",
				"/sysroot/usr/lib/confexts", "/sysroot/usr/local/lib/confexts",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SearchDirs(tc.class, tc.root)
			if !eqStrings(got, tc.want) {
				t.Errorf("SearchDirs(%v, %q) = %v, want %v", tc.class, tc.root, got, tc.want)
			}
		})
	}
}

// --- ImageType.String ------------------------------------------------------

func TestImageTypeString(t *testing.T) {
	tests := []struct {
		t    ImageType
		want string
	}{
		{TypeDirectory, "directory"},
		{TypeRaw, "raw"},
		{ImageType(42), "ImageType(42)"},
	}
	for _, tc := range tests {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("ImageType(%d).String() = %q, want %q", int(tc.t), got, tc.want)
		}
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
			want: []want{
				{"dirext", TypeDirectory},
				{"rawext", TypeRaw},
			},
		},
		{
			name:  "priority shadowing across dirs",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				// foo exists as a non-empty dir in /etc (highest)
				// and as a raw image in /var/lib (lowest): /etc wins.
				mkdirImage(t, filepath.Join(root, "etc/extensions"), "foo")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "foo.raw")
				// bar exists in /run and /var/lib: /run wins.
				mkrawImage(t, filepath.Join(root, "run/extensions"), "bar.raw")
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), "bar")
			},
			want: []want{
				{"bar", TypeRaw},
				{"foo", TypeDirectory},
			},
		},
		{
			name:  "empty dir masks lower-priority extension",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkemptyDir(t, filepath.Join(root, "etc/extensions"), "masked")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "masked.raw")
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), "kept")
			},
			want: []want{{"kept", TypeDirectory}},
		},
		{
			name:  "lone empty dir is excluded too",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkemptyDir(t, filepath.Join(root, "var/lib/extensions"), "empty")
			},
			want: nil,
		},
		{
			name:  "non-empty winner shadows but does not mask",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "etc/extensions"), "foo")
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), "foo")
			},
			want: []want{{"foo", TypeDirectory}},
		},
		{
			name:  "symlinks resolved to dir and raw",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				realDir := mkdirImage(t, filepath.Join(root, "images"), "realdir")
				realRaw := mkrawImage(t, filepath.Join(root, "images"), "real.raw")
				mksymlink(t, realDir, filepath.Join(root, "var/lib/extensions/linkdir"))
				mksymlink(t, realRaw, filepath.Join(root, "var/lib/extensions/linkraw.raw"))
			},
			want: []want{
				{"linkdir", TypeDirectory},
				{"linkraw", TypeRaw},
			},
		},
		{
			name:  "broken symlink skipped",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mksymlink(t, filepath.Join(root, "nonexistent"),
					filepath.Join(root, "var/lib/extensions/dangling.raw"))
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "ok.raw")
			},
			want: []want{{"ok", TypeRaw}},
		},
		{
			name:  "hidden entries skipped",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkdirImage(t, filepath.Join(root, "var/lib/extensions"), ".hiddendir")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), ".hidden.raw")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "visible.raw")
			},
			want: []want{{"visible", TypeRaw}},
		},
		{
			name:  "non-raw regular files ignored",
			class: release.Sysext,
			setup: func(t *testing.T, root string) {
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "notes.txt")
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "good.raw")
			},
			want: []want{{"good", TypeRaw}},
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
				// sysext dir must be ignored for confext
				mkrawImage(t, filepath.Join(root, "var/lib/extensions"), "wrongclass.raw")
			},
			want: []want{
				{"conf", TypeRaw},
				{"localconf", TypeDirectory},
			},
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
			want: []want{
				{"foo-1", TypeRaw},
				{"foo-2", TypeRaw},
				{"foo-10", TypeRaw},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got, err := Discover(tc.class, root)
			if err != nil {
				t.Fatalf("Discover() error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Discover() = %v, want %d images %v", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if got[i].Name != w.name || got[i].Type != w.typ {
					t.Errorf("image[%d] = {%q %s}, want {%q %s}",
						i, got[i].Name, got[i].Type, w.name, w.typ)
				}
				if got[i].Path == "" {
					t.Errorf("image[%d] %q has empty Path", i, got[i].Name)
				}
				if got[i].ModTime == 0 {
					t.Errorf("image[%d] %q has zero ModTime", i, got[i].Name)
				}
			}
		})
	}
}

func TestDiscoverSymlinkPathResolved(t *testing.T) {
	root := t.TempDir()
	realRaw := mkrawImage(t, filepath.Join(root, "images"), "real.raw")
	mksymlink(t, realRaw, filepath.Join(root, "var/lib/extensions/alias.raw"))

	got, err := Discover(release.Sysext, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d images, want 1: %v", len(got), got)
	}
	// Path must point at the resolved target, not the symlink. The temp
	// dir itself may contain symlinks (e.g. /tmp), so resolve expectation.
	wantPath, err := filepath.EvalSymlinks(realRaw)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Path != wantPath {
		t.Errorf("Path = %q, want resolved target %q", got[0].Path, wantPath)
	}
	if got[0].Name != "alias" {
		t.Errorf("Name = %q, want %q (named after the symlink)", got[0].Name, "alias")
	}
}

// --- CompareVersions / Sort --------------------------------------------------

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int // sign only
	}{
		// equality
		{"", "", 0},
		{"1.0", "1.0", 0},
		{"foo", "foo", 0},
		{"007", "7", 0},    // leading zeros ignored
		{"1!.0", "1.0", 0}, // invalid chars skipped
		{"1..0", "1..0", 0},

		// plain numeric segments
		{"foo-1", "foo-2", -1},
		{"foo-2", "foo-10", -1},
		{"foo-1", "foo-10", -1},
		{"1", "2", -1},
		{"9", "10", -1},
		{"2.1", "2.10", -1},
		{"123", "123.1", -1}, // end-of-string < '.'

		// '~' sorts before everything, including end-of-string
		{"1.0~rc1", "1.0", -1},
		{"1.0~rc1", "1.0~rc2", -1},
		{"1.0~~", "1.0~", -1},
		{"~", "", -1},
		{"1.0~rc1", "1.0.0", -1},

		// plain alpha
		{"abc", "abd", -1},
		{"a", "ab", -1},
		{"alpha", "beta", -1},
		{"A", "a", -1}, // ASCII, case-sensitive

		// separator precedence: '-' < '^' < '.' < letters < digits
		{"1-2", "1^2", -1},
		{"1^2", "1.2", -1},
		{"1.0.1", "1.0a", -1},  // '.' < letter
		{"1.0a", "1.01", -1},   // digit run beats letter at same position
		{"foo.a", "foo.1", -1}, // letter segment < digit segment
		// Within an unbroken alpha run the whole run is compared first
		// (common prefix, then longer run is newer), so the digit never
		// gets aligned against the trailing letter here:
		{"foo1", "fooa", -1},

		// end-of-string before separators (except '~')
		{"1", "1-1", -1},
		{"1", "1.1", -1},
		{"1", "1a", -1},
	}
	for _, tc := range tests {
		got := sgn(CompareVersions(tc.a, tc.b))
		if got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
		// antisymmetry
		rev := sgn(CompareVersions(tc.b, tc.a))
		if rev != -tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want sign %d", tc.b, tc.a, rev, -tc.want)
		}
	}
}

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

func TestSort(t *testing.T) {
	imgs := []Image{
		{Name: "foo-10"},
		{Name: "bar"},
		{Name: "foo-2"},
		{Name: "foo-1.0~rc1"},
		{Name: "foo-1"},
		{Name: "foo-1.0"},
	}
	Sort(imgs)
	want := []string{"bar", "foo-1", "foo-1.0~rc1", "foo-1.0", "foo-2", "foo-10"}
	if got := names(imgs); !eqStrings(got, want) {
		t.Errorf("Sort() order = %v, want %v", got, want)
	}
}

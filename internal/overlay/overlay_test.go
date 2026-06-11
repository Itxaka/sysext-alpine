package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/itxaka/sysext-alpine/internal/release"
)

func TestErrAlreadyMergedSentinel(t *testing.T) {
	if ErrAlreadyMerged == nil {
		t.Fatal("ErrAlreadyMerged must be a non-nil sentinel")
	}
	wrapped := errors.Join(ErrAlreadyMerged)
	if !errors.Is(wrapped, ErrAlreadyMerged) {
		t.Fatal("ErrAlreadyMerged must survive errors.Is through wrapping")
	}
}

func TestHierarchies(t *testing.T) {
	if got := Hierarchies(release.Sysext); !reflect.DeepEqual(got, []string{"/usr", "/opt"}) {
		t.Errorf("Sysext hierarchies = %v", got)
	}
	if got := Hierarchies(release.Confext); !reflect.DeepEqual(got, []string{"/etc"}) {
		t.Errorf("Confext hierarchies = %v", got)
	}
}

func TestWorkspace(t *testing.T) {
	cases := []struct {
		class release.Class
		root  string
		want  string
	}{
		{release.Sysext, "", "/run/systemd/sysext"},
		{release.Sysext, "/", "/run/systemd/sysext"},
		{release.Confext, "", "/run/systemd/confext"},
		{release.Sysext, "/alt/root", "/alt/root/run/systemd/sysext"},
		{release.Confext, "/alt/root/", "/alt/root/run/systemd/confext"},
	}
	for _, c := range cases {
		if got := Workspace(c.class, c.root); got != c.want {
			t.Errorf("Workspace(%v, %q) = %q, want %q", c.class, c.root, got, c.want)
		}
	}
}

func TestMarkerDirName(t *testing.T) {
	if got := MarkerDirName(release.Sysext); got != ".systemd-sysext" {
		t.Errorf("sysext marker = %q", got)
	}
	if got := MarkerDirName(release.Confext); got != ".systemd-confext" {
		t.Errorf("confext marker = %q", got)
	}
}

func TestEscapeHierarchy(t *testing.T) {
	cases := map[string]string{
		"/usr":            "usr",
		"/opt":            "opt",
		"/etc":            "etc",
		"/some/deep/path": "some-deep-path",
		"/usr/":           "usr",
	}
	for in, want := range cases {
		if got := escapeHierarchy(in); got != want {
			t.Errorf("escapeHierarchy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildLowerdir(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"/a", "/b", "/c"}, "/a:/b:/c"},
		{[]string{"/run/systemd/sysext/meta/usr", "/usr"}, "/run/systemd/sysext/meta/usr:/usr"},
		// overlayfs option escaping: ':' ',' '\' get backslash-escaped
		{[]string{"/with:colon", "/plain"}, `/with\:colon:/plain`},
		{[]string{"/with,comma"}, `/with\,comma`},
		{[]string{`/with\backslash`}, `/with\\backslash`},
		{[]string{`/a:b,c\d`, "/e"}, `/a\:b\,c\\d:/e`},
		{nil, ""},
	}
	for _, c := range cases {
		if got := buildLowerdir(c.in); got != c.want {
			t.Errorf("buildLowerdir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOverlayMountFlags(t *testing.T) {
	const (
		msRdonly = 0x1
		msNosuid = 0x2
		msNodev  = 0x4
		msNoexec = 0x8
	)
	if got := overlayMountFlags(release.Sysext, false); got != msRdonly|msNodev {
		t.Errorf("sysext flags = %#x", got)
	}
	// sysext never gets noexec, regardless of the option
	if got := overlayMountFlags(release.Sysext, true); got != msRdonly|msNodev {
		t.Errorf("sysext flags (noexec opt) = %#x", got)
	}
	if got := overlayMountFlags(release.Confext, true); got != msRdonly|msNodev|msNosuid|msNoexec {
		t.Errorf("confext flags (noexec) = %#x", got)
	}
	if got := overlayMountFlags(release.Confext, false); got != msRdonly|msNodev|msNosuid {
		t.Errorf("confext flags (no noexec) = %#x", got)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	markerDir := filepath.Join(dir, "meta", "usr", ".systemd-sysext")

	names := []string{"foo", "bar-1.2", "baz"}
	origins := []originEntry{
		{Name: "foo", Path: "/var/lib/extensions/foo.raw", Type: "raw"},
		{Name: "bar-1.2", Path: "/var/lib/extensions/bar-1.2", Type: "directory"},
		{Name: "baz", Path: "/run/extensions/baz.raw", Type: "raw"},
	}
	if err := writeMarker(markerDir, names, origins); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}

	gotNames, err := readExtensionsFile(filepath.Join(markerDir, "extensions"))
	if err != nil {
		t.Fatalf("readExtensionsFile: %v", err)
	}
	if !reflect.DeepEqual(gotNames, names) {
		t.Errorf("extensions round-trip = %v, want %v", gotNames, names)
	}

	gotOrigins, err := readOriginFile(filepath.Join(markerDir, "origin"))
	if err != nil {
		t.Fatalf("readOriginFile: %v", err)
	}
	if !reflect.DeepEqual(gotOrigins, origins) {
		t.Errorf("origin round-trip = %+v, want %+v", gotOrigins, origins)
	}

	// raw content sanity: newline-delimited list ending in newline
	raw, err := os.ReadFile(filepath.Join(markerDir, "extensions"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "foo\nbar-1.2\nbaz\n" {
		t.Errorf("extensions file content = %q", raw)
	}
}

func TestCollectRawOriginPaths(t *testing.T) {
	ws := t.TempDir()
	marker := MarkerDirName(release.Sysext)
	if err := writeMarker(filepath.Join(ws, "meta", "usr", marker),
		[]string{"a", "b"},
		[]originEntry{
			{Name: "a", Path: "/x/a.raw", Type: "raw"},
			{Name: "b", Path: "/x/b", Type: "directory"},
		}); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(filepath.Join(ws, "meta", "opt", marker),
		[]string{"a"},
		[]originEntry{{Name: "a", Path: "/x/a.raw", Type: "raw"}}); err != nil {
		t.Fatal(err)
	}
	got := collectRawOriginPaths(release.Sysext, ws)
	if !reflect.DeepEqual(got, []string{"/x/a.raw"}) {
		t.Errorf("collectRawOriginPaths = %v, want [/x/a.raw] (raw only, deduped)", got)
	}
}

func TestIsMountPointNegative(t *testing.T) {
	dir := t.TempDir()

	// plain subdirectory of the same filesystem → not a mount point
	sub := filepath.Join(dir, "usr")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mp, err := isMountPoint(sub)
	if err != nil {
		t.Fatalf("isMountPoint(%s): %v", sub, err)
	}
	if mp {
		t.Errorf("plain dir %s reported as mount point", sub)
	}

	// missing path → false, no error
	mp, err = isMountPoint(filepath.Join(dir, "does-not-exist"))
	if err != nil || mp {
		t.Errorf("missing path: mp=%v err=%v, want false,nil", mp, err)
	}

	// root is always a mount point
	mp, err = isMountPoint("/")
	if err != nil || !mp {
		t.Errorf("/: mp=%v err=%v, want true,nil", mp, err)
	}
}

func TestIsMergedByUsNegative(t *testing.T) {
	root := t.TempDir()

	// hierarchy missing entirely
	merged, err := IsMergedByUs(release.Sysext, root, "/usr")
	if err != nil || merged {
		t.Errorf("missing hierarchy: merged=%v err=%v, want false,nil", merged, err)
	}

	// hierarchy exists but is not a mount point
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	merged, err = IsMergedByUs(release.Sysext, root, "/usr")
	if err != nil || merged {
		t.Errorf("plain dir: merged=%v err=%v, want false,nil", merged, err)
	}

	// even with a marker dev file present, a non-mount-point is not merged
	markerDir := filepath.Join(root, "usr", MarkerDirName(release.Sysext))
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "dev"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err = IsMergedByUs(release.Sysext, root, "/usr")
	if err != nil || merged {
		t.Errorf("marker without mount: merged=%v err=%v, want false,nil", merged, err)
	}

	// confext on the same unmerged root
	merged, err = IsMergedByUs(release.Confext, root, "/etc")
	if err != nil || merged {
		t.Errorf("confext unmerged: merged=%v err=%v, want false,nil", merged, err)
	}
}

func TestCurrentStatusUnmerged(t *testing.T) {
	root := t.TempDir()
	for _, h := range []string{"usr", "opt", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, h), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	st, err := CurrentStatus(release.Sysext, root)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if len(st) != 2 || st[0].Hierarchy != "/usr" || st[1].Hierarchy != "/opt" {
		t.Fatalf("unexpected statuses: %+v", st)
	}
	for _, s := range st {
		if s.Merged || s.Extensions != nil || s.Since != 0 {
			t.Errorf("unmerged %s should be empty: %+v", s.Hierarchy, s)
		}
	}

	st, err = CurrentStatus(release.Confext, root)
	if err != nil {
		t.Fatalf("CurrentStatus(confext): %v", err)
	}
	if len(st) != 1 || st[0].Hierarchy != "/etc" || st[0].Merged {
		t.Fatalf("unexpected confext statuses: %+v", st)
	}
}

func TestMergedExtensionsUnmerged(t *testing.T) {
	root := t.TempDir()
	names, err := MergedExtensions(release.Sysext, root, "/usr")
	if err != nil {
		t.Fatalf("MergedExtensions: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("unmerged hierarchy returned extensions: %v", names)
	}
}

func TestMergeNoImagesIsNoop(t *testing.T) {
	root := t.TempDir()
	if err := Merge(release.Sysext, nil, MergeOptions{Root: root}); err != nil {
		t.Fatalf("Merge with no images should be a no-op, got %v", err)
	}
	if _, err := os.Stat(Workspace(release.Sysext, root)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no-op merge must not create the workspace")
	}
}

func TestUnmergeIdempotentOnUnmergedRoot(t *testing.T) {
	root := t.TempDir()
	if err := Unmerge(release.Sysext, root); err != nil {
		t.Fatalf("Unmerge on clean root: %v", err)
	}
	// stale workspace with markers but no mounts → removed, no error
	ws := Workspace(release.Sysext, root)
	marker := MarkerDirName(release.Sysext)
	if err := writeMarker(filepath.Join(ws, "meta", "usr", marker),
		[]string{"a"},
		[]originEntry{{Name: "a", Path: "/x/a", Type: "directory"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "extensions", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "overlay", "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Unmerge(release.Sysext, root); err != nil {
		t.Fatalf("Unmerge with stale workspace: %v", err)
	}
	if _, err := os.Stat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace not removed by Unmerge")
	}
	// and again, fully idempotent
	if err := Unmerge(release.Sysext, root); err != nil {
		t.Fatalf("second Unmerge: %v", err)
	}
}

func TestDirNonEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(full, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirNonEmpty(empty) {
		t.Error("empty dir reported non-empty")
	}
	if !dirNonEmpty(full) {
		t.Error("non-empty dir reported empty")
	}
	if dirNonEmpty(file) {
		t.Error("regular file reported as non-empty dir")
	}
	if dirNonEmpty(filepath.Join(dir, "missing")) {
		t.Error("missing path reported as non-empty dir")
	}
}

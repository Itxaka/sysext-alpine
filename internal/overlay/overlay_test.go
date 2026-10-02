package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

func TestParseHierarchies(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"/usr", []string{"/usr"}},
		{"/usr:/opt:/srv/a/b", []string{"/usr", "/opt", "/srv/a/b"}},
		{"/usr/", []string{"/usr/"}},
		{"/usr:/usr", []string{"/usr", "/usr"}},
		{"", nil},
		{"usr", nil},
		{"/usr:opt", nil},
		{"/", nil},
		{"/usr:/", nil},
		{"/usr//local", nil},
		{"/usr/../etc", nil},
		{"/usr/./etc", nil},
		{"/usr:", nil},
		{":/usr", nil},
		{":", nil},
	}
	for _, c := range cases {
		got, err := parseHierarchies(c.in)
		if c.want == nil {
			if err == nil || !errors.Is(err, unix.EINVAL) {
				t.Errorf("parseHierarchies(%q) = %v, %v; want EINVAL", c.in, got, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseHierarchies(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

func TestHierarchies(t *testing.T) {
	got, err := Hierarchies(release.Sysext)
	if err != nil || !reflect.DeepEqual(got, []string{"/usr", "/opt"}) {
		t.Errorf("sysext defaults = %v, %v", got, err)
	}
	got, err = Hierarchies(release.Confext)
	if err != nil || !reflect.DeepEqual(got, []string{"/etc"}) {
		t.Errorf("confext defaults = %v, %v", got, err)
	}

	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "/usr:/srv")
	if got, err := Hierarchies(release.Sysext); err != nil || !reflect.DeepEqual(got, []string{"/usr", "/srv"}) {
		t.Errorf("sysext override = %v, %v", got, err)
	}
	if got, err := Hierarchies(release.Confext); err != nil || !reflect.DeepEqual(got, []string{"/etc"}) {
		t.Errorf("sysext override leaked into confext: %v, %v", got, err)
	}
	for _, bad := range []string{"", "relative", "/usr:"} {
		t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", bad)
		if got, err := Hierarchies(release.Sysext); err == nil || !strings.Contains(err.Error(), "failed to determine sysext hierarchies") {
			t.Errorf("Hierarchies with %q = %v, %v; want an error", bad, got, err)
		}
	}
	t.Setenv("SYSTEMD_CONFEXT_HIERARCHIES", "/etc:/srv/conf")
	if got, err := Hierarchies(release.Confext); err != nil || !reflect.DeepEqual(got, []string{"/etc", "/srv/conf"}) {
		t.Errorf("confext override = %v, %v", got, err)
	}
}

func TestAlreadyMergedError(t *testing.T) {
	err := error(&AlreadyMergedError{Hierarchy: "/usr"})
	if err.Error() != "Hierarchy '/usr' is already merged." {
		t.Errorf("message = %q", err.Error())
	}
	if !errors.Is(err, ErrAlreadyMerged) {
		t.Error("AlreadyMergedError does not match ErrAlreadyMerged")
	}
}

func TestWorkspace(t *testing.T) {
	dir := withRuntimeDir(t)
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		class release.Class
		root  string
		want  string
	}{
		{release.Sysext, "", "sysext"},
		{release.Sysext, "/", "sysext"},
		{release.Confext, "", "confext"},
	} {
		got, err := Workspace(c.class, c.root)
		if err != nil || got != filepath.Join(dir, c.want) {
			t.Errorf("Workspace(%v, %q) = %q, %v", c.class, c.root, got, err)
		}
	}
	ws, err := Workspace(release.Sysext, root)
	if err != nil || filepath.Dir(ws) != dir || !strings.HasPrefix(filepath.Base(ws), "sysext.") {
		t.Errorf("Workspace for a foreign root = %q, %v; want a sysext.* directory in %s", ws, err, dir)
	}
	if viaLink, _ := Workspace(release.Sysext, link); viaLink != ws {
		t.Errorf("Workspace through a symlinked root = %q, want %q", viaLink, ws)
	}
	if other, _ := Workspace(release.Confext, root); other == ws {
		t.Error("sysext and confext share a workspace")
	}
	if _, err := Workspace(release.Sysext, filepath.Join(root, "missing")); err == nil {
		t.Error("Workspace accepted a missing root")
	}
}

func TestMarkerNames(t *testing.T) {
	if MarkerDirName(release.Sysext) != ".systemd-sysext" || MarkerDirName(release.Confext) != ".systemd-confext" {
		t.Error("marker directory names")
	}
	if listFileName(release.Sysext) != "extensions" || listFileName(release.Confext) != "confexts" {
		t.Error("list file names")
	}
	if classIdentifier(release.Sysext) != "sysext" || classIdentifier(release.Confext) != "confext" {
		t.Error("class identifiers")
	}
}

func TestIsMergedByUsNegative(t *testing.T) {
	root := t.TempDir()
	if merged, err := IsMergedByUs(release.Sysext, root, "/usr"); err != nil || merged {
		t.Errorf("missing hierarchy: %v, %v", merged, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/.systemd-sysext"), 0o755); err != nil {
		t.Fatal(err)
	}
	if merged, err := IsMergedByUs(release.Sysext, root, "/usr"); err != nil || merged {
		t.Errorf("plain directory: %v, %v", merged, err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/.systemd-sysext/dev"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if merged, err := IsMergedByUs(release.Sysext, root, "/usr"); err != nil || merged {
		t.Errorf("marker on a directory that is no mount point: %v, %v", merged, err)
	}
	if merged, err := isOurMountPoint(release.Sysext, "/"); err != nil || merged {
		t.Errorf("/ without a marker: %v, %v", merged, err)
	}
}

func TestCurrentStatusUnmerged(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := CurrentStatus(release.Sysext, root)
	if err != nil || !reflect.DeepEqual(st, []Status{{Hierarchy: "/usr"}}) {
		t.Errorf("CurrentStatus = %+v, %v; want /usr unmerged and /opt (missing) skipped", st, err)
	}
	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "bogus")
	if _, err := CurrentStatus(release.Sysext, root); err == nil {
		t.Error("CurrentStatus accepted an invalid hierarchy list")
	}
}

func TestUnmergeNothingMerged(t *testing.T) {
	withRuntimeDir(t)
	root := t.TempDir()
	if got, err := Unmerge(release.Sysext, root); err != nil || got != nil {
		t.Fatalf("Unmerge on a clean root = %v, %v", got, err)
	}
	ws, err := Workspace(release.Sysext, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "meta/usr/.systemd-sysext"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := Unmerge(release.Sysext, root); err != nil || got != nil {
		t.Fatalf("Unmerge with a stale workspace = %v, %v", got, err)
	}
	if _, err := os.Lstat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale workspace not removed")
	}
}

func TestMergeWithoutImages(t *testing.T) {
	withRuntimeDir(t)
	root := t.TempDir()
	for _, refresh := range []bool{false, true} {
		out, err := run(release.Sysext, nil, MergeOptions{Root: root, NoExec: NoExecDefault}, refresh)
		if err != nil || !reflect.DeepEqual(out, Outcome{Result: NothingFound}) {
			t.Errorf("refresh=%v without images = %+v, %v", refresh, out, err)
		}
	}
	ws, _ := Workspace(release.Sysext, root)
	if _, err := os.Lstat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Error("a merge without images created the workspace")
	}
	if _, err := Merge(release.Sysext, nil, MergeOptions{Root: root, Mutable: "bogus"}); err == nil {
		t.Error("Merge accepted an invalid mutable mode")
	}
}

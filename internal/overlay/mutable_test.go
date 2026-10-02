package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNormalizeMutableMode(t *testing.T) {
	for in, want := range map[string]string{
		"": "no", "no": "no", "auto": "auto", "yes": "yes", "import": "import",
		"ephemeral": "ephemeral", "ephemeral-import": "ephemeral-import",
	} {
		if got, err := normalizeMutableMode(in); err != nil || got != want {
			t.Errorf("normalizeMutableMode(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"bogus", "true", "help", "Yes"} {
		if _, err := normalizeMutableMode(bad); err == nil {
			t.Errorf("normalizeMutableMode(%q) accepted", bad)
		}
	}
}

func modeOf(t *testing.T, p string) uint32 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Mode & 0o777
}

func TestResolveMutableDir(t *testing.T) {
	root := t.TempDir()
	ws := t.TempDir()
	routing := filepath.Join(root, "var/lib/extensions.mutable")

	for _, mode := range []string{"no", "auto", "import", "ephemeral-import"} {
		dir, err := resolveMutableDir(mode, root, "/usr", 0o755, filepath.Join(ws, mode))
		if mode == "ephemeral-import" {
			if err != nil || dir != filepath.Join(ws, mode, "usr") {
				t.Errorf("%s: %q, %v", mode, dir, err)
			}
			continue
		}
		if err != nil || dir != "" {
			t.Errorf("%s without a mutable directory = %q, %v", mode, dir, err)
		}
	}
	if _, err := os.Stat(routing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a mode other than yes created the mutable directory")
	}

	old := syscall.Umask(0o077)
	dir, err := resolveMutableDir("yes", root, "/srv/a/b", 0o751, "")
	syscall.Umask(old)
	if err != nil || dir != filepath.Join(routing, "srv.a.b") {
		t.Fatalf("yes = %q, %v", dir, err)
	}
	if got := modeOf(t, dir); got != 0o751 {
		t.Errorf("created mutable directory mode = %o, want the hierarchy mode 751", got)
	}
	if dir, err := resolveMutableDir("auto", root, "/srv/a/b", 0o751, ""); err != nil || dir != filepath.Join(routing, "srv.a.b") {
		t.Errorf("auto with a mutable directory = %q, %v", dir, err)
	}
	if _, err := resolveMutableDir("auto", root, "/srv/a/b", 0o755, ""); err == nil || !strings.Contains(err.Error(), "has mode 0751, ought to have mode 0755") {
		t.Errorf("auto with a mode mismatch = %v", err)
	}
	if _, err := resolveMutableDir("yes", root, "/srv/a/b", 0o755, ""); err == nil {
		t.Error("yes accepted a mode mismatch")
	}
	if dir, err := resolveMutableDir("ephemeral", root, "/usr", 0o700, ws); err != nil || dir != filepath.Join(ws, "usr") || modeOf(t, dir) != 0o700 {
		t.Errorf("ephemeral = %q, %v", dir, err)
	}

	if err := os.Symlink("/elsewhere", filepath.Join(routing, "etc")); err != nil {
		t.Fatal(err)
	}
	if dir, err := resolveMutableDir("import", root, "/etc", 0o755, ""); err != nil || dir != "" {
		t.Errorf("dangling absolute link must resolve inside the root: %q, %v", dir, err)
	}
}

func TestImportedMutableDir(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "usr")
	routing := filepath.Join(root, "var/lib/extensions.mutable")
	for _, d := range []string{host, routing} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if dir, err := importedMutableDir("yes", root, "/usr", host, host); err != nil || dir != "" {
		t.Errorf("yes imports nothing: %q, %v", dir, err)
	}
	if _, err := importedMutableDir("import", root, "/usr", host, host); !errors.Is(err, unix.ELOOP) {
		t.Errorf("importing the hierarchy into itself = %v, want ELOOP", err)
	}
	if err := os.Symlink("../../../usr", filepath.Join(routing, "usr")); err != nil {
		t.Fatal(err)
	}
	if _, err := importedMutableDir("ephemeral-import", root, "/usr", host, "/ws/usr"); !errors.Is(err, unix.ELOOP) {
		t.Errorf("ephemeral-import of the hierarchy itself = %v, want ELOOP", err)
	}
	if err := os.Remove(filepath.Join(routing, "usr")); err != nil {
		t.Fatal(err)
	}
	if dir, err := importedMutableDir("ephemeral-import", root, "/usr", host, "/ws/usr"); err != nil || dir != "" {
		t.Errorf("ephemeral-import without a mutable directory = %q, %v", dir, err)
	}
	if err := os.Mkdir(filepath.Join(routing, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if dir, err := importedMutableDir("ephemeral-import", root, "/usr", host, "/ws/usr"); err != nil || dir != filepath.Join(routing, "usr") {
		t.Errorf("ephemeral-import = %q, %v", dir, err)
	}
}

func TestHostAsLowerDir(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "usr")
	if err := os.Mkdir(host, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "routing")
	if err := os.Symlink(host, link); err != nil {
		t.Fatal(err)
	}
	if use, err := hostAsLowerDir("no", "", ""); err != nil || use {
		t.Errorf("missing host = %v, %v", use, err)
	}
	if use, err := hostAsLowerDir("no", host, ""); err != nil || use {
		t.Errorf("empty host = %v, %v", use, err)
	}
	if err := os.WriteFile(filepath.Join(host, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode, mutable string
		want          bool
	}{
		{"no", "", true},
		{"yes", filepath.Join(root, "other"), true},
		{"yes", link, false},
		{"auto", host, false},
		{"import", host, true},
		{"ephemeral-import", host, true},
	} {
		if c.mutable != "" && c.mutable != host && c.mutable != link {
			if err := os.MkdirAll(c.mutable, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if use, err := hostAsLowerDir(c.mode, host, c.mutable); err != nil || use != c.want {
			t.Errorf("hostAsLowerDir(%s, %s) = %v, %v; want %v", c.mode, c.mutable, use, err, c.want)
		}
	}
}

func TestUpperAndWorkDir(t *testing.T) {
	dir := t.TempDir()
	if upper, err := upperDir("import", dir); err != nil || upper != "" {
		t.Errorf("import has no upper dir: %q, %v", upper, err)
	}
	if upper, err := upperDir("yes", ""); err != nil || upper != "" {
		t.Errorf("no mutable directory: %q, %v", upper, err)
	}
	if upper, err := upperDir("yes", dir); err != nil || upper != dir {
		t.Errorf("upperDir = %q, %v", upper, err)
	}
	if work, err := workDirFor("/srv/a", filepath.Join(dir, "srv.a")); err == nil {
		t.Errorf("workDirFor a missing upper dir = %q", work)
	}
	if err := os.Mkdir(filepath.Join(dir, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if work, err := workDirFor("/usr", filepath.Join(dir, "usr")); err != nil || work != filepath.Join(dir, ".systemd-usr-workdir") {
		t.Errorf("workDirFor = %q, %v", work, err)
	}
	if _, err := workDirFor("/usr", "/proc"); !errors.Is(err, unix.EXDEV) {
		t.Errorf("workDirFor across filesystems = %v, want EXDEV", err)
	}
}

func TestDirIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if empty, err := dirIsEmpty(dir); err != nil || !empty {
		t.Errorf("empty dir = %v, %v", empty, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if empty, err := dirIsEmpty(dir); err != nil || empty {
		t.Errorf("dir with a hidden file = %v, %v", empty, err)
	}
	if _, err := dirIsEmpty(filepath.Join(dir, ".hidden")); err == nil {
		t.Error("dirIsEmpty of a file succeeded")
	}
}

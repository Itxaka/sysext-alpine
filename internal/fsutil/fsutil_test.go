package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestChase(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ln := func(target, p string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(root, p)); err != nil {
			t.Fatal(err)
		}
	}
	mk("var/opt")
	mk("usr/lib")
	ln("var/opt", "opt")
	ln("/var/opt", "absopt")
	ln("../../../../usr", "escape")
	ln("loop1", "loop2")
	ln("loop2", "loop1")
	ln("missing/dir", "dangling")
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path  string
		flags ChaseFlags
		want  string
		err   error
	}{
		{"/usr", 0, "/usr", nil},
		{"/opt", 0, "/var/opt", nil},
		{"/absopt", 0, "/var/opt", nil},
		{"/escape/lib", 0, "/usr/lib", nil},
		{"/../../usr/./lib/", 0, "/usr/lib", nil},
		{"/", 0, "/", nil},
		{"", 0, "/", nil},
		{"/loop1", 0, "", syscall.ELOOP},
		{"/nope", 0, "", syscall.ENOENT},
		{"/nope/a/b", ChaseNonexistent, "/nope/a/b", nil},
		{"/opt/new/x", ChaseNonexistent, "/var/opt/new/x", nil},
		{"/dangling", ChaseNonexistent, "/missing/dir", nil},
		{"/file/x", 0, "", syscall.ENOTDIR},
	}
	for _, tt := range tests {
		got, err := Chase(root, tt.path, tt.flags)
		if tt.err != nil {
			if !errors.Is(err, tt.err) {
				t.Errorf("Chase(%q) err = %v, want %v", tt.path, err, tt.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Chase(%q) unexpected error: %v", tt.path, err)
			continue
		}
		if want := filepath.Join(root, tt.want); got != want {
			t.Errorf("Chase(%q) = %q, want %q", tt.path, got, want)
		}
	}
}

func TestChaseHostRoot(t *testing.T) {
	got, err := Chase("", "/proc/self/..", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/proc" {
		t.Errorf("got %q, want /proc", got)
	}
}

func TestIsMountPoint(t *testing.T) {
	for path, want := range map[string]bool{"/": true, "/proc": true} {
		got, err := IsMountPoint(path)
		if err != nil || got != want {
			t.Errorf("IsMountPoint(%q) = %v, %v; want %v", path, got, err, want)
		}
	}
	dir := t.TempDir()
	if got, err := IsMountPoint(dir); err != nil || got {
		t.Errorf("IsMountPoint(tempdir) = %v, %v", got, err)
	}
	if got, err := IsMountPoint(filepath.Join(dir, "missing")); err != nil || got {
		t.Errorf("IsMountPoint(missing) = %v, %v", got, err)
	}
}

func TestParseMountInfo(t *testing.T) {
	in := `22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
30 22 0:40 / /run/a\040b rw,relatime shared:3 master:1 - tmpfs tmp\134fs rw,mode=755
31 30 7:0 /sub /usr ro,nodev,relatime - overlay overlay ro,lowerdir=/a:/b
`
	mounts, err := ParseMountInfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 3 {
		t.Fatalf("got %d mounts", len(mounts))
	}
	m := mounts[1]
	if m.ID != 30 || m.ParentID != 22 || m.Major != 0 || m.Minor != 40 || m.MountPoint != "/run/a b" ||
		m.FSType != "tmpfs" || m.Source != `tmp\fs` || !m.Shared() || len(m.Optional) != 2 {
		t.Errorf("unexpected entry %+v", m)
	}
	if mounts[2].Shared() || mounts[2].Root != "/sub" || mounts[2].SuperOpts != "ro,lowerdir=/a:/b" {
		t.Errorf("unexpected entry %+v", mounts[2])
	}
	if _, err := ParseMountInfo(strings.NewReader("garbage\n")); err == nil {
		t.Error("expected error for malformed line")
	}
	below := MountsBelow(mounts, "/run")
	if len(below) != 1 || below[0].ID != 30 {
		t.Errorf("MountsBelow(/run) = %+v", below)
	}
	if len(MountsBelow(mounts, "/run/a b")) != 0 {
		t.Error("MountsBelow must exclude the prefix itself")
	}
}

func TestIsBelow(t *testing.T) {
	for _, tt := range []struct {
		path, dir string
		want      bool
	}{
		{"/usr", "/usr", true},
		{"/usr/lib", "/usr", true},
		{"/usrx", "/usr", false},
		{"/", "/usr", false},
		{"/usr/..x", "/usr", true},
		{"/anything", "/", true},
	} {
		if got := IsBelow(tt.path, tt.dir); got != tt.want {
			t.Errorf("IsBelow(%q, %q) = %v", tt.path, tt.dir, got)
		}
	}
}

func TestReadMountInfo(t *testing.T) {
	mounts, err := ReadMountInfo()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mounts {
		if m.MountPoint == "/" {
			found = true
		}
	}
	if !found {
		t.Error("no / entry in /proc/self/mountinfo")
	}
}

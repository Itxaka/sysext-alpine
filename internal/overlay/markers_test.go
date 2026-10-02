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

func TestDevnum(t *testing.T) {
	if got := formatDevnum(unix.Mkdev(0, 52)); got != "0:52" {
		t.Errorf("formatDevnum = %q", got)
	}
	if got := formatDevnum(unix.Mkdev(259, 1048575)); got != "259:1048575" {
		t.Errorf("formatDevnum = %q", got)
	}
	cases := []struct {
		in   string
		want uint64
		err  error
	}{
		{"0:52", unix.Mkdev(0, 52), nil},
		{"8:2", unix.Mkdev(8, 2), nil},
		{"4095:1048575", unix.Mkdev(4095, 1048575), nil},
		{"52", 52, nil},
		{"4096:0", 0, unix.ERANGE},
		{"0:1048576", 0, unix.ERANGE},
		{":1", 0, unix.EINVAL},
		{"1:", 0, unix.EINVAL},
		{"a:b", 0, unix.EINVAL},
		{"-1:2", 0, unix.EINVAL},
		{"1:2:3", 0, unix.EINVAL},
		{" 1:2", 0, unix.EINVAL},
		{"", 0, unix.EINVAL},
	}
	for _, c := range cases {
		got, err := parseDevnum(c.in)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("parseDevnum(%q) = %d, %v; want %v", c.in, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseDevnum(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, dev := range []uint64{unix.Mkdev(0, 1), unix.Mkdev(7, 0), unix.Mkdev(253, 12345)} {
		if got, err := parseDevnum(formatDevnum(dev)); err != nil || got != dev {
			t.Errorf("devnum round trip of %d = %d, %v", dev, got, err)
		}
	}
}

func TestList(t *testing.T) {
	if got := formatList(nil); got != "" {
		t.Errorf("empty list = %q", got)
	}
	if got := formatList([]string{"bar", "foo"}); got != "bar\nfoo\n" {
		t.Errorf("list = %q", got)
	}
	if got := parseList("bar\nfoo\n"); !reflect.DeepEqual(got, []string{"bar", "foo"}) {
		t.Errorf("parseList = %v", got)
	}
	if got := parseList(""); got != nil {
		t.Errorf("parseList(\"\") = %v", got)
	}
}

func TestCEscape(t *testing.T) {
	cases := map[string]string{
		"var/lib/extensions.mutable/.systemd-usr-workdir": "var/lib/extensions.mutable/.systemd-usr-workdir",
		"a b":     "a b",
		"a\nb":    `a\nb`,
		`back\sl`: `back\\sl`,
		"q\"'":    `q\"\'`,
		"\t\x01":  `\t\001`,
		"é":       `\303\251`,
		"\x7f":    `\177`,
	}
	for in, want := range cases {
		got := cescape(in)
		if got != want {
			t.Errorf("cescape(%q) = %q, want %q", in, got, want)
		}
		if back, err := cunescape(got); err != nil || back != in {
			t.Errorf("cunescape(%q) = %q, %v; want %q", got, back, err, in)
		}
	}
	for in, want := range map[string]string{`\s`: " ", `\x41`: "A", `é`: "é", `\U0001F600`: "😀"} {
		if got, err := cunescape(in); err != nil || got != want {
			t.Errorf("cunescape(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`\`, `\q`, `\x0`, `\x00`, `\000`, `\777`, `\u12`, `\ud800`} {
		if got, err := cunescape(bad); err == nil {
			t.Errorf("cunescape(%q) = %q, want an error", bad, got)
		}
	}
}

func TestPathIsNormalized(t *testing.T) {
	cases := map[string]bool{
		"var/lib/x": true,
		"/usr":      true,
		"/usr/":     true,
		"a":         true,
		"":          false,
		".":         false,
		"./a":       false,
		"a/.":       false,
		"a/./b":     false,
		"..":        false,
		"../a":      false,
		"a/../b":    false,
		"a//b":      false,
		"//usr":     false,
	}
	cases["a/"+strings.Repeat("x", 256)] = false
	for p, want := range cases {
		if got := pathIsNormalized(p); got != want {
			t.Errorf("pathIsNormalized(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSinglePathComponent(t *testing.T) {
	for in, want := range map[string]string{
		"/usr":         "usr",
		"/opt/":        "opt",
		"/foo/bar/baz": "foo.bar.baz",
		"/srv/a/b/":    "srv.a.b",
	} {
		if got := singlePathComponent(in); got != want {
			t.Errorf("singlePathComponent(%q) = %q, want %q", in, got, want)
		}
	}
	if got := workDirName("/srv/a/b"); got != ".systemd-srv.a.b-workdir" {
		t.Errorf("workDirName = %q", got)
	}
}

func TestRecordedWorkDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "usr")
	marker := filepath.Join(dir, ".systemd-sysext")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var/lib/extensions.mutable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere", filepath.Join(root, "var/lib/escape")); err != nil {
		t.Fatal(err)
	}
	if got, err := recordedWorkDir(release.Sysext, root, dir, "/usr"); err != nil || got != "" {
		t.Errorf("without marker = %q, %v", got, err)
	}
	cases := []struct {
		content, want string
		ok            bool
	}{
		{"var/lib/extensions.mutable/.systemd-usr-workdir\n", filepath.Join(root, "var/lib/extensions.mutable/.systemd-usr-workdir"), true},
		{`var/lib/extensions\x2emutable/.systemd-usr-workdir`, filepath.Join(root, "var/lib/extensions.mutable/.systemd-usr-workdir"), true},
		{"var/lib/escape/.systemd-usr-workdir\n", "", true},
		{"/var/lib/extensions.mutable/.systemd-usr-workdir\n", "", false},
		{"var/lib/../../.systemd-usr-workdir\n", "", false},
		{"var//lib/.systemd-usr-workdir\n", "", false},
		{"var/lib/extensions.mutable/.usr-workdir\n", "", false},
		{"var/lib/extensions.mutable/precious\n", "", false},
		{`var/lib/\q/.systemd-usr-workdir`, "", false},
	}
	for _, c := range cases {
		if err := os.WriteFile(filepath.Join(marker, "work_dir"), []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := recordedWorkDir(release.Sysext, root, dir, "/usr")
		if c.ok != (err == nil) || got != c.want {
			t.Errorf("work_dir %q = %q, %v; want %q (ok=%v)", c.content, got, err, c.want, c.ok)
		}
	}
}

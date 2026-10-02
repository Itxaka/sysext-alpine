package overlay

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestOriginGolden compares with origin files written by systemd-sysext 262.
func TestOriginGolden(t *testing.T) {
	dirs := &origin{
		Mode:  "no",
		Names: []string{"bar", "foo"},
		Images: map[string]imageIdentity{
			"bar": {Path: "/var/lib/extensions/bar", MountID: 2147519467, HandleType: 248,
				Handle: mustHex(t, "00000000fb1d0401eaccc778d23b4f7fbf96962de04581742500060123a8110b"), CrTime: 1790934805978956},
			"foo": {Path: "/var/lib/extensions/foo", MountID: 2147519467, HandleType: 248,
				Handle: mustHex(t, "00000000fb1d0401eaccc778d23b4f7fbf96962de04581741a00060133b2ac0c"), CrTime: 1790934805972956},
		},
	}
	want := `{
	"mutable" : {
		"mode" : "no"
	},
	"extensions" : {
		"bar" : {
			"path" : "/var/lib/extensions/bar",
			"onMountId" : 2147519467,
			"fileHandle" : {
				"type" : 248,
				"handle" : "00000000fb1d0401eaccc778d23b4f7fbf96962de04581742500060123a8110b"
			},
			"crtime" : 1790934805978956,
			"mtime" : 0
		},
		"foo" : {
			"path" : "/var/lib/extensions/foo",
			"onMountId" : 2147519467,
			"fileHandle" : {
				"type" : 248,
				"handle" : "00000000fb1d0401eaccc778d23b4f7fbf96962de04581741a00060133b2ac0c"
			},
			"crtime" : 1790934805972956,
			"mtime" : 0
		}
	}
}
`
	if got := dirs.String(); got != want {
		t.Errorf("directory images:\n%s\nwant:\n%s", got, want)
	}

	mutable := &origin{
		Mode: "yes",
		MutableDirs: []mutableDir{
			{"/usr", "/var/lib/extensions.mutable/usr"},
			{"/opt", "/var/lib/extensions.mutable/opt"},
		},
	}
	want = `{
	"mutable" : {
		"mode" : "yes",
		"mutableDirs" : {
			"/usr" : "/var/lib/extensions.mutable/usr",
			"/opt" : "/var/lib/extensions.mutable/opt"
		}
	}
}
`
	if got := mutable.String(); got != want {
		t.Errorf("mutable without images:\n%s\nwant:\n%s", got, want)
	}

	raw := &origin{
		Mode:         "no",
		MountOptions: "xino=off",
		Names:        []string{"q", "v"},
		Images: map[string]imageIdentity{
			"q": {Path: "/var/lib/extensions/q.raw", MountID: 7, Inode: 42, CrTime: 1, MTime: 2},
			"v": {Path: "/var/lib/extensions/v.raw", VerityHash: "abcd", MountID: 9, Inode: 1},
		},
	}
	want = `{
	"mutable" : {
		"mode" : "no"
	},
	"mountOptions" : "xino=off",
	"extensions" : {
		"q" : {
			"path" : "/var/lib/extensions/q.raw",
			"onMountId" : 7,
			"inode" : 42,
			"crtime" : 1,
			"mtime" : 2
		},
		"v" : {
			"path" : "/var/lib/extensions/v.raw",
			"verityHash" : "abcd"
		}
	}
}
`
	if got := raw.String(); got != want {
		t.Errorf("raw images:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatJSONString(t *testing.T) {
	var b strings.Builder
	formatJSONString(&b, "a\"b\\c\nd\te\x01\x7fé")
	if got := b.String(); got != `"a\"b\\c\nd\te\u0001`+"\x7f"+`é"` {
		t.Errorf("formatJSONString = %s", got)
	}
}

func TestOriginEqual(t *testing.T) {
	base := `{"mutable":{"mode":"no"},"extensions":{"a":{"path":"/a","mtime":1},"b":{"path":"/b","mtime":2}}}`
	for _, c := range []struct {
		other string
		want  bool
	}{
		{"{\n\t\"extensions\" : {\"b\":{\"mtime\":2,\"path\":\"/b\"},\"a\":{\"path\":\"/a\",\"mtime\":1}},\"mutable\":{\"mode\":\"no\"}\n}\n", true},
		{`{"mutable":{"mode":"no"},"extensions":{"a":{"path":"/a","mtime":1},"b":{"path":"/b","mtime":3}}}`, false},
		{`{"mutable":{"mode":"yes"},"extensions":{"a":{"path":"/a","mtime":1},"b":{"path":"/b","mtime":2}}}`, false},
		{`{"mutable":{"mode":"no"},"mountOptions":"x","extensions":{"a":{"path":"/a","mtime":1},"b":{"path":"/b","mtime":2}}}`, false},
		{`{"mutable":{"mode":"no"},"extensions":{"a":{"path":"/a","mtime":1}}}`, false},
		{`[{"name":"a","path":"/a","type":"raw"}]`, false},
	} {
		got, err := originEqual(base, c.other)
		if err != nil || got != c.want {
			t.Errorf("originEqual(%s) = %v, %v; want %v", c.other, got, err, c.want)
		}
	}
	if _, err := originEqual(base, "{broken"); err == nil {
		t.Error("originEqual accepted a broken old origin")
	}
	big := `{"inode":18446744073709551615}`
	if got, err := originEqual(big, `{"inode":18446744073709551614}`); err != nil || got {
		t.Errorf("large numbers compared equal: %v, %v", got, err)
	}
}

func TestPathWithoutRoot(t *testing.T) {
	for _, c := range []struct {
		path  string
		roots []string
		want  string
	}{
		{"/var/lib/extensions/a", nil, "/var/lib/extensions/a"},
		{"/var/lib/extensions/a", []string{"/"}, "/var/lib/extensions/a"},
		{"/x/var/lib/extensions/a", []string{"/x"}, "/var/lib/extensions/a"},
		{"/xy/var/a", []string{"/x"}, "/xy/var/a"},
		{"/real/var/a", []string{"/link", "/real"}, "/var/a"},
	} {
		if got := pathWithoutRoot(c.path, c.roots...); got != c.want {
			t.Errorf("pathWithoutRoot(%q, %v) = %q, want %q", c.path, c.roots, got, c.want)
		}
	}
}

func TestIdentify(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.raw")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(f, &st); err != nil {
		t.Fatal(err)
	}
	img := discover.Image{Name: "a", Path: f, Type: discover.TypeRaw, CrTime: 11, MTime: 22}
	id, err := identify(img, "/var/lib/extensions/a.raw")
	if err != nil {
		t.Fatal(err)
	}
	if id.Path != "/var/lib/extensions/a.raw" || id.CrTime != 11 || id.MTime != 22 || id.Inode != st.Ino || id.MountID == 0 {
		t.Errorf("identify = %+v", id)
	}
	again, err := identify(img, id.Path)
	if err != nil || again.MountID != id.MountID || string(again.Handle) != string(id.Handle) {
		t.Errorf("identify is not stable: %+v vs %+v (%v)", again, id, err)
	}
	if _, err := identify(discover.Image{Path: filepath.Join(dir, "missing")}, "/x"); err == nil {
		t.Error("identify of a missing image succeeded")
	}
}

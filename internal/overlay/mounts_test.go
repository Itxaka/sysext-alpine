package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

const testMountInfo = `22 1 0:20 / / rw shared:1 - ext4 /dev/root rw
30 22 0:30 / /usr rw shared:2 - overlay sysext ro
31 30 0:31 / /usr/local rw shared:3 - tmpfs tmpfs rw
32 31 0:32 / /usr/local/nested rw - tmpfs tmpfs rw
33 30 0:33 / /usr/share/with\040space rw - tmpfs tmpfs rw
34 30 0:34 /.systemd-sysext /usr/.systemd-sysext ro - overlay sysext ro
35 22 0:35 / /usr2 rw - tmpfs tmpfs rw
36 30 0:36 / /usr/src rw - tmpfs tmpfs rw
37 36 0:37 / /usr/src rw - tmpfs tmpfs rw
38 30 0:38 / /usr/hidden rw - tmpfs tmpfs rw
39 30 0:39 / /usr/local-x rw - tmpfs tmpfs rw
40 22 0:40 / /run/systemd/sysext rw - tmpfs sysext rw
`

func TestSelectSubmounts(t *testing.T) {
	mounts, err := fsutil.ParseMountInfo(strings.NewReader(testMountInfo))
	if err != nil {
		t.Fatal(err)
	}
	visible := func(m fsutil.MountInfo) bool { return m.ID != 36 && m.ID != 38 }
	skip := func(p string) bool { return p == "/usr/.systemd-sysext" }
	got := selectSubmounts(mounts, "/usr", skip, visible)
	want := []string{"/usr/local", "/usr/local-x", "/usr/share/with space", "/usr/src"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selectSubmounts(/usr) = %q, want %q", got, want)
	}
	if got := selectSubmounts(mounts, "/usr/local", nil, visible); !reflect.DeepEqual(got, []string{"/usr/local/nested"}) {
		t.Errorf("selectSubmounts(/usr/local) = %q", got)
	}
	if got := selectSubmounts(mounts, "/", belowWorkspace("/run/systemd/sysext", "/"), visible); !reflect.DeepEqual(got, []string{"/usr", "/usr2"}) {
		t.Errorf("selectSubmounts(/) = %q", got)
	}
	if skip := belowWorkspace("/run/systemd/sysext", "/run/systemd/sysext/overlay/usr"); skip != nil {
		t.Error("mounts below a staged overlay must all be carried along")
	}
	if got := selectSubmounts(mounts, "/opt", nil, visible); got != nil {
		t.Errorf("selectSubmounts(/opt) = %q", got)
	}
}

func TestComparePaths(t *testing.T) {
	if comparePaths("/usr/local/sub", "/usr/local-x") >= 0 {
		t.Error("a directory's children must sort before its siblings with a common prefix")
	}
	if comparePaths("/usr/local", "/usr/local/sub") >= 0 || comparePaths("/a", "/a") != 0 {
		t.Error("comparePaths ordering")
	}
}

func TestMangleMountOptions(t *testing.T) {
	cases := []struct {
		opts      string
		flags     uintptr
		wantFlags uintptr
		wantData  string
	}{
		{"lowerdir=/a:/b", unix.MS_RDONLY, unix.MS_RDONLY, "lowerdir=/a:/b"},
		{"lowerdir=/a,redirect_dir=on,noatime,metacopy=off,index=off", unix.MS_NODEV, unix.MS_NODEV | unix.MS_NOATIME, "lowerdir=/a,redirect_dir=on,metacopy=off,index=off"},
		{"lowerdir=/a,rw,exec,x-foo,X-Bar=1,defaults", unix.MS_RDONLY | unix.MS_NOEXEC, 0, "lowerdir=/a"},
		{"lowerdir=/a,nosuid,nodev,noexec,ro,xino=off", 0, unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_RDONLY, "lowerdir=/a,xino=off"},
		{`lowerdir=/with\,comma:/b,ro`, 0, unix.MS_RDONLY, `lowerdir=/with\,comma:/b`},
		{"lowerdir=/a,,", 0, 0, "lowerdir=/a"},
	}
	for _, c := range cases {
		flags, data := mangleMountOptions(c.opts, c.flags)
		if flags != c.wantFlags || data != c.wantData {
			t.Errorf("mangleMountOptions(%q, %#x) = %#x, %q; want %#x, %q", c.opts, c.flags, flags, data, c.wantFlags, c.wantData)
		}
	}
}

func TestMountFlagsString(t *testing.T) {
	cases := []struct {
		flags uintptr
		want  string
	}{
		{unix.MS_RDONLY | unix.MS_NODEV, "MS_RDONLY|MS_NODEV"},
		{unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_RDONLY, "MS_RDONLY|MS_NOSUID|MS_NODEV|MS_NOEXEC"},
		{unix.MS_BIND | unix.MS_REC, "MS_BIND|MS_REC"},
		{0, "0"},
		{1 << 31, "80000000"},
		{unix.MS_NODEV | 1<<31, "MS_NODEV|80000000"},
	}
	for _, c := range cases {
		if got := mountFlagsString(c.flags); got != c.want {
			t.Errorf("mountFlagsString(%#x) = %q, want %q", c.flags, got, c.want)
		}
	}
}

func TestOverlayOptions(t *testing.T) {
	if got := escapeLayer(`/a:b,c\d`); got != `/a\:b\,c\\d` {
		t.Errorf("escapeLayer = %q", got)
	}
	if got := overlayOptions([]string{"/m", "/e:1", "/usr"}, "", "", ""); got != `lowerdir=/m:/e\:1:/usr` {
		t.Errorf("read-only options = %q", got)
	}
	if got := overlayOptions([]string{"/m"}, "/u", "/w", "index=off"); got != "lowerdir=/m,upperdir=/u,workdir=/w,index=off" {
		t.Errorf("mutable options = %q", got)
	}
}

func TestOverlayMountFlags(t *testing.T) {
	lower := []string{"/m", "/usr"}
	cases := []struct {
		class     release.Class
		noexec    int
		upper     string
		extra     *string
		wantFlags uintptr
		wantData  string
	}{
		{release.Sysext, NoExecDefault, "", nil, unix.MS_RDONLY | unix.MS_NODEV, "lowerdir=/m:/usr"},
		{release.Sysext, NoExecOn, "", nil, unix.MS_RDONLY | unix.MS_NODEV | unix.MS_NOEXEC, "lowerdir=/m:/usr"},
		{release.Confext, NoExecDefault, "", nil, unix.MS_RDONLY | unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC, "lowerdir=/m:/usr"},
		{release.Confext, NoExecOff, "", nil, unix.MS_RDONLY | unix.MS_NODEV | unix.MS_NOSUID, "lowerdir=/m:/usr"},
		{release.Sysext, NoExecDefault, "/u", nil, unix.MS_NODEV | unix.MS_NOATIME,
			"lowerdir=/m:/usr,upperdir=/u,workdir=/w,redirect_dir=on,metacopy=off,index=off"},
		{release.Sysext, NoExecDefault, "/u", new("xino=off"), unix.MS_NODEV, "lowerdir=/m:/usr,upperdir=/u,workdir=/w,xino=off"},
		{release.Sysext, NoExecDefault, "/u", new(""), unix.MS_NODEV, "lowerdir=/m:/usr,upperdir=/u,workdir=/w"},
		{release.Sysext, NoExecDefault, "", new("noatime,xino=off"), unix.MS_RDONLY | unix.MS_NODEV | unix.MS_NOATIME, "lowerdir=/m:/usr,xino=off"},
		{release.Sysext, NoExecDefault, "", new(""), unix.MS_RDONLY | unix.MS_NODEV, "lowerdir=/m:/usr"},
	}
	for _, c := range cases {
		op := &mergeOp{class: c.class, opts: MergeOptions{NoExec: c.noexec, MountOptions: c.extra}}
		work := ""
		if c.upper != "" {
			work = "/w"
		}
		flags, data := op.overlayMount(lower, c.upper, work)
		if flags != c.wantFlags || data != c.wantData {
			t.Errorf("%+v: flags %#x data %q; want %#x %q", c, flags, data, c.wantFlags, c.wantData)
		}
	}
}

func TestMakeMountPoint(t *testing.T) {
	dir := t.TempDir()
	if err := makeMountPoint(dir, "a/b/c", unix.S_IFDIR); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a/b/c")); err != nil || !fi.IsDir() {
		t.Errorf("directory mount point: %v", err)
	}
	if err := makeMountPoint(dir, "a/file", unix.S_IFREG); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a/file")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("file mount point: %v", err)
	}
	if err := makeMountPoint(dir, "a/b/c", unix.S_IFDIR); err != nil {
		t.Errorf("existing mount point: %v", err)
	}
	if err := os.Symlink("/etc", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := makeMountPoint(dir, "link/x", unix.S_IFDIR); !errors.Is(err, unix.ELOOP) {
		t.Errorf("mount point below a symlink = %v, want ELOOP", err)
	}
}

func TestRemoveTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	if err := os.MkdirAll(filepath.Join(dir, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeTree(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("tree not removed")
	}
	if err := removeTree("/proc"); err == nil {
		t.Error("removeTree accepted a directory with mounts")
	}
}

func TestPathInRoot(t *testing.T) {
	for _, c := range []struct {
		p, root, want string
		ok            bool
	}{
		{"/var/lib/x", "/", "var/lib/x", true},
		{"/r/var/lib/x", "/r", "var/lib/x", true},
		{"/rx/var", "/r", "", false},
		{"/r", "/r", "", false},
		{"/", "/", "", false},
		{"", "/", "", false},
	} {
		if got, ok := pathInRoot(c.p, c.root); got != c.want || ok != c.ok {
			t.Errorf("pathInRoot(%q, %q) = %q, %v", c.p, c.root, got, ok)
		}
	}
}

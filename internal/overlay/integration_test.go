package overlay

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// The tests in this file mount things and need CAP_SYS_ADMIN; run them as
// root or in a user namespace: unshare -rm go test ./internal/overlay/

type testEnv struct {
	t    *testing.T
	root string
	run  string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root; run under unshare -rm")
	}
	base := t.TempDir()
	e := &testEnv{t: t, root: filepath.Join(base, "root"), run: filepath.Join(base, "run")}
	for _, d := range []string{e.root, e.run} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount("tmpfs", d, "tmpfs", 0, "mode=0755"); errors.Is(err, unix.EPERM) {
			t.Skip("needs CAP_SYS_ADMIN; run under unshare -rm")
		} else if err != nil {
			t.Fatalf("mount tmpfs: %v", err)
		}
	}
	old := runtimeDir
	runtimeDir = filepath.Join(e.run, "systemd")
	t.Cleanup(func() {
		runtimeDir = old
		_ = detachAll(e.root)
		_ = detachAll(e.run)
	})
	for _, d := range []string{"usr/lib", "usr/bin", "opt", "etc", "var/lib/extensions", "var/lib/confexts"} {
		e.mkdir(d)
	}
	e.write("usr/lib/os-release", "ID=test\n")
	e.write("usr/bin/host-tool", "host\n")
	e.write("etc/hostname", "host\n")
	return e
}

func (e *testEnv) path(rel string) string { return filepath.Join(e.root, rel) }

func (e *testEnv) mkdir(rel string) {
	e.t.Helper()
	if err := os.MkdirAll(e.path(rel), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) write(rel, data string) {
	e.t.Helper()
	e.mkdir(filepath.Dir(rel))
	if err := os.WriteFile(e.path(rel), []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) read(rel string) string {
	e.t.Helper()
	data, err := os.ReadFile(e.path(rel))
	if err != nil {
		return "ERR: " + err.Error()
	}
	return string(data)
}

// ext creates a directory sysext named name shipping the given files.
func (e *testEnv) ext(name string, files ...string) {
	e.t.Helper()
	dir := "var/lib/extensions/" + name
	e.write(dir+"/usr/lib/extension-release.d/extension-release."+name, "ID=_any\n")
	for _, f := range files {
		e.write(dir+"/"+f, name+"\n")
	}
}

func (e *testEnv) images(class release.Class) []discover.Image {
	e.t.Helper()
	images, err := discover.Discover(class, e.root)
	if err != nil {
		e.t.Fatal(err)
	}
	return images
}

func (e *testEnv) opts() MergeOptions {
	return MergeOptions{Root: e.root, NoExec: NoExecDefault}
}

// mutableOpts returns options for a mutable merge. Inside a user namespace
// overlayfs refuses redirect_dir=on, so the defaults are replaced there.
func (e *testEnv) mutableOpts(mode string) MergeOptions {
	opts := e.opts()
	opts.Mutable = mode
	if data, err := os.ReadFile("/proc/self/uid_map"); err == nil && !strings.Contains(string(data), "4294967295") {
		opts.MountOptions = new("noatime,metacopy=off,index=off")
	}
	return opts
}

func (e *testEnv) merge(class release.Class, opts MergeOptions) Outcome {
	e.t.Helper()
	out, err := Merge(class, e.images(class), opts)
	if err != nil {
		e.t.Fatalf("Merge: %v", err)
	}
	return out
}

func (e *testEnv) refresh(class release.Class, opts MergeOptions) Outcome {
	e.t.Helper()
	out, err := Refresh(class, e.images(class), opts)
	if err != nil {
		e.t.Fatalf("Refresh: %v", err)
	}
	return out
}

func (e *testEnv) unmerge(class release.Class) []string {
	e.t.Helper()
	unmerged, err := Unmerge(class, e.root)
	if err != nil {
		e.t.Fatalf("Unmerge: %v", err)
	}
	return unmerged
}

// generations lists the refresh generations left in the workspace.
func (e *testEnv) generations(class release.Class) []string {
	e.t.Helper()
	ws, err := Workspace(class, e.root)
	if err != nil {
		e.t.Fatal(err)
	}
	gens, err := filepath.Glob(filepath.Join(ws, "next.*"))
	if err != nil {
		e.t.Fatal(err)
	}
	return gens
}

func (e *testEnv) mountsBelow(dir string) []string {
	e.t.Helper()
	mounts, err := fsutil.ReadMountInfo()
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, m := range fsutil.MountsBelow(mounts, dir) {
		out = append(out, m.MountPoint)
	}
	return out
}

// assertClean checks that nothing but the test's own mounts is left.
func (e *testEnv) assertClean(want ...string) {
	e.t.Helper()
	if got := e.mountsBelow(e.run); len(got) != 0 {
		e.t.Errorf("mounts left in the runtime dir: %v", got)
	}
	got := e.mountsBelow(e.root)
	for i := range want {
		want[i] = e.path(want[i])
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		e.t.Errorf("mounts below root = %v, want %v", got, want)
	}
	for _, class := range []release.Class{release.Sysext, release.Confext} {
		ws, err := Workspace(class, e.root)
		if err != nil {
			e.t.Fatal(err)
		}
		if _, err := os.Lstat(ws); !errors.Is(err, os.ErrNotExist) {
			e.t.Errorf("workspace %s left behind", ws)
		}
	}
}

func (e *testEnv) mountOptions(rel string) string {
	e.t.Helper()
	mounts, err := fsutil.ReadMountInfo()
	if err != nil {
		e.t.Fatal(err)
	}
	opts := ""
	for _, m := range mounts {
		if m.MountPoint == e.path(rel) {
			opts = m.Options
		}
	}
	return opts
}

func devnumOf(t *testing.T, p string) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		t.Fatal(err)
	}
	return formatDevnum(uint64(st.Dev)) + "\n"
}

func TestIntegrationMergeUnmerge(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f", "opt/foo/f")
	e.ext("bar", "usr/share/bar/f")

	old := syscall.Umask(0o077)
	out := e.merge(release.Sysext, e.opts())
	syscall.Umask(old)

	want := Outcome{Result: Merged, Merged: []string{"bar", "foo"},
		Hierarchies: []string{e.path("usr"), e.path("opt")}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("Merge = %+v, want %+v", out, want)
	}
	for _, f := range []string{"usr/share/foo/f", "usr/share/bar/f", "opt/foo/f", "usr/bin/host-tool"} {
		if _, err := os.Stat(e.path(f)); err != nil {
			t.Errorf("%s not visible: %v", f, err)
		}
	}
	for _, h := range []string{"usr", "opt"} {
		m := h + "/.systemd-sysext/"
		if got, want := e.read(m+"dev"), devnumOf(t, e.path(h)); got != want {
			t.Errorf("%s dev marker = %q, want %q", h, got, want)
		}
		if got := e.read(m + "extensions"); got != "bar\nfoo\n" {
			t.Errorf("%s extensions marker = %q", h, got)
		}
		if _, err := os.Stat(e.path(m + "work_dir")); err == nil {
			t.Errorf("%s has a work_dir marker in read-only mode", h)
		}
		var st unix.Stat_t
		if err := unix.Stat(e.path(h), &st); err != nil || st.Mode&0o777 != 0o755 {
			t.Errorf("%s mode = %o under umask 077, want 755", h, st.Mode&0o777)
		}
	}
	if e.read("usr/.systemd-sysext/origin") != e.read("opt/.systemd-sysext/origin") {
		t.Error("origin markers differ between hierarchies")
	}
	var org map[string]any
	if err := json.Unmarshal([]byte(e.read("usr/.systemd-sysext/origin")), &org); err != nil {
		t.Fatalf("origin: %v", err)
	}
	ext := org["extensions"].(map[string]any)["foo"].(map[string]any)
	if ext["path"] != "/var/lib/extensions/foo" || ext["mtime"] != float64(0) {
		t.Errorf("origin entry for foo = %v", ext)
	}
	if !strings.Contains(e.mountOptions("usr"), "nodev") || !strings.HasPrefix(e.mountOptions("usr"), "ro") {
		t.Errorf("/usr mount options = %q", e.mountOptions("usr"))
	}

	ws, err := Workspace(release.Sysext, e.root)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "extensions/foo/usr/share/foo/f")); err != nil || string(data) != "foo\n" {
		t.Errorf("image tree not reachable in the workspace: %q %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(ws, "extensions/foo/x"), nil, 0o644); !errors.Is(err, unix.EROFS) {
		t.Errorf("directory image bind mount is writable: %v", err)
	}

	statuses, err := CurrentStatus(release.Sysext, e.root)
	if err != nil || len(statuses) != 2 || !statuses[0].Merged || !slices.Equal(statuses[0].Extensions, []string{"bar", "foo"}) || statuses[0].Since == 0 {
		t.Errorf("CurrentStatus = %+v, %v", statuses, err)
	}

	_, err = Merge(release.Sysext, e.images(release.Sysext), e.opts())
	var already *AlreadyMergedError
	if !errors.As(err, &already) || already.Hierarchy != "/usr" || !errors.Is(err, ErrAlreadyMerged) {
		t.Errorf("second Merge = %v, want already merged", err)
	}

	if got := e.unmerge(release.Sysext); !slices.Equal(got, []string{e.path("usr"), e.path("opt")}) {
		t.Errorf("Unmerge = %v", got)
	}
	if _, err := os.Stat(e.path("usr/share/foo")); !errors.Is(err, os.ErrNotExist) {
		t.Error("extension content still visible after unmerge")
	}
	e.assertClean()
	if got := e.unmerge(release.Sysext); got != nil {
		t.Errorf("second Unmerge = %v", got)
	}
}

func TestIntegrationSubmounts(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	e.mkdir("usr/local/sub")
	if err := unix.Mount("tmpfs", e.path("usr/local/sub"), "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	e.write("usr/local/sub/marker", "sub\n")
	e.mkdir("usr/local/sub/nested")
	if err := unix.Mount("tmpfs", e.path("usr/local/sub/nested"), "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	e.write("usr/local/sub/nested/n", "nested\n")
	e.write("persist/hostname", "persistent\n")
	if err := unix.Mount(e.path("persist/hostname"), e.path("etc/hostname"), "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}

	e.merge(release.Sysext, e.opts())
	if got := e.read("usr/local/sub/marker"); got != "sub\n" {
		t.Errorf("submount hidden by merge: %q", got)
	}
	if got := e.read("usr/local/sub/nested/n"); got != "nested\n" {
		t.Errorf("nested submount hidden by merge: %q", got)
	}
	e.refresh(release.Sysext, MergeOptions{Root: e.root, NoExec: NoExecDefault, AlwaysRefresh: true})
	if got := e.read("usr/local/sub/marker"); got != "sub\n" {
		t.Errorf("submount lost by refresh: %q", got)
	}
	if err := unix.Mount("tmpfs", e.path("usr/bin"), "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	e.unmerge(release.Sysext)
	if got := e.read("usr/local/sub/nested/n"); got != "nested\n" {
		t.Errorf("submounts not restored by unmerge: %q", got)
	}
	if mp, _ := fsutil.IsMountPoint(e.path("usr/bin")); !mp {
		t.Error("mount made below the merged hierarchy lost on unmerge")
	}
	_ = detach(e.path("usr/bin"))

	e.write("var/lib/confexts/conf/etc/extension-release.d/extension-release.conf", "ID=_any\n")
	e.write("var/lib/confexts/conf/etc/conf/f", "c\n")
	out := e.merge(release.Confext, e.opts())
	if out.Result != Merged || e.read("etc/conf/f") != "c\n" {
		t.Fatalf("confext merge = %+v", out)
	}
	if got := e.read("etc/hostname"); got != "persistent\n" {
		t.Errorf("file bind mount hidden by confext merge: %q", got)
	}
	if got := e.read("etc/.systemd-confext/confexts"); got != "conf\n" {
		t.Errorf("confext list marker = %q", got)
	}
	if _, err := os.Stat(e.path("etc/.systemd-confext/extensions")); err == nil {
		t.Error("confext wrote an extensions marker")
	}
	if opts := e.mountOptions("etc"); !strings.Contains(opts, "noexec") || !strings.Contains(opts, "nosuid") {
		t.Errorf("/etc mount options = %q", opts)
	}
	e.unmerge(release.Confext)
	if got := e.read("etc/hostname"); got != "persistent\n" {
		t.Errorf("file bind mount lost by confext unmerge: %q", got)
	}
	e.assertClean("etc/hostname", "usr/local/sub", "usr/local/sub/nested")
}

func TestIntegrationSharedPropagation(t *testing.T) {
	e := newTestEnv(t)
	for _, d := range []string{e.root, e.run} {
		if err := unix.Mount("", d, "", unix.MS_SHARED|unix.MS_REC, ""); err != nil {
			t.Fatal(err)
		}
	}
	e.ext("foo", "usr/share/foo/f")
	e.merge(release.Sysext, e.opts())
	if e.read("usr/share/foo/f") != "foo\n" {
		t.Fatal("not merged")
	}
	e.refresh(release.Sysext, MergeOptions{Root: e.root, NoExec: NoExecDefault, AlwaysRefresh: true})
	e.unmerge(release.Sysext)
	e.assertClean()
}

func TestIntegrationRefresh(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	e.ext("bar", "usr/share/bar/f")
	e.merge(release.Sysext, e.opts())
	dev := e.read("usr/.systemd-sysext/dev")

	if out := e.refresh(release.Sysext, e.opts()); out.Result != Unchanged || !slices.Equal(out.Merged, []string{"bar", "foo"}) {
		t.Errorf("unchanged Refresh = %+v", out)
	}
	if e.read("usr/.systemd-sysext/dev") != dev {
		t.Error("unchanged refresh remounted")
	}
	if gens := e.generations(release.Sysext); gens != nil {
		t.Errorf("skipped refresh left its staging area behind: %v", gens)
	}

	if err := os.RemoveAll(e.path("var/lib/extensions/bar")); err != nil {
		t.Fatal(err)
	}
	e.ext("bar", "usr/share/bar/g")
	out := e.refresh(release.Sysext, e.opts())
	if out.Result != Merged || !slices.Equal(out.Unmerged, []string{e.path("usr")}) {
		t.Errorf("Refresh after replacing an image = %+v", out)
	}
	if e.read("usr/share/bar/g") != "bar\n" {
		t.Error("replaced image content not merged")
	}
	ws, _ := Workspace(release.Sysext, e.root)
	if _, err := os.Stat(filepath.Join(ws, "extensions/bar/usr/share/bar/g")); err != nil {
		t.Errorf("new image tree not moved into the workspace: %v", err)
	}
	if gens := e.generations(release.Sysext); gens != nil {
		t.Errorf("refresh left its staging area behind: %v", gens)
	}

	if out := e.refresh(release.Sysext, e.mutableOpts("ephemeral")); out.Result != Merged {
		t.Errorf("Refresh with another mutable mode = %+v", out)
	}
	if err := os.WriteFile(e.path("usr/eph"), nil, 0o644); err != nil {
		t.Errorf("ephemeral merge not writable: %v", err)
	}
	if out := e.refresh(release.Sysext, e.mutableOpts("ephemeral")); out.Result != Unchanged {
		t.Errorf("Refresh with the same mutable mode = %+v", out)
	}

	for _, n := range []string{"foo", "bar"} {
		if err := os.RemoveAll(e.path("var/lib/extensions/" + n)); err != nil {
			t.Fatal(err)
		}
	}
	out = e.refresh(release.Sysext, e.opts())
	if out.Result != NothingFound || !slices.Equal(out.Unmerged, []string{e.path("usr"), e.path("opt")}) {
		t.Errorf("Refresh without images = %+v", out)
	}
	e.assertClean()
}

func TestIntegrationRefreshFailureKeepsMerge(t *testing.T) {
	e := newTestEnv(t)
	e.ext("good", "usr/share/good/f")
	e.merge(release.Sysext, e.opts())
	dev := e.read("usr/.systemd-sysext/dev")
	e.ext("bad", "usr/share/bad/f")

	opts := e.opts()
	opts.Check = func(img discover.Image, tree string) (bool, error) {
		if img.Name == "bad" {
			return false, errors.New("broken image")
		}
		return true, nil
	}
	if _, err := Refresh(release.Sysext, e.images(release.Sysext), opts); err == nil {
		t.Fatal("Refresh succeeded despite a failing check")
	}
	if e.read("usr/share/good/f") != "good\n" || e.read("usr/.systemd-sysext/dev") != dev {
		t.Error("failed refresh touched the merge")
	}
	opts.Mutable = "bogus"
	if _, err := Refresh(release.Sysext, e.images(release.Sysext), opts); err == nil {
		t.Error("Refresh accepted an invalid mutable mode")
	}
	if gens := e.generations(release.Sysext); gens != nil {
		t.Errorf("failed refresh left its staging area behind: %v", gens)
	}

	opts.Mutable = ""
	opts.Check = func(img discover.Image, tree string) (bool, error) { return img.Name != "bad", nil }
	out, err := Refresh(release.Sysext, e.images(release.Sysext), opts)
	if err != nil || out.Result != Unchanged || out.Ignored != 1 {
		t.Errorf("Refresh ignoring the bad image = %+v, %v", out, err)
	}
	e.unmerge(release.Sysext)
	e.assertClean()
}

func TestIntegrationIgnoredAndNothingFound(t *testing.T) {
	e := newTestEnv(t)
	e.ext("a", "usr/share/a/f")
	e.ext("b", "usr/share/b/f")
	opts := e.opts()
	var seen []string
	opts.Check = func(img discover.Image, tree string) (bool, error) {
		seen = append(seen, img.Name)
		if _, err := os.Stat(filepath.Join(tree, "usr/share", img.Name, "f")); err != nil {
			t.Errorf("check got no tree for %s: %v", img.Name, err)
		}
		return img.Name == "a", nil
	}
	out := e.merge(release.Sysext, opts)
	if out.Ignored != 1 || !slices.Equal(out.Merged, []string{"a"}) || !slices.Equal(seen, []string{"a", "b"}) {
		t.Errorf("Merge = %+v (checked %v)", out, seen)
	}
	e.unmerge(release.Sysext)

	opts.Check = func(discover.Image, string) (bool, error) { return false, nil }
	out = e.merge(release.Sysext, opts)
	if out.Result != NothingFound || out.Ignored != 2 {
		t.Errorf("Merge with only ignored images = %+v", out)
	}
	out = e.merge(release.Sysext, MergeOptions{Root: e.root, NoExec: NoExecDefault})
	if out.Result != Merged {
		t.Errorf("Merge = %+v", out)
	}
	e.unmerge(release.Sysext)
	if out, err := Merge(release.Sysext, nil, e.opts()); err != nil || out.Result != NothingFound || out.Ignored != 0 {
		t.Errorf("Merge without images = %+v, %v", out, err)
	}
	e.assertClean()
}

func TestIntegrationMutable(t *testing.T) {
	e := newTestEnv(t)
	opts := e.mutableOpts("yes")
	out := e.merge(release.Sysext, opts)
	if out.Result != Merged || len(out.Merged) != 0 || len(out.Hierarchies) != 2 {
		t.Fatalf("mutable Merge without images = %+v", out)
	}
	if err := os.WriteFile(e.path("usr/new"), []byte("x"), 0o644); err != nil {
		t.Fatalf("mutable /usr not writable: %v", err)
	}
	if e.read("var/lib/extensions.mutable/usr/new") != "x" {
		t.Error("write not routed into the mutable directory")
	}
	if err := os.WriteFile(e.path("usr/.systemd-sysext/dev"), nil, 0o644); !errors.Is(err, unix.EROFS) {
		t.Errorf("metadata writable in mutable mode: %v", err)
	}
	if got := e.read("usr/.systemd-sysext/work_dir"); got != "var/lib/extensions.mutable/.systemd-usr-workdir\n" {
		t.Errorf("work_dir marker = %q", got)
	}
	if got := e.read("usr/.systemd-sysext/extensions"); got != "" {
		t.Errorf("extensions marker = %q, want empty", got)
	}
	var org struct {
		Mutable struct {
			Mode        string
			MutableDirs map[string]string
		}
	}
	if err := json.Unmarshal([]byte(e.read("usr/.systemd-sysext/origin")), &org); err != nil ||
		org.Mutable.Mode != "yes" || org.Mutable.MutableDirs["/opt"] != "/var/lib/extensions.mutable/opt" {
		t.Errorf("origin = %+v, %v", org, err)
	}
	e.unmerge(release.Sysext)
	if _, err := os.Stat(e.path("var/lib/extensions.mutable/.systemd-usr-workdir")); !errors.Is(err, os.ErrNotExist) {
		t.Error("workdir not removed on unmerge")
	}
	if e.read("var/lib/extensions.mutable/usr/new") != "x" {
		t.Error("mutable data lost on unmerge")
	}

	e.mkdir("var/lib/extensions.mutable/usr/.systemd-sysext")
	e.write("var/lib/extensions.mutable/usr/.systemd-sysext/dev", "1:1\n")
	e.ext("foo", "usr/share/foo/f")
	e.merge(release.Sysext, opts)
	if got, want := e.read("usr/.systemd-sysext/dev"), devnumOf(t, e.path("usr")); got != want {
		t.Errorf("stale metadata in the mutable directory shadows the marker: %q, want %q", got, want)
	}
	e.unmerge(release.Sysext)

	if err := os.Chmod(e.path("var/lib/extensions.mutable/usr"), 0o700); err != nil {
		t.Fatal(err)
	}
	opts.Mutable = "auto"
	if _, err := Merge(release.Sysext, e.images(release.Sysext), opts); err == nil || !strings.Contains(err.Error(), "ought to have mode 0755") {
		t.Errorf("Merge with a mutable directory of the wrong mode = %v", err)
	}
	if err := os.Chmod(e.path("var/lib/extensions.mutable/usr"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(e.path("var/lib/extensions.mutable/opt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../opt", e.path("var/lib/extensions.mutable/opt")); err != nil {
		t.Fatal(err)
	}
	opts.Mutable = "import"
	if _, err := Merge(release.Sysext, e.images(release.Sysext), opts); !errors.Is(err, unix.ELOOP) {
		t.Errorf("import of the hierarchy into itself = %v, want ELOOP", err)
	}
	if err := os.Remove(e.path("var/lib/extensions.mutable/opt")); err != nil {
		t.Fatal(err)
	}
	e.write("var/lib/extensions.mutable/opt/imported", "i\n")
	e.merge(release.Sysext, opts)
	if e.read("opt/imported") != "i\n" {
		t.Error("import mode did not import the mutable directory")
	}
	if err := os.WriteFile(e.path("usr/x"), nil, 0o644); !errors.Is(err, unix.EROFS) {
		t.Errorf("import mode is writable: %v", err)
	}
	e.unmerge(release.Sysext)
	opts.Mutable = "ephemeral"
	e.merge(release.Sysext, opts)
	if err := os.WriteFile(e.path("usr/eph"), nil, 0o644); err != nil {
		t.Errorf("ephemeral /usr not writable: %v", err)
	}
	if _, err := os.Stat(e.path("usr/.systemd-sysext/work_dir")); err == nil {
		t.Error("work_dir marker written in ephemeral mode")
	}
	e.unmerge(release.Sysext)
	if _, err := os.Stat(e.path("usr/eph")); !errors.Is(err, os.ErrNotExist) {
		t.Error("ephemeral write survived unmerge")
	}
	e.assertClean()
}

func TestIntegrationHierarchyResolution(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f", "opt/foo/f", "srv/new/x")
	if err := os.Remove(e.path("opt")); err != nil {
		t.Fatal(err)
	}
	e.mkdir("var/opt")
	if err := os.Symlink("/var/opt", e.path("opt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "/usr:/opt:/srv/new:/srv/none")
	out := e.merge(release.Sysext, e.opts())
	if !slices.Equal(out.Hierarchies, []string{e.path("usr"), e.path("var/opt"), e.path("srv/new")}) {
		t.Errorf("merged hierarchies = %v", out.Hierarchies)
	}
	if merged, err := IsMergedByUs(release.Sysext, e.root, "/opt"); err != nil || !merged {
		t.Errorf("symlinked hierarchy not seen as merged: %v %v", merged, err)
	}
	if e.read("srv/new/x") != "foo\n" {
		t.Error("missing hierarchy not created and merged")
	}
	if _, err := os.Stat(e.path("srv/none")); !errors.Is(err, os.ErrNotExist) {
		t.Error("hierarchy no extension ships was created")
	}
	statuses, err := CurrentStatus(release.Sysext, e.root)
	if err != nil || len(statuses) != 3 {
		t.Errorf("CurrentStatus = %+v, %v", statuses, err)
	}
	e.unmerge(release.Sysext)
	e.assertClean()
}

func TestIntegrationNoHierarchyMerged(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "/opt")
	out := e.merge(release.Sysext, e.opts())
	if out.Result != Merged || !slices.Equal(out.Merged, []string{"foo"}) || out.Hierarchies != nil {
		t.Errorf("Merge = %+v", out)
	}
	e.assertClean()
}

func TestIntegrationMalformedMarker(t *testing.T) {
	e := newTestEnv(t)
	if err := unix.Mount("tmpfs", e.path("opt"), "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	e.write("opt/.systemd-sysext/dev", "garbage\n")
	if _, err := IsMergedByUs(release.Sysext, e.root, "/opt"); err == nil || !strings.Contains(err.Error(), "failed to parse device major/minor") {
		t.Errorf("IsMergedByUs with a malformed marker = %v", err)
	}
	e.ext("foo", "usr/share/foo/f")
	if _, err := Merge(release.Sysext, e.images(release.Sysext), e.opts()); err == nil {
		t.Error("Merge ignored a malformed marker")
	}
	e.write("opt/.systemd-sysext/dev", "1:2\n")
	if merged, err := IsMergedByUs(release.Sysext, e.root, "/opt"); err != nil || merged {
		t.Errorf("foreign tree with a mismatching marker = %v, %v", merged, err)
	}
	e.write("opt/.systemd-sysext/dev", devnumOf(t, e.path("opt")))
	if merged, err := IsMergedByUs(release.Sysext, e.root, "/opt"); err != nil || !merged {
		t.Errorf("matching marker = %v, %v", merged, err)
	}
	_ = detach(e.path("opt"))
	e.assertClean()
}

func TestIntegrationPanicRollback(t *testing.T) {
	e := newTestEnv(t)
	e.ext("a", "usr/share/a/f")
	e.ext("b", "usr/share/b/f")
	opts := e.opts()
	opts.Check = func(img discover.Image, tree string) (bool, error) {
		if img.Name == "b" {
			panic("boom")
		}
		return true, nil
	}
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want the original panic", r)
			}
		}()
		_, _ = Merge(release.Sysext, e.images(release.Sysext), opts)
	}()
	e.assertClean()
}

func TestIntegrationWorkspaceStaysOutsideRoot(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	e.write("elsewhere/systemd/sysext/meta/usr/precious", "keep\n")
	if err := os.Symlink(e.path("elsewhere"), e.path("run")); err != nil {
		t.Fatal(err)
	}
	e.merge(release.Sysext, e.opts())
	e.unmerge(release.Sysext)
	if e.read("elsewhere/systemd/sysext/meta/usr/precious") != "keep\n" {
		t.Error("merge or unmerge reached through the root's /run")
	}
	e.assertClean()
}

func TestIntegrationSubmountCloneFailureKeepsSubmounts(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	for _, d := range []string{"usr/a", "usr/b"} {
		e.mkdir(d)
		if err := unix.Mount("tmpfs", e.path(d), "tmpfs", 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	e.write("usr/a/data", "a\n")
	if err := unix.Mount("", e.path("usr/b"), "", unix.MS_UNBINDABLE, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Merge(release.Sysext, e.images(release.Sysext), e.opts()); err == nil {
		t.Fatal("Merge carried an unbindable submount along")
	}
	if mp, _ := fsutil.IsMountPoint(e.path("usr/a")); !mp || e.read("usr/a/data") != "a\n" {
		t.Error("submount lost because cloning another one failed")
	}
	e.assertClean("usr/a", "usr/b")
}

func TestIntegrationRollbackKeepsSubmounts(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f", "opt/foo/f")
	e.mkdir("usr/local/sub")
	if err := unix.Mount("tmpfs", e.path("usr/local/sub"), "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	e.write("usr/local/sub/marker", "sub\n")
	// Importing /opt into itself fails in the second hierarchy, while the
	// overlay of /usr, carrying the submount, is still at its staging point.
	loop := func() {
		t.Helper()
		e.mkdir("var/lib/extensions.mutable")
		if err := os.Symlink("/opt", e.path("var/lib/extensions.mutable/opt")); err != nil {
			t.Fatal(err)
		}
	}
	submountBack := func(what string) {
		t.Helper()
		if mp, _ := fsutil.IsMountPoint(e.path("usr/local/sub")); !mp || e.read("usr/local/sub/marker") != "sub\n" {
			t.Errorf("submount of /usr lost by %s", what)
		}
	}
	opts := e.opts()
	opts.Mutable = "import"

	loop()
	if _, err := Merge(release.Sysext, e.images(release.Sysext), opts); !errors.Is(err, unix.ELOOP) {
		t.Fatalf("Merge = %v, want ELOOP", err)
	}
	submountBack("a failed merge")

	if err := os.Remove(e.path("var/lib/extensions.mutable/opt")); err != nil {
		t.Fatal(err)
	}
	e.merge(release.Sysext, opts)
	loop()
	opts.AlwaysRefresh = true
	if _, err := Refresh(release.Sysext, e.images(release.Sysext), opts); !errors.Is(err, unix.ELOOP) {
		t.Fatalf("Refresh = %v, want ELOOP", err)
	}
	submountBack("a failed refresh")
	e.assertClean("usr/local/sub")
}

func TestIntegrationRefreshAfterFailedPromote(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	e.merge(release.Sysext, e.opts())
	ws, err := Workspace(release.Sysext, e.root)
	if err != nil {
		t.Fatal(err)
	}
	// A mount below the layers of the old merge makes the clean up after
	// the new overlay went live fail.
	blocker := filepath.Join(ws, "meta/usr/blocker")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", blocker, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	always := e.opts()
	always.AlwaysRefresh = true
	if out, err := Refresh(release.Sysext, e.images(release.Sysext), always); !errors.Is(err, ErrCleanup) || out.Result != Merged || len(out.Hierarchies) != 1 {
		t.Fatalf("Refresh with a failing clean up = %+v, %v", out, err)
	}
	if err := detach(blocker); err != nil {
		t.Fatal(err)
	}
	dev := e.read("usr/.systemd-sysext/dev")
	if gens := e.generations(release.Sysext); len(gens) != 1 {
		t.Fatalf("live generation not kept: %v", gens)
	}

	if out := e.refresh(release.Sysext, e.opts()); out.Result != Unchanged {
		t.Errorf("Refresh after a failed clean up = %+v, want unchanged", out)
	}
	if e.read("usr/share/foo/f") != "foo\n" || e.read("usr/.systemd-sysext/dev") != dev {
		t.Error("refresh damaged the live merge")
	}
	if _, err := CurrentStatus(release.Sysext, e.root); err != nil {
		t.Errorf("CurrentStatus: %v", err)
	}
	if out := e.refresh(release.Sysext, always); out.Result != Merged || e.read("usr/share/foo/f") != "foo\n" {
		t.Errorf("forced Refresh after a failed clean up = %+v", out)
	}
	e.unmerge(release.Sysext)
	e.assertClean()
}

func TestIntegrationBeforeUnmergeFailure(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	e.merge(release.Sysext, e.opts())
	dev := e.read("usr/.systemd-sysext/dev")

	opts := e.opts()
	opts.Check = func(discover.Image, string) (bool, error) { return false, nil }
	opts.BeforeUnmerge = func() error { return errors.New("no metadata") }
	if out, err := Refresh(release.Sysext, e.images(release.Sysext), opts); err == nil || out.Unmerged != nil {
		t.Errorf("Refresh with a failing BeforeUnmerge = %+v, %v", out, err)
	}
	if e.read("usr/share/foo/f") != "foo\n" || e.read("usr/.systemd-sysext/dev") != dev {
		t.Error("refresh unmerged although BeforeUnmerge failed")
	}

	called := false
	opts.BeforeUnmerge = func() error {
		called = e.read("usr/share/foo/f") == "foo\n"
		return nil
	}
	if out := e.refresh(release.Sysext, opts); out.Result != NothingFound || !called {
		t.Errorf("Refresh = %+v, BeforeUnmerge called with the merge in place: %v", out, called)
	}
	e.assertClean()
}

func TestIntegrationBeforeMerge(t *testing.T) {
	e := newTestEnv(t)
	e.ext("foo", "usr/share/foo/f")
	e.ext("bar", "usr/share/bar/f")
	opts := e.opts()
	opts.Check = func(img discover.Image, _ string) (bool, error) { return img.Name != "bar", nil }
	var calls [][]string
	opts.BeforeMerge = func(merged []string) {
		if mp, _ := fsutil.IsMountPoint(e.path("usr")); mp {
			t.Error("BeforeMerge called with the hierarchy merged already")
		}
		calls = append(calls, merged)
	}
	e.merge(release.Sysext, opts)
	if !reflect.DeepEqual(calls, [][]string{{"foo"}}) {
		t.Errorf("BeforeMerge calls = %q", calls)
	}
	calls = nil
	if out := e.refresh(release.Sysext, opts); out.Result != Unchanged || calls != nil {
		t.Errorf("unchanged refresh = %+v, BeforeMerge calls %q", out, calls)
	}
	e.unmerge(release.Sysext)

	opts.MountOptions = new("bogus=1")
	ws, err := Workspace(release.Sysext, e.root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Merge(release.Sysext, e.images(release.Sysext), opts)
	want := "failed to mount sysext (type overlay) on " + ws + `/overlay/usr (MS_RDONLY|MS_NODEV "lowerdir=`
	if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.HasSuffix(err.Error(), `,bogus=1"): invalid argument`) || !errors.Is(err, unix.EINVAL) {
		t.Errorf("Merge with an invalid mount option = %v, want %q...", err, want)
	}
	if len(calls) != 1 {
		t.Errorf("BeforeMerge not called before the failing overlay mount: %q", calls)
	}

	e.write("var/lib/extensions/junk.raw", strings.Repeat("\x00", 64<<10))
	opts.MountOptions = nil
	_, err = Merge(release.Sysext, e.images(release.Sysext), opts)
	ie, ok := errors.AsType[*ImageError](err)
	if !ok || ie.Image.Name != "junk" || !errors.Is(err, unix.ENOPKG) ||
		!strings.HasPrefix(err.Error(), "failed to mount image junk: "+e.path("var/lib/extensions/junk.raw")+": ") {
		t.Errorf("Merge with an unusable raw image = %v", err)
	}
	e.assertClean()
}

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/errno"
	"github.com/itxaka/sysext-alpine/internal/image"
	"github.com/itxaka/sysext-alpine/internal/overlay"
	"github.com/itxaka/sysext-alpine/internal/release"
	"github.com/itxaka/sysext-alpine/internal/service"
)

// tree creates an image tree with the given files (path → content).
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, data := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func sysextRelease(name, content string) map[string]string {
	return map[string]string{"usr/lib/extension-release.d/extension-release." + name: content}
}

func TestCheckImage(t *testing.T) {
	host := release.Fields{"ID": "alpine", "VERSION_ID": "3.24.1"}
	osRelease := "usr/lib/os-release"
	cases := []struct {
		name  string
		typ   discover.ImageType
		class release.Class
		force bool
		files map[string]string
		ok    bool
		fatal string
	}{
		{name: "good", files: sysextRelease("good", "ID=_any\n"), ok: true},
		{name: "same", files: sysextRelease("same", "ID=alpine\nVERSION_ID=3.24.1\n"), ok: true},
		{name: "fedora", files: sysextRelease("fedora", "ID=fedora\n")},
		{name: "fedora", force: true, files: sysextRelease("fedora", "ID=fedora\n"), ok: true},
		{name: "stale", files: sysextRelease("stale", "ID=alpine\nVERSION_ID=3.24.0\n")},
		{name: "initrd", files: sysextRelease("initrd", "ID=_any\nSYSEXT_SCOPE=initrd\n")},
		{name: "arch", files: sysextRelease("arch", "ID=_any\nARCHITECTURE=s390\n")},
		{name: "norel", files: map[string]string{"usr/share/x": ""}},
		{name: "osr", files: map[string]string{osRelease: "ID=evil\n", "usr/lib/extension-release.d/extension-release.osr": "ID=_any\n"},
			fatal: "Failed to read metadata for image osr: No medium found"},
		{name: "osr", force: true, files: map[string]string{osRelease: "ID=evil\n"},
			fatal: "Failed to read metadata for image osr: No medium found"},
		{name: "conf", class: release.Confext, files: map[string]string{osRelease: "ID=x\n", "etc/extension-release.d/extension-release.conf": "ID=_any\n"}, ok: true},
		{name: "raw", typ: discover.TypeRaw, files: sysextRelease("raw", "ID=_any\n"), ok: true},
		{name: "rawos", typ: discover.TypeRaw, files: map[string]string{osRelease: "ID=x\n", "usr/lib/extension-release.d/extension-release.rawos": "ID=_any\n"}},
		{name: "rawos", typ: discover.TypeRaw, force: true, files: map[string]string{osRelease: "ID=x\n", "usr/lib/extension-release.d/extension-release.rawos": "ID=_any\n"}, ok: true},
		{name: "rawnone", typ: discover.TypeRaw, files: map[string]string{"usr/share/x": ""},
			fatal: "Failed to read metadata for image rawnone: No medium found"},
		{name: "rawnone", typ: discover.TypeRaw, force: true, files: map[string]string{"usr/share/x": ""},
			fatal: "Failed to read metadata for image rawnone: No medium found"},
		{name: "rawconf", typ: discover.TypeRaw, files: map[string]string{"etc/extension-release.d/extension-release.rawconf": "ID=_any\n"}},
	}
	for _, c := range cases {
		var stderr bytes.Buffer
		cl := &cli{cfg: &config{class: c.class, force: c.force}, log: &logger{w: &stderr, level: logInfo}}
		check := cl.checkImage(host, "system")
		img := discover.Image{Name: c.name, Type: c.typ}
		ok, err := check(img, tree(t, c.files))
		if c.name == "rawos" && !c.force && stderr.String() != "Failed to mount image: No medium found\n" {
			t.Errorf("rawos: stderr = %q", stderr.String())
		}
		if c.fatal != "" {
			if err == nil || err.Error() != c.fatal {
				t.Errorf("%s (force=%v): err = %v, want %q", c.name, c.force, err, c.fatal)
			} else if !errors.Is(err, unix.ENOMEDIUM) {
				t.Errorf("%s: error does not wrap ENOMEDIUM", c.name)
			}
			continue
		}
		if err != nil || ok != c.ok {
			t.Errorf("%s (force=%v): ok=%v err=%v; want ok=%v", c.name, c.force, ok, err, c.ok)
		}
	}

	var stderr bytes.Buffer
	cl := &cli{cfg: &config{}, log: &logger{w: &stderr, level: logDebug}}
	if ok, _ := cl.checkImage(host, "system")(discover.Image{Name: "fedora"}, tree(t, sysextRelease("fedora", "ID=fedora\n"))); ok ||
		!strings.Contains(stderr.String(), "fedora") {
		t.Errorf("the reason for ignoring an image is logged at debug level: %q", stderr.String())
	}
}

func TestReportMerge(t *testing.T) {
	cases := []struct {
		out  overlay.Outcome
		want string
	}{
		{overlay.Outcome{Result: overlay.NothingFound}, "No extensions found.\n"},
		{overlay.Outcome{Result: overlay.NothingFound, Ignored: 2, Unmerged: []string{"/usr"}},
			"No suitable extensions found (2 ignored due to incompatible image(s)).\nUnmerged '/usr'.\n"},
		{overlay.Outcome{Result: overlay.Unchanged, Merged: []string{"bar"}},
			"Skipping extension refresh because no change was found, use --always-refresh=yes to always do a refresh.\n"},
		{overlay.Outcome{Result: overlay.Merged, Merged: []string{"bar", "baz", "foo"}, Hierarchies: []string{"/usr", "/opt"}},
			"Merged extensions into '/usr'.\nMerged extensions into '/opt'.\n"},
		{overlay.Outcome{Result: overlay.Merged, Hierarchies: []string{"/usr"}, Unmerged: []string{"/usr"}},
			"Unmerged '/usr'.\nMerged extensions into '/usr'.\n"},
	}
	for _, c := range cases {
		var stderr bytes.Buffer
		cl := &cli{cfg: &config{}, stdout: &bytes.Buffer{}, log: &logger{w: &stderr, level: logInfo}}
		cl.reportMerge(c.out)
		if stderr.String() != c.want {
			t.Errorf("reportMerge(%+v) =\n%q\nwant\n%q", c.out, stderr.String(), c.want)
		}
	}
}

func TestReportUsing(t *testing.T) {
	images := []discover.Image{
		{Name: "foo", Path: "/var/lib/extensions/foo_1.10.raw"},
		{Name: "bar", Path: "/var/lib/extensions/bar"},
		{Name: "baz", Path: "/var/lib/extensions/foo_1.9.raw"},
	}
	for _, c := range []struct {
		merged []string
		want   string
	}{
		{[]string{"bar", "baz", "foo"}, "Using extensions 'bar', 'foo_1.9.raw', 'foo_1.10.raw'.\n"},
		{nil, "No extensions found, proceeding in mutable mode.\n"},
	} {
		var stderr bytes.Buffer
		cl := &cli{cfg: &config{}, log: &logger{w: &stderr, level: logInfo}}
		cl.reportUsing(c.merged, images)
		if stderr.String() != c.want {
			t.Errorf("reportUsing(%q) = %q, want %q", c.merged, stderr.String(), c.want)
		}
	}
}

func TestMergeFailed(t *testing.T) {
	gpt := discover.Image{Name: "gpt", Path: "/var/lib/extensions/gpt.raw", Type: discover.TypeRaw}
	policyErr := &overlay.ImageError{Image: gpt, Err: fmt.Errorf("%s: %w", gpt.Path, errno.New(unix.ERFKILL, "image does not satisfy image policy: root partition exists, but the policy requires it to be absent"))}
	verityErr := &overlay.ImageError{Image: gpt, Err: fmt.Errorf("%s: %w", gpt.Path, &image.VerityError{Partition: "root", Err: errors.New("verity signature verification failed: no trusted certificates")})}
	brokenErr := &overlay.ImageError{Image: gpt, Err: fmt.Errorf("%s: %w", gpt.Path, errno.New(unix.ENOPKG, "no suitable partition table or file system found"))}
	cases := []struct {
		name   string
		policy string
		err    error
		stderr string
		want   string
	}{
		{"os-release", "", failf(fs.ErrNotExist, "Failed to acquire 'os-release' data of OS tree '/x'"),
			"Failed to acquire 'os-release' data of OS tree '/x': No such file or directory\n", "Failed to merge hierarchies"},
		{"synthetic", "", errno.New(unix.EINVAL, "Mutable directory '/m' has mode 0700, ought to have mode 0755"),
			"Mutable directory '/m' has mode 0700, ought to have mode 0755\n", "Failed to merge hierarchies"},
		{"overlay mount", "", fmt.Errorf("failed to mount sysext (type overlay) on /run/x (MS_RDONLY \"o\"): %w", unix.EINVAL),
			"Failed to mount sysext (type overlay) on /run/x (MS_RDONLY \"o\"): Invalid argument\n", "Failed to merge hierarchies"},
		{"already merged", "", &overlay.AlreadyMergedError{Hierarchy: "/usr"}, "", "Hierarchy '/usr' is already merged."},
		{"metadata", "", metadataFailure(unix.ENOMEDIUM, "osr"), "", "Failed to read metadata for image osr: No medium found"},
		{"policy, default", "", policyErr, "", "Failed to read metadata for image gpt: Operation not possible due to RF-kill"},
		{"policy, configured", "root=absent", policyErr, "/var/lib/extensions/gpt.raw: Image does not match image policy.\n", "Failed to merge hierarchies"},
		{"verity, configured", "root=signed", verityErr, "", "Failed to merge hierarchies"},
		{"broken, default", "", brokenErr, "", "Failed to read metadata for image gpt: Package not installed"},
		{"broken, configured", "root=signed", brokenErr, "", "Failed to read metadata for image gpt: Package not installed"},
	}
	for _, c := range cases {
		var stderr bytes.Buffer
		cl := &cli{cfg: &config{imagePolicy: c.policy}, log: &logger{w: &stderr, level: logInfo}}
		err := cl.mergeFailed(c.err)
		if err == nil || errorText(err) != c.want || stderr.String() != c.stderr {
			t.Errorf("%s: mergeFailed = %v, logged %q; want %q, logged %q", c.name, err, stderr.String(), c.want, c.stderr)
		}
	}

	for _, c := range []struct {
		policy string
		err    error
		detail string
	}{
		{"root=signed", verityErr, "no trusted certificates"},
		{"", brokenErr, "no suitable partition table or file system found"},
	} {
		var stderr bytes.Buffer
		cl := &cli{cfg: &config{imagePolicy: c.policy}, log: &logger{w: &stderr, level: logDebug}}
		_ = cl.mergeFailed(c.err)
		if !strings.Contains(stderr.String(), c.detail) {
			t.Errorf("the reason an image failed is logged at debug level: %q", stderr.String())
		}
	}
}

func TestErrnoRendering(t *testing.T) {
	err := failf(unix.ENOTUNIQ, "Failed to read metadata for image %s", "x")
	if err.Error() != "Failed to read metadata for image x: Name not unique on network" || !errors.Is(err, unix.ENOTUNIQ) {
		t.Errorf("failf = %v", err)
	}
	for _, c := range []struct {
		err      error
		strerror string
		text     string
	}{
		{unix.ENOPKG, "Package not installed", "Package not installed"},
		{fmt.Errorf("no os-release file found: %w", fs.ErrNotExist), "No such file or directory", "No os-release file found: No such file or directory"},
		{&fs.PathError{Op: "open", Path: "/x", Err: unix.EACCES}, "Permission denied", "Open /x: Permission denied"},
		{fmt.Errorf("failed to bind mount /x: %w", unix.EBUSY), "Device or resource busy", "Failed to bind mount /x: Device or resource busy"},
		{errno.New(unix.EROFS, "Can't use '/m' as an upperdir as it is read-only."), "Read-only file system", "Can't use '/m' as an upperdir as it is read-only."},
		{errors.New("dev marker not visible"), "Invalid argument", "Dev marker not visible"},
	} {
		if got := strerror(c.err); got != c.strerror {
			t.Errorf("strerror(%v) = %q, want %q", c.err, got, c.strerror)
		}
		if got := errorText(c.err); got != c.text {
			t.Errorf("errorText(%v) = %q, want %q", c.err, got, c.text)
		}
	}
}

func TestCritical(t *testing.T) {
	old := redeliver
	t.Cleanup(func() { redeliver = old })
	var got []unix.Signal
	redeliver = func(sig unix.Signal) { got = append(got, sig) }
	var stderr bytes.Buffer
	c := &cli{cfg: &config{root: t.TempDir()}, log: &logger{w: &stderr, level: logInfo}}

	err := c.critical(func() error {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		return failMsg("boom")
	})
	if err == nil || len(got) != 1 || got[0] != unix.SIGHUP || stderr.String() != "boom\n" {
		t.Errorf("signal during the critical section: err=%v redelivered=%v stderr=%q", err, got, stderr.String())
	}
	got = nil
	if err := c.critical(func() error { return nil }); err != nil || len(got) != 0 {
		t.Errorf("no signal: err=%v redelivered=%v", err, got)
	}
}

func TestWarnNoexecInitScripts(t *testing.T) {
	root := t.TempDir()
	warning := "/etc/ is merged with noexec, OpenRC cannot execute the init scripts in /etc/init.d/ now; use --noexec=no to avoid that.\n"
	for _, tc := range []struct {
		class       release.Class
		noExec      int
		hierarchies []string
		initd       bool
		warn        bool
	}{
		{release.Confext, overlay.NoExecDefault, []string{"/etc"}, true, true},
		{release.Confext, overlay.NoExecOn, []string{"/srv", "/etc/"}, true, true},
		{release.Confext, overlay.NoExecOff, []string{"/etc"}, true, false},
		{release.Confext, overlay.NoExecDefault, []string{"/srv"}, true, false},
		{release.Confext, overlay.NoExecDefault, []string{"/etc"}, false, false},
		{release.Sysext, overlay.NoExecOn, []string{"/etc"}, true, false},
	} {
		os.RemoveAll(filepath.Join(root, "etc"))
		if tc.initd {
			if err := os.MkdirAll(filepath.Join(root, "etc/init.d"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		var stderr bytes.Buffer
		c := &cli{cfg: &config{class: tc.class, noExec: tc.noExec, root: root}, hierarchies: tc.hierarchies, log: &logger{w: &stderr, level: logInfo}}
		c.warnNoexecInitScripts()
		if want := map[bool]string{true: warning}[tc.warn]; stderr.String() != want {
			t.Errorf("%+v: stderr = %q", tc, stderr.String())
		}
	}
}

func TestServiceActionsGating(t *testing.T) {
	var stderr bytes.Buffer
	log := &logger{w: &stderr, level: logDebug}
	var calls []string
	rc := &service.OpenRC{Log: log, RunDir: t.TempDir(), Run: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	c := &cli{cfg: &config{noReload: true}, log: log, rc: rc}
	if a, err := c.extensionActions(); err != nil || !a.Empty() {
		t.Errorf("--no-reload collects nothing: %+v %v", a, err)
	}
	c.applyActions(service.Actions{Reload: true})
	if len(calls) != 0 || stderr.String() != "OpenRC is not running, not reloading the service manager.\n" {
		t.Errorf("without OpenRC: calls=%q stderr=%q", calls, stderr.String())
	}
	if err := os.WriteFile(filepath.Join(rc.RunDir, "softlevel"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c.applyActions(service.Actions{})
	c.applyActions(service.Actions{Reload: true})
	if !slices.Equal(calls, []string{"rc-update -u"}) {
		t.Errorf("calls = %q", calls)
	}
	calls = nil
	c.cfg.root = t.TempDir()
	c.stopProvidedServices(service.Actions{Restart: []string{"x.service"}})
	if len(calls) != 0 {
		t.Errorf("no merged extensions, nothing stopped: %q", calls)
	}
}

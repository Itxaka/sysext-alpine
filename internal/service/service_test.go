package service

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

func TestUnitNameIsValid(t *testing.T) {
	for name, want := range map[string]bool{
		"foo.service":                         true,
		"foo@bar.service":                     true,
		"foo-bar_baz:x\\y.socket":             true,
		"a.timer":                             true,
		"foo@.service":                        false,
		"@foo.service":                        false,
		".service":                            false,
		"foo":                                 false,
		"foo.bogus":                           false,
		"foo bar.service":                     false,
		"bogus/name.service":                  false,
		"":                                    false,
		strings.Repeat("a", 248) + ".service": false,
		strings.Repeat("a", 247) + ".service": true,
	} {
		if got := UnitNameIsValid(name); got != want {
			t.Errorf("UnitNameIsValid(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestServiceName(t *testing.T) {
	for _, tt := range []struct {
		unit, want string
		ok         bool
	}{
		{"sshd.service", "sshd", true},
		{"getty@tty1.service", "getty.tty1", true},
		{"net@eth0.service", "net.eth0", true},
		{"foo.socket", "", false},
		{".service", "", false},
	} {
		got, ok := ServiceName(tt.unit)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ServiceName(%q) = %q, %v; want %q, %v", tt.unit, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCollectActions(t *testing.T) {
	a, warnings := CollectActions([]Extension{
		{Name: "a", Fields: release.Fields{"EXTENSION_RELOAD_MANAGER": "no"}},
		{Name: "b", Fields: release.Fields{
			"EXTENSION_RESTART_UNITS":           "  y.service x.service bogus/name ",
			"EXTENSION_RELOAD_OR_RESTART_UNITS": "x.service z@1.service",
		}},
		{Name: "c", Fields: release.Fields{"EXTENSION_RELOAD_MANAGER": "maybe"}},
	})
	if !a.Reload {
		t.Error("listing units must imply a reload")
	}
	if !slices.Equal(a.Restart, []string{"x.service", "y.service"}) {
		t.Errorf("Restart = %v", a.Restart)
	}
	if !slices.Equal(a.ReloadOrRestart, []string{"z@1.service"}) {
		t.Errorf("ReloadOrRestart = %v (restart must win)", a.ReloadOrRestart)
	}
	want := []string{
		"Invalid unit name 'bogus/name' in EXTENSION_RESTART_UNITS= of b, ignoring.",
		"Failed to parse EXTENSION_RELOAD_MANAGER= of c, ignoring: Invalid argument",
	}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q", warnings)
	}

	for _, tt := range []struct {
		fields release.Fields
		reload bool
	}{
		{release.Fields{"EXTENSION_RELOAD_MANAGER": "1"}, true},
		{release.Fields{"EXTENSION_RELOAD_MANAGER": "yes"}, true},
		{release.Fields{"EXTENSION_RELOAD_MANAGER": "off"}, false},
		{release.Fields{"EXTENSION_RESTART_UNITS": "   "}, false},
		{release.Fields{"EXTENSION_RESTART_UNITS": "bogus"}, true},
		{release.Fields{}, false},
	} {
		a, _ := CollectActions([]Extension{{Name: "e", Fields: tt.fields}})
		if a.Reload != tt.reload {
			t.Errorf("CollectActions(%v).Reload = %v", tt.fields, a.Reload)
		}
	}
	if !(Actions{}).Empty() || (Actions{Reload: true}).Empty() {
		t.Error("Empty")
	}
}

func TestCmdlineBool(t *testing.T) {
	for _, tt := range []struct {
		cmdline string
		initrd  bool
		want    bool
		err     bool
	}{
		{"quiet", false, true, false},
		{"systemd.sysext=0", false, false, false},
		{"systemd.sysext", false, true, false},
		{"systemd.sysext=no systemd.sysext", false, false, false},
		{"systemd.sysext systemd.sysext=off", false, false, false},
		{"systemd.sysext=1 systemd.sysext=0 systemd.sysext", false, false, false},
		{"systemd.sysext=yes systemd.sysext=false", false, false, false},
		{"systemd_sysext=0", false, true, false},
		{"systemd.sysextx=0", false, true, false},
		{"systemd.sysext=maybe", false, false, true},
		{"rd.systemd.sysext=0", false, true, false},
		{`"systemd.sysext=0"`, false, false, false},
	} {
		got, err := CmdlineBool(splitCmdline(tt.cmdline), "systemd.sysext", tt.initrd)
		if (err != nil) != tt.err || (err == nil && got != tt.want) || (err != nil && !errors.Is(err, unix.EINVAL)) {
			t.Errorf("CmdlineBool(%q) = %v, %v; want %v, err=%v", tt.cmdline, got, err, tt.want, tt.err)
		}
	}
	if got, err := CmdlineBool([]string{"foo_bar-baz=0"}, "foo-bar_baz", false); err != nil || got {
		t.Errorf("'-' and '_' must compare equal: %v, %v", got, err)
	}
	got, err := CmdlineBool(splitCmdline("rd.systemd.sysext=0"), "rd.systemd.sysext", true)
	if err != nil || got {
		t.Errorf("rd. key inside the initrd = %v, %v", got, err)
	}
}

func TestSplitCmdline(t *testing.T) {
	got := splitCmdline(" a  b=\"c d\" 'e f'g h\\ i\n")
	want := []string{"a", "b=c d", "e fg", `h\ i`}
	if !slices.Equal(got, want) {
		t.Errorf("splitCmdline = %q, want %q", got, want)
	}
}

func TestKernelCmdlineOverride(t *testing.T) {
	t.Setenv("SYSTEMD_PROC_CMDLINE", "systemd.confext=0 x")
	words, err := KernelCmdline()
	if err != nil || !slices.Equal(words, []string{"systemd.confext=0", "x"}) {
		t.Errorf("KernelCmdline = %q, %v", words, err)
	}
}

// fakeHost points KernelCmdline at a directory with the given files and
// makes this process PID pid.
func fakeHost(t *testing.T, pid int, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	for name, data := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot, oldPid := hostRoot, getpid
	hostRoot, getpid = root, func() int { return pid }
	t.Cleanup(func() { hostRoot, getpid = oldRoot, oldPid })
}

func TestKernelCmdlineContainer(t *testing.T) {
	const kernel = "systemd.sysext=0 quiet\n"
	const pid1 = "/sbin/init\x00--log-level\x00debug\x00systemd.sysext=1\x00-z\x00x\x00\x00"
	cmdline := map[string]string{"proc/cmdline": kernel, "proc/1/cmdline": pid1}
	with := func(extra map[string]string) map[string]string {
		m := maps.Clone(cmdline)
		maps.Copy(m, extra)
		return m
	}
	for _, tc := range []struct {
		name  string
		pid   int
		env   string // $container of this process, "-" for unset
		files map[string]string
		want  []string
	}{
		{"host", 42, "-", cmdline, []string{"systemd.sysext=0", "quiet"}},
		{"docker", 42, "-", with(map[string]string{".dockerenv": ""}), []string{"systemd.sysext=1"}},
		{"podman", 42, "-", with(map[string]string{"run/.containerenv": ""}), []string{"systemd.sysext=1"}},
		{"container manager file", 42, "-", with(map[string]string{"run/host/container-manager": "nspawn\n"}), []string{"systemd.sysext=1"}},
		{"empty container manager file", 42, "-", with(map[string]string{"run/host/container-manager": ""}), []string{"systemd.sysext=0", "quiet"}},
		{"systemd container file", 42, "-", with(map[string]string{"run/systemd/container": "lxc\n"}), []string{"systemd.sysext=1"}},
		{"PID 1 environment", 42, "-", with(map[string]string{"proc/1/environ": "PATH=/bin\x00container=oci\x00"}), []string{"systemd.sysext=1"}},
		{"empty $container of PID 1", 42, "-", with(map[string]string{"proc/1/environ": "container=\x00"}), []string{"systemd.sysext=1"}},
		{"other PID 1 environment", 42, "-", with(map[string]string{"proc/1/environ": "containers=x\x00"}), []string{"systemd.sysext=0", "quiet"}},
		{"own environment of another process", 42, "lxc", cmdline, []string{"systemd.sysext=0", "quiet"}},
		{"own environment as PID 1", 1, "lxc", cmdline, []string{"systemd.sysext=1"}},
		{"empty environment as PID 1 is authoritative", 1, "", with(map[string]string{".dockerenv": ""}), []string{"systemd.sysext=0", "quiet"}},
		{"unset environment as PID 1", 1, "-", with(map[string]string{".dockerenv": ""}), []string{"systemd.sysext=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "-" {
				t.Setenv("container", "")
				os.Unsetenv("container")
			} else {
				t.Setenv("container", tc.env)
			}
			fakeHost(t, tc.pid, tc.files)
			words, err := KernelCmdline()
			if err != nil || !slices.Equal(words, tc.want) {
				t.Errorf("KernelCmdline = %q, %v; want %q", words, err, tc.want)
			}
		})
	}

	t.Setenv("container", "")
	os.Unsetenv("container")
	fakeHost(t, 42, map[string]string{"proc/cmdline": kernel, "proc/1/cmdline": "", ".dockerenv": ""})
	if words, err := KernelCmdline(); !errors.Is(err, unix.ENOENT) {
		t.Errorf("container PID 1 without a command line = %q, %v", words, err)
	}
	t.Setenv("SYSTEMD_PROC_CMDLINE", "x")
	if words, err := KernelCmdline(); err != nil || !slices.Equal(words, []string{"x"}) {
		t.Errorf("$SYSTEMD_PROC_CMDLINE must win in a container: %q, %v", words, err)
	}
}

func TestFilterPID1Args(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"/sbin/init"}, nil},
		{[]string{"init", "a", "--system", "b"}, []string{"a", "b"}},
		{[]string{"init", "--log-level", "debug", "--log-level=info", "c"}, []string{"c"}},
		{[]string{"init", "--log-color", "d"}, []string{"d"}},
		{[]string{"init", "--unknown", "e", "--log-levelx", "f"}, []string{"e", "f"}},
		{[]string{"init", "-z", "g", "-zh", "i", "-bz", "j", "-hD", "k"}, []string{"i", "k"}},
		{[]string{"init", "-", "-x", "l", ""}, []string{"l", ""}},
		{[]string{"init", "--", "--system", "-z"}, []string{"--system", "-z"}},
		{[]string{"init", "--unit"}, nil},
	} {
		if got := filterPID1Args(tc.argv); !slices.Equal(got, tc.want) {
			t.Errorf("filterPID1Args(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestSplitNulstr(t *testing.T) {
	for in, want := range map[string][]string{
		"":                   nil,
		"\x00\x00":           nil,
		"a":                  {"a"},
		"a\x00b\x00":         {"a", "b"},
		"a\x00\x00b\x00\x00": {"a", "", "b"},
	} {
		if got := splitNulstr([]byte(in)); !slices.Equal(got, want) {
			t.Errorf("splitNulstr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInvokedByServiceManager(t *testing.T) {
	t.Setenv("RC_SVCNAME", "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	if InvokedByServiceManager() {
		t.Error("not invoked by a service manager")
	}
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	if !InvokedByServiceManager() {
		t.Error("SYSTEMD_EXEC_PID matches")
	}
	t.Setenv("SYSTEMD_EXEC_PID", "*")
	if !InvokedByServiceManager() {
		t.Error("SYSTEMD_EXEC_PID=* is accepted for testing")
	}
	t.Setenv("SYSTEMD_EXEC_PID", "1")
	if InvokedByServiceManager() {
		t.Error("SYSTEMD_EXEC_PID names another process")
	}
	t.Setenv("SYSTEMD_EXEC_PID", "")
	t.Setenv("RC_SVCNAME", "sysext")
	if !InvokedByServiceManager() {
		t.Error("RC_SVCNAME set")
	}
}

func TestInInitrd(t *testing.T) {
	old := initrdRelease
	t.Cleanup(func() { initrdRelease = old })
	initrdRelease = filepath.Join(t.TempDir(), "initrd-release")
	t.Setenv("SYSTEMD_IN_INITRD", "")
	os.Unsetenv("SYSTEMD_IN_INITRD")
	if InInitrd() {
		t.Error("no initrd-release")
	}
	if err := os.WriteFile(initrdRelease, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !InInitrd() {
		t.Error("initrd-release exists")
	}
	t.Setenv("SYSTEMD_IN_INITRD", "no")
	if InInitrd() {
		t.Error("SYSTEMD_IN_INITRD=no overrides the file")
	}
	t.Setenv("SYSTEMD_IN_INITRD", "maybe")
	if !InInitrd() {
		t.Error("an invalid SYSTEMD_IN_INITRD is ignored")
	}
}

type recLog struct{ debug, warn []string }

func (l *recLog) Debugf(f string, a ...any) { l.debug = append(l.debug, fmt.Sprintf(f, a...)) }
func (l *recLog) Warnf(f string, a ...any)  { l.warn = append(l.warn, fmt.Sprintf(f, a...)) }

type fakeRC struct {
	calls    []string
	scripts  map[string]string
	started  map[string]bool
	noReload map[string]bool
	failing  map[string]bool
}

func (f *fakeRC) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "rc-update" {
		return nil, nil
	}
	switch args[0] {
	case "-e":
		if _, ok := f.scripts[args[1]]; ok {
			return nil, nil
		}
		return nil, errors.New("exit 1")
	case "-r":
		if p, ok := f.scripts[args[1]]; ok {
			return []byte(p + "\n"), nil
		}
		return nil, errors.New("exit 1")
	}
	svc, verb := args[0], args[1]
	switch {
	case verb == "status" && !f.started[svc]:
		return nil, errors.New("exit 3")
	case verb == "reload" && f.noReload[svc]:
		return []byte("unknown function"), errors.New("exit 1")
	case f.failing[svc] && verb != "status":
		return []byte("boom"), errors.New("exit 1")
	}
	return nil, nil
}

func TestApply(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/sbin/openrc-run\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	withReload := "extra_started_commands=reload\nreload() {\n\ttrue\n}\n"
	f := &fakeRC{
		scripts: map[string]string{
			"a": script("a", ""), "bad": script("bad", ""),
			"b": script("b", withReload), "c": script("c", "  reload ()  { :; }\n"),
			"d": script("d", withReload), "e": script("e", "# reload() is not defined here\n"),
		},
		started:  map[string]bool{"b": true, "c": true, "e": true},
		noReload: map[string]bool{"c": true},
		failing:  map[string]bool{"bad": true},
	}
	log := &recLog{}
	o := &OpenRC{Log: log, Run: f.run}
	o.Apply(Actions{
		Reload:          true,
		Restart:         []string{"a.service", "gone.service", "bad.service", "t.timer"},
		ReloadOrRestart: []string{"b.service", "c.service", "d.service", "e.service"},
	})
	want := []string{
		"rc-update -u",
		"rc-service -e a", "rc-service a restart",
		"rc-service -e gone",
		"rc-service -e bad", "rc-service bad restart",
		"rc-service -e b", "rc-service b status", "rc-service -r b", "rc-service b reload",
		"rc-service -e c", "rc-service c status", "rc-service -r c", "rc-service c reload", "rc-service c restart",
		"rc-service -e d", "rc-service d status", "rc-service d restart",
		"rc-service -e e", "rc-service e status", "rc-service -r e", "rc-service e restart",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Contains(log.debug, "Unit 'gone.service' is already gone, nothing to stop.") {
		t.Errorf("debug = %q", log.debug)
	}
	wantWarn := []string{
		"Failed to RestartUnit unit 'bad.service': exit 1: boom",
		"Unit 't.timer' is not a service, OpenRC cannot RestartUnit it, ignoring.",
	}
	if !slices.Equal(log.warn, wantWarn) {
		t.Errorf("warn = %q", log.warn)
	}
}

func TestStopProvidedBy(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "etc/init.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ext", "extstopped"} {
		if err := os.WriteFile(filepath.Join(tree, "etc/init.d", s), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeRC{
		scripts: map[string]string{"ext": "/etc/init.d/ext", "extstopped": "/etc/init.d/extstopped", "host": "/etc/init.d/host"},
		started: map[string]bool{"ext": true, "host": true},
	}
	o := &OpenRC{Log: &recLog{}, Run: f.run}
	o.StopProvidedBy(Actions{
		Restart:         []string{"ext.service", "host.service"},
		ReloadOrRestart: []string{"extstopped.service", "missing.service"},
	}, []string{t.TempDir(), tree})
	if !slices.Contains(f.calls, "rc-service ext stop") {
		t.Errorf("extension-provided started service not stopped: %q", f.calls)
	}
	for _, c := range f.calls {
		if strings.HasSuffix(c, " stop") && c != "rc-service ext stop" {
			t.Errorf("unexpected stop: %q", c)
		}
	}
}

func TestAvailable(t *testing.T) {
	dir := t.TempDir()
	o := &OpenRC{RunDir: filepath.Join(dir, "missing"), Run: (&fakeRC{}).run}
	if o.Available() {
		t.Error("missing run dir must not be available")
	}
	o.RunDir = dir
	if o.Available() {
		t.Error("run dir without softlevel must not be available")
	}
	if err := os.WriteFile(filepath.Join(dir, "softlevel"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !o.Available() {
		t.Error("run dir with softlevel must be available")
	}
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itxaka/sysext-alpine/internal/overlay"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// hermetic clears the environment variables the CLI reads.
func hermetic(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SYSTEMD_LOG_LEVEL", "DEBUG_INVOCATION", "RC_SVCNAME", "SYSTEMD_EXEC_PID",
		"SYSTEMD_INVOKED_AS", "SYSTEMD_IN_INITRD", "SYSTEMD_OS_RELEASE",
		"SYSTEMD_SYSEXT_HIERARCHIES", "SYSTEMD_CONFEXT_HIERARCHIES",
		"SYSTEMD_SYSEXT_MUTABLE_MODE", "SYSTEMD_CONFEXT_MUTABLE_MODE",
		"SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS", "SYSTEMD_CONFEXT_OVERLAYFS_MOUNT_OPTIONS",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("SYSTEMD_PROC_CMDLINE", "")
	t.Setenv("COLUMNS", "80")
}

// runCLI runs the command line and returns stdout, stderr and the status.
func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rc := run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), rc
}

// parse parses args into a fresh cli.
func parse(t *testing.T, args ...string) (*cli, bool, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	c := &cli{stdout: &stdout, log: &logger{w: &stderr, level: logInfo}}
	done, err := c.parseArgs(args)
	return c, done, err
}

// writeFile creates <root>/<rel> with content, making parent dirs.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClassFromInvocation(t *testing.T) {
	hermetic(t)
	for argv0, want := range map[string]release.Class{
		"sysext":                   release.Sysext,
		"/usr/bin/systemd-sysext":  release.Sysext,
		"confext":                  release.Confext,
		"/usr/bin/systemd-confext": release.Confext,
		"/some/confext-dir/sysext": release.Sysext,
	} {
		if got := classFromInvocation(argv0); got != want {
			t.Errorf("classFromInvocation(%q) = %v, want %v", argv0, got, want)
		}
	}
	t.Setenv("SYSTEMD_INVOKED_AS", "/usr/bin/systemd-confext")
	if classFromInvocation("sysext") != release.Confext {
		t.Error("$SYSTEMD_INVOKED_AS must win over argv[0]")
	}
}

func TestParseArgsDefaults(t *testing.T) {
	hermetic(t)
	c, done, err := parse(t, "sysext")
	if err != nil || done {
		t.Fatalf("parseArgs: done=%v err=%v", done, err)
	}
	cfg := c.cfg
	if cfg.class != release.Sysext || cfg.noExec != overlay.NoExecDefault || cfg.jsonMode != jsonOff ||
		!cfg.legend || cfg.root != "" || cfg.force || cfg.noReload || cfg.alwaysRefresh ||
		cfg.mutableSet || cfg.imagePolicySet || len(cfg.args) != 0 {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestParseArgs(t *testing.T) {
	hermetic(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args  []string
		check func(*config) bool
	}{
		{[]string{"--root=/mnt/"}, func(c *config) bool { return c.root == "/mnt" && c.noReload }},
		{[]string{"--root", "/mnt"}, func(c *config) bool { return c.root == "/mnt" && c.noReload }},
		{[]string{"--root=rel/x"}, func(c *config) bool { return c.root == filepath.Join(cwd, "rel/x") && c.noReload }},
		{[]string{"--root="}, func(c *config) bool { return c.root == "" && c.noReload }},
		{[]string{"--root=/"}, func(c *config) bool { return c.root == "/" && c.noReload }},
		{[]string{"--force"}, func(c *config) bool { return c.force }},
		{[]string{"--for"}, func(c *config) bool { return c.force }},
		{[]string{"--noexec=false"}, func(c *config) bool { return c.noExec == overlay.NoExecOff }},
		{[]string{"--noexec", "yes"}, func(c *config) bool { return c.noExec == overlay.NoExecOn }},
		{[]string{"--noe=on"}, func(c *config) bool { return c.noExec == overlay.NoExecOn }},
		{[]string{"--json=short"}, func(c *config) bool { return c.jsonMode == jsonShort }},
		{[]string{"--js", "pretty"}, func(c *config) bool { return c.jsonMode == jsonPretty }},
		{[]string{"--no-reload"}, func(c *config) bool { return c.noReload }},
		{[]string{"--no-r"}, func(c *config) bool { return c.noReload }},
		{[]string{"--always-refresh=yes"}, func(c *config) bool { return c.alwaysRefresh }},
		{[]string{"--always-refresh", "0"}, func(c *config) bool { return !c.alwaysRefresh }},
		{[]string{"--no-pager", "--no-legend"}, func(c *config) bool { return !c.legend }},
		{[]string{"--confext"}, func(c *config) bool { return c.class == release.Confext }},
		{[]string{"--conf"}, func(c *config) bool { return c.class == release.Confext }},
		{[]string{"--mutable=true"}, func(c *config) bool { return c.mutable == "yes" && c.mutableSet }},
		{[]string{"--mutable=ephemeral-import"}, func(c *config) bool { return c.mutable == "ephemeral-import" }},
		{[]string{"--image-policy=root=verity+signed:usr=absent"}, func(c *config) bool {
			return c.imagePolicy == "root=verity+signed:usr=absent" && c.imagePolicySet
		}},
		{[]string{"--image-policy="}, func(c *config) bool { return c.imagePolicy == "-" && c.imagePolicySet }},
		{[]string{"merge", "--force"}, func(c *config) bool { return c.force && len(c.args) == 1 && c.args[0] == "merge" }},
		{[]string{"--", "--force"}, func(c *config) bool { return !c.force && len(c.args) == 1 && c.args[0] == "--force" }},
		{[]string{"-"}, func(c *config) bool { return len(c.args) == 1 && c.args[0] == "-" }},
	}
	for _, tc := range cases {
		c, done, err := parse(t, append([]string{"sysext"}, tc.args...)...)
		if err != nil || done {
			t.Errorf("parseArgs(%q): done=%v err=%v", tc.args, done, err)
			continue
		}
		if !tc.check(c.cfg) {
			t.Errorf("parseArgs(%q) = %+v", tc.args, c.cfg)
		}
	}
	if c, _, _ := parse(t, "/usr/bin/confext"); c.cfg.class != release.Confext || c.cfg.progName != "confext" {
		t.Errorf("argv[0] confext: %+v", c.cfg)
	}
}

func TestParseArgsErrors(t *testing.T) {
	hermetic(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, "sysext: unrecognized option '--bogus'"},
		{[]string{"--bogus=1"}, "sysext: unrecognized option '--bogus'"},
		{[]string{"--=x"}, "sysext: unrecognized option '--'"},
		{[]string{"-x"}, "sysext: unrecognized option '-x'"},
		{[]string{"-xh"}, "sysext: unrecognized option '-x'"},
		{[]string{"--force=1"}, "sysext: option '--force' doesn't allow an argument"},
		{[]string{"--for=1"}, "sysext: option '--for' doesn't allow an argument"},
		{[]string{"--root"}, "sysext: option '--root' requires an argument"},
		{[]string{"--no"}, "sysext: option '--no' is ambiguous; possibilities: --noexec, --no-reload, --no-pager, --no-legend"},
		{[]string{"--no-"}, "sysext: option '--no-' is ambiguous; possibilities: --no-reload, --no-pager, --no-legend"},
		{[]string{"--i"}, "sysext: option '--i' is ambiguous; possibilities: --image-policy, --introspect-cli"},
		{[]string{"--json=banana"}, "Unknown argument to --json= switch: banana"},
		{[]string{"--noexec=banana"}, "Failed to parse boolean argument to '--noexec': banana"},
		{[]string{"--always-refresh=maybe"}, "Failed to parse boolean argument to '--always-refresh': maybe"},
		{[]string{"--mutable=banana"}, "Failed to parse argument to --mutable=: banana"},
		{[]string{"--image-policy=garbage"}, "Failed to parse image policy: garbage"},
		{[]string{"--image-policy=root=verity:root=signed"}, "Duplicate rule in image policy: root=verity:root=signed"},
		{[]string{"--image-policy=foo=verity"}, "Unknown partition type in image policy: foo=verity"},
		{[]string{"--image-policy=root=bogus"}, "Unknown partition policy flag in image policy: root=bogus"},
	} {
		_, _, err := parse(t, append([]string{"sysext"}, tc.args...)...)
		if err == nil || err.Error() != tc.want {
			t.Errorf("parseArgs(%q) = %v, want %q", tc.args, err, tc.want)
		}
	}
	if _, _, err := parse(t, "/usr/bin/confext", "--bogus"); err == nil || err.Error() != "confext: unrecognized option '--bogus'" {
		t.Errorf("confext prefix: %v", err)
	}
}

const sysextHelp = `> sysext [OPTION…] COMMAND …

Merge system extension images into /usr/ and /opt/.

Commands:
  [status]                 Show current merge status (default)
  merge                    Merge extensions into relevant hierarchies
  unmerge                  Unmerge extensions from relevant hierarchies
  refresh                  Unmerge/merge extensions again
  list                     List installed extensions

Options:
  -h --help                Show this help
     --version             Show package version
     --root=PATH           Operate relative to root PATH
     --mutable=MODE        Specify a mutability mode (yes, no, auto, import,
                           ephemeral, ephemeral-import, help)
     --image-policy=POLICY Specify disk image dissection policy
     --noexec=BOOL         Whether to mount extension overlay with noexec
     --force               Ignore version incompatibilities
     --no-reload           Do not reload the service manager (OpenRC)
     --always-refresh=BOOL Whether to refresh when no changes were found
     --no-pager            Do not start a pager
     --no-legend           Do not show headers and footers
     --json=FORMAT         Generate JSON output (pretty, short, or off)
     --confext             Operate on configuration extensions in /etc/

See the systemd-sysext(8) man page for details.
`

func TestHelp(t *testing.T) {
	hermetic(t)
	for _, args := range [][]string{
		{"sysext", "--help"}, {"sysext", "-h"}, {"sysext", "-hx"}, {"sysext", "--he"},
		{"sysext", "help"}, {"sysext", "help", "extra"}, {"sysext", "merge", "-h"},
	} {
		stdout, stderr, rc := runCLI(t, args...)
		if rc != 0 || stderr != "" || stdout != sysextHelp {
			t.Errorf("%q: rc=%d stderr=%q stdout:\n%s", args, rc, stderr, stdout)
		}
	}
	// Options are handled in order: an error before --help wins.
	if _, stderr, rc := runCLI(t, "sysext", "--json=bogus", "--help"); rc != 1 || stderr != "Unknown argument to --json= switch: bogus\n" {
		t.Errorf("error before --help: rc=%d %q", rc, stderr)
	}

	stdout, _, _ := runCLI(t, "/usr/bin/confext", "--help")
	want := strings.NewReplacer(
		"> sysext", "> confext",
		"Merge system extension images into /usr/ and /opt/.", "Merge configuration extension images into /etc/.",
		"systemd-sysext(8)", "systemd-confext(8)",
	).Replace(sysextHelp)
	if stdout != want {
		t.Errorf("confext help:\n%s", stdout)
	}

	t.Setenv("SYSTEMD_INVOKED_AS", "/usr/bin/systemd-sysext")
	if stdout, _, _ := runCLI(t, "sysext", "-h"); !strings.HasPrefix(stdout, "> systemd-sysext [OPTION…] COMMAND …\n") {
		t.Errorf("$SYSTEMD_INVOKED_AS names the program: %q", stdout)
	}
	t.Setenv("COLUMNS", "200")
	if stdout, _, _ := runCLI(t, "sysext", "-h"); !strings.Contains(stdout, "(yes, no, auto, import, ephemeral, ephemeral-import, help)\n") {
		t.Errorf("wide terminal must not wrap:\n%s", stdout)
	}
}

func TestPrintingOptions(t *testing.T) {
	hermetic(t)
	modes := "no\nyes\nauto\nimport\nephemeral\nephemeral-import\n"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--mutable=help"}, "Known mutability modes:\n" + modes},
		{[]string{"--mutable=help", "--no-legend"}, "Known mutability modes:\n" + modes},
		{[]string{"--no-legend", "--mutable=help"}, modes},
		{[]string{"--json=help"}, "pretty\nshort\noff\n"},
		{[]string{"--version"}, "sysext-alpine " + version + " (systemd-sysext 262)\n"},
	} {
		stdout, stderr, rc := runCLI(t, append([]string{"sysext"}, tc.args...)...)
		if rc != 0 || stderr != "" || stdout != tc.want {
			t.Errorf("%q: rc=%d stderr=%q stdout=%q, want %q", tc.args, rc, stderr, stdout, tc.want)
		}
	}
	stdout, _, rc := runCLI(t, "sysext", "--introspect-cli")
	for _, want := range []string{
		`{"mediaType":"application/vnd.io.systemd.cli-introspection-0","commands":[{"names":["sysext"],"project":"sysext-alpine",`,
		`{"names":["status"],"abstract":["Show current merge status (default)"],"maxArguments":1,"isDefault":true}`,
		`{"names":["--root"],"argument":"required_argument","metavar":"PATH","help":"Operate relative to root PATH"}`,
		`{"names":["--introspect-cli"],"argument":"no_argument"}`,
		`{"names":["help"]}]},{"names":["confext"]`,
	} {
		if rc != 0 || !strings.Contains(stdout, want) {
			t.Errorf("--introspect-cli lacks %s:\n%s", want, stdout)
		}
	}
}

func TestVerbErrors(t *testing.T) {
	hermetic(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"frob"}, "Unknown command verb 'frob', did you mean 'merge'?\n"},
		{[]string{"stat"}, "Unknown command verb 'stat', did you mean 'status'?\n"},
		{[]string{"-"}, "Unknown command verb '-', did you mean 'list'?\n"},
		{[]string{"refrseh"}, "Unknown command verb 'refrseh', did you mean 'refresh'?\n"},
		{[]string{"supercalifragilistic"}, "Unknown command verb 'supercalifragilistic'.\n"},
		{[]string{"merge", "unmerge"}, "Too many arguments.\n"},
		{[]string{"status", "x"}, "Too many arguments.\n"},
	} {
		stdout, stderr, rc := runCLI(t, append([]string{"sysext"}, tc.args...)...)
		if rc != 1 || stdout != "" || stderr != tc.want {
			t.Errorf("%q: rc=%d stdout=%q stderr=%q, want %q", tc.args, rc, stdout, stderr, tc.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	for _, tc := range []struct {
		x, y string
		want int
	}{
		{"", "", 0}, {"abc", "", 3}, {"", "ab", 2}, {"merge", "merge", 0},
		{"merge", "mrege", 1}, {"status", "stat", 2}, {"list", "-", 4}, {"merge", "frob", 4},
	} {
		if got := levenshtein(tc.x, tc.y); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.x, tc.y, got, tc.want)
		}
	}
}

func TestLogLevel(t *testing.T) {
	hermetic(t)
	for _, tc := range []struct {
		env   string
		level int
		warn  bool
	}{
		{"debug", logDebug, false},
		{"4", logWarning, false},
		{"notice", logNotice, false},
		{"err", logErr, false},
		{"console:debug", logInfo, false},
		{"console:debug,debug", logDebug, false},
		{"debug,console:err", logErr, false},
		{"kmsg:err", logInfo, false},
		{"bogus", logInfo, true},
		{"8", logInfo, true},
		{"foo:err", logInfo, true},
	} {
		t.Setenv("SYSTEMD_LOG_LEVEL", tc.env)
		var stderr bytes.Buffer
		l := newLogger(&stderr)
		want := ""
		if tc.warn {
			want = "Failed to parse log level '" + tc.env + "', ignoring: Invalid argument\n"
		}
		if l.level != tc.level || stderr.String() != want {
			t.Errorf("SYSTEMD_LOG_LEVEL=%s: level %d stderr %q, want %d %q", tc.env, l.level, stderr.String(), tc.level, want)
		}
	}
	os.Unsetenv("SYSTEMD_LOG_LEVEL")
	t.Setenv("DEBUG_INVOCATION", "1")
	if l := newLogger(&bytes.Buffer{}); l.level != logDebug {
		t.Error("DEBUG_INVOCATION=1 must enable debug logging")
	}

	var b bytes.Buffer
	l := &logger{w: &b, level: logNotice}
	l.Debugf("d")
	l.Infof("i")
	l.Noticef("n")
	l.Warnf("w %d", 1)
	l.Errorf("e")
	if b.String() != "n\nw 1\ne\n" {
		t.Errorf("filtered output = %q", b.String())
	}
}

func TestKillSwitch(t *testing.T) {
	hermetic(t)
	root := t.TempDir()
	notice := "Disabled by the kernel command line option 'systemd.sysext=', skipping execution.\n"
	t.Setenv("SYSTEMD_PROC_CMDLINE", "quiet systemd.sysext=0")

	stdout, stderr, rc := runCLI(t, "sysext", "--root="+root, "status")
	if rc != 0 || stderr != "" || !strings.HasPrefix(stdout, "HIERARCHY") {
		t.Errorf("manual invocation must not be affected: rc=%d %q %q", rc, stdout, stderr)
	}
	t.Setenv("RC_SVCNAME", "sysext")
	for _, args := range [][]string{{"status"}, {"merge"}, {"frob"}, {"list", "x"}} {
		stdout, stderr, rc := runCLI(t, append([]string{"sysext", "--root=" + root}, args...)...)
		if rc != 0 || stdout != "" || stderr != notice {
			t.Errorf("%q under OpenRC: rc=%d stdout=%q stderr=%q", args, rc, stdout, stderr)
		}
	}
	if stdout, _, rc := runCLI(t, "sysext", "--json=help"); rc != 0 || stdout == "" {
		t.Error("options are handled before the kill switch")
	}
	if _, stderr, _ := runCLI(t, "confext", "--root="+root, "status"); stderr != "" {
		t.Errorf("systemd.sysext= does not disable confext: %q", stderr)
	}
	t.Setenv("SYSTEMD_PROC_CMDLINE", "systemd.confext=no")
	if _, stderr, rc := runCLI(t, "confext", "--root="+root, "status"); rc != 0 ||
		stderr != "Disabled by the kernel command line option 'systemd.confext=', skipping execution.\n" {
		t.Errorf("confext: rc=%d %q", rc, stderr)
	}
	t.Setenv("SYSTEMD_PROC_CMDLINE", "systemd.sysext=0 systemd.sysext=1")
	if _, stderr, _ := runCLI(t, "sysext", "--root="+root, "status"); stderr != "" {
		t.Errorf("last value wins: %q", stderr)
	}
	t.Setenv("SYSTEMD_PROC_CMDLINE", "systemd.sysext=banana")
	t.Setenv("SYSTEMD_LOG_LEVEL", "debug")
	if _, stderr, rc := runCLI(t, "sysext", "--root="+root, "status"); rc != 0 ||
		!strings.HasPrefix(stderr, "Failed to check 'systemd.sysext=' kernel command line option, proceeding: Invalid argument\n") {
		t.Errorf("invalid value proceeds: rc=%d %q", rc, stderr)
	}
	os.Unsetenv("SYSTEMD_LOG_LEVEL")

	t.Setenv("SYSTEMD_IN_INITRD", "1")
	t.Setenv("SYSTEMD_PROC_CMDLINE", "rd.systemd.sysext=0")
	if _, stderr, _ := runCLI(t, "sysext", "list"); stderr != "Disabled by the kernel command line option 'rd.systemd.sysext=', skipping execution.\n" {
		t.Errorf("initrd uses the rd. switch: %q", stderr)
	}
	if _, stderr, _ := runCLI(t, "sysext", "--root="+root, "list"); stderr != "No OS extensions found.\n" {
		t.Errorf("with --root= the rd. switch does not apply: %q", stderr)
	}
}

func TestStatusAndListRoot(t *testing.T) {
	hermetic(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, rc := runCLI(t, "sysext", "--root="+root, "status")
	if rc != 0 || stderr != "" || stdout != "HIERARCHY EXTENSIONS SINCE\n/usr      -          -\n" {
		t.Errorf("status: rc=%d stderr=%q stdout:\n%s", rc, stderr, stdout)
	}
	stdout, _, _ = runCLI(t, "sysext", "--root="+root, "status", "--json=short")
	if stdout != `[{"hierarchy":"/usr","extensions":[],"since":null}]`+"\n" {
		t.Errorf("status json = %q", stdout)
	}
	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "/usr:/nonexistent")
	if stdout, _, _ = runCLI(t, "sysext", "--root="+root, "--no-legend"); stdout != "/usr - -\n" {
		t.Errorf("missing hierarchies are left out: %q", stdout)
	}
	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "relative")
	if _, stderr, rc = runCLI(t, "sysext", "--root="+root, "status"); rc != 1 || stderr != "Failed to determine sysext hierarchies: Invalid argument\n" {
		t.Errorf("invalid hierarchies: rc=%d %q", rc, stderr)
	}
	if _, stderr, rc = runCLI(t, "sysext", "--root="+root, "list"); rc != 0 || stderr != "No OS extensions found.\n" {
		t.Errorf("list does not need hierarchies: rc=%d %q", rc, stderr)
	}
	os.Unsetenv("SYSTEMD_SYSEXT_HIERARCHIES")

	stdout, stderr, rc = runCLI(t, "sysext", "--root="+root, "list")
	if rc != 0 || stdout != "" || stderr != "No OS extensions found.\n" {
		t.Errorf("empty list: rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
	if stdout, stderr, _ = runCLI(t, "sysext", "--root="+root, "list", "--json=pretty"); stdout != "[]\n" || stderr != "" {
		t.Errorf("empty list json: %q %q", stdout, stderr)
	}

	missing := filepath.Join(root, "missing")
	if _, stderr, rc = runCLI(t, "sysext", "--root="+missing, "list"); rc != 1 || stderr != "Failed to discover images: No such file or directory\n" {
		t.Errorf("list on a missing root: rc=%d %q", rc, stderr)
	}
	stdout, stderr, rc = runCLI(t, "sysext", "--root="+missing, "status")
	wantErr := "Failed to open root directory '" + missing + "': No such file or directory\n" +
		"Failed to parse sysext config file, ignoring: No such file or directory\n"
	if rc != 0 || stdout != "HIERARCHY EXTENSIONS SINCE\n" || stderr != wantErr {
		t.Errorf("status on a missing root: rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
}

func TestLoadContext(t *testing.T) {
	hermetic(t)
	root := t.TempDir()
	writeFile(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=ephemeral\nImagePolicy=root=verity+bogus:bogus=open\n")
	ctx := func(args ...string) (*cli, string) {
		t.Helper()
		var stderr bytes.Buffer
		c := &cli{stdout: &bytes.Buffer{}, log: &logger{w: &stderr, level: logInfo}}
		if _, err := c.parseArgs(append([]string{"sysext", "--root=" + root}, args...)); err != nil {
			t.Fatal(err)
		}
		if err := c.loadContext(); err != nil {
			t.Fatal(err)
		}
		return c, stderr.String()
	}

	c, stderr := ctx()
	if c.cfg.mutable != "ephemeral" || c.cfg.imagePolicy != "=ignore:root=verity" || stderr != "" {
		t.Errorf("config: mutable=%q policy=%q stderr=%q", c.cfg.mutable, c.cfg.imagePolicy, stderr)
	}
	if len(c.hierarchies) != 2 || c.mountOptions != nil {
		t.Errorf("hierarchies=%q mountOptions=%v", c.hierarchies, c.mountOptions)
	}

	t.Setenv("SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS", "")
	if c, _ = ctx(); c.mountOptions == nil || *c.mountOptions != "" {
		t.Errorf("empty mount options from the environment = %v, want set and empty", c.mountOptions)
	}
	t.Setenv("SYSTEMD_SYSEXT_MUTABLE_MODE", "no")
	t.Setenv("SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS", "xino=off")
	if c, _ = ctx(); c.cfg.mutable != "no" || c.mountOptions == nil || *c.mountOptions != "xino=off" {
		t.Errorf("environment beats config: %q %v", c.cfg.mutable, c.mountOptions)
	}
	if c, _ = ctx("--mutable=auto", "--image-policy=*"); c.cfg.mutable != "auto" || c.cfg.imagePolicy != "*" {
		t.Errorf("command line beats environment: %q %q", c.cfg.mutable, c.cfg.imagePolicy)
	}
	t.Setenv("SYSTEMD_SYSEXT_MUTABLE_MODE", "bogus")
	c, stderr = ctx()
	if c.cfg.mutable != "ephemeral" || stderr != "Failed to parse SYSTEMD_SYSEXT_MUTABLE_MODE environment variable value 'bogus'. Ignoring.\n" {
		t.Errorf("invalid environment value: %q %q", c.cfg.mutable, stderr)
	}
	t.Setenv("SYSTEMD_SYSEXT_MUTABLE_MODE", "help")
	if _, stderr = ctx("--mutable=yes"); !strings.Contains(stderr, "value 'help'. Ignoring.") {
		t.Errorf("the environment is checked even with --mutable=: %q", stderr)
	}
	t.Setenv("SYSTEMD_CONFEXT_MUTABLE_MODE", "yes")
	os.Unsetenv("SYSTEMD_SYSEXT_MUTABLE_MODE")
	if c, _ = ctx(); c.cfg.mutable != "ephemeral" {
		t.Errorf("sysext must not read SYSTEMD_CONFEXT_MUTABLE_MODE: %q", c.cfg.mutable)
	}

	writeFile(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=banana\n")
	c, stderr = ctx()
	if c.cfg.mutable != "no" || stderr != "/etc/systemd/sysext.conf:2: Failed to parse Mutable=banana, ignoring: Invalid argument\n" {
		t.Errorf("invalid config value: %q %q", c.cfg.mutable, stderr)
	}
	writeFile(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=yes\nImagePolicy=root=verity:root=signed\n")
	if c, stderr = ctx(); c.cfg.mutable != "no" || !strings.HasSuffix(stderr, "Failed to parse sysext config file, ignoring: Name not unique on network\n") {
		t.Errorf("an invalid policy discards the whole config: %q %q", c.cfg.mutable, stderr)
	}
}

func TestPrivileges(t *testing.T) {
	hermetic(t)
	old := haveCapSysAdmin
	t.Cleanup(func() { haveCapSysAdmin = old })
	haveCapSysAdmin = func() (bool, error) { return false, nil }
	root := t.TempDir()
	writeFile(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=banana\n")
	for _, verb := range []string{"merge", "unmerge", "refresh"} {
		stdout, stderr, rc := runCLI(t, "sysext", "--root="+root, verb)
		if rc != 1 || stdout != "" || stderr != "Need to be privileged.\n" {
			t.Errorf("%s: rc=%d stdout=%q stderr=%q", verb, rc, stdout, stderr)
		}
	}
	if _, stderr, rc := runCLI(t, "sysext", "--root="+root, "status"); rc != 0 || strings.Contains(stderr, "privileged") {
		t.Errorf("status needs no privileges: %q", stderr)
	}
	if _, err := old(); err != nil {
		t.Errorf("capget: %v", err)
	}
}

func TestErrorText(t *testing.T) {
	if got := errorText(failMsg("sysext: lower")); got != "sysext: lower" {
		t.Errorf("failure = %q", got)
	}
	if got := errorText(&overlay.AlreadyMergedError{Hierarchy: "/usr"}); got != "Hierarchy '/usr' is already merged." {
		t.Errorf("already merged = %q", got)
	}
	if got := errorText(os.ErrNotExist); got != "No such file or directory" {
		t.Errorf("plain error = %q", got)
	}
}

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	extconf "github.com/itxaka/sysext-alpine/internal/config"
	"github.com/itxaka/sysext-alpine/internal/image"
	"github.com/itxaka/sysext-alpine/internal/overlay"
	"github.com/itxaka/sysext-alpine/internal/release"
	"github.com/itxaka/sysext-alpine/internal/service"
)

// version is the build version, injected via -ldflags "-X main.version=...".
var version = "0.1.0"

// systemdVersion is the systemd-sysext release whose interface is
// implemented.
const systemdVersion = "262"

// JSON output modes (--json=).
const (
	jsonOff    = "off"
	jsonShort  = "short"
	jsonPretty = "pretty"
)

// option is one command line option, in systemd's option table order.
type option struct {
	short   byte
	long    string
	metavar string // argument name; "" for options without argument
	help    string // "" hides the option from --help
}

var options = []option{
	{'h', "help", "", "Show this help"},
	{0, "version", "", "Show package version"},
	{0, "root", "PATH", "Operate relative to root PATH"},
	{0, "mutable", "MODE", "Specify a mutability mode (yes, no, auto, import, ephemeral, ephemeral-import, help)"},
	{0, "image-policy", "POLICY", "Specify disk image dissection policy"},
	{0, "noexec", "BOOL", "Whether to mount extension overlay with noexec"},
	{0, "force", "", "Ignore version incompatibilities"},
	{0, "no-reload", "", "Do not reload the service manager (OpenRC)"},
	{0, "always-refresh", "BOOL", "Whether to refresh when no changes were found"},
	{0, "no-pager", "", "Do not start a pager"},
	{0, "no-legend", "", "Do not show headers and footers"},
	{0, "json", "FORMAT", "Generate JSON output (pretty, short, or off)"},
	{0, "confext", "", "Operate on configuration extensions in /etc/"},
	{0, "introspect-cli", "", ""},
}

// verb is one command verb; the first is the default.
type verb struct {
	name string
	help string // "" hides the verb from --help
}

var verbs = []verb{
	{"status", "Show current merge status (default)"},
	{"merge", "Merge extensions into relevant hierarchies"},
	{"unmerge", "Unmerge extensions from relevant hierarchies"},
	{"refresh", "Unmerge/merge extensions again"},
	{"list", "List installed extensions"},
	{"help", ""},
}

// mutableModes are the --mutable= modes in systemd's string table order.
var mutableModes = []string{"no", "yes", "auto", "import", "ephemeral", "ephemeral-import"}

// config is the parsed command line.
type config struct {
	progName       string // for option parser errors
	class          release.Class
	root           string // absolute --root=, "" for the host
	force          bool
	noReload       bool // --no-reload or any --root=
	alwaysRefresh  bool
	noExec         int // overlay.NoExecDefault, NoExecOff or NoExecOn
	jsonMode       string
	legend         bool
	mutable        string
	mutableSet     bool
	imagePolicy    string // "" for the class default
	imagePolicySet bool
	args           []string
}

// cli is one invocation.
type cli struct {
	cfg    *config
	stdout io.Writer
	log    *logger
	rc     *service.OpenRC

	// Set up for the verbs operating on hierarchies (loadContext).
	hierarchies  []string
	mountOptions *string // nil when the environment does not set any
}

// errorText renders an error as a log line. Failures are worded already.
// Messages from other packages are capitalized like systemd's, and an errno
// they end in is described in the C library's words instead of Go's, as
// systemd's %m does.
func errorText(err error) string {
	s := err.Error()
	if _, ok := errors.AsType[*failure](err); ok || s == "" {
		return s
	}
	if e, text := errnoOf(err); text != "" && strings.HasSuffix(s, text) {
		s = strings.TrimSuffix(s, text) + strerror(e)
	}
	return capitalize(s)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// classFromInvocation selects confext behaviour when the program is invoked
// through a name containing "confext" ($SYSTEMD_INVOKED_AS, else argv[0]),
// like systemd's invoked_as().
func classFromInvocation(argv0 string) release.Class {
	name := os.Getenv("SYSTEMD_INVOKED_AS")
	if name == "" {
		name = argv0
	}
	if strings.Contains(filepath.Base(name), "confext") {
		return release.Confext
	}
	return release.Sysext
}

// classIdentifier is "sysext" or "confext".
func classIdentifier(class release.Class) string {
	if class == release.Confext {
		return "confext"
	}
	return "sysext"
}

// parseArgs parses argv like systemd's option parser: options and positional
// arguments may be mixed, "--" ends the options, -h may be combined with
// other short options and unambiguous prefixes of long options are
// accepted. Options that print something (help, --version, --mutable=help,
// --json=help, --introspect-cli) end the program when parsed, done reports
// that.
func (c *cli) parseArgs(args []string) (done bool, err error) {
	cfg := &config{progName: "sysext", noExec: overlay.NoExecDefault, jsonMode: jsonOff, legend: true}
	if len(args) > 0 {
		cfg.progName = filepath.Base(args[0])
		cfg.class = classFromInvocation(args[0])
	}
	c.cfg = cfg
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			cfg.args = append(cfg.args, args[i+1:]...)
			return false, nil
		case strings.HasPrefix(arg, "--"):
			optname, value, hasValue := strings.Cut(arg, "=")
			opt, err := c.lookupOption(optname)
			if err != nil {
				return false, err
			}
			switch {
			case hasValue && opt.metavar == "":
				return false, failMsg("%s: option '%s' doesn't allow an argument", cfg.progName, optname)
			case !hasValue && opt.metavar != "":
				if i+1 >= len(args) {
					return false, failMsg("%s: option '%s' requires an argument", cfg.progName, optname)
				}
				i++
				value = args[i]
			}
			if done, err := c.handleOption(opt, value); done || err != nil {
				return done, err
			}
		case len(arg) > 1 && arg[0] == '-':
			// -h is the only short option and ends the parsing.
			if arg[1] != 'h' {
				return false, failMsg("%s: unrecognized option '-%c'", cfg.progName, arg[1])
			}
			return c.handleOption(options[0], "")
		default:
			cfg.args = append(cfg.args, arg)
		}
	}
	return false, nil
}

// lookupOption finds the long option optname ("--name"): an exact match, or
// the only option the name is a prefix of.
func (c *cli) lookupOption(optname string) (option, error) {
	name := optname[2:]
	if name == "" {
		return option{}, failMsg("%s: unrecognized option '%s'", c.cfg.progName, optname)
	}
	var partial []option
	for _, o := range options {
		if o.long == name {
			return o, nil
		}
		if strings.HasPrefix(o.long, name) {
			partial = append(partial, o)
		}
	}
	switch len(partial) {
	case 0:
		return option{}, failMsg("%s: unrecognized option '%s'", c.cfg.progName, optname)
	case 1:
		return partial[0], nil
	}
	names := make([]string, len(partial))
	for i, o := range partial {
		names[i] = "--" + o.long
	}
	return option{}, failMsg("%s: option '%s' is ambiguous; possibilities: %s", c.cfg.progName, optname, strings.Join(names, ", "))
}

func (c *cli) handleOption(opt option, value string) (done bool, err error) {
	cfg := c.cfg
	switch opt.long {
	case "help":
		c.printHelp()
		return true, nil
	case "version":
		fmt.Fprintf(c.stdout, "sysext-alpine %s (systemd-sysext %s)\n", version, systemdVersion)
		return true, nil
	case "root":
		cfg.root = ""
		if value != "" {
			abs, err := filepath.Abs(value)
			if err != nil {
				return false, failf(err, "Failed to parse path \"%s\" and make it absolute", value)
			}
			cfg.root = abs
		}
		// With --root= the service manager of the host has nothing to do
		// with the extensions.
		cfg.noReload = true
	case "mutable":
		if value == "help" {
			if cfg.legend {
				fmt.Fprintln(c.stdout, "Known mutability modes:")
			}
			fmt.Fprintln(c.stdout, strings.Join(mutableModes, "\n"))
			return true, nil
		}
		m, err := extconf.ParseMutable(value)
		if err != nil {
			return false, failMsg("Failed to parse argument to --mutable=: %s", value)
		}
		cfg.mutable, cfg.mutableSet = m, true
	case "image-policy":
		if value == "" {
			value = "-"
		}
		if err := image.ValidatePolicy(value); err != nil {
			return false, policyError(err, value)
		}
		cfg.imagePolicy, cfg.imagePolicySet = value, true
	case "noexec":
		b, err := parseBooleanArgument(opt.long, value)
		if err != nil {
			return false, err
		}
		cfg.noExec = overlay.NoExecOff
		if b {
			cfg.noExec = overlay.NoExecOn
		}
	case "force":
		cfg.force = true
	case "no-reload":
		cfg.noReload = true
	case "always-refresh":
		b, err := parseBooleanArgument(opt.long, value)
		if err != nil {
			return false, err
		}
		cfg.alwaysRefresh = b
	case "no-pager":
	case "no-legend":
		cfg.legend = false
	case "json":
		switch value {
		case jsonPretty, jsonShort, jsonOff:
			cfg.jsonMode = value
		case "help":
			fmt.Fprintln(c.stdout, "pretty\nshort\noff")
			return true, nil
		default:
			return false, failMsg("Unknown argument to --json= switch: %s", value)
		}
	case "confext":
		cfg.class = release.Confext
	case "introspect-cli":
		fmt.Fprint(c.stdout, formatJSON(introspection(), cfg.jsonMode == jsonPretty))
		return true, nil
	}
	return false, nil
}

func parseBooleanArgument(name, value string) (bool, error) {
	b, err := release.ParseBoolean(value)
	if err != nil {
		return false, failMsg("Failed to parse boolean argument to '--%s': %s", name, value)
	}
	return b, nil
}

// policyError words an invalid --image-policy= like
// parse_image_policy_argument().
func policyError(err error, policy string) error {
	switch {
	case errors.Is(err, unix.ENOTUNIQ):
		return failMsg("Duplicate rule in image policy: %s", policy)
	case errors.Is(err, unix.EBADSLT):
		return failMsg("Unknown partition type in image policy: %s", policy)
	case errors.Is(err, unix.EBADRQC):
		return failMsg("Unknown partition policy flag in image policy: %s", policy)
	}
	return failMsg("Failed to parse image policy: %s", policy)
}

// disabledByCmdline implements the kernel command line switch
// systemd.sysext= (systemd.confext=, rd.-prefixed in the initrd): when it
// is false and the program runs as a service, nothing is done.
func (c *cli) disabledByCmdline() bool {
	inInitrd := c.cfg.root == "" && service.InInitrd()
	key := "systemd." + classIdentifier(c.cfg.class)
	if inInitrd {
		key = "rd." + key
	}
	words, err := service.KernelCmdline()
	enabled := true
	if err == nil {
		enabled, err = service.CmdlineBool(words, key, inInitrd)
	}
	if err != nil {
		c.log.Debugf("Failed to check '%s=' kernel command line option, proceeding: %s", key, strerror(err))
		return false
	}
	if enabled || !service.InvokedByServiceManager() {
		return false
	}
	c.log.Noticef("Disabled by the kernel command line option '%s=', skipping execution.", key)
	return true
}

func (c *cli) dispatch() error {
	name := verbs[0].name
	if len(c.cfg.args) > 0 {
		name = c.cfg.args[0]
	}
	if name == "help" {
		c.printHelp()
		return nil
	}
	known := false
	for _, v := range verbs {
		known = known || v.name == name
	}
	if !known {
		if closest := closestVerb(name); closest != "" {
			return failMsg("Unknown command verb '%s', did you mean '%s'?", name, closest)
		}
		return failMsg("Unknown command verb '%s'.", name)
	}
	if len(c.cfg.args) > 1 {
		return failMsg("Too many arguments.")
	}
	switch name {
	case "merge":
		return c.cmdMerge()
	case "unmerge":
		return c.cmdUnmerge()
	case "refresh":
		return c.cmdRefresh()
	case "list":
		return c.cmdList()
	}
	return c.cmdStatus()
}

// closestVerb is strv_find_closest(): the verb name starts with with the
// fewest characters left over, else the nearest by Levenshtein distance (at
// most 5).
func closestVerb(name string) string {
	best, bestLen := "", -1
	for _, v := range verbs {
		if rest, ok := strings.CutPrefix(v.name, name); ok && (bestLen < 0 || len(rest) < bestLen) {
			best, bestLen = v.name, len(rest)
		}
	}
	if best != "" {
		return best
	}
	bestDist := -1
	for _, v := range verbs {
		d := levenshtein(v.name, name)
		if d <= 5 && (bestDist < 0 || d < bestDist) {
			best, bestDist = v.name, d
		}
	}
	return best
}

// levenshtein is systemd's strlevenshtein(), which also counts a swap of
// two adjacent characters as one edit.
func levenshtein(x, y string) int {
	if x == y {
		return 0
	}
	if x == "" {
		return len(y)
	}
	if y == "" {
		return len(x)
	}
	t0 := make([]int, len(y)+1)
	t1 := make([]int, len(y)+1)
	t2 := make([]int, len(y)+1)
	for i := range t1 {
		t1[i] = i
	}
	for i := range len(x) {
		t2[0] = i + 1
		for j := range len(y) {
			t2[j+1] = t1[j]
			if x[i] != y[j] {
				t2[j+1]++
			}
			if i > 0 && j > 0 && x[i-1] == y[j] && x[i] == y[j-1] && t2[j+1] > t0[j-1]+1 {
				t2[j+1] = t0[j-1] + 1
			}
			t2[j+1] = min(t2[j+1], t1[j+1]+1, t2[j]+1)
		}
		t0, t1, t2 = t1, t2, t0
	}
	return t1[len(y)]
}

// loadContext is systemd's context_from_cmdline(): the hierarchies, then
// the mutable mode and overlayfs mount options from the environment, then
// the configuration files; the command line wins over the environment,
// which wins over the configuration. Problems with the environment or the
// configuration are only warned about.
func (c *cli) loadContext() error {
	class := c.cfg.class
	id := classIdentifier(class)
	hierarchies, err := overlay.Hierarchies(class)
	if err != nil {
		return failf(err, "Failed to determine %s hierarchies", id)
	}
	c.hierarchies = hierarchies

	envSet := false
	modeEnv := "SYSTEMD_" + strings.ToUpper(id) + "_MUTABLE_MODE"
	if v, ok := os.LookupEnv(modeEnv); ok {
		m, err := extconf.ParseMutable(v)
		switch {
		case err != nil:
			c.log.Warnf("Failed to parse %s environment variable value '%s'. Ignoring.", modeEnv, v)
		case !c.cfg.mutableSet:
			c.cfg.mutable, envSet = m, true
		}
	}
	if v, ok := os.LookupEnv("SYSTEMD_" + strings.ToUpper(id) + "_OVERLAYFS_MOUNT_OPTIONS"); ok {
		c.mountOptions = &v
	}

	fileCfg, messages := extconf.Load(class, c.cfg.root, func(s string) error {
		_, err := image.NormalizePolicy(s)
		return err
	})
	for _, m := range messages {
		c.log.Warnf("%s", m)
	}
	if !c.cfg.mutableSet && !envSet {
		c.cfg.mutable = fileCfg.Mutable
		if c.cfg.mutable == "" {
			c.cfg.mutable = "no"
		}
	}
	if !c.cfg.imagePolicySet {
		c.cfg.imagePolicy, _ = image.NormalizePolicy(fileCfg.ImagePolicy)
	}
	return nil
}

// failure is an error worded like the systemd-sysext message it mirrors.
type failure struct {
	msg string
	err error
}

func (f *failure) Error() string { return f.msg }
func (f *failure) Unwrap() error { return f.err }

// failMsg builds a failure with a fixed message.
func failMsg(format string, args ...any) error {
	return &failure{msg: fmt.Sprintf(format, args...)}
}

// failf builds a failure "MESSAGE: STRERROR", like systemd's %m.
func failf(err error, format string, args ...any) error {
	return &failure{msg: fmt.Sprintf(format, args...) + ": " + strerror(err), err: err}
}

// errnoSentinels are the io/fs errors Go reports some errnos as.
var errnoSentinels = []struct {
	err   error
	errno unix.Errno
}{
	{fs.ErrNotExist, unix.ENOENT},
	{fs.ErrExist, unix.EEXIST},
	{fs.ErrPermission, unix.EACCES},
	{fs.ErrInvalid, unix.EINVAL},
	{fs.ErrClosed, unix.EBADF},
}

// errnoOf returns the errno behind err and how Go words it in err's message:
// the first unix.Errno in the chain, else the io/fs error err matches; 0 and
// "" when err carries no errno.
func errnoOf(err error) (unix.Errno, string) {
	if e, ok := errors.AsType[unix.Errno](err); ok {
		return e, e.Error()
	}
	for _, s := range errnoSentinels {
		if errors.Is(err, s.err) {
			return s.errno, s.err.Error()
		}
	}
	return 0, ""
}

// strerror renders the errno behind err like the C library's strerror(),
// systemd's %m. An error without one is reported as EINVAL: the Go wording
// of an error never ends up in a message systemd words with an errno.
func strerror(err error) string {
	e, _ := errnoOf(err)
	if e == 0 {
		e = unix.EINVAL
	}
	return capitalize(e.Error())
}

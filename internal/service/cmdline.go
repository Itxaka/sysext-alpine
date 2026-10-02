package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// InvokedByServiceManager reports whether the process runs as part of a
// service: started by OpenRC (RC_SVCNAME is exported to init scripts) or
// directly by systemd (SYSTEMD_EXEC_PID names this process, "*" is accepted
// for testing like invoked_by_systemd()).
func InvokedByServiceManager() bool {
	if os.Getenv("RC_SVCNAME") != "" {
		return true
	}
	pid := os.Getenv("SYSTEMD_EXEC_PID")
	return pid == "*" || pid == strconv.Itoa(os.Getpid())
}

// InInitrd is systemd's in_initrd(): $SYSTEMD_IN_INITRD when it is a
// boolean, otherwise whether /etc/initrd-release exists.
func InInitrd() bool {
	if v, ok := os.LookupEnv("SYSTEMD_IN_INITRD"); ok {
		if b, err := release.ParseBoolean(v); err == nil {
			return b
		}
	}
	_, err := os.Stat(initrdRelease)
	return err == nil
}

var initrdRelease = "/etc/initrd-release"

// hostRoot prefixes the paths KernelCmdline reads; tests point it
// elsewhere.
var hostRoot = "/"

var getpid = os.Getpid

func hostPath(p string) string { return filepath.Join(hostRoot, p) }

// KernelCmdline returns the kernel command line words like systemd's
// proc_cmdline_strv_internal() with PID 1 option filtering:
// $SYSTEMD_PROC_CMDLINE when set, inside a container the arguments of PID 1
// without the options of systemd's own command line (container managers
// pass the kernel command line options there), /proc/cmdline otherwise.
func KernelCmdline() ([]string, error) {
	if v, ok := os.LookupEnv("SYSTEMD_PROC_CMDLINE"); ok {
		return splitCmdline(v), nil
	}
	if inContainer() {
		data, err := os.ReadFile(hostPath("/proc/1/cmdline"))
		if err != nil {
			return nil, err
		}
		args := splitNulstr(data)
		if len(args) == 0 {
			return nil, fmt.Errorf("PID 1 has no command line: %w", unix.ENOENT)
		}
		return filterPID1Args(args), nil
	}
	data, err := os.ReadFile(hostPath("/proc/cmdline"))
	if err != nil {
		return nil, err
	}
	return splitCmdline(string(data)), nil
}

// inContainer is systemd's detect_container() > 0 with its cheap and
// reliable signals, in its order: /run/host/container-manager, $container
// (ours when we are PID 1, else /run/systemd/container or PID 1's), then
// the /run/.containerenv and /.dockerenv marker files. The OpenVZ, WSL,
// proot and PID namespace heuristics are left out. Like there, a file that
// cannot be read means no container.
func inContainer() bool {
	if found, err := nonEmptyFile("/run/host/container-manager"); found || err != nil {
		return found
	}
	if getpid() == 1 {
		if v, ok := os.LookupEnv("container"); ok {
			return v != ""
		}
	} else {
		if found, err := nonEmptyFile("/run/systemd/container"); found || err != nil {
			return found
		}
		if pid1HasEnv("container") {
			return true
		}
	}
	for _, f := range []string{"/run/.containerenv", "/.dockerenv"} {
		if _, err := os.Stat(hostPath(f)); err == nil {
			return true
		}
	}
	return false
}

// nonEmptyFile reports whether the file has any content; a missing file is
// not an error.
func nonEmptyFile(p string) (bool, error) {
	data, err := os.ReadFile(hostPath(p))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return len(data) > 0, err
}

// pid1HasEnv reports whether getenv_for_pid(1, name) finds the variable,
// even empty; an unreadable environment counts as not set.
func pid1HasEnv(name string) bool {
	data, err := os.ReadFile(hostPath("/proc/1/environ"))
	if err != nil {
		return false
	}
	for kv := range strings.SplitSeq(string(data), "\x00") {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// splitNulstr is strv_parse_nulstr_full() with drop_trailing_nuls: the
// NUL-separated strings, empty ones in between included.
func splitNulstr(data []byte) []string {
	s := strings.TrimRight(string(data), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

type argType int

const (
	noArgument argType = iota
	requiredArgument
	optionalArgument
)

// pid1Options are the long options of systemd's PID 1 that
// proc_cmdline_filter_pid1_args() removes from its command line.
var pid1Options = []struct {
	name   string
	hasArg argType
}{
	{"log-level", requiredArgument},
	{"log-target", requiredArgument},
	{"log-color", optionalArgument},
	{"log-location", optionalArgument},
	{"log-time", optionalArgument},
	{"unit", requiredArgument},
	{"system", noArgument},
	{"user", noArgument},
	{"test", noArgument},
	{"no-pager", noArgument},
	{"help", noArgument},
	{"version", noArgument},
	{"introspect-cli", noArgument},
	{"dump-configuration-items", noArgument},
	{"dump-bus-properties", noArgument},
	{"bus-introspect", requiredArgument},
	{"dump-core", optionalArgument},
	{"crash-chvt", requiredArgument},
	{"crash-vt", requiredArgument},
	{"crash-shell", optionalArgument},
	{"crash-reboot", optionalArgument},
	{"crash-action", requiredArgument},
	{"confirm-spawn", optionalArgument},
	{"show-status", optionalArgument},
	{"deserialize", requiredArgument},
	{"switched-root", noArgument},
	{"default-standard-output", requiredArgument},
	{"default-standard-error", requiredArgument},
	{"machine-id", requiredArgument},
	{"service-watchdogs", requiredArgument},
	{"exit-code", requiredArgument},
	{"timeout", requiredArgument},
}

const pid1ShortOptions = "hDbsz:"

// filterPID1Args is proc_cmdline_filter_pid1_args(): argv without argv[0]
// and without the options PID 1 understands (and their arguments); "--"
// ends option processing.
func filterPID1Args(argv []string) []string {
	var filtered []string
	state := noArgument
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		prev := state
		state = noArgument
		if prev == requiredArgument || (prev == optionalArgument && !strings.HasPrefix(a, "-")) {
			continue
		}
		if !strings.HasPrefix(a, "-") {
			filtered = append(filtered, a)
			continue
		}
		if long, ok := strings.CutPrefix(a, "--"); ok {
			if long == "" {
				filtered = append(filtered, argv[i+1:]...)
				break
			}
			for _, o := range pid1Options {
				q, ok := strings.CutPrefix(long, o.name)
				if !ok || (q != "" && q[0] != '=') {
					continue
				}
				if q == "" && o.hasArg == requiredArgument {
					state = requiredArgument
				}
				break
			}
			continue
		}
		for j := 1; j < len(a); j++ {
			k := strings.IndexByte(pid1ShortOptions, a[j])
			if k < 0 || k+1 >= len(pid1ShortOptions) || pid1ShortOptions[k+1] != ':' {
				continue
			}
			switch {
			case j+1 < len(a):
				state = noArgument
			case k+2 < len(pid1ShortOptions) && pid1ShortOptions[k+2] == ':':
				state = optionalArgument
			default:
				state = requiredArgument
			}
			break
		}
	}
	return filtered
}

// CmdlineBool is proc_cmdline_get_bool() with PROC_CMDLINE_TRUE_WHEN_MISSING:
// the value of the last "key=value" word is parsed with systemd's boolean
// rules; without any, the switch is true, whether the key is given bare or
// not at all. "-" and "_" in keys compare equal; "rd."-prefixed words only
// count inside the initrd.
func CmdlineBool(words []string, key string, inInitrd bool) (bool, error) {
	value, valued := "", false
	for _, w := range words {
		if strings.HasPrefix(w, "rd.") && !inInitrd {
			continue
		}
		if rest, ok := cmdlineKeyPrefix(w, key); ok && strings.HasPrefix(rest, "=") {
			value, valued = rest[1:], true
		}
	}
	if !valued {
		return true, nil
	}
	b, err := release.ParseBoolean(value)
	if err != nil {
		return false, fmt.Errorf("invalid boolean '%s' for %s=: %w", value, key, unix.EINVAL)
	}
	return b, nil
}

func cmdlineKeyPrefix(word, key string) (string, bool) {
	if len(word) < len(key) {
		return "", false
	}
	for i := range len(key) {
		a, b := word[i], key[i]
		if a != b && (a != '-' || b != '_') && (a != '_' || b != '-') {
			return "", false
		}
	}
	return word[len(key):], true
}

// splitCmdline splits like systemd's strv_split_full() with
// EXTRACT_UNQUOTE|EXTRACT_RELAX|EXTRACT_RETAIN_ESCAPE: whitespace separates
// words, quotes group and are removed, backslashes are kept.
func splitCmdline(s string) []string {
	var words []string
	var b strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words = append(words, b.String())
				b.Reset()
				inWord = false
			}
		case c == '\\' && i+1 < len(s):
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i++
			inWord = true
		default:
			b.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, b.String())
	}
	return words
}

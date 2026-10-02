package image

import (
	"os"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/service"
)

// parseBoolean mirrors systemd's parse_boolean().
func parseBoolean(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "1", "yes", "y", "true", "t", "on":
		return true, true
	case "0", "no", "n", "false", "f", "off":
		return false, true
	}
	return false, false
}

// envDisabled reports whether the environment variable is set to a false
// boolean. Unset or unparsable values leave the feature it guards enabled,
// matching systemd's "r == 0" checks of secure_getenv_bool().
func envDisabled(name string) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false
	}
	b, ok := parseBoolean(v)
	return ok && !b
}

// envStrictlyEnabled reports whether the environment variable is unset or
// set to a true boolean; unparsable values count as false.
func envStrictlyEnabled(name string) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return true
	}
	b, ok := parseBoolean(v)
	return ok && b
}

// cmdlineBool reads a boolean kernel command line switch like systemd's
// proc_cmdline_get_bool() with PROC_CMDLINE_TRUE_WHEN_MISSING. A command
// line that cannot be read or a value that cannot be parsed is false, like
// the failures systemd refuses on.
func cmdlineBool(key string) bool {
	words, err := service.KernelCmdline()
	if err != nil {
		return false
	}
	b, err := service.CmdlineBool(words, key, service.InInitrd())
	return err == nil && b
}

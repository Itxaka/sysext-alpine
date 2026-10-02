package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Logger receives the messages systemd-sysext would log.
type Logger interface {
	Debugf(format string, args ...any)
	Warnf(format string, args ...any)
}

// OpenRC drives OpenRC through rc-update(8) and rc-service(8).
type OpenRC struct {
	Log Logger
	// Run executes a command and returns its combined output; nil runs it
	// for real. Tests substitute it.
	Run func(name string, args ...string) ([]byte, error)
	// RunDir is OpenRC's state directory; "" means /run/openrc.
	RunDir string
}

func (o *OpenRC) run(name string, args ...string) ([]byte, error) {
	if o.Run != nil {
		return o.Run(name, args...)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	return exec.Command(path, args...).CombinedOutput()
}

// Available reports whether OpenRC booted the system, i.e. its state
// directory holds the softlevel file openrc-run checks for, and rc-service is
// installed.
func (o *OpenRC) Available() bool {
	dir := o.RunDir
	if dir == "" {
		dir = "/run/openrc"
	}
	if _, err := os.Stat(filepath.Join(dir, "softlevel")); err != nil {
		return false
	}
	if o.Run != nil {
		return true
	}
	_, err := exec.LookPath("rc-service")
	return err == nil
}

// ReloadManager refreshes the OpenRC dependency cache (rc-update -u), the
// counterpart of a systemd daemon-reload: init scripts added or removed by
// extensions become known.
func (o *OpenRC) ReloadManager() {
	if out, err := o.run("rc-update", "-u"); err != nil {
		o.Log.Warnf("Failed to reload the service manager: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

func (o *OpenRC) exists(svc string) bool {
	_, err := o.run("rc-service", "-e", svc)
	return err == nil
}

func (o *OpenRC) started(svc string) bool {
	_, err := o.run("rc-service", svc, "status")
	return err == nil
}

// scriptPath resolves the init script of svc ("" when it does not exist).
func (o *OpenRC) scriptPath(svc string) string {
	out, err := o.run("rc-service", "-r", svc)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// canReload reports whether the init script of svc implements reload.
// openrc-run's template reload() does nothing and succeeds for services
// without a supervisor, so only scripts defining their own reload() count.
func (o *OpenRC) canReload(svc string) bool {
	script := o.scriptPath(svc)
	if script == "" {
		return false
	}
	data, err := os.ReadFile(script)
	return err == nil && reloadFunc.Match(data)
}

var reloadFunc = regexp.MustCompile(`(?m)^[ \t]*reload[ \t]*\(\)`)

func (o *OpenRC) do(verb, svc string) error {
	out, err := o.run("rc-service", svc, verb)
	if err != nil {
		if msg := lastLine(out); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Apply runs the actions after a merge, refresh or unmerge: the dependency
// cache refresh first, then the restarts (which start stopped services, like
// RestartUnit), then the reload-or-restarts (a started service whose script
// implements reload is reloaded, any other one restarted). Units whose init
// script does not exist are skipped, failures only warn.
func (o *OpenRC) Apply(a Actions) {
	if a.Reload {
		o.ReloadManager()
	}
	o.dispatch(a.Restart, "RestartUnit", func(svc string) error {
		return o.do("restart", svc)
	})
	o.dispatch(a.ReloadOrRestart, "ReloadOrRestartUnit", func(svc string) error {
		if o.started(svc) && o.canReload(svc) {
			if err := o.do("reload", svc); err == nil {
				return nil
			}
		}
		return o.do("restart", svc)
	})
}

func (o *OpenRC) dispatch(units []string, method string, fn func(svc string) error) {
	for _, unit := range units {
		svc, ok := ServiceName(unit)
		if !ok {
			o.Log.Warnf("Unit '%s' is not a service, OpenRC cannot %s it, ignoring.", unit, method)
			continue
		}
		if !o.exists(svc) {
			o.Log.Debugf("Unit '%s' is already gone, nothing to stop.", unit)
			continue
		}
		if err := fn(svc); err != nil {
			o.Log.Warnf("Failed to %s unit '%s': %v", method, unit, err)
		}
	}
}

// StopProvidedBy stops the started services among the listed units whose
// init script is shipped by one of the extension trees. It runs before an
// unmerge: once the script disappears OpenRC can no longer stop the service,
// whereas systemd falls back to StopUnit on the still-loaded unit.
func (o *OpenRC) StopProvidedBy(a Actions, trees []string) {
	for _, unit := range append(append([]string(nil), a.Restart...), a.ReloadOrRestart...) {
		svc, ok := ServiceName(unit)
		if !ok {
			continue
		}
		script := o.scriptPath(svc)
		if script == "" || !providedBy(script, trees) || !o.started(svc) {
			continue
		}
		if err := o.do("stop", svc); err != nil {
			o.Log.Warnf("Failed to StopUnit unit '%s': %v", unit, err)
		}
	}
}

func providedBy(script string, trees []string) bool {
	for _, t := range trees {
		if _, err := os.Lstat(filepath.Join(t, script)); err == nil {
			return true
		}
	}
	return false
}

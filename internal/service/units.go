// Package service maps systemd-sysext's service manager interactions
// (EXTENSION_RELOAD_MANAGER=, EXTENSION_RESTART_UNITS=,
// EXTENSION_RELOAD_OR_RESTART_UNITS= and the systemd.sysext= kernel command
// line switch) onto OpenRC.
package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/release"
)

const unitNameMax = 256

var unitTypes = []string{
	"service", "socket", "target", "device", "mount", "automount",
	"swap", "timer", "path", "slice", "scope",
}

// UnitNameIsValid is systemd's unit_name_is_valid() with
// UNIT_NAME_PLAIN|UNIT_NAME_INSTANCE: "foo.service" and "foo@bar.service" are
// valid, templates ("foo@.service") are not.
func UnitNameIsValid(n string) bool {
	if n == "" || len(n) >= unitNameMax {
		return false
	}
	e := strings.LastIndexByte(n, '.')
	if e <= 0 || !slices.Contains(unitTypes, n[e+1:]) {
		return false
	}
	at := -1
	for i := range e {
		c := n[i]
		if c == '@' && at < 0 {
			at = i
		}
		if !validUnitChar(c) {
			return false
		}
	}
	switch {
	case at == 0:
		return false
	case at < 0:
		return true
	default:
		return e > at+1
	}
}

func validUnitChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("@:-_.\\", c) >= 0
}

// ServiceName maps a systemd unit name onto an OpenRC service name:
// "foo.service" → "foo", "foo@bar.service" → "foo.bar" (OpenRC's multiplexed
// service convention). Units of other types have no OpenRC equivalent.
func ServiceName(unit string) (string, bool) {
	base, ok := strings.CutSuffix(unit, ".service")
	if !ok || base == "" {
		return "", false
	}
	prefix, instance, templated := strings.Cut(base, "@")
	if templated {
		return prefix + "." + instance, true
	}
	return base, true
}

// Extension is one merged extension's name and parsed extension-release.
type Extension struct {
	Name   string
	Fields release.Fields
}

// Actions is what the service manager must do after merging, refreshing or
// unmerging a set of extensions.
type Actions struct {
	Reload          bool
	Restart         []string
	ReloadOrRestart []string
}

// Empty reports whether no action is needed.
func (a Actions) Empty() bool {
	return !a.Reload && len(a.Restart) == 0 && len(a.ReloadOrRestart) == 0
}

// CollectActions aggregates the service manager metadata of the merged
// extensions like systemd's get_extension_release_metadata(): any
// EXTENSION_RELOAD_MANAGER= true value or any listed unit requests a reload,
// and a unit listed in both unit fields is only restarted. The returned
// warnings are systemd's messages for ignored values.
func CollectActions(exts []Extension) (Actions, []string) {
	var a Actions
	var warnings []string
	restart := map[string]bool{}
	reloadOrRestart := map[string]bool{}
	for _, ext := range exts {
		if v := ext.Fields["EXTENSION_RELOAD_MANAGER"]; v != "" {
			b, err := release.ParseBoolean(v)
			switch {
			case err != nil:
				warnings = append(warnings, fmt.Sprintf("Failed to parse EXTENSION_RELOAD_MANAGER= of %s, ignoring: Invalid argument", ext.Name))
			case b:
				a.Reload = true
			}
		}
		for _, f := range []struct {
			field string
			set   map[string]bool
		}{
			{"EXTENSION_RESTART_UNITS", restart},
			{"EXTENSION_RELOAD_OR_RESTART_UNITS", reloadOrRestart},
		} {
			v := ext.Fields[f.field]
			if strings.TrimSpace(v) == "" {
				continue
			}
			a.Reload = true
			for u := range strings.FieldsSeq(v) {
				if !UnitNameIsValid(u) {
					warnings = append(warnings, fmt.Sprintf("Invalid unit name '%s' in %s= of %s, ignoring.", u, f.field, ext.Name))
					continue
				}
				f.set[u] = true
			}
		}
	}
	for u := range restart {
		delete(reloadOrRestart, u)
	}
	a.Restart = sortedKeys(restart)
	a.ReloadOrRestart = sortedKeys(reloadOrRestart)
	return a, warnings
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

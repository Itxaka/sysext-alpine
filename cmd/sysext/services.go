package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/itxaka/sysext-alpine/internal/overlay"
	"github.com/itxaka/sysext-alpine/internal/release"
	"github.com/itxaka/sysext-alpine/internal/service"
)

// extensionActions is get_extension_release_metadata(): it reads the
// extension-release files of the extensions merged into each hierarchy from
// the merged tree and collects what the service manager has to do. Nothing
// is collected with --no-reload or --root=.
func (c *cli) extensionActions() (service.Actions, error) {
	if c.cfg.noReload {
		return service.Actions{}, nil
	}
	statuses, err := overlay.CurrentStatus(c.cfg.class, c.cfg.root)
	if err != nil {
		return service.Actions{}, err
	}
	var exts []service.Extension
	for _, s := range statuses {
		if !s.Merged {
			continue
		}
		for _, name := range s.Extensions {
			fields, err := release.FindExtensionRelease("/", name, c.cfg.class, true)
			if err != nil {
				c.log.Debugf("Failed to parse extension-release metadata of %s, ignoring: %s", name, strerror(err))
				continue
			}
			exts = append(exts, service.Extension{Name: name, Fields: fields})
		}
	}
	actions, warnings := service.CollectActions(exts)
	for _, w := range warnings {
		c.log.Warnf("%s", w)
	}
	return actions, nil
}

// stopProvidedServices stops, before an unmerge, the listed services whose
// init scripts come with the merged extensions: OpenRC cannot stop them
// once the scripts are gone.
func (c *cli) stopProvidedServices(a service.Actions) {
	if len(a.Restart) == 0 && len(a.ReloadOrRestart) == 0 || !c.rc.Available() {
		return
	}
	ws, err := overlay.Workspace(c.cfg.class, c.cfg.root)
	if err != nil {
		c.log.Debugf("%s", errorText(err))
		return
	}
	entries, err := os.ReadDir(filepath.Join(ws, "extensions"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.log.Debugf("%s", errorText(err))
		}
		return
	}
	trees := make([]string, 0, len(entries))
	for _, e := range entries {
		trees = append(trees, filepath.Join(ws, "extensions", e.Name()))
	}
	c.rc.StopProvidedBy(a, trees)
}

// applyActions reloads OpenRC and restarts the services after a merge,
// refresh or unmerge; without a running OpenRC there is nothing to do.
func (c *cli) applyActions(a service.Actions) {
	if a.Empty() {
		return
	}
	if !c.rc.Available() {
		c.log.Debugf("OpenRC is not running, not reloading the service manager.")
		return
	}
	c.rc.Apply(a)
}

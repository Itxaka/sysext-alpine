package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/image"
	"github.com/itxaka/sysext-alpine/internal/overlay"
	"github.com/itxaka/sysext-alpine/internal/release"
	"github.com/itxaka/sysext-alpine/internal/service"
)

// cmdStatus is verb_status(): hierarchies that do not exist are left out,
// errors for single hierarchies are logged and the table is printed anyway.
func (c *cli) cmdStatus() error {
	if err := c.loadContext(); err != nil {
		return err
	}
	statuses, err := overlay.CurrentStatus(c.cfg.class, c.cfg.root)
	if err != nil && len(statuses) == 0 && errors.Is(err, fs.ErrNotExist) {
		err = nil // a missing --root= has no hierarchies
	}
	if err != nil {
		c.log.Errorf("%s", errorText(err))
	}
	fmt.Fprint(c.stdout, statusOutput(statuses, c.cfg.jsonMode, c.cfg.legend))
	if err != nil {
		return errLogged
	}
	return nil
}

// cmdList is verb_list(); it reads no configuration.
func (c *cli) cmdList() error {
	images, err := c.discover()
	if err != nil {
		return err
	}
	if len(images) == 0 && c.cfg.jsonMode == jsonOff {
		c.log.Infof("No OS extensions found.")
		return nil
	}
	fmt.Fprint(c.stdout, listOutput(images, c.cfg.jsonMode, c.cfg.legend))
	return nil
}

func (c *cli) discover() ([]discover.Image, error) {
	images, err := discover.Discover(c.cfg.class, c.cfg.root)
	if err != nil {
		return nil, failf(err, "Failed to discover images")
	}
	return images, nil
}

// haveCapSysAdmin is have_effective_cap(CAP_SYS_ADMIN).
var haveCapSysAdmin = func() (bool, error) {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return false, err
	}
	return data[unix.CAP_SYS_ADMIN/32].Effective&(1<<(unix.CAP_SYS_ADMIN%32)) != 0, nil
}

func requirePrivileges() error {
	ok, err := haveCapSysAdmin()
	if err != nil {
		return failf(err, "Failed to check if we have enough privileges")
	}
	if !ok {
		return failMsg("Need to be privileged.")
	}
	return nil
}

// cmdMerge is verb_merge(): it refuses to merge over a merged hierarchy,
// then mounts and checks every image and merges those that qualify.
func (c *cli) cmdMerge() error {
	if err := requirePrivileges(); err != nil {
		return err
	}
	if err := c.loadContext(); err != nil {
		return err
	}
	images, err := c.discover()
	if err != nil {
		return err
	}
	for _, h := range c.hierarchies {
		merged, err := overlay.IsMergedByUs(c.cfg.class, c.cfg.root, h)
		if err != nil {
			return err
		}
		if merged {
			return &overlay.AlreadyMergedError{Hierarchy: h}
		}
	}
	opts, err := c.mergeOptions(images)
	if err != nil {
		return c.mergeFailed(err)
	}
	var out overlay.Outcome
	err = c.critical(func() (err error) {
		if out, err = overlay.Merge(c.cfg.class, images, opts); err != nil && !errors.Is(err, overlay.ErrCleanup) {
			err = c.mergeFailed(err)
		}
		return err
	})
	if err != nil && !errors.Is(err, overlay.ErrCleanup) {
		return err
	}
	c.reportMerge(out)
	if err != nil || out.Result != overlay.Merged {
		return err
	}
	actions, err := c.extensionActions()
	if err != nil {
		return err
	}
	c.applyActions(actions)
	return nil
}

// cmdUnmerge is verb_unmerge(). Nothing merged is not an error.
func (c *cli) cmdUnmerge() error {
	if err := requirePrivileges(); err != nil {
		return err
	}
	if err := c.loadContext(); err != nil {
		return err
	}
	actions, err := c.extensionActions()
	if err != nil {
		return err
	}
	c.stopProvidedServices(actions)
	var unmerged []string
	err = c.critical(func() (err error) {
		unmerged, err = overlay.Unmerge(c.cfg.class, c.cfg.root)
		return err
	})
	c.reportUnmerged(unmerged)
	if err != nil {
		return err
	}
	c.applyActions(actions)
	return nil
}

// cmdRefresh is verb_refresh(): the images are mounted and checked, and the
// merge is replaced only when they differ from what is merged (always with
// --always-refresh=yes). Without any qualifying image the extensions are
// unmerged, with the service manager handled like for unmerge.
func (c *cli) cmdRefresh() error {
	if err := requirePrivileges(); err != nil {
		return err
	}
	if err := c.loadContext(); err != nil {
		return err
	}
	images, err := c.discover()
	if err != nil {
		return err
	}
	opts, err := c.mergeOptions(images)
	if err != nil {
		return c.mergeFailed(err)
	}
	var unmergeActions service.Actions
	unmerging := false
	opts.BeforeUnmerge = func() error {
		unmerging = true
		a, err := c.extensionActions()
		if err != nil {
			return err
		}
		c.stopProvidedServices(a)
		unmergeActions = a
		return nil
	}
	var out overlay.Outcome
	err = c.critical(func() (err error) {
		out, err = overlay.Refresh(c.cfg.class, images, opts)
		switch {
		case err == nil, errors.Is(err, overlay.ErrCleanup):
		case unmerging:
			c.reportMerge(out)
		default:
			c.reportUnmerged(out.Unmerged)
			err = c.mergeFailed(err)
		}
		return err
	})
	if err != nil && !errors.Is(err, overlay.ErrCleanup) {
		return err
	}
	c.reportMerge(out)
	if err != nil {
		return err
	}
	switch out.Result {
	case overlay.Merged:
		actions, err := c.extensionActions()
		if err != nil {
			return err
		}
		c.applyActions(actions)
	case overlay.NothingFound:
		c.applyActions(unmergeActions)
	case overlay.Unchanged:
	}
	return nil
}

// mergeOptions reads the host data every merge validates against and
// returns the overlay options with the image check of systemd's
// merge_subprocess().
func (c *cli) mergeOptions(images []discover.Image) (overlay.MergeOptions, error) {
	root := c.cfg.root
	if root == "" {
		root = "/"
	}
	host, err := release.HostOSRelease(c.cfg.root)
	if err != nil {
		return overlay.MergeOptions{}, failf(err, "Failed to acquire 'os-release' data of OS tree '%s'", root)
	}
	if host["ID"] == "" {
		return overlay.MergeOptions{}, failMsg("'ID' field not found or empty in 'os-release' data of OS tree '%s'.", root)
	}
	scope, err := release.HostScope(c.cfg.root)
	if err != nil {
		return overlay.MergeOptions{}, failf(err, "Failed to check for /etc/initrd-release")
	}
	return overlay.MergeOptions{
		Root:          c.cfg.root,
		Mutable:       c.cfg.mutable,
		MountOptions:  c.mountOptions,
		ImagePolicy:   c.cfg.imagePolicy,
		Arch:          release.NativeArchitecture(),
		NoExec:        c.cfg.noExec,
		AlwaysRefresh: c.cfg.alwaysRefresh,
		Check:         c.checkImage(host, scope),
		Warnf:         c.log.Warnf,
		BeforeMerge:   func(merged []string) { c.reportUsing(merged, images) },
	}, nil
}

// checkImage validates a mounted image like merge_subprocess() and
// image_read_metadata(): broken images are fatal even with --force, images
// that do not fit the host are ignored unless --force is given.
func (c *cli) checkImage(host release.Fields, scope string) func(discover.Image, string) (bool, error) {
	return func(img discover.Image, tree string) (bool, error) {
		raw := img.Type != discover.TypeDirectory
		switch {
		case raw:
			ok, err := release.CheckImageTree(tree, img.Name, true)
			if err == nil && !ok {
				err = unix.ENOMEDIUM
			}
			if err != nil {
				return false, metadataFailure(err, img.Name)
			}
		case c.cfg.class == release.Sysext:
			forbidden, err := release.HasForbiddenContent(tree)
			if err == nil && forbidden {
				err = unix.ENOMEDIUM
			}
			if err != nil {
				return false, metadataFailure(err, img.Name)
			}
		}
		if c.cfg.force {
			return true, nil
		}
		if raw {
			ok, err := release.CheckImageTree(tree, img.Name, false)
			if err != nil {
				return false, failf(err, "Failed to mount image")
			}
			if !ok {
				c.log.Errorf("Failed to mount image: No medium found")
				return false, nil
			}
		}
		ext, err := release.FindExtensionRelease(tree, img.Name, c.cfg.class, false)
		if err != nil {
			ext = nil
		}
		ok, reason := release.Validate(img.Name, host, ext, c.cfg.class, release.HostArchitecture(), scope)
		if !ok {
			c.log.Debugf("%s", reason)
		}
		return ok, nil
	}
}

// preMergeError is a failure systemd-sysext hits before it forks off the
// merge, reading the metadata of the images; it is not followed by "Failed
// to merge hierarchies".
type preMergeError struct{ error }

func (e preMergeError) Unwrap() error { return e.error }

// metadataFailure is image_read_metadata() failing.
func metadataFailure(err error, name string) error {
	return preMergeError{failf(err, "Failed to read metadata for image %s", name)}
}

// mergeFailed logs err, a failure of the merge, and returns systemd's
// "Failed to merge hierarchies". Failures systemd hits before the merge are
// returned as they are.
func (c *cli) mergeFailed(err error) error {
	if _, ok := errors.AsType[preMergeError](err); ok || errors.Is(err, overlay.ErrAlreadyMerged) {
		return err
	}
	if ie, ok := errors.AsType[*overlay.ImageError](err); ok {
		if err := c.imageFailure(ie); err != nil {
			return err
		}
	} else {
		c.log.Errorf("%s", errorText(err))
	}
	return failMsg("Failed to merge hierarchies")
}

// imageFailure reports a raw image that failed to mount the way systemd
// does, which reads the metadata of every image with the class default
// image policy before the merge: an image that is broken, or rejected while
// no image policy is configured, fails there (returned). An image only the
// configured policy rejects fails the merge, the dissection logging that it
// does not match the policy, a failed verity activation (a signature that
// cannot be verified) logging nothing. The details go to the debug log.
func (c *cli) imageFailure(ie *overlay.ImageError) error {
	c.log.Debugf("%s", errorText(ie))
	_, verity := errors.AsType[*image.VerityError](ie)
	switch {
	case c.cfg.imagePolicy == "":
	case verity:
		return nil
	case errors.Is(ie, unix.ERFKILL):
		c.log.Errorf("%s: Image does not match image policy.", ie.Image.Path)
		return nil
	}
	return metadataFailure(ie.Err, ie.Image.Name)
}

// critical runs fn, an operation that should not stop halfway, with
// SIGINT, SIGTERM and SIGHUP held back (unless they are ignored, as under
// nohup); other signals, SIGKILL among them, still end the process at once.
// A held back signal is delivered again once fn returned, terminating the
// process as it would have been. Waiting for a concurrent merge, refresh or
// unmerge to finish happens before, while these signals still work; only
// when yet another one takes the lock in between does fn wait for it with
// them held back.
func (c *cli) critical(fn func() error) error {
	if unlock, err := overlay.Lock(c.cfg.class, c.cfg.root); err == nil {
		unlock()
	}
	sigs := make(chan os.Signal, 1)
	for _, sig := range []os.Signal{unix.SIGINT, unix.SIGTERM, unix.SIGHUP} {
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	err := fn()
	signal.Stop(sigs)
	select {
	case sig := <-sigs:
		if err != nil {
			c.log.Errorf("%s", errorText(err))
		}
		redeliver(sig.(unix.Signal))
	default:
	}
	return err
}

// redeliver terminates the process by the default action of sig.
var redeliver = func(sig unix.Signal) {
	signal.Reset(sig)
	_ = unix.Kill(unix.Getpid(), sig)
	time.Sleep(time.Second)
	os.Exit(128 + int(sig))
}

// reportMerge logs systemd-sysext's messages for a merge or refresh, except
// for those reportUsing logged when it went ahead.
func (c *cli) reportMerge(out overlay.Outcome) {
	switch out.Result {
	case overlay.NothingFound:
		if out.Ignored > 0 {
			c.log.Infof("No suitable extensions found (%d ignored due to incompatible image(s)).", out.Ignored)
		} else {
			c.log.Infof("No extensions found.")
		}
		c.reportUnmerged(out.Unmerged)
	case overlay.Unchanged:
		c.log.Infof("Skipping extension refresh because no change was found, use --always-refresh=yes to always do a refresh.")
	case overlay.Merged:
		c.reportUnmerged(out.Unmerged)
		for _, h := range out.Hierarchies {
			c.log.Infof("Merged extensions into '%s'.", h)
		}
		c.warnNoexecInitScripts()
	}
}

// reportUsing logs which extensions a merge uses, before it mounts them.
func (c *cli) reportUsing(merged []string, images []discover.Image) {
	if len(merged) == 0 {
		c.log.Infof("No extensions found, proceeding in mutable mode.")
	} else {
		c.log.Infof("Using extensions %s.", extensionFiles(merged, images))
	}
}

func (c *cli) reportUnmerged(hierarchies []string) {
	for _, h := range hierarchies {
		c.log.Infof("Unmerged '%s'.", h)
	}
}

// extensionFiles renders the file names of the merged extensions the way
// systemd lists them: quoted, version-sorted, comma-separated.
func extensionFiles(names []string, images []discover.Image) string {
	files := make([]string, 0, len(names))
	for _, name := range names {
		for _, img := range images {
			if img.Name == name {
				files = append(files, filepath.Base(img.Path))
				break
			}
		}
	}
	slices.SortStableFunc(files, discover.CompareVersions)
	return "'" + strings.Join(files, "', '") + "'"
}

// warnNoexecInitScripts warns when a confext merge made /etc noexec while it
// holds OpenRC init scripts: OpenRC executes them directly.
func (c *cli) warnNoexecInitScripts() {
	if c.cfg.class != release.Confext || c.cfg.noExec == overlay.NoExecOff {
		return
	}
	if !slices.ContainsFunc(c.hierarchies, func(h string) bool { return comparePaths(h, "/etc") == 0 }) {
		return
	}
	if _, err := os.Stat(filepath.Join(c.cfg.root, "/etc/init.d")); err != nil {
		return
	}
	c.log.Warnf("/etc/ is merged with noexec, OpenRC cannot execute the init scripts in /etc/init.d/ now; use --noexec=no to avoid that.")
}

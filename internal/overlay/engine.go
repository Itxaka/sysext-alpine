package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/errno"
	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/image"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// mergeOp is one Merge or Refresh, holding everything needed to undo it.
//
// The workspace (ws) is a private tmpfs. A merge builds its layers directly
// in it: extensions/<name> (image mounts), meta/<hierarchy> (top layer with
// the metadata), overlay/<hierarchy> (staging mount point) and
// mh_workspace/<hierarchy> (ephemeral upper and work directories). A
// refresh of a live merge builds the same layout in a new directory
// ws/next.* and moves it into place once the old overlays are gone.
type mergeOp struct {
	class   release.Class
	opts    MergeOptions
	mode    string
	root    string   // resolved root
	roots   []string // root prefixes image paths may carry
	ws      string
	gen     string // where this operation builds its layout
	freshWS bool   // the workspace was mounted by this operation
	hiers   []*hierarchyState
	images  []*mountedImage
	ignored int

	swapping  bool // old merges are being replaced
	committed bool
}

type mountedImage struct {
	img     discover.Image
	dir     string
	mounted *image.Mounted // nil for bind-mounted directory images
}

type hierarchyState struct {
	path   string // as configured
	target string // resolved inside the root, may not exist yet
	host   string // resolved existing host hierarchy, "" if missing
	merged bool   // merged before this operation
	used   []string
	mount  bool // gets an overlay

	staging        string
	stagingMounted bool
	attached       bool
	unmerged       bool
	subs           []savedMount
}

func run(class release.Class, images []discover.Image, opts MergeOptions, refresh bool) (out Outcome, err error) {
	mode, err := normalizeMutableMode(opts.Mutable)
	if err != nil {
		return out, err
	}
	hierarchies, err := Hierarchies(class)
	if err != nil {
		return out, err
	}
	root, err := resolveRoot(opts.Root)
	if err != nil {
		return out, err
	}
	unlock, err := lock(class, root)
	if err != nil {
		return out, err
	}
	defer unlock()

	op := &mergeOp{class: class, opts: opts, mode: mode, root: root, ws: workspaceFor(class, root)}
	if opts.Root != "" {
		abs, _ := filepath.Abs(opts.Root)
		op.roots = []string{abs, root}
	}
	defer func() {
		if r := recover(); r != nil {
			op.abort()
			panic(r)
		}
	}()
	out, err = op.run(images, hierarchies, refresh)
	if err != nil {
		op.abort()
	}
	return out, err
}

func (op *mergeOp) run(images []discover.Image, hierarchies []string, refresh bool) (Outcome, error) {
	anyMerged := false
	for _, h := range hierarchies {
		target, err := fsutil.Chase(op.root, h, fsutil.ChaseNonexistent)
		if err != nil {
			return Outcome{}, fmt.Errorf("failed to resolve hierarchy '%s': %w", h, err)
		}
		host, err := resolveHierarchy(op.root, h)
		if err != nil {
			return Outcome{}, err
		}
		hs := &hierarchyState{path: h, target: target, host: host}
		if host != "" {
			if hs.merged, err = isOurMountPoint(op.class, host); err != nil {
				return Outcome{}, err
			}
		}
		if hs.merged && !refresh {
			return Outcome{}, &AlreadyMergedError{Hierarchy: h}
		}
		anyMerged = anyMerged || hs.merged
		op.hiers = append(op.hiers, hs)
	}

	if len(images) > 0 || op.mode != "no" {
		var err error
		if refresh && anyMerged {
			err = op.useWorkspace()
		} else {
			err = op.resetWorkspace()
		}
		if err != nil {
			return Outcome{}, err
		}
		for _, img := range images {
			if err := op.mountImage(img); err != nil {
				return Outcome{}, err
			}
		}
	}
	names := make([]string, len(op.images))
	trees := make([]string, len(op.images))
	for i, mi := range op.images {
		names[i], trees[i] = mi.img.Name, mi.dir
	}

	if len(op.images) == 0 && op.mode == "no" {
		op.discard()
		out := Outcome{Result: NothingFound, Ignored: op.ignored}
		if !anyMerged {
			return out, nil
		}
		if op.opts.BeforeUnmerge != nil {
			if err := op.opts.BeforeUnmerge(); err != nil {
				return out, err
			}
		}
		for _, hs := range op.hiers {
			if !hs.merged {
				continue
			}
			n, err := unmergeInPlace(op.class, op.root, hs.path, hs.host, op.ws)
			if n > 0 {
				out.Unmerged = append(out.Unmerged, hs.host)
			}
			if err != nil {
				return out, err
			}
		}
		return out, teardownWorkspace(op.ws)
	}

	content, err := op.plan(trees)
	if err != nil {
		return Outcome{}, err
	}
	if old := op.oldOrigin(); old != "" {
		equal, err := originEqual(content, old)
		if err != nil {
			return Outcome{}, err
		}
		if equal && !op.opts.AlwaysRefresh {
			op.discard()
			return Outcome{Result: Unchanged, Merged: names, Ignored: op.ignored}, nil
		}
	}

	if op.opts.BeforeMerge != nil {
		op.opts.BeforeMerge(names)
	}
	out := Outcome{Result: Merged, Merged: names, Ignored: op.ignored}
	op.swapping = true
	for _, hs := range op.hiers {
		if !hs.merged {
			continue
		}
		subs, n, err := unmergeHierarchy(op.class, op.root, hs.path, hs.host, op.ws)
		hs.subs = append(hs.subs, subs...)
		if n > 0 {
			hs.unmerged = true
			out.Unmerged = append(out.Unmerged, hs.host)
		}
		if err != nil {
			return out, err
		}
	}
	for _, hs := range op.hiers {
		if !hs.mount {
			if err := attachSubmounts(hs.target, hs.subs); err != nil {
				return out, err
			}
			continue
		}
		if err := op.mergeHierarchy(hs, names, content); err != nil {
			return out, err
		}
	}
	for _, hs := range op.hiers {
		if !hs.mount {
			continue
		}
		if err := os.MkdirAll(hs.target, 0o755); err != nil {
			return out, fmt.Errorf("failed to create hierarchy mount point '%s': %w", hs.target, err)
		}
		if err := unix.Mount(hs.staging, hs.target, "", unix.MS_MOVE, ""); err != nil {
			return out, fmt.Errorf("failed to move overlay onto %s: %w", hs.target, err)
		}
		hs.stagingMounted, hs.attached = false, true
		out.Hierarchies = append(out.Hierarchies, hs.target)
	}
	op.committed = true
	for _, hs := range op.hiers {
		closeSaved(hs.subs)
	}
	cleanup := op.promote
	if len(out.Hierarchies) == 0 {
		cleanup = func() error { return teardownWorkspace(op.ws) }
	}
	if err := cleanup(); err != nil {
		return out, fmt.Errorf("%w %s: %w", ErrCleanup, op.ws, err)
	}
	return out, nil
}

// plan decides which extensions contribute to each hierarchy and builds the
// origin content, resolving (and for "yes" creating) the mutable
// directories like systemd does before it compares origins.
func (op *mergeOp) plan(trees []string) (string, error) {
	org := &origin{Mode: op.mode, Images: map[string]imageIdentity{}}
	if op.opts.MountOptions != nil {
		org.MountOptions = *op.opts.MountOptions
	}
	for _, mi := range op.images {
		path := pathWithoutRoot(mi.img.Path, op.roots...)
		var id imageIdentity
		if mi.mounted != nil && mi.mounted.RootHash != "" {
			id = imageIdentity{Path: path, VerityHash: mi.mounted.RootHash}
		} else {
			var err error
			if id, err = identify(mi.img, path); err != nil {
				return "", fmt.Errorf("failed to create origin entry for '%s': %w", mi.img.Name, err)
			}
		}
		org.Names = append(org.Names, mi.img.Name)
		org.Images[mi.img.Name] = id
	}
	for _, hs := range op.hiers {
		used, err := usedLayers(hs.path, trees)
		if err != nil {
			return "", err
		}
		hs.used = used
		hs.mount = len(used) > 0 || op.mode != "no"

		var dir string
		if modeIsEphemeral(op.mode) {
			dir = ephemeralWorkspace + "/" + singlePathComponent(hs.path)
		} else {
			if dir, err = resolveMutableDir(op.mode, op.root, hs.path, hierarchyMode(hs.host), ""); err != nil {
				return "", err
			}
			dir = pathWithoutRoot(dir, op.root)
		}
		if dir != "" {
			org.MutableDirs = append(org.MutableDirs, mutableDir{hs.path, dir})
		}
	}
	return org.String(), nil
}

// oldOrigin returns the origin recorded by the current merge: that of the
// first merged hierarchy with a readable one.
func (op *mergeOp) oldOrigin() string {
	for _, hs := range op.hiers {
		if !hs.merged {
			continue
		}
		if data, err := os.ReadFile(filepath.Join(hs.host, MarkerDirName(op.class), "origin")); err == nil {
			return string(data)
		}
	}
	return ""
}

// usedLayers is determine_used_extensions(): the non-empty hierarchy
// directories of the image trees, newest image (topmost layer) first.
func usedLayers(hierarchy string, trees []string) ([]string, error) {
	var used []string
	for _, tree := range slices.Backward(trees) {
		p, err := fsutil.Chase(tree, hierarchy, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to resolve hierarchy '%s' in extension '%s': %w", hierarchy, tree, err)
		}
		empty, err := dirIsEmpty(p)
		if err != nil {
			return nil, fmt.Errorf("failed to check if hierarchy '%s' in extension '%s' is empty: %w", p, tree, err)
		}
		if !empty {
			used = append(used, p)
		}
	}
	return used, nil
}

// hierarchyMode is the mode the merged hierarchy keeps: that of the host
// hierarchy, 0755 when there is none.
func hierarchyMode(host string) uint32 {
	var st unix.Stat_t
	if host == "" || unix.Stat(host, &st) != nil {
		return 0o755
	}
	return st.Mode & 0o777
}

// blockDevice is get_block_device() without the btrfs lookup: st_dev of a
// filesystem on a block device, 0 otherwise.
func blockDevice(p string) uint64 {
	var st unix.Stat_t
	if unix.Stat(p, &st) != nil || unix.Major(uint64(st.Dev)) == 0 {
		return 0
	}
	return uint64(st.Dev)
}

// mergeHierarchy is merge_hierarchy(): it assembles and mounts the overlay
// of one hierarchy at its staging point, records the metadata and carries
// the mounts below the hierarchy over.
func (op *mergeOp) mergeHierarchy(hs *hierarchyState, names []string, content string) error {
	meta := filepath.Join(op.gen, "meta", hs.path)
	staging := filepath.Join(op.gen, "overlay", hs.path)
	marker := MarkerDirName(op.class)

	mode := hierarchyMode(hs.host)
	mutable, err := resolveMutableDir(op.mode, op.root, hs.path, mode, filepath.Join(op.gen, "mh_workspace", hs.path))
	if err != nil {
		return err
	}
	lower := []string{meta}
	imported, err := importedMutableDir(op.mode, op.root, hs.path, hs.host, mutable)
	if err != nil {
		return err
	}
	if imported != "" {
		lower = append(lower, imported)
	}
	lower = append(lower, hs.used...)
	useHost, err := hostAsLowerDir(op.mode, hs.host, mutable)
	if err != nil {
		return err
	}
	var backing uint64
	if useHost {
		lower = append(lower, hs.host)
		backing = blockDevice(hs.host)
	}
	upper, err := upperDir(op.mode, mutable)
	if err != nil {
		return err
	}
	var work string
	if upper != "" {
		if work, err = workDirFor(hs.path, upper); err != nil {
			return err
		}
	}

	for _, d := range []string{staging, meta, work} {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("failed to make directory '%s': %w", d, err)
		}
	}
	top := meta
	if upper != "" {
		top = upper
		if err := removeTree(filepath.Join(upper, marker)); err != nil {
			return fmt.Errorf("failed to remove stale metadata from %s: %w", upper, err)
		}
	}
	if err := os.Chmod(top, fs.FileMode(mode)); err != nil {
		return fmt.Errorf("failed to set permissions of '%s' to %04o: %w", top, mode, err)
	}
	if err := op.writeMeta(meta, names, content, work, backing); err != nil {
		return err
	}

	flags, data := op.overlayMount(lower, upper, work)
	if err := unix.Mount(classIdentifier(op.class), staging, "overlay", flags, data); err != nil {
		return fmt.Errorf("failed to mount %s (type overlay) on %s (%s \"%s\"): %w", classIdentifier(op.class), staging, mountFlagsString(flags), data, err)
	}
	hs.staging, hs.stagingMounted = staging, true

	if err := op.writeDev(staging, meta, flags, data); err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(meta, now, now); err != nil {
		return fmt.Errorf("failed to fix mtime of '%s': %w", meta, err)
	}
	if upper != "" {
		m := filepath.Join(staging, marker)
		if err := unix.Mount(m, m, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("failed to bind mount %s: %w", m, err)
		}
		if err := makeReadOnly(m, flags); err != nil {
			return fmt.Errorf("failed to remount '%s' as read-only: %w", m, err)
		}
	}

	if !hs.merged && hs.host != "" {
		subs, err := takeSubmounts(hs.host, belowWorkspace(op.ws, hs.host))
		hs.subs = append(hs.subs, subs...)
		if err != nil {
			return err
		}
	}
	return attachSubmounts(staging, hs.subs)
}

// overlayMount is the flag and option computation of mount_overlayfs().
func (op *mergeOp) overlayMount(lower []string, upper, work string) (uintptr, string) {
	flags := uintptr(unix.MS_RDONLY | unix.MS_NODEV)
	if op.class == release.Confext {
		flags |= unix.MS_NOSUID | unix.MS_NOEXEC
	}
	switch {
	case op.opts.NoExec > 0:
		flags |= unix.MS_NOEXEC
	case op.opts.NoExec == 0:
		flags &^= unix.MS_NOEXEC
	}
	var extra string
	if op.opts.MountOptions != nil {
		extra = *op.opts.MountOptions
	}
	if upper != "" {
		flags &^= unix.MS_RDONLY
		if op.opts.MountOptions == nil {
			extra = mutableMountOptions
		}
	}
	return mangleMountOptions(overlayOptions(lower, upper, work, extra), flags)
}

// writeMeta is store_info_in_meta() except for the dev marker, which needs
// the mounted overlay.
func (op *mergeOp) writeMeta(meta string, names []string, content, work string, backing uint64) error {
	dir := filepath.Join(meta, MarkerDirName(op.class))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory '%s': %w", dir, err)
	}
	files := map[string]string{
		listFileName(op.class): formatList(names),
		"origin":               content,
	}
	if work != "" && !modeIsEphemeral(op.mode) {
		rel, ok := pathInRoot(work, op.root)
		if !ok {
			return errno.New(unix.EINVAL, "Workdir '%s' must not be outside root '%s'", work, op.root)
		}
		files["work_dir"] = cescape(rel) + "\n"
	}
	if backing != 0 {
		files["backing"] = formatDevnum(backing) + "\n"
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o666); err != nil {
			return fmt.Errorf("failed to write extension meta file '%s': %w", filepath.Join(dir, name), err)
		}
	}
	return nil
}

// pathInRoot returns p relative to root, without a leading slash.
func pathInRoot(p, root string) (string, bool) {
	prefix := strings.TrimSuffix(root, "/") + "/"
	rel, ok := strings.CutPrefix(p, prefix)
	if !ok || rel == "" {
		return "", false
	}
	return rel, true
}

// writeDev records the st_dev of the staged overlay and makes sure the
// marker reads back through it: when a lookup cached before the file
// existed hides it, the overlay is mounted again.
func (op *mergeOp) writeDev(staging, meta string, flags uintptr, data string) error {
	marker := MarkerDirName(op.class)
	f := filepath.Join(meta, marker, "dev")
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			if err := unix.Unmount(staging, 0); err != nil {
				return fmt.Errorf("unmounting %s: %w", staging, err)
			}
			if err := unix.Mount(classIdentifier(op.class), staging, "overlay", flags, data); err != nil {
				return fmt.Errorf("remounting overlay on %s: %w", staging, err)
			}
		}
		var st unix.Stat_t
		if err := unix.Stat(staging, &st); err != nil {
			return fmt.Errorf("failed to stat mount '%s': %w", staging, err)
		}
		want := formatDevnum(uint64(st.Dev)) + "\n"
		if err := os.WriteFile(f, []byte(want), 0o666); err != nil {
			return fmt.Errorf("failed to write '%s': %w", f, err)
		}
		got, err := os.ReadFile(filepath.Join(staging, marker, "dev"))
		if err == nil && string(got) == want {
			return nil
		}
		lastErr = err
		if err == nil {
			lastErr = fmt.Errorf("found %q", got)
		}
	}
	return fmt.Errorf("dev marker not visible through overlay %s: %w", staging, lastErr)
}

// makeReadOnly marks the mount at p read-only, keeping its other flags.
func makeReadOnly(p string, flags uintptr) error {
	err := unix.MountSetattr(unix.AT_FDCWD, p, unix.AT_SYMLINK_NOFOLLOW, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY})
	if !errnoIsNotSupported(err) {
		return err
	}
	keep := flags & (unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NOATIME | unix.MS_NODIRATIME | unix.MS_RELATIME | unix.MS_STRICTATIME)
	return unix.Mount("", p, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|keep, "")
}

// mountImage mounts one image at extensions/<name> of the generation and
// runs the check; rejected images are unmounted again.
func (op *mergeOp) mountImage(img discover.Image) error {
	dir := filepath.Join(op.gen, "extensions", img.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	mi := &mountedImage{img: img, dir: dir}
	if img.Type == discover.TypeDirectory {
		if err := unix.Mount(img.Path, dir, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("failed to bind mount %s on %s: %w", img.Path, dir, err)
		}
		op.images = append(op.images, mi)
		if err := makeReadOnly(dir, 0); err != nil {
			return fmt.Errorf("failed to make bind mount '%s' read-only: %w", dir, err)
		}
	} else {
		m, err := image.MountWithOpts(img, dir, image.MountOpts{
			Arch:      op.opts.Arch,
			Policy:    op.opts.ImagePolicy,
			Class:     op.class,
			TrustDirs: image.TrustDirs(op.opts.Root),
			Warnf:     op.opts.Warnf,
		})
		if err != nil {
			return &ImageError{Image: img, Err: err}
		}
		mi.mounted = m
		op.images = append(op.images, mi)
	}
	if op.opts.Check == nil {
		return nil
	}
	ok, err := op.opts.Check(img, dir)
	if err != nil || ok {
		return err
	}
	op.images = op.images[:len(op.images)-1]
	op.ignored++
	if err := unmountImage(mi); err != nil {
		return err
	}
	return os.Remove(dir)
}

func unmountImage(mi *mountedImage) error {
	if mi.mounted != nil {
		return mi.mounted.Unmount()
	}
	return detach(mi.dir)
}

// resetWorkspace replaces whatever is left at the workspace with a fresh
// private tmpfs.
func (op *mergeOp) resetWorkspace() error {
	if err := teardownWorkspace(op.ws); err != nil {
		return err
	}
	if err := mountWorkspace(op.class, op.ws); err != nil {
		return err
	}
	op.freshWS, op.gen = true, op.ws
	return nil
}

// useWorkspace keeps the workspace of a live merge and creates a new
// directory in it for the next generation. A generation left behind by a
// refresh that failed to move it into place may still back the merged
// overlays, so it is never reused or removed; it goes away with the
// workspace on unmerge.
func (op *mergeOp) useWorkspace() error {
	mp, err := fsutil.IsMountPoint(op.ws)
	if err != nil {
		return err
	}
	if !mp {
		if err := teardownWorkspace(op.ws); err != nil {
			return err
		}
		if err := mountWorkspace(op.class, op.ws); err != nil {
			return err
		}
		op.freshWS = true
	}
	gen, err := os.MkdirTemp(op.ws, "next.")
	if err != nil {
		return fmt.Errorf("failed to create a directory for the new generation in %s: %w", op.ws, err)
	}
	op.gen = gen
	return nil
}

// mountWorkspace mounts the workspace tmpfs, private so that nothing mounted
// inside propagates anywhere and staged mounts can be moved out of it even
// below shared mounts.
func mountWorkspace(class release.Class, ws string) error {
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return fmt.Errorf("failed to create '%s': %w", ws, err)
	}
	if err := unix.Mount(classIdentifier(class), ws, "tmpfs", 0, "mode=0700"); err != nil {
		return fmt.Errorf("failed to mount tmpfs on %s: %w", ws, err)
	}
	if err := unix.Mount("", ws, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		_ = detach(ws)
		return fmt.Errorf("failed to make %s private: %w", ws, err)
	}
	return nil
}

// teardownWorkspace detaches everything mounted at or below the workspace
// and removes it. Merged overlays keep their layers alive on their own.
func teardownWorkspace(ws string) error {
	if _, err := os.Lstat(ws); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := detachAll(ws); err != nil {
		return err
	}
	return removeTree(ws)
}

// discard drops what this operation built before any merge was touched.
func (op *mergeOp) discard() {
	for _, mi := range slices.Backward(op.images) {
		_ = unmountImage(mi)
	}
	op.images = nil
	switch {
	case op.freshWS:
		_ = teardownWorkspace(op.ws)
		op.freshWS = false
	case op.gen != "" && op.gen != op.ws:
		_ = detachAll(op.gen)
		_ = removeTree(op.gen)
	}
	op.gen = ""
}

// abort undoes an operation that failed: hierarchies already switched over
// are unmerged, with the mounts below them back in place, and the new
// layers are released. Merges not touched yet stay as they are.
func (op *mergeOp) abort() {
	if op.committed {
		return
	}
	for _, hs := range slices.Backward(op.hiers) {
		op.restoreHost(hs)
	}
	if op.swapping {
		for _, mi := range slices.Backward(op.images) {
			_ = unmountImage(mi)
		}
		op.images = nil
		_ = teardownWorkspace(op.ws)
		return
	}
	op.discard()
}

// restoreHost takes the new overlay of a hierarchy down again, wherever it
// is (on the hierarchy or still at its staging point), and puts the mounts
// below it back onto the host hierarchy, together with those saved from the
// merge it replaced.
func (op *mergeOp) restoreHost(hs *hierarchyState) {
	marker := MarkerDirName(op.class)
	var from string
	switch {
	case hs.attached:
		from = hs.target
	case hs.stagingMounted:
		from = hs.staging
	}
	if from != "" {
		_ = detach(filepath.Join(from, marker))
		subs, _ := takeSubmounts(from, belowWorkspace(op.ws, from))
		hs.subs = append(hs.subs, subs...)
		_ = detach(from)
	}
	hs.attached, hs.stagingMounted = false, false
	_ = attachSubmounts(hs.target, hs.subs)
	closeSaved(hs.subs)
	hs.subs = nil
}

// promote moves the generation built in ws/next.* into place, replacing
// the layers of the previous merge, which no overlay uses anymore.
func (op *mergeOp) promote() error {
	if op.gen == op.ws {
		return nil
	}
	ext := filepath.Join(op.ws, "extensions")
	if err := detachAll(ext); err != nil {
		return err
	}
	if err := removeTree(ext); err != nil {
		return err
	}
	for _, hs := range op.hiers {
		if !hs.attached && !hs.unmerged {
			continue
		}
		for _, d := range []string{"meta", "overlay", "mh_workspace"} {
			if err := removeTree(filepath.Join(op.ws, d, hs.path)); err != nil {
				return err
			}
		}
	}
	if err := os.Mkdir(ext, 0o700); err != nil {
		return err
	}
	for _, mi := range op.images {
		dst := filepath.Join(ext, mi.img.Name)
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		if err := unix.Mount(mi.dir, dst, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("moving %s to %s: %w", mi.dir, dst, err)
		}
		mi.dir = dst
	}
	for _, hs := range op.hiers {
		if !hs.attached {
			continue
		}
		for _, d := range []string{"meta", "overlay", "mh_workspace"} {
			src, dst := filepath.Join(op.gen, d, hs.path), filepath.Join(op.ws, d, hs.path)
			if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	return removeTree(op.gen)
}

// unmergeInPlace unmerges one hierarchy and puts the mounts that were below
// the overlay back onto the host hierarchy.
func unmergeInPlace(class release.Class, root, hierarchy, dir, ws string) (int, error) {
	subs, n, err := unmergeHierarchy(class, root, hierarchy, dir, ws)
	if aerr := attachSubmounts(dir, subs); aerr != nil {
		err = errors.Join(err, aerr)
	}
	closeSaved(subs)
	return n, err
}

// Package overlay assembles, mounts and dismantles the merged overlayfs
// hierarchies and the /run/systemd/{sysext,confext} workspace, mirroring
// systemd's runtime layout per docs/SPEC.md §4-5.
package overlay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/image"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// ErrAlreadyMerged is returned by Merge when a hierarchy is already merged.
var ErrAlreadyMerged = errors.New("already merged")

// Hierarchies returns the merge targets for the class:
// Sysext: ["/usr", "/opt"]; Confext: ["/etc"].
func Hierarchies(class release.Class) []string {
	if class == release.Confext {
		return []string{"/etc"}
	}
	return []string{"/usr", "/opt"}
}

// Workspace returns the runtime workspace dir for the class under root:
// /run/systemd/sysext or /run/systemd/confext. Subdirs used:
// extensions/<name>, meta/<escaped hierarchy>, overlay/<escaped hierarchy>.
func Workspace(class release.Class, root string) string {
	name := "sysext"
	if class == release.Confext {
		name = "confext"
	}
	return filepath.Join(root, "/run/systemd", name)
}

// MarkerDirName returns ".systemd-sysext" or ".systemd-confext".
func MarkerDirName(class release.Class) string {
	if class == release.Confext {
		return ".systemd-confext"
	}
	return ".systemd-sysext"
}

// Status describes one hierarchy's merge state for `status` output.
type Status struct {
	Hierarchy  string   `json:"hierarchy"`
	Merged     bool     `json:"merged"`
	Extensions []string `json:"extensions"` // names, merge order; nil if unmerged
	Since      int64    `json:"since"`      // unix mtime of marker dir; 0 if unmerged
}

// MergeOptions tunes Merge behavior.
type MergeOptions struct {
	Root   string // --root; "" = real root
	NoExec bool   // confext only; default true (apply MS_NOEXEC)
	Force  bool   // informational; version checks happen in caller
	Arch   string // host architecture (release.HostArchitecture())
}

// escapeHierarchy turns a hierarchy path into a workspace directory name:
// the leading '/' is trimmed and remaining '/' become '-'
// (e.g. "/usr" → "usr", "/some/path" → "some-path").
func escapeHierarchy(hierarchy string) string {
	return strings.ReplaceAll(strings.TrimPrefix(filepath.Clean(hierarchy), "/"), "/", "-")
}

// escapeOverlayPath escapes a single lowerdir path per overlayfs mount-option
// escaping: '\', ':' and ',' are backslash-escaped.
func escapeOverlayPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\\', ':', ',':
			b.WriteByte('\\')
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// buildLowerdir joins lowerdir paths with ':' applying overlayfs escaping to
// each component. Pure; unit-tested.
func buildLowerdir(paths []string) string {
	escaped := make([]string, len(paths))
	for i, p := range paths {
		escaped[i] = escapeOverlayPath(p)
	}
	return strings.Join(escaped, ":")
}

// overlayMountFlags returns the mount flags for the merged overlay:
// sysext ro,nodev; confext ro,nodev,nosuid[,noexec].
func overlayMountFlags(class release.Class, noExec bool) uintptr {
	if class == release.Confext {
		flags := uintptr(unix.MS_RDONLY | unix.MS_NODEV | unix.MS_NOSUID)
		if noExec {
			flags |= unix.MS_NOEXEC
		}
		return flags
	}
	return uintptr(unix.MS_RDONLY | unix.MS_NODEV)
}

// originEntry is one element of the marker `origin` JSON array.
type originEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

// imageTypeString maps discover.ImageType to the origin "type" value without
// relying on the (stub) String method.
func imageTypeString(t discover.ImageType) string {
	if t == discover.TypeRaw {
		return "raw"
	}
	return "directory"
}

// writeMarker creates markerDir and writes the `extensions` (newline list,
// merge order ascending) and `origin` (JSON array) marker files.
func writeMarker(markerDir string, names []string, origins []originEntry) error {
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return err
	}
	ext := strings.Join(names, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(markerDir, "extensions"), []byte(ext), 0o644); err != nil {
		return err
	}
	js, err := json.Marshal(origins)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(markerDir, "origin"), append(js, '\n'), 0o644)
}

// readExtensionsFile parses a marker `extensions` file into names.
func readExtensionsFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// readOriginFile parses a marker `origin` JSON file.
func readOriginFile(path string) ([]originEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []originEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// dirNonEmpty reports whether path is an existing directory with at least
// one entry.
func dirNonEmpty(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.IsDir() {
		return false
	}
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

// isMountPoint is the path_is_mount_point equivalent: lstat path and its
// parent and compare st_dev; the root directory (path == parent) is always a
// mount point, and a path sharing dev *and* inode with its parent (only
// possible for "/"-like self-references) is too. A missing path is not a
// mount point.
func isMountPoint(path string) (bool, error) {
	path = filepath.Clean(path)
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return false, nil
		}
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return true, nil // "/" edge case
	}
	var pst unix.Stat_t
	if err := unix.Lstat(parent, &pst); err != nil {
		return false, fmt.Errorf("lstat %s: %w", parent, err)
	}
	if st.Dev != pst.Dev {
		return true, nil
	}
	// Same device: only a mount point if it is the same directory as its
	// parent (bind-mount of root onto itself style edge case).
	return st.Ino == pst.Ino, nil
}

// IsMergedByUs reports whether hierarchy (relative to root) is currently
// overmounted by our overlay: it is a mount point, carries
// <hierarchy>/<marker>/dev, and that dev equals stat(hierarchy).st_dev.
func IsMergedByUs(class release.Class, root, hierarchy string) (bool, error) {
	target := filepath.Join(root, hierarchy)
	mp, err := isMountPoint(target)
	if err != nil || !mp {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(target, MarkerDirName(class), "dev"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
			return false, nil
		}
		return false, err
	}
	want, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return false, nil // malformed marker → not ours
	}
	var st unix.Stat_t
	if err := unix.Stat(target, &st); err != nil {
		return false, fmt.Errorf("stat %s: %w", target, err)
	}
	return uint64(st.Dev) == want, nil
}

// mergeState tracks resources acquired during Merge for rollback.
type mergeState struct {
	workspace string
	mounted   []*image.Mounted // image mounts, in mount order
	staged    []string         // overlay staging mounts not yet moved
	moved     []string         // hierarchy targets already MS_MOVE'd
}

// rollback undoes everything in reverse order. Errors are ignored: this is
// best-effort cleanup on a failure path.
func (s *mergeState) rollback() {
	for i := len(s.moved) - 1; i >= 0; i-- {
		_ = unmountWithRetry(s.moved[i])
	}
	for i := len(s.staged) - 1; i >= 0; i-- {
		_ = unmountWithRetry(s.staged[i])
	}
	for i := len(s.mounted) - 1; i >= 0; i-- {
		_ = s.mounted[i].Unmount()
	}
	_ = os.RemoveAll(s.workspace)
}

// Merge mounts the merged overlays for the given already-validated images.
// Steps per SPEC §4:
//  1. create workspace dirs (0700)
//  2. mount every image under workspace/extensions/<name> (image.Mount)
//  3. per hierarchy: build lowerdir = meta : images reverse-sorted : host,
//     skipping images lacking the hierarchy and a missing/empty host dir
//  4. write marker meta dir (extensions list + origin JSON)
//  5. mount overlay at workspace/overlay/<h> with class mount flags,
//     stat it, write `dev` marker, then MS_MOVE onto the hierarchy
//
// Fails (ErrAlreadyMerged) if any target hierarchy is already merged by us.
// On error, everything mounted so far is rolled back.
func Merge(class release.Class, images []discover.Image, opts MergeOptions) error {
	for _, h := range Hierarchies(class) {
		merged, err := IsMergedByUs(class, opts.Root, h)
		if err != nil {
			return fmt.Errorf("checking merge state of %s: %w", h, err)
		}
		if merged {
			return fmt.Errorf("%w: %s", ErrAlreadyMerged, h)
		}
	}
	if len(images) == 0 {
		return nil
	}

	st := &mergeState{workspace: Workspace(class, opts.Root)}
	if err := doMerge(class, images, opts, st); err != nil {
		st.rollback()
		return err
	}
	return nil
}

func doMerge(class release.Class, images []discover.Image, opts MergeOptions, st *mergeState) error {
	ws := st.workspace
	for _, d := range []string{ws, filepath.Join(ws, "extensions"), filepath.Join(ws, "meta"), filepath.Join(ws, "overlay")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("creating workspace %s: %w", d, err)
		}
	}

	// Mount every image under extensions/<name>. images is version-sorted
	// ascending; remember each mounted root by index.
	roots := make([]string, len(images))
	for i, img := range images {
		mp := filepath.Join(ws, "extensions", img.Name)
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return fmt.Errorf("creating image mount point %s: %w", mp, err)
		}
		m, err := image.Mount(img, mp, opts.Arch)
		if err != nil {
			return fmt.Errorf("mounting image %s: %w", img.Name, err)
		}
		st.mounted = append(st.mounted, m)
		roots[i] = m.Root
	}

	for _, h := range Hierarchies(class) {
		if err := mergeHierarchy(class, images, roots, opts, h, st); err != nil {
			return err
		}
	}
	return nil
}

// mergeHierarchy assembles and mounts the overlay for one hierarchy. Skips
// the hierarchy entirely (no overlay) when no extension contributes to it.
func mergeHierarchy(class release.Class, images []discover.Image, roots []string, opts MergeOptions, hierarchy string, st *mergeState) error {
	ws := st.workspace
	esc := escapeHierarchy(hierarchy)
	rel := strings.TrimPrefix(filepath.Clean(hierarchy), "/")

	// Contributing extension dirs, reverse version order (newest first =
	// topmost lowerdir); contributor names recorded ascending (merge order).
	var extDirs []string
	var contributors []discover.Image
	for i := range images {
		if dirNonEmpty(filepath.Join(roots[i], rel)) {
			contributors = append(contributors, images[i])
		}
	}
	if len(contributors) == 0 {
		return nil
	}
	for i := len(images) - 1; i >= 0; i-- {
		d := filepath.Join(roots[i], rel)
		if dirNonEmpty(d) {
			extDirs = append(extDirs, d)
		}
	}

	// Marker metadata (topmost lowerdir).
	metaDir := filepath.Join(ws, "meta", esc)
	markerDir := filepath.Join(metaDir, MarkerDirName(class))
	names := make([]string, len(contributors))
	origins := make([]originEntry, len(contributors))
	for i, img := range contributors {
		names[i] = img.Name
		origins[i] = originEntry{Name: img.Name, Path: img.Path, Type: imageTypeString(img.Type)}
	}
	if err := writeMarker(markerDir, names, origins); err != nil {
		return fmt.Errorf("writing marker for %s: %w", hierarchy, err)
	}

	// lowerdir = meta : newest..oldest extension : host (if present/non-empty)
	lower := append([]string{metaDir}, extDirs...)
	hostDir := filepath.Join(opts.Root, hierarchy)
	if dirNonEmpty(hostDir) {
		lower = append(lower, hostDir)
	}

	staging := filepath.Join(ws, "overlay", esc)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("creating staging dir %s: %w", staging, err)
	}
	flags := overlayMountFlags(class, opts.NoExec)
	data := "lowerdir=" + buildLowerdir(lower)
	if err := unix.Mount("overlay", staging, "overlay", flags, data); err != nil {
		return fmt.Errorf("mounting overlay for %s (%s): %w", hierarchy, data, err)
	}
	st.staged = append(st.staged, staging)

	if err := writeAndVerifyDev(staging, markerDir, MarkerDirName(class), flags, data); err != nil {
		return fmt.Errorf("recording dev marker for %s: %w", hierarchy, err)
	}

	if err := unix.Mount(staging, hostDir, "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("moving overlay onto %s: %w", hostDir, err)
	}
	st.staged = st.staged[:len(st.staged)-1]
	st.moved = append(st.moved, hostDir)
	return nil
}

// writeAndVerifyDev stats the staged overlay mount, writes its st_dev as a
// decimal string into <markerDir>/dev (which lives in the topmost lowerdir),
// then verifies the file is readable *through* the staged overlay. Because
// the dev file is created after the overlay was mounted, a cached negative
// dentry may hide it; in that case the overlay is unmounted and remounted
// (the file now exists at mount time), restat'd (anonymous overlay devs
// change across mounts), rewritten and re-verified.
func writeAndVerifyDev(staging, markerDir, markerName string, flags uintptr, data string) error {
	const attempts = 3
	devFile := filepath.Join(markerDir, "dev")
	mergedDevFile := filepath.Join(staging, markerName, "dev")
	var lastErr error
	for i := 0; i < attempts; i++ {
		var stt unix.Stat_t
		if err := unix.Stat(staging, &stt); err != nil {
			return fmt.Errorf("stat %s: %w", staging, err)
		}
		want := strconv.FormatUint(uint64(stt.Dev), 10)
		if err := os.WriteFile(devFile, []byte(want+"\n"), 0o644); err != nil {
			return err
		}
		got, err := os.ReadFile(mergedDevFile)
		if err == nil && strings.TrimSpace(string(got)) == want {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("dev marker mismatch: merged tree has %q, want %q", strings.TrimSpace(string(got)), want)
		}
		if i == attempts-1 {
			break
		}
		// Remount so the now-existing dev file is visible, then retry.
		if err := unix.Unmount(staging, 0); err != nil {
			return fmt.Errorf("unmounting %s for dev remount: %w", staging, err)
		}
		if err := unix.Mount("overlay", staging, "overlay", flags, data); err != nil {
			return fmt.Errorf("remounting overlay at %s: %w", staging, err)
		}
	}
	return fmt.Errorf("dev marker not visible through overlay at %s: %w", staging, lastErr)
}

// unmountWithRetry unmounts target, retrying on EBUSY and finally falling
// back to a lazy MNT_DETACH. "Not mounted" conditions (EINVAL, ENOENT) are
// treated as success so callers stay idempotent.
func unmountWithRetry(target string) error {
	// Not a mount point (or gone) → nothing to do. This also keeps cleanup
	// idempotent for unprivileged callers, where umount2 on a non-mount
	// reports EPERM before EINVAL.
	if mp, err := isMountPoint(target); err == nil && !mp {
		return nil
	}
	var err error
	for i := 0; i < 5; i++ {
		err = unix.Unmount(target, 0)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	derr := unix.Unmount(target, unix.MNT_DETACH)
	if derr == nil || errors.Is(derr, unix.EINVAL) || errors.Is(derr, unix.ENOENT) {
		return nil
	}
	return fmt.Errorf("unmounting %s: %w (lazy detach: %v)", target, err, derr)
}

// collectRawOriginPaths gathers the backing-file paths of raw images from
// all origin markers in the workspace, deduplicated.
func collectRawOriginPaths(class release.Class, ws string) []string {
	seen := map[string]bool{}
	var paths []string
	entries, err := os.ReadDir(filepath.Join(ws, "meta"))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		origins, err := readOriginFile(filepath.Join(ws, "meta", e.Name(), MarkerDirName(class), "origin"))
		if err != nil {
			continue
		}
		for _, o := range origins {
			if o.Type == "raw" && !seen[o.Path] {
				seen[o.Path] = true
				paths = append(paths, o.Path)
			}
		}
	}
	return paths
}

// Unmerge detaches all merged hierarchies for the class: for each hierarchy
// merged by us, unmount the overlay (MNT_DETACH fallback), then unmount image
// mounts and detach loop devices, then remove workspace dirs. Idempotent —
// returns nil when nothing is merged.
func Unmerge(class release.Class, root string) error {
	var errs []error
	for _, h := range Hierarchies(class) {
		merged, err := IsMergedByUs(class, root, h)
		if err != nil {
			errs = append(errs, fmt.Errorf("checking %s: %w", h, err))
			continue
		}
		if !merged {
			continue
		}
		if err := unmountWithRetry(filepath.Join(root, h)); err != nil {
			errs = append(errs, err)
		}
	}

	ws := Workspace(class, root)
	if _, err := os.Stat(ws); errors.Is(err, os.ErrNotExist) {
		return errors.Join(errs...)
	}

	// Leftover staged overlays from an interrupted merge.
	if entries, err := os.ReadDir(filepath.Join(ws, "overlay")); err == nil {
		for _, e := range entries {
			_ = unmountWithRetry(filepath.Join(ws, "overlay", e.Name()))
		}
	}

	// Backing-file paths must be collected before the workspace is removed.
	rawPaths := collectRawOriginPaths(class, ws)

	// Image mounts under extensions/<name>.
	if entries, err := os.ReadDir(filepath.Join(ws, "extensions")); err == nil {
		for _, e := range entries {
			_ = unmountWithRetry(filepath.Join(ws, "extensions", e.Name()))
		}
	}

	// Detach any loop devices still backed by raw image files.
	for _, p := range rawPaths {
		if err := image.DetachAllLoopsFor(p); err != nil {
			errs = append(errs, fmt.Errorf("detaching loops for %s: %w", p, err))
		}
	}

	if err := os.RemoveAll(ws); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// CurrentStatus reads the per-hierarchy merge state from the markers.
func CurrentStatus(class release.Class, root string) ([]Status, error) {
	hierarchies := Hierarchies(class)
	statuses := make([]Status, 0, len(hierarchies))
	for _, h := range hierarchies {
		s := Status{Hierarchy: h}
		merged, err := IsMergedByUs(class, root, h)
		if err != nil {
			return nil, err
		}
		if merged {
			s.Merged = true
			markerDir := filepath.Join(root, h, MarkerDirName(class))
			if names, err := readExtensionsFile(filepath.Join(markerDir, "extensions")); err == nil {
				s.Extensions = names
			}
			if fi, err := os.Stat(markerDir); err == nil {
				s.Since = fi.ModTime().Unix()
			}
		}
		statuses = append(statuses, s)
	}
	return statuses, nil
}

// MergedExtensions returns the extension names recorded in the marker of a
// merged hierarchy (empty if unmerged). Used by refresh change detection.
func MergedExtensions(class release.Class, root, hierarchy string) ([]string, error) {
	merged, err := IsMergedByUs(class, root, hierarchy)
	if err != nil || !merged {
		return nil, err
	}
	return readExtensionsFile(filepath.Join(root, hierarchy, MarkerDirName(class), "extensions"))
}

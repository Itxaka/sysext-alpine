// Package overlay merges extension images into their hierarchies the way
// systemd-sysext 262 does: one read-only (or mutable) overlayfs per
// hierarchy, whose top layer carries the .systemd-sysext (.systemd-confext)
// metadata directory, with mounts below the hierarchy carried over.
//
// Image mounts and overlay layers live in a private tmpfs workspace below
// /run/systemd that stays mounted while extensions are merged.
package overlay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// ErrAlreadyMerged is matched by the error Merge returns when a hierarchy is
// already merged.
var ErrAlreadyMerged = errors.New("already merged")

// ErrCleanup is matched by the error Merge and Refresh return when the
// extensions were merged, but what the merge left behind or replaced in the
// workspace could not be cleaned up; the Outcome describes the merge.
var ErrCleanup = errors.New("extensions merged, but failed to clean up")

// AlreadyMergedError names the hierarchy found merged.
type AlreadyMergedError struct {
	Hierarchy string
}

func (e *AlreadyMergedError) Error() string {
	return fmt.Sprintf("Hierarchy '%s' is already merged.", e.Hierarchy)
}

// Is makes errors.Is(err, ErrAlreadyMerged) hold.
func (e *AlreadyMergedError) Is(target error) bool { return target == ErrAlreadyMerged }

// runtimeDir holds the workspaces and lock files.
var runtimeDir = "/run/systemd"

// Hierarchies returns the merge targets of the class: /usr and /opt for
// sysext, /etc for confext, or the colon-separated list of absolute,
// normalized paths in $SYSTEMD_SYSEXT_HIERARCHIES ($SYSTEMD_CONFEXT_HIERARCHIES)
// when set. An invalid list is an error, like in systemd.
func Hierarchies(class release.Class) ([]string, error) {
	env, def := "SYSTEMD_SYSEXT_HIERARCHIES", []string{"/usr", "/opt"}
	if class == release.Confext {
		env, def = "SYSTEMD_CONFEXT_HIERARCHIES", []string{"/etc"}
	}
	value, ok := os.LookupEnv(env)
	if !ok {
		return def, nil
	}
	list, err := parseHierarchies(value)
	if err != nil {
		return nil, fmt.Errorf("failed to determine %s hierarchies: $%s: %w", classIdentifier(class), env, err)
	}
	return list, nil
}

// parseHierarchies is getenv_path_list().
func parseHierarchies(value string) ([]string, error) {
	var list []string
	if value != "" {
		for _, p := range splitColon(value) {
			switch {
			case !filepath.IsAbs(p):
				return nil, fmt.Errorf("path '%s' is not absolute, refusing: %w", p, unix.EINVAL)
			case !pathIsNormalized(p):
				return nil, fmt.Errorf("path '%s' is not normalized, refusing: %w", p, unix.EINVAL)
			case filepath.Clean(p) == "/":
				return nil, fmt.Errorf("path '%s' is the root fs, refusing: %w", p, unix.EINVAL)
			}
			list = append(list, p)
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no paths specified, refusing: %w", unix.EINVAL)
	}
	return list, nil
}

func splitColon(s string) []string {
	var out []string
	for {
		i := 0
		for i < len(s) && s[i] != ':' {
			i++
		}
		out = append(out, s[:i])
		if i == len(s) {
			return out
		}
		s = s[i+1:]
	}
}

// resolveRoot resolves --root like systemd's chase(root, NULL,
// CHASE_MUST_BE_DIRECTORY); "" is the host root.
func resolveRoot(root string) (string, error) {
	if root == "" || root == "/" {
		return "/", nil
	}
	abs, err := filepath.Abs(root)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err == nil {
		err = mustBeDir(abs)
	}
	if err != nil {
		return "", fmt.Errorf("failed to resolve --root='%s': %w", root, err)
	}
	return abs, nil
}

// workspaceFor returns the workspace of class for a resolved root. Merges
// into a foreign root keep their workspace on the host as well, never inside
// the root, in a directory named after the root.
func workspaceFor(class release.Class, root string) string {
	name := classIdentifier(class)
	if root != "/" {
		sum := sha256.Sum256([]byte(root))
		name += "." + hex.EncodeToString(sum[:8])
	}
	return filepath.Join(runtimeDir, name)
}

// Workspace returns the directory below which the images merged into root
// are mounted, at extensions/<name>.
func Workspace(class release.Class, root string) (string, error) {
	r, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	return workspaceFor(class, r), nil
}

// resolveHierarchy is chase(hierarchy, root, CHASE_PREFIX_ROOT): "" when the
// hierarchy does not exist.
func resolveHierarchy(root, hierarchy string) (string, error) {
	p, err := fsutil.Chase(root, hierarchy, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to resolve path to hierarchy '%s': %w", hierarchy, err)
	}
	return p, nil
}

// IsMergedByUs reports whether hierarchy, resolved inside root, is an
// overlay set up by a merge: a mount point whose metadata dev marker
// matches it.
func IsMergedByUs(class release.Class, root, hierarchy string) (bool, error) {
	r, err := resolveRoot(root)
	if err != nil {
		return false, err
	}
	p, err := resolveHierarchy(r, hierarchy)
	if err != nil || p == "" {
		return false, err
	}
	return isOurMountPoint(class, p)
}

// Status describes the merge state of one hierarchy.
type Status struct {
	Hierarchy  string
	Merged     bool
	Extensions []string // merged extension names, in merge order
	Since      int64    // mtime of the merged hierarchy in µs; 0 if unmerged
}

// CurrentStatus reports the state of every existing hierarchy of the class,
// like systemd-sysext status. Hierarchies that cannot be inspected are
// reported in the error and left out.
func CurrentStatus(class release.Class, root string) ([]Status, error) {
	hierarchies, err := Hierarchies(class)
	if err != nil {
		return nil, err
	}
	r, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	var statuses []Status
	var errs []error
	for _, h := range hierarchies {
		p, err := resolveHierarchy(r, h)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p == "" {
			continue
		}
		ours, err := isOurMountPoint(class, p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ours {
			statuses = append(statuses, Status{Hierarchy: h})
			continue
		}
		f := filepath.Join(p, MarkerDirName(class), listFileName(class))
		data, err := os.ReadFile(f)
		if err != nil {
			return statuses, fmt.Errorf("failed to open '%s': %w", f, err)
		}
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			return statuses, fmt.Errorf("stat %s: %w", p, err)
		}
		statuses = append(statuses, Status{
			Hierarchy:  h,
			Merged:     true,
			Extensions: parseList(string(data)),
			Since:      st.Mtim.Nano() / 1_000,
		})
	}
	return statuses, errors.Join(errs...)
}

// NoExec values of MergeOptions.NoExec, systemd's --noexec= tristate.
const (
	NoExecDefault = -1 // the class default: exec for sysext, noexec for confext
	NoExecOff     = 0
	NoExecOn      = 1
)

// MergeOptions tune Merge and Refresh.
type MergeOptions struct {
	// Root is the OS tree to merge into; "" is the host.
	Root string
	// Mutable is the --mutable= mode: "no" ("" means "no"), "auto",
	// "yes", "import", "ephemeral" or "ephemeral-import".
	Mutable string
	// MountOptions, when set, are the extra overlayfs mount options
	// ($SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS). They replace the default
	// options of mutable overlays, also when empty; nil keeps them.
	MountOptions *string
	// ImagePolicy is the image policy applied to disk images; "" selects
	// the class default.
	ImagePolicy string
	// Arch is the architecture partitions are picked for; "" is the
	// running one.
	Arch string
	// NoExec is one of NoExecDefault, NoExecOff and NoExecOn.
	NoExec int
	// AlwaysRefresh makes Refresh remerge even when nothing changed.
	AlwaysRefresh bool
	// Check, when set, is called for every image once it is mounted, with
	// the directory holding its tree: false ignores the image (counted in
	// Outcome.Ignored), an error aborts the merge.
	Check func(img discover.Image, tree string) (bool, error)
	// Warnf, when set, receives non-fatal diagnostics.
	Warnf func(format string, args ...any)
	// BeforeMerge, when set, is called once the images are mounted and
	// checked and the merge goes ahead, before any hierarchy is touched,
	// with the names of the extensions to merge (none for a mutable merge
	// without extensions).
	BeforeMerge func(merged []string)
	// BeforeUnmerge, when set, is called by Refresh right before it unmerges
	// the class because no extension qualified, while the old merge is
	// still in place. An error aborts the refresh, leaving the merge as it
	// is.
	BeforeUnmerge func() error
}

// ImageError is the error of an image that could not be mounted.
type ImageError struct {
	Image discover.Image
	Err   error
}

func (e *ImageError) Error() string {
	return "failed to mount image " + e.Image.Name + ": " + e.Err.Error()
}

func (e *ImageError) Unwrap() error { return e.Err }

// Result is what Merge or Refresh did.
type Result int

const (
	// NothingFound: no extension qualified (and no mutable mode asked for
	// a merge anyway); nothing was merged. Refresh unmerged instead.
	NothingFound Result = iota
	// Merged: the hierarchies are merged now.
	Merged
	// Unchanged: Refresh found the merged state up to date.
	Unchanged
)

// Outcome reports the result of Merge or Refresh.
type Outcome struct {
	Result Result
	// Merged lists the merged extension names, in merge order.
	Merged []string
	// Ignored counts the images rejected by MergeOptions.Check.
	Ignored int
	// Hierarchies lists the resolved hierarchy paths merged.
	Hierarchies []string
	// Unmerged lists the resolved hierarchy paths whose previous merge was
	// removed.
	Unmerged []string
}

// Merge mounts the images (sorted in merge order, as discover returns them)
// into the hierarchies of the class. It fails with an AlreadyMergedError if
// a hierarchy is merged already. On error everything is rolled back.
func Merge(class release.Class, images []discover.Image, opts MergeOptions) (Outcome, error) {
	return run(class, images, opts, false)
}

// Refresh brings the merged state in line with images like
// systemd-sysext refresh: images are mounted and checked first and compared
// with the recorded origin of the current merge; only when something changed
// (or AlwaysRefresh is set) the old merge is replaced. A failure before that
// leaves the old merge in place. When no extension qualifies the class is
// unmerged. The whole operation runs under one lock.
func Refresh(class release.Class, images []discover.Image, opts MergeOptions) (Outcome, error) {
	return run(class, images, opts, true)
}

// Unmerge removes the merged overlays of the class, restoring mounts that
// were made below the hierarchies, and releases the workspace. It returns
// the resolved hierarchy paths it unmerged; nothing merged is not an error.
func Unmerge(class release.Class, root string) ([]string, error) {
	hierarchies, err := Hierarchies(class)
	if err != nil {
		return nil, err
	}
	r, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	unlock, err := lock(class, r)
	if err != nil {
		return nil, err
	}
	defer unlock()

	ws := workspaceFor(class, r)
	var unmerged []string
	var errs []error
	for _, h := range hierarchies {
		p, err := resolveHierarchy(r, h)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p == "" {
			continue
		}
		n, err := unmergeInPlace(class, r, h, p, ws)
		if n > 0 {
			unmerged = append(unmerged, p)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return unmerged, errors.Join(errs...)
	}
	return unmerged, teardownWorkspace(ws)
}

// unmergeHierarchy is unmerge_hierarchy(): it unmounts our overlays at dir
// (several when stacked), after taking the mounts below it aside, which
// the caller attaches where they belong. The recorded workdirs are removed.
func unmergeHierarchy(class release.Class, root, hierarchy, dir, ws string) ([]savedMount, int, error) {
	var subs []savedMount
	n := 0
	for {
		ours, err := isOurMountPoint(class, dir)
		if err != nil {
			return subs, n, err
		}
		if !ours {
			return subs, n, nil
		}
		workDir, err := recordedWorkDir(class, root, dir, hierarchy)
		if err != nil {
			return subs, n, err
		}
		if err := detach(filepath.Join(dir, MarkerDirName(class))); err != nil {
			return subs, n, err
		}
		s, err := takeSubmounts(dir, belowWorkspace(ws, dir))
		subs = append(subs, s...)
		if err != nil {
			return subs, n, err
		}
		if err := unix.Unmount(dir, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW); err != nil {
			return subs, n, fmt.Errorf("failed to unmount %s: %w", dir, err)
		}
		n++
		if workDir != "" {
			if err := removeTree(workDir); err != nil {
				return subs, n, fmt.Errorf("failed to remove '%s': %w", workDir, err)
			}
		}
	}
}

// belowWorkspace keeps the mounts of the workspace out of the submounts
// carried along with dir, unless dir itself lies inside the workspace (an
// overlay at its staging point, whose submounts all came from the host).
func belowWorkspace(ws, dir string) func(string) bool {
	if fsutil.IsBelow(dir, ws) {
		return nil
	}
	return func(p string) bool { return fsutil.IsBelow(p, ws) }
}

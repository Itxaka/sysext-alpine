package overlay

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/errno"
	"github.com/itxaka/sysext-alpine/internal/fsutil"
)

// The --mutable= modes follow systemd-sysext(8) "MUTABILITY"; see
// docs/MUTABLE.md.
const (
	// mutableBase holds the persistent upper directories, one per
	// hierarchy, relative to the root.
	mutableBase = "/var/lib/extensions.mutable"

	// mutableMountOptions is MUTABLE_EXTENSIONS_MOUNT_OPTIONS, used for
	// writable overlays unless overridden by MergeOptions.MountOptions.
	mutableMountOptions = "redirect_dir=on,noatime,metacopy=off,index=off"

	// ephemeralWorkspace is systemd's fixed workspace path, recorded in the
	// origin as the mutable directory of the ephemeral modes.
	ephemeralWorkspace = "/run/systemd/sysext"
)

// normalizeMutableMode validates a mutable mode and maps "" to "no".
func normalizeMutableMode(mode string) (string, error) {
	switch mode {
	case "", "no":
		return "no", nil
	case "auto", "yes", "import", "ephemeral", "ephemeral-import":
		return mode, nil
	}
	return "", fmt.Errorf("invalid mutable mode %q", mode)
}

func modeIsEphemeral(mode string) bool {
	return mode == "ephemeral" || mode == "ephemeral-import"
}

// resolveMutableDir is resolve_mutable_directory(): the directory serving
// as upper (or, for "import", extra lower) layer of hierarchy, created and
// given the hierarchy's mode where the mode asks for it. workspace replaces
// /var/lib/extensions.mutable for the ephemeral modes. "" means none.
func resolveMutableDir(mode, root, hierarchy string, hierarchyMode uint32, workspace string) (string, error) {
	if mode == "no" {
		return "", nil
	}
	name := singlePathComponent(hierarchy)
	if modeIsEphemeral(mode) {
		dir := filepath.Join(workspace, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("failed to create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, fs.FileMode(hierarchyMode)); err != nil {
			return "", fmt.Errorf("failed to chmod directory %s: %w", dir, err)
		}
		return dir, nil
	}

	rel := filepath.Join(mutableBase, name)
	if mode == "yes" || mode == "auto" {
		if err := mutableDirModeMatches(root, rel, hierarchyMode); err != nil {
			return "", err
		}
	}
	if mode == "yes" {
		p, err := fsutil.Chase(root, rel, fsutil.ChaseNonexistent)
		if err == nil {
			err = os.MkdirAll(p, 0o755)
		}
		if err == nil {
			p, err = fsutil.Chase(root, rel, 0)
		}
		if err == nil {
			err = mustBeDir(p)
		}
		if err == nil {
			err = os.Chmod(p, fs.FileMode(hierarchyMode))
		}
		if err != nil {
			return "", fmt.Errorf("failed to create mutable directory %s: %w", rel, err)
		}
	}
	resolved, err := fsutil.Chase(root, rel, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to resolve mutable directory %s: %w", rel, err)
	}
	return resolved, nil
}

// mutableDirModeMatches is mutable_directory_mode_matches_hierarchy(): an
// existing mutable directory must have the hierarchy's mode, as the merged
// hierarchy takes the mode of its top layer.
func mutableDirModeMatches(root, rel string, hierarchyMode uint32) error {
	p, err := fsutil.Chase(root, rel, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("failed to stat mutable directory %s: %w", p, err)
	}
	if mode := st.Mode & 0o777; mode != hierarchyMode {
		return errno.New(unix.EINVAL, "Mutable directory '%s' has mode %04o, ought to have mode %04o", p, mode, hierarchyMode)
	}
	return nil
}

func mustBeDir(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return &fs.PathError{Op: "chase", Path: p, Err: unix.ENOTDIR}
	}
	return nil
}

// importedMutableDir is the extra lowerdir directly below the metadata of
// the "import" (the mutable directory itself) and "ephemeral-import" (the
// persistent mutable directory) modes. Importing the hierarchy into itself
// is refused like systemd does (ELOOP).
func importedMutableDir(mode, root, hierarchy, resolvedHierarchy, mutableDir string) (string, error) {
	var dir string
	switch mode {
	case "import":
		dir = mutableDir
	case "ephemeral-import":
		p, err := fsutil.Chase(root, filepath.Join(mutableBase, singlePathComponent(hierarchy)), 0)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("failed to resolve mutable directory for %s: %w", hierarchy, err)
		}
		dir = p
	}
	if dir == "" {
		return "", nil
	}
	same, err := sameDir(resolvedHierarchy, dir)
	if err != nil {
		return "", err
	}
	if same {
		return "", errno.New(unix.ELOOP, "Not importing mutable directory for hierarchy %s as a lower dir, because it points to the hierarchy itself", hierarchy)
	}
	return dir, nil
}

// hostAsLowerDir is hierarchy_as_lower_dir(): the host hierarchy is the
// bottom layer unless it is missing or empty, or serves as upper directory.
func hostAsLowerDir(mode, resolvedHierarchy, mutableDir string) (bool, error) {
	if resolvedHierarchy == "" {
		return false, nil
	}
	empty, err := dirIsEmpty(resolvedHierarchy)
	if err != nil {
		return false, fmt.Errorf("failed to check if host hierarchy %s is empty: %w", resolvedHierarchy, err)
	}
	if empty {
		return false, nil
	}
	if mode == "import" || mode == "ephemeral-import" || mutableDir == "" {
		return true, nil
	}
	same, err := sameDir(resolvedHierarchy, mutableDir)
	return !same, err
}

// upperDir is determine_upper_dir(): the mutable directory, which must be
// writable, unless the mode only imports it.
func upperDir(mode, mutableDir string) (string, error) {
	if mode == "import" || mutableDir == "" {
		return "", nil
	}
	ro, err := isReadOnlyFS(mutableDir)
	if err != nil {
		return "", fmt.Errorf("failed to determine if mutable directory %s is on read-only filesystem: %w", mutableDir, err)
	}
	if ro {
		return "", errno.New(unix.EROFS, "Can't use '%s' as an upperdir as it is read-only.", mutableDir)
	}
	return mutableDir, nil
}

// workDirFor is work_dir_for_hierarchy(): the workdir is a hidden sibling of
// the upper directory, which must share its filesystem.
func workDirFor(hierarchy, upper string) (string, error) {
	parent := filepath.Dir(upper)
	same, err := sameFS(upper, parent)
	if err != nil {
		return "", err
	}
	if !same {
		return "", errno.New(unix.EXDEV, "Unable to find a suitable workdir location for upperdir '%s' for host hierarchy '%s' - parent directory of the upperdir is in a different filesystem", upper, hierarchy)
	}
	return filepath.Join(parent, workDirName(hierarchy)), nil
}

// sameDir is path_equal_or_inode_same().
func sameDir(a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true, nil
	}
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino && sa.Mode&unix.S_IFMT == sb.Mode&unix.S_IFMT, nil
}

func sameFS(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}

// isReadOnlyFS is path_is_read_only_fs().
func isReadOnlyFS(p string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return false, err
	}
	if st.Flags&unix.ST_RDONLY != 0 {
		return true, nil
	}
	return errors.Is(unix.Access(p, unix.W_OK), unix.EROFS), nil
}

// dirIsEmpty is dir_is_empty() without ignoring hidden entries.
func dirIsEmpty(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(names) == 0, nil
}

package overlay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
)

// savedMount is a mount detached from its place as an independent clone,
// waiting to be attached elsewhere (systemd's move_submounts()).
type savedMount struct {
	rel  string // path relative to the directory it was taken from
	fd   int    // open_tree() clone; -1 once attached
	mode uint32 // st_mode of the mount root, to create a mount point
}

// selectSubmounts picks the mounts strictly below dir that have to be cloned
// to carry everything below dir along: the visible top-level ones, sorted.
// Nested mounts travel inside the recursive clone of their parent. visible
// reports whether a mountinfo entry is the mount currently reachable at its
// path (not over-mounted).
func selectSubmounts(mounts []fsutil.MountInfo, dir string, skip func(string) bool, visible func(fsutil.MountInfo) bool) []string {
	var paths []string
	for _, m := range fsutil.MountsBelow(mounts, dir) {
		if skip != nil && skip(m.MountPoint) {
			continue
		}
		if slices.Contains(paths, m.MountPoint) || !visible(m) {
			continue
		}
		paths = append(paths, m.MountPoint)
	}
	var top []string
	for _, p := range paths {
		nested := false
		for _, q := range paths {
			if q != p && fsutil.IsBelow(p, q) {
				nested = true
				break
			}
		}
		if !nested {
			top = append(top, p)
		}
	}
	slices.SortFunc(top, comparePaths)
	return top
}

// comparePaths is path_compare(): component-wise, so that a directory sorts
// directly before its children.
func comparePaths(a, b string) int {
	return slices.Compare(strings.Split(strings.Trim(a, "/"), "/"), strings.Split(strings.Trim(b, "/"), "/"))
}

// mountVisible reports whether the mount m is the one reachable at its
// mount point, like libmount_fs_id_matches_path().
func mountVisible(m fsutil.MountInfo) bool {
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, m.MountPoint, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT|unix.AT_STATX_DONT_SYNC, unix.STATX_MNT_ID, &stx)
	if err != nil {
		return false
	}
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return true
	}
	return stx.Mnt_id == uint64(m.ID)
}

// takeSubmounts detaches the mounts below dir and returns them as
// independent clones; skip excludes mount points. All mounts are cloned
// first (recursively, with private propagation so that detaching an
// original cannot propagate into its clone) and the originals are detached
// only once every clone exists: on error nothing is detached.
func takeSubmounts(dir string, skip func(string) bool) ([]savedMount, error) {
	mounts, err := fsutil.ReadMountInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get submounts for %s: %w", dir, err)
	}
	var saved []savedMount
	var originals []string
	for _, p := range selectSubmounts(mounts, dir, skip, mountVisible) {
		s, err := cloneMount(dir, p)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			closeSaved(saved)
			return nil, err
		}
		saved = append(saved, s)
		originals = append(originals, p)
	}
	for _, p := range originals {
		_ = unix.Unmount(p, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW)
	}
	return saved, nil
}

// cloneMount clones the mount tree at p, a mount below dir.
func cloneMount(dir, p string) (savedMount, error) {
	fd, err := unix.OpenTree(unix.AT_FDCWD, p, unix.OPEN_TREE_CLONE|unix.O_CLOEXEC|unix.AT_RECURSIVE|unix.AT_NO_AUTOMOUNT|unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return savedMount{}, fmt.Errorf("failed to open subtree of mounted filesystem %s: %w", p, err)
	}
	err = unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &unix.MountAttr{Propagation: unix.MS_PRIVATE})
	if err != nil && !errnoIsNotSupported(err) {
		unix.Close(fd)
		return savedMount{}, fmt.Errorf("failed to make subtree %s private: %w", p, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return savedMount{}, fmt.Errorf("stat %s: %w", p, err)
	}
	rel, _ := filepath.Rel(dir, p)
	return savedMount{rel: rel, fd: fd, mode: st.Mode}, nil
}

// attachSubmounts attaches the saved clones below dst, creating mount points
// as needed. Clones that cannot be attached stay pending (fd >= 0); the
// errors of all of them are returned.
func attachSubmounts(dst string, saved []savedMount) error {
	var errs []error
	for i := range saved {
		s := &saved[i]
		if s.fd < 0 {
			continue
		}
		target := filepath.Join(dst, s.rel)
		if err := makeMountPoint(dst, s.rel, s.mode); err != nil {
			errs = append(errs, fmt.Errorf("failed to create mountpoint %s: %w", target, err))
			continue
		}
		if err := unix.MoveMount(s.fd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
			errs = append(errs, fmt.Errorf("failed to move mount to %s: %w", target, err))
			continue
		}
		unix.Close(s.fd)
		s.fd = -1
	}
	return errors.Join(errs...)
}

func closeSaved(saved []savedMount) {
	for i := range saved {
		if saved[i].fd >= 0 {
			unix.Close(saved[i].fd)
			saved[i].fd = -1
		}
	}
}

// makeMountPoint creates dst/rel (parents as directories, the last component
// matching mode) without following symlinks below dst. Existing entries are
// fine.
func makeMountPoint(dst, rel string, mode uint32) error {
	parts := strings.Split(rel, "/")
	cur := dst
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		last := i == len(parts)-1
		var st unix.Stat_t
		if err := unix.Lstat(cur, &st); err == nil {
			if st.Mode&unix.S_IFMT == unix.S_IFLNK {
				return &os.PathError{Op: "mkdir", Path: cur, Err: unix.ELOOP}
			}
			continue
		}
		var err error
		if !last || mode&unix.S_IFMT == unix.S_IFDIR {
			err = unix.Mkdir(cur, 0o755)
		} else {
			err = unix.Mknod(cur, unix.S_IFREG|0o644, 0)
		}
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return &os.PathError{Op: "mkdir", Path: cur, Err: err}
		}
	}
	return nil
}

// detach lazily unmounts target without following symlinks; "not mounted"
// is fine.
func detach(target string) error {
	err := unix.Unmount(target, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW)
	if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return fmt.Errorf("failed to unmount %s: %w", target, err)
}

// detachAll unmounts every mount at or below dir, top-level mounts first
// (a lazy unmount takes the mounts below along), until none is left.
func detachAll(dir string) error {
	for range 64 {
		mounts, err := fsutil.ReadMountInfo()
		if err != nil {
			return err
		}
		var targets []string
		for _, m := range mounts {
			if fsutil.IsBelow(m.MountPoint, dir) && !slices.Contains(targets, m.MountPoint) {
				targets = append(targets, m.MountPoint)
			}
		}
		if len(targets) == 0 {
			return nil
		}
		for _, t := range selectTop(targets) {
			if err := detach(t); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("mounts below %s keep reappearing", dir)
}

func selectTop(paths []string) []string {
	var top []string
	for _, p := range paths {
		nested := false
		for _, q := range paths {
			if q != p && fsutil.IsBelow(p, q) {
				nested = true
				break
			}
		}
		if !nested {
			top = append(top, p)
		}
	}
	return top
}

// hasMountsBelow reports whether anything is mounted at or below dir.
func hasMountsBelow(dir string) (bool, error) {
	mounts, err := fsutil.ReadMountInfo()
	if err != nil {
		return false, err
	}
	for _, m := range mounts {
		if fsutil.IsBelow(m.MountPoint, dir) {
			return true, nil
		}
	}
	return false, nil
}

// removeTree deletes dir recursively after making sure no mount lives below
// it, so that removal can never reach into another filesystem.
func removeTree(dir string) error {
	busy, err := hasMountsBelow(dir)
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("refusing to remove %s: something is mounted below it", dir)
	}
	return os.RemoveAll(dir)
}

// mountFlag is one entry of libmount's MNT_LINUX_MAP.
type mountFlag struct {
	flag   uintptr
	invert bool
}

var mountFlags = map[string]mountFlag{
	"defaults":      {},
	"ro":            {unix.MS_RDONLY, false},
	"rw":            {unix.MS_RDONLY, true},
	"exec":          {unix.MS_NOEXEC, true},
	"noexec":        {unix.MS_NOEXEC, false},
	"suid":          {unix.MS_NOSUID, true},
	"nosuid":        {unix.MS_NOSUID, false},
	"dev":           {unix.MS_NODEV, true},
	"nodev":         {unix.MS_NODEV, false},
	"sync":          {unix.MS_SYNCHRONOUS, false},
	"async":         {unix.MS_SYNCHRONOUS, true},
	"dirsync":       {unix.MS_DIRSYNC, false},
	"silent":        {unix.MS_SILENT, false},
	"loud":          {unix.MS_SILENT, true},
	"mand":          {unix.MS_MANDLOCK, false},
	"nomand":        {unix.MS_MANDLOCK, true},
	"atime":         {unix.MS_NOATIME, true},
	"noatime":       {unix.MS_NOATIME, false},
	"iversion":      {unix.MS_I_VERSION, false},
	"noiversion":    {unix.MS_I_VERSION, true},
	"diratime":      {unix.MS_NODIRATIME, true},
	"nodiratime":    {unix.MS_NODIRATIME, false},
	"relatime":      {unix.MS_RELATIME, false},
	"norelatime":    {unix.MS_RELATIME, true},
	"strictatime":   {unix.MS_STRICTATIME, false},
	"nostrictatime": {unix.MS_STRICTATIME, true},
	"lazytime":      {unix.MS_LAZYTIME, false},
	"nolazytime":    {unix.MS_LAZYTIME, true},
	"symfollow":     {unix.MS_NOSYMFOLLOW, true},
	"nosymfollow":   {unix.MS_NOSYMFOLLOW, false},
}

// mangleMountOptions is mount_option_mangle(): generic mount flags in a
// comma-separated option string become MS_* flags applied to flags, "x-"
// options are dropped and everything else is passed on as data. Backslash
// escaped commas do not split.
func mangleMountOptions(options string, flags uintptr) (uintptr, string) {
	var data []string
	for _, word := range splitOptions(options) {
		if f, ok := mountFlags[word]; ok {
			if f.invert {
				flags &^= f.flag
			} else {
				flags |= f.flag
			}
			continue
		}
		if word == "" || strings.HasPrefix(strings.ToLower(word), "x-") {
			continue
		}
		data = append(data, word)
	}
	return flags, strings.Join(data, ",")
}

func splitOptions(s string) []string {
	var words []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			cur.WriteByte(s[i])
			cur.WriteByte(s[i+1])
			i++
		case s[i] == ',':
			words = append(words, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(words, cur.String())
}

var mountFlagNames = []struct {
	flag uintptr
	name string
}{
	{unix.MS_RDONLY, "MS_RDONLY"},
	{unix.MS_NOSUID, "MS_NOSUID"},
	{unix.MS_NODEV, "MS_NODEV"},
	{unix.MS_NOEXEC, "MS_NOEXEC"},
	{unix.MS_SYNCHRONOUS, "MS_SYNCHRONOUS"},
	{unix.MS_REMOUNT, "MS_REMOUNT"},
	{unix.MS_MANDLOCK, "MS_MANDLOCK"},
	{unix.MS_DIRSYNC, "MS_DIRSYNC"},
	{unix.MS_NOSYMFOLLOW, "MS_NOSYMFOLLOW"},
	{unix.MS_NOATIME, "MS_NOATIME"},
	{unix.MS_NODIRATIME, "MS_NODIRATIME"},
	{unix.MS_BIND, "MS_BIND"},
	{unix.MS_MOVE, "MS_MOVE"},
	{unix.MS_REC, "MS_REC"},
	{unix.MS_SILENT, "MS_SILENT"},
	{unix.MS_POSIXACL, "MS_POSIXACL"},
	{unix.MS_UNBINDABLE, "MS_UNBINDABLE"},
	{unix.MS_PRIVATE, "MS_PRIVATE"},
	{unix.MS_SLAVE, "MS_SLAVE"},
	{unix.MS_SHARED, "MS_SHARED"},
	{unix.MS_RELATIME, "MS_RELATIME"},
	{unix.MS_KERNMOUNT, "MS_KERNMOUNT"},
	{unix.MS_I_VERSION, "MS_I_VERSION"},
	{unix.MS_STRICTATIME, "MS_STRICTATIME"},
	{unix.MS_LAZYTIME, "MS_LAZYTIME"},
}

// mountFlagsString is mount_flags_to_string(): the names of the MS_* flags
// joined by "|", unknown bits (or no flags at all) in hex.
func mountFlagsString(flags uintptr) string {
	var names []string
	for _, f := range mountFlagNames {
		if flags&f.flag != 0 {
			names = append(names, f.name)
			flags &^= f.flag
		}
	}
	if len(names) == 0 || flags != 0 {
		names = append(names, strconv.FormatUint(uint64(flags), 16))
	}
	return strings.Join(names, "|")
}

// escapeLayer escapes a layer path for the overlayfs lowerdir/upperdir/
// workdir options.
func escapeLayer(p string) string {
	var b strings.Builder
	for i := range len(p) {
		switch p[i] {
		case '\\', ':', ',':
			b.WriteByte('\\')
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// overlayOptions is the option string of mount_overlayfs() before mangling.
func overlayOptions(lower []string, upper, work, extra string) string {
	escaped := make([]string, len(lower))
	for i, l := range lower {
		escaped[i] = escapeLayer(l)
	}
	opts := "lowerdir=" + strings.Join(escaped, ":")
	if upper != "" {
		opts += ",upperdir=" + escapeLayer(upper) + ",workdir=" + escapeLayer(work)
	}
	if extra != "" {
		opts += "," + extra
	}
	return opts
}

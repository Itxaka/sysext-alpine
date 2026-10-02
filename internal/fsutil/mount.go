package fsutil

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// IsMountPoint reports whether path is the root of a mount. Unlike a plain
// st_dev comparison it also detects bind mounts within one filesystem. A
// missing path is not a mount point. Symlinks are not followed.
func IsMountPoint(path string) (bool, error) {
	path = filepath.Clean(path)
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, unix.STATX_BASIC_STATS, &stx)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return false, nil
		}
		if !errors.Is(err, unix.ENOSYS) {
			return false, fmt.Errorf("statx %s: %w", path, err)
		}
	} else if stx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 {
		return stx.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0, nil
	}
	return isMountPointByDev(path)
}

func isMountPointByDev(path string) (bool, error) {
	var st, pst unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return false, nil
		}
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return true, nil
	}
	if err := unix.Lstat(parent, &pst); err != nil {
		return false, fmt.Errorf("lstat %s: %w", parent, err)
	}
	return st.Dev != pst.Dev || st.Ino == pst.Ino, nil
}

// MountInfo is one line of /proc/self/mountinfo.
type MountInfo struct {
	ID         int
	ParentID   int
	Major      uint32
	Minor      uint32
	Root       string
	MountPoint string
	Options    string
	Optional   []string
	FSType     string
	Source     string
	SuperOpts  string
}

// Shared reports whether the mount has shared propagation.
func (m MountInfo) Shared() bool {
	for _, f := range m.Optional {
		if strings.HasPrefix(f, "shared:") {
			return true
		}
	}
	return false
}

// ReadMountInfo parses /proc/self/mountinfo.
func ReadMountInfo() ([]MountInfo, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMountInfo(f)
}

// ParseMountInfo parses the proc(5) mountinfo format.
func ParseMountInfo(r io.Reader) ([]MountInfo, error) {
	var out []MountInfo
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			return nil, fmt.Errorf("malformed mountinfo line %q", line)
		}
		a := strings.Fields(pre)
		b := strings.Fields(post)
		if len(a) < 6 || len(b) < 2 {
			return nil, fmt.Errorf("malformed mountinfo line %q", line)
		}
		var m MountInfo
		var err error
		if m.ID, err = strconv.Atoi(a[0]); err != nil {
			return nil, fmt.Errorf("malformed mountinfo id in %q", line)
		}
		if m.ParentID, err = strconv.Atoi(a[1]); err != nil {
			return nil, fmt.Errorf("malformed mountinfo parent id in %q", line)
		}
		maj, mnr, ok := strings.Cut(a[2], ":")
		if !ok {
			return nil, fmt.Errorf("malformed mountinfo device in %q", line)
		}
		major, err1 := strconv.ParseUint(maj, 10, 32)
		minor, err2 := strconv.ParseUint(mnr, 10, 32)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("malformed mountinfo device in %q", line)
		}
		m.Major, m.Minor = uint32(major), uint32(minor)
		m.Root = unescapeMountInfo(a[3])
		m.MountPoint = unescapeMountInfo(a[4])
		m.Options = a[5]
		m.Optional = a[6:]
		m.FSType = b[0]
		m.Source = unescapeMountInfo(b[1])
		if len(b) > 2 {
			m.SuperOpts = b[2]
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

// MountsBelow returns the mounts strictly below prefix, in mountinfo order.
func MountsBelow(mounts []MountInfo, prefix string) []MountInfo {
	prefix = filepath.Clean(prefix)
	var out []MountInfo
	for _, m := range mounts {
		if m.MountPoint != prefix && IsBelow(m.MountPoint, prefix) {
			out = append(out, m)
		}
	}
	return out
}

// IsBelow reports whether path equals dir or lies inside it.
func IsBelow(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func unescapeMountInfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

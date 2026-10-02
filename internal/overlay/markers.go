package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// The metadata directory placed at the top of every merged hierarchy holds
// these files, in the formats systemd-sysext writes them:
//
//	dev         the merged overlay's st_dev as MAJOR:MINOR plus newline
//	backing     st_dev of the host hierarchy used as bottom layer (block
//	            devices only)
//	extensions  (confexts for confext) the merged extension names, one per
//	            line, empty without extensions
//	origin      a JSON object identifying the merged images and settings
//	work_dir    the overlayfs workdir relative to the root, C-escaped
//	            (persistent mutable modes only)

// MarkerDirName returns ".systemd-sysext" or ".systemd-confext".
func MarkerDirName(class release.Class) string {
	if class == release.Confext {
		return ".systemd-confext"
	}
	return ".systemd-sysext"
}

// listFileName is systemd's short_identifier_plural: the marker file
// listing the merged extensions.
func listFileName(class release.Class) string {
	if class == release.Confext {
		return "confexts"
	}
	return "extensions"
}

// classIdentifier is systemd's short_identifier, also used as the source of
// the overlay and workspace mounts.
func classIdentifier(class release.Class) string {
	if class == release.Confext {
		return "confext"
	}
	return "sysext"
}

// formatDevnum is FORMAT_DEVNUM: "MAJOR:MINOR".
func formatDevnum(dev uint64) string {
	return fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))
}

// parseDevnum is systemd's parse_devnum(): decimal major and minor
// separated by a colon, each within the kernel's limits. A plain decimal
// dev_t, as written by earlier versions of this tool, is accepted too.
func parseDevnum(s string) (uint64, error) {
	majs, mins, ok := strings.Cut(s, ":")
	if !ok {
		if isDecimal(s) {
			return strconv.ParseUint(s, 10, 64)
		}
		return 0, unix.EINVAL
	}
	if !isDecimal(majs) || !isDecimal(mins) {
		return 0, unix.EINVAL
	}
	major, err1 := strconv.ParseUint(majs, 10, 32)
	minor, err2 := strconv.ParseUint(mins, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, unix.ERANGE
	}
	if major >= 1<<12 || minor >= 1<<20 {
		return 0, unix.ERANGE
	}
	return unix.Mkdev(uint32(major), uint32(minor)), nil
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// formatList renders the extensions marker: one name per line, nothing at
// all for an empty list.
func formatList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, "\n") + "\n"
}

// parseList is strv_split_newlines().
func parseList(data string) []string {
	var names []string
	for line := range strings.SplitSeq(strings.TrimSuffix(data, "\n"), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

// readFirstLine is read_one_line_file(): the first line without its
// newline.
func readFirstLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSuffix(line, "\r"), nil
}

// isOurMountPoint is systemd's is_our_mount_point(): dir, a resolved
// hierarchy, is a mount point carrying a dev marker that matches its st_dev.
func isOurMountPoint(class release.Class, dir string) (bool, error) {
	mp, err := fsutil.IsMountPoint(dir)
	if err != nil {
		return false, fmt.Errorf("checking whether %s is a mount point: %w", dir, err)
	}
	if !mp {
		return false, nil
	}
	marker := MarkerDirName(class)
	line, err := readFirstLine(filepath.Join(dir, marker, "dev"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s/%s/dev: %w", dir, marker, err)
	}
	dev, err := parseDevnum(line)
	if err != nil {
		return false, fmt.Errorf("failed to parse device major/minor stored in '%s/dev' file on '%s': %w", marker, dir, err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	return uint64(st.Dev) == dev, nil
}

// cescape is systemd's cescape(): C-style escaping of a string so that it
// fits on one line.
func cescape(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\v':
			b.WriteString(`\v`)
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\'':
			b.WriteString(`\'`)
		default:
			if c < ' ' || c >= 127 {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// cunescape is systemd's cunescape() without flags: it reverses cescape()
// and also accepts \s, \xHH, \uHHHH and \UHHHHHHHH. NUL bytes and unknown
// escapes are rejected.
func cunescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", unix.EINVAL
		}
		switch c := s[i]; c {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '"', '\'':
			b.WriteByte(c)
		case 's':
			b.WriteByte(' ')
		case 'x':
			if i+2 >= len(s) {
				return "", unix.EINVAL
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil || v == 0 {
				return "", unix.EINVAL
			}
			b.WriteByte(byte(v))
			i += 2
		case 'u', 'U':
			n := 4
			if c == 'U' {
				n = 8
			}
			if i+n >= len(s) {
				return "", unix.EINVAL
			}
			v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if err != nil || v == 0 || !utf8.ValidRune(rune(v)) {
				return "", unix.EINVAL
			}
			b.WriteRune(rune(v))
			i += n
		case '0', '1', '2', '3', '4', '5', '6', '7':
			if i+2 >= len(s) {
				return "", unix.EINVAL
			}
			v, err := strconv.ParseUint(s[i:i+3], 8, 8)
			if err != nil || v == 0 {
				return "", unix.EINVAL
			}
			b.WriteByte(byte(v))
			i += 2
		default:
			return "", unix.EINVAL
		}
	}
	return b.String(), nil
}

// pathIsNormalized is systemd's path_is_normalized(): no empty, "." or ".."
// components, no repeated slashes, within PATH_MAX and NAME_MAX.
func pathIsNormalized(p string) bool {
	if p == "" || len(p) >= unix.PathMax {
		return false
	}
	if strings.Contains(p, "//") {
		return false
	}
	for c := range strings.SplitSeq(strings.Trim(p, "/"), "/") {
		if c == "." || c == ".." || len(c) > 255 {
			return false
		}
	}
	return true
}

// singlePathComponent is systemd's hierarchy_as_single_path_component():
// the hierarchy without leading and trailing slashes, inner slashes turned
// into dots ("/usr" → "usr", "/foo/bar/" → "foo.bar").
func singlePathComponent(hierarchy string) string {
	return strings.ReplaceAll(strings.Trim(hierarchy, "/"), "/", ".")
}

// workDirName is the basename systemd gives the overlayfs workdir of a
// hierarchy, next to its upper directory.
func workDirName(hierarchy string) string {
	return ".systemd-" + singlePathComponent(hierarchy) + "-workdir"
}

// recordedWorkDir reads the work_dir marker of a merged hierarchy and
// returns the workdir it names inside root, or "" when there is none. Like
// systemd it rejects absolute and non-normalized paths; in addition the
// basename must be the one generated for hierarchy, and the path is
// resolved without leaving root.
func recordedWorkDir(class release.Class, root, dir, hierarchy string) (string, error) {
	f := filepath.Join(dir, MarkerDirName(class), "work_dir")
	line, err := readFirstLine(f)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", f, err)
	}
	rel, err := cunescape(line)
	if err != nil {
		return "", fmt.Errorf("failed to unescape work directory path: %w", err)
	}
	if filepath.IsAbs(rel) || !pathIsNormalized(rel) {
		return "", fmt.Errorf("invalid work directory path '%s'", rel)
	}
	if filepath.Base(rel) != workDirName(hierarchy) {
		return "", fmt.Errorf("work directory path '%s' does not belong to hierarchy %s", rel, hierarchy)
	}
	resolved, err := fsutil.Chase(root, filepath.Dir(rel), 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(rel)), nil
}

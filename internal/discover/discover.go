// Package discover finds installed extension images the way systemd 262's
// image_discover() (src/shared/discover-image.c) does for the sysext and
// confext classes: search directories in priority order, symlinks resolved
// inside the root, naming rules with optional class suffixes, versioned
// ".v" directories and the first image of a name shadowing later ones.
package discover

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// ImageType discriminates how an extension is shipped.
type ImageType int

const (
	// TypeDirectory is a plain directory (or btrfs subvolume) image.
	TypeDirectory ImageType = iota
	// TypeRaw is a regular disk image file (bare filesystem or GPT DDI).
	TypeRaw
	// TypeBlock is a block device holding a disk image.
	TypeBlock
)

func (t ImageType) String() string {
	switch t {
	case TypeDirectory:
		return "directory"
	case TypeRaw:
		return "raw"
	case TypeBlock:
		return "block"
	default:
		return fmt.Sprintf("ImageType(%d)", int(t))
	}
}

// Image is one discovered extension image.
type Image struct {
	// Name is the extension name: the entry name without the .raw format
	// suffix and the optional class suffix (.sysext/.confext), or the base
	// name of a versioned ".v" directory.
	Name string
	// Path is the resolved path of the directory, file or block device,
	// including the root prefix.
	Path string
	Type ImageType
	// MTime is the modification time in microseconds since the epoch for
	// raw images; 0 for directory and block images, like systemd.
	MTime int64
	// CrTime is the creation (birth) time in microseconds since the epoch,
	// or 0 when unknown.
	CrTime int64
}

// Time returns the timestamp systemd-sysext list shows for the image:
// MTime, or CrTime when MTime is 0.
func (img Image) Time() int64 {
	if img.MTime != 0 {
		return img.MTime
	}
	return img.CrTime
}

// sysextDirs / confextDirs in priority order (highest first), systemd's
// image_search_path[].
var (
	sysextDirs = []string{
		"/etc/extensions",
		"/run/extensions",
		"/var/lib/extensions",
	}
	confextDirs = []string{
		"/run/confexts",
		"/var/lib/confexts",
		"/usr/local/lib/confexts",
		"/usr/lib/confexts",
	}
)

func searchPath(class release.Class) []string {
	if class == release.Confext {
		return confextDirs
	}
	return sysextDirs
}

func classSuffix(class release.Class) string {
	if class == release.Confext {
		return ".confext"
	}
	return ".sysext"
}

// SearchDirs returns the class' search directories in priority order
// (highest first), each prefixed with root:
//
//	Sysext:  /etc/extensions, /run/extensions, /var/lib/extensions
//	Confext: /run/confexts, /var/lib/confexts, /usr/local/lib/confexts,
//	         /usr/lib/confexts
func SearchDirs(class release.Class, root string) []string {
	if root == "" {
		root = "/"
	}
	base := searchPath(class)
	dirs := make([]string, len(base))
	for i, d := range base {
		dirs[i] = filepath.Join(root, d)
	}
	return dirs
}

// Discover scans the search directories of class under root ("" = the real
// root) and returns the installed images sorted by name (see Sort). Rules,
// as in systemd's image_discover():
//   - search directories and entries are resolved inside root; dangling
//     entries are skipped, ".sysupdate.*" temporaries ignored
//   - regular files need the .raw suffix and become TypeRaw; directories
//     become TypeDirectory; block devices become TypeBlock under their
//     entry name
//   - an optional class suffix (.sysext or .confext, only the class's own)
//     is stripped from file and directory names, and names must be valid
//     image names
//   - a directory named <name>[.sysext|.confext][.raw].v holds versioned
//     entries <name>_<version>[_<arch>][+<left>[-<done>]]<suffix>; the best
//     one for the native architecture becomes image <name> (see pickVersion),
//     an empty one is skipped and shadows nothing
//   - the first image of a name, in search path order, shadows later ones;
//     an empty directory therefore masks a same-named image (and is itself
//     ignored at merge time because it carries no release data)
func Discover(class release.Class, root string) ([]Image, error) {
	root, err := absRoot(root)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var images []Image

	for _, dir := range searchPath(class) {
		searchDir, err := fsutil.Chase(root, dir, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		names, err := readDirNames(searchDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		searchRel := relativeToRoot(root, searchDir)

		for _, fname := range names {
			if strings.HasPrefix(fname, ".sysupdate.") {
				continue
			}
			entry := filepath.Join(searchRel, fname)
			resolved, err := fsutil.Chase(root, entry, 0)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			fi, err := os.Stat(resolved)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}

			var name string
			var ok bool
			switch mode := fi.Mode(); {
			case mode.IsRegular():
				name, _, ok = extractImageBasename(fname, classSuffix(class), []string{".raw"})
			case mode.IsDir():
				if nov, versioned := strings.CutSuffix(fname, ".v"); versioned {
					var suffix string
					name, suffix, ok = extractImageBasename(nov, classSuffix(class), []string{".raw", ".mstack", ""})
					if !ok {
						continue
					}
					want := typeDir
					if strings.HasSuffix(suffix, ".raw") {
						want = typeReg | typeBlk
					}
					pick, err := pickVersion(root, entry, name, suffix, want)
					if err != nil || pick == nil {
						continue
					}
					resolved, fi = pick.path, pick.info
				} else {
					name, _, ok = extractImageBasename(fname, classSuffix(class), []string{".mstack", ""})
				}
			case mode&fs.ModeDevice != 0 && mode&fs.ModeCharDevice == 0:
				name, _, ok = extractImageBasename(fname, "", nil)
			}
			if !ok || seen[name] {
				continue
			}

			img, ok := makeImage(name, resolved, fi)
			if !ok {
				continue
			}
			seen[name] = true
			images = append(images, img)
		}
	}

	Sort(images)
	return images, nil
}

// makeImage is image_make() for an already named and resolved entry. Images
// systemd-sysext cannot merge (.mstack directories) are refused.
func makeImage(name, path string, fi fs.FileInfo) (Image, bool) {
	img := Image{Name: name, Path: path}
	switch mode := fi.Mode(); {
	case mode.IsDir():
		if strings.HasSuffix(path, ".mstack") {
			return Image{}, false
		}
		img.Type = TypeDirectory
		img.CrTime = crtime(path)
	case mode.IsRegular():
		img.Type = TypeRaw
		img.CrTime = crtime(path)
		img.MTime = fi.ModTime().UnixMicro()
	case mode&fs.ModeDevice != 0 && mode&fs.ModeCharDevice == 0:
		img.Type = TypeBlock
	default:
		return Image{}, false
	}
	return img, true
}

// extractImageBasename is systemd's extract_image_basename(): one of
// formatSuffixes is required (when given) and removed, then classSuffix is
// removed if present; the rest must be a valid image name. suffix is the
// removed tail.
func extractImageBasename(fname, classSuffix string, formatSuffixes []string) (name, suffix string, ok bool) {
	name = fname
	if formatSuffixes != nil {
		found := false
		for _, s := range formatSuffixes {
			if base, ok := strings.CutSuffix(name, s); ok {
				name, suffix, found = base, s, true
				break
			}
		}
		if !found {
			return "", "", false
		}
	}
	if classSuffix != "" {
		if base, ok := strings.CutSuffix(name, classSuffix); ok {
			name, suffix = base, classSuffix+suffix
		}
	}
	if !release.ImageNameIsValid(name) {
		return "", "", false
	}
	return name, suffix, true
}

// crtime is systemd's getcrtime_at(): the older of the statx birth time and
// the user.crtime_usec xattr, in microseconds; 0 when neither is available.
func crtime(path string) int64 {
	var best uint64 = math.MaxUint64
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC, unix.STATX_BTIME, &stx)
	if err == nil && stx.Mask&unix.STATX_BTIME != 0 && stx.Btime.Sec != 0 {
		best = uint64(stx.Btime.Sec)*1_000_000 + uint64(stx.Btime.Nsec)/1000
	}
	var buf [8]byte
	if n, err := unix.Lgetxattr(path, "user.crtime_usec", buf[:]); err == nil && n == len(buf) {
		if u := binary.LittleEndian.Uint64(buf[:]); u != 0 && u != math.MaxUint64 {
			best = min(best, u)
		}
	}
	if best == math.MaxUint64 || best > math.MaxInt64 {
		return 0
	}
	return int64(best)
}

func readDirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// absRoot makes root absolute; a root other than "/" must be an existing
// directory.
func absRoot(root string) (string, error) {
	if root == "" || root == "/" {
		return "/", nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", &fs.PathError{Op: "open", Path: root, Err: unix.ENOTDIR}
	}
	return abs, nil
}

// relativeToRoot returns the root-prefixed path p as an absolute path
// inside the absolute root.
func relativeToRoot(root, p string) string {
	if root == "/" {
		return p
	}
	rel, ok := strings.CutPrefix(p, root)
	if !ok || (rel != "" && rel[0] != '/') {
		return p
	}
	if rel == "" {
		return "/"
	}
	return rel
}

// Sort orders images by name with CompareVersions (systemd sorts the merged
// extensions with strverscmp_improved()). The merge lowerdir construction
// relies on this order.
func Sort(images []Image) {
	sort.SliceStable(images, func(i, j int) bool {
		return CompareVersions(images[i].Name, images[j].Name) < 0
	})
}

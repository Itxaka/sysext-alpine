package release

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
)

var (
	// ErrNoExtensionRelease reports that an image tree has no
	// extension-release file for the image (systemd: ENOENT). It matches
	// fs.ErrNotExist.
	ErrNoExtensionRelease = fmt.Errorf("no extension-release file found: %w", fs.ErrNotExist)
	// ErrExtensionReleaseNotUnique reports that the name-mismatch fallback
	// found more than one candidate release file (systemd: ENOTUNIQ).
	ErrExtensionReleaseNotUnique = fmt.Errorf("extension-release file is ambiguous: %w", unix.ENOTUNIQ)

	errNoOSRelease = fmt.Errorf("no os-release file found: %w", fs.ErrNotExist)
)

const strictXattr = "user.extension-release.strict"

// HostOSRelease parses the os-release file of the OS tree at root ("" or "/"
// is the running system). Like systemd's parse_os_release(), the path in
// $SYSTEMD_OS_RELEASE is used when set (no fallback); otherwise
// etc/os-release, then usr/lib/os-release. Symlinks are resolved inside
// root.
func HostOSRelease(root string) (Fields, error) {
	p, err := lookupOSRelease(root)
	if err != nil {
		return nil, err
	}
	return ParseFile(p)
}

func lookupOSRelease(root string) (string, error) {
	if e, ok := os.LookupEnv("SYSTEMD_OS_RELEASE"); ok {
		return fsutil.Chase(root, e, 0)
	}
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		resolved, err := fsutil.Chase(root, p, 0)
		if !errors.Is(err, fs.ErrNotExist) {
			return resolved, err
		}
	}
	return "", errNoOSRelease
}

// FindExtensionRelease locates and parses the extension-release file of the
// image called name inside the image tree imageRoot, following systemd's
// open_extension_release(): extension-release.<name> in the class's release
// directory is used if it exists (symlinks resolved inside imageRoot).
// Otherwise every regular, non-symlink extension-release.<X> file in that
// directory is a candidate when X equals the image's base name (name without
// .sysext.raw/.confext.raw/.raw, truncated at the first '_' or '+'), when the
// file's user.extension-release.strict xattr is boolean false, or, with
// relax, unconditionally. Exactly one candidate must exist. A missing file
// yields an error matching ErrNoExtensionRelease, an ambiguous fallback
// ErrExtensionReleaseNotUnique.
func FindExtensionRelease(imageRoot, name string, class Class, relax bool) (Fields, error) {
	p, err := lookupExtensionRelease(imageRoot, name, class, relax)
	if err != nil {
		return nil, err
	}
	return ParseFile(p)
}

func lookupExtensionRelease(root, name string, class Class, relax bool) (string, error) {
	if !ImageNameIsValid(name) {
		return "", fmt.Errorf("the extension name %s is invalid: %w", name, unix.EINVAL)
	}
	dirRel := "/" + class.ReleaseFileDir()

	p, err := fsutil.Chase(root, dirRel+"/extension-release."+name, 0)
	if !errors.Is(err, fs.ErrNotExist) {
		return p, err
	}

	dir, err := fsutil.Chase(root, dirRel, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNoExtensionRelease
		}
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNoExtensionRelease
		}
		return "", err
	}

	found := ""
	for _, e := range entries {
		fname := e.Name()
		if HiddenOrBackupFile(fname) || !e.Type().IsRegular() {
			continue
		}
		suffix, ok := strings.CutPrefix(fname, "extension-release.")
		if !ok || !ImageNameIsValid(suffix) {
			continue
		}
		candidate := filepath.Join(dir, fname)
		fi, err := os.Lstat(candidate)
		if err != nil {
			return "", err
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if !relax {
			base, ok := imageBaseName(name)
			if !ok {
				continue
			}
			if suffix != base && !strictXattrIsFalse(candidate) {
				continue
			}
		}
		if found != "" {
			return "", ErrExtensionReleaseNotUnique
		}
		found = candidate
	}
	if found == "" {
		return "", ErrNoExtensionRelease
	}
	return found, nil
}

// strictXattrIsFalse reports whether the user.extension-release.strict
// xattr of path parses as boolean false. A missing or unreadable xattr is
// not false.
func strictXattrIsFalse(path string) bool {
	buf := make([]byte, 128)
	for range 8 {
		n, err := unix.Lgetxattr(path, strictXattr, buf)
		if errors.Is(err, unix.ERANGE) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		if err != nil {
			return false
		}
		v := buf[:n]
		if i := bytes.IndexByte(v, 0); i >= 0 {
			v = v[:i]
		}
		b, err := ParseBoolean(string(v))
		return err == nil && !b
	}
	return false
}

// imageBaseName is systemd's path_extract_image_name(): the image name
// without a .sysext.raw, .confext.raw or .raw suffix, truncated at the first
// '_' or '+' (version and boot counter separators).
func imageBaseName(name string) (string, bool) {
	isDir := strings.HasSuffix(name, "/")
	name = strings.TrimRight(name, "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if !isDir {
		for _, sfx := range []string{".sysext.raw", ".confext.raw", ".raw"} {
			if s, ok := strings.CutSuffix(name, sfx); ok {
				name = s
				break
			}
		}
	}
	if i := strings.IndexAny(name, "_+"); i >= 0 {
		name = name[:i]
	}
	return name, ImageNameIsValid(name)
}

// IsOSTree reports whether tree carries an os-release file (systemd's
// path_is_os_tree()). tree itself must exist.
func IsOSTree(tree string) (bool, error) {
	if _, err := os.Lstat(tree); err != nil {
		return false, err
	}
	_, err := lookupOSRelease(tree)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// IsExtensionTree reports whether tree carries an extension-release file of
// class for the image called name (systemd's path_is_extension_tree()). An
// ambiguous fallback is an error. tree itself must exist.
func IsExtensionTree(tree, name string, class Class, relax bool) (bool, error) {
	if _, err := os.Lstat(tree); err != nil {
		return false, err
	}
	_, err := lookupExtensionRelease(tree, name, class, relax)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// HasForbiddenContent is systemd's extension_has_forbidden_content(): an
// extension tree must not ship usr/lib/os-release (resolved inside tree; a
// dangling symlink does not count), since merging it would replace the
// host's OS identity.
func HasForbiddenContent(tree string) (bool, error) {
	_, err := fsutil.Chase(tree, "/usr/lib/os-release", 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// CheckImageTree is the check systemd's dissected_image_mount() applies to
// the mounted root of a disk image named name with
// DISSECT_IMAGE_VALIDATE_OS_EXT, plus DISSECT_IMAGE_VALIDATE_OS when
// acceptOSTree is set: the tree qualifies when it is an OS tree (only with
// acceptOSTree), or when it carries no forbidden content and has a sysext or
// confext extension-release file for name. false corresponds to systemd's
// ENOMEDIUM; errors (e.g. ErrExtensionReleaseNotUnique) are fatal there.
func CheckImageTree(tree, name string, acceptOSTree bool) (bool, error) {
	if acceptOSTree {
		ok, err := IsOSTree(tree)
		if err != nil || ok {
			return ok, err
		}
	}
	if name == "" {
		return false, nil
	}
	forbidden, err := HasForbiddenContent(tree)
	if err != nil || forbidden {
		return false, err
	}
	ok, err := IsExtensionTree(tree, name, Sysext, false)
	if err == nil && !ok {
		ok, err = IsExtensionTree(tree, name, Confext, false)
	}
	return ok, err
}

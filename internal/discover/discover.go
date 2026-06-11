// Package discover scans the extension search directories and resolves the
// set of installed extension images, applying precedence and masking rules
// per docs/SPEC.md §1.
package discover

import (
	"github.com/itxaka/sysext-alpine/internal/release"
)

// ImageType discriminates how an extension is shipped.
type ImageType int

const (
	// TypeDirectory is a plain directory (or btrfs subvolume) image.
	TypeDirectory ImageType = iota
	// TypeRaw is a *.raw disk image file (bare filesystem or GPT DDI).
	TypeRaw
)

func (t ImageType) String() string {
	panic("unimplemented")
}

// Image is one discovered extension image.
type Image struct {
	// Name is the extension name: directory basename, or .raw filename
	// without the suffix.
	Name string
	// Path is the absolute path (symlinks resolved) to the directory or
	// .raw file.
	Path string
	Type ImageType
	// ModTime (unix seconds) of the image, for `list` output.
	ModTime int64
}

// SearchDirs returns the class' search directories in priority order
// (highest first), each prefixed with root:
//
//	Sysext:  /etc/extensions, /run/extensions, /var/lib/extensions
//	Confext: /run/confexts, /var/lib/confexts, /usr/lib/confexts,
//	         /usr/local/lib/confexts
func SearchDirs(class release.Class, root string) []string {
	panic("unimplemented")
}

// Discover scans the search dirs for class under root and returns installed
// images sorted by name with strverscmp_improved semantics (see Sort).
// Rules:
//   - subdirectories are TypeDirectory images; *.raw files are TypeRaw
//   - symlinks are followed (broken symlinks skipped with a warning)
//   - a name found in a higher-priority dir shadows the same name in lower
//     ones; an *empty directory* shadowing entry acts as a mask: the
//     extension is dropped entirely
//   - hidden entries (leading '.') are ignored
func Discover(class release.Class, root string) ([]Image, error) {
	panic("unimplemented")
}

// Sort orders images by name using strverscmp_improved-compatible version
// sort (GNU strverscmp with systemd's improvements: handles embedded version
// numbers naturally, "~" sorts before anything, "" handling). The merge
// lowerdir construction relies on this order.
func Sort(images []Image) {
	panic("unimplemented")
}

// CompareVersions is the underlying strverscmp_improved comparison, exported
// for tests and reuse. Returns <0, 0, >0.
func CompareVersions(a, b string) int {
	panic("unimplemented")
}

// Package discover scans the extension search directories and resolves the
// set of installed extension images, applying precedence and masking rules
// per docs/SPEC.md §1.
package discover

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	switch t {
	case TypeDirectory:
		return "directory"
	case TypeRaw:
		return "raw"
	default:
		return fmt.Sprintf("ImageType(%d)", int(t))
	}
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

// sysextDirs / confextDirs in priority order (highest first), per SPEC §1.
var (
	sysextDirs = []string{
		"/etc/extensions",
		"/run/extensions",
		"/var/lib/extensions",
	}
	confextDirs = []string{
		"/run/confexts",
		"/var/lib/confexts",
		"/usr/lib/confexts",
		"/usr/local/lib/confexts",
	}
)

// SearchDirs returns the class' search directories in priority order
// (highest first), each prefixed with root:
//
//	Sysext:  /etc/extensions, /run/extensions, /var/lib/extensions
//	Confext: /run/confexts, /var/lib/confexts, /usr/lib/confexts,
//	         /usr/local/lib/confexts
func SearchDirs(class release.Class, root string) []string {
	if root == "" {
		root = "/"
	}
	var base []string
	if class == release.Confext {
		base = confextDirs
	} else {
		base = sysextDirs
	}
	dirs := make([]string, len(base))
	for i, d := range base {
		dirs[i] = filepath.Join(root, d)
	}
	return dirs
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
	seen := make(map[string]bool)
	var images []Image

	for _, dir := range SearchDirs(class, root) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Missing search dirs are not errors.
				continue
			}
			return nil, fmt.Errorf("reading search dir %s: %w", dir, err)
		}

		for _, ent := range entries {
			base := ent.Name()
			if strings.HasPrefix(base, ".") {
				continue // hidden entry
			}
			path := filepath.Join(dir, base)

			// Resolve symlinks; broken links are skipped with a
			// warning on stderr.
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, err)
				continue
			}
			fi, err := os.Stat(resolved)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, err)
				continue
			}

			switch {
			case fi.IsDir():
				name := base
				if seen[name] {
					continue // shadowed by higher-priority dir
				}
				seen[name] = true
				sub, err := os.ReadDir(resolved)
				if err != nil {
					return nil, fmt.Errorf("reading image dir %s: %w", resolved, err)
				}
				if len(sub) == 0 {
					// Empty directory masks the name entirely.
					continue
				}
				images = append(images, Image{
					Name:    name,
					Path:    resolved,
					Type:    TypeDirectory,
					ModTime: fi.ModTime().Unix(),
				})

			case fi.Mode().IsRegular() && strings.HasSuffix(base, ".raw"):
				name := strings.TrimSuffix(base, ".raw")
				if seen[name] {
					continue // shadowed by higher-priority dir
				}
				seen[name] = true
				images = append(images, Image{
					Name:    name,
					Path:    resolved,
					Type:    TypeRaw,
					ModTime: fi.ModTime().Unix(),
				})

			default:
				// Regular files without .raw suffix, sockets,
				// devices etc. are not extension images.
				continue
			}
		}
	}

	Sort(images)
	return images, nil
}

// Sort orders images by name using strverscmp_improved-compatible version
// sort (GNU strverscmp with systemd's improvements: handles embedded version
// numbers naturally, "~" sorts before anything, "" handling). The merge
// lowerdir construction relies on this order.
func Sort(images []Image) {
	sort.SliceStable(images, func(i, j int) bool {
		return CompareVersions(images[i].Name, images[j].Name) < 0
	})
}

// CompareVersions is the underlying strverscmp_improved comparison, exported
// for tests and reuse. Returns <0, 0, >0.
//
// This implements systemd's strverscmp_improved() from
// src/fundamental/string-util-fundamental.c (which itself implements the
// UAPI-group Version Format Specification). The algorithm compares the two
// strings left to right in a loop:
//
//  1. Characters outside [a-zA-Z0-9.~^-] are skipped (ignored entirely).
//  2. '~' (pre-release separator) sorts before EVERYTHING, including
//     end-of-string ("1.0~rc1" < "1.0").
//  3. End-of-string sorts before any remaining character: the string that
//     still has characters is newer ("1.0" < "1.0.1").
//  4. '-' (version/release separator) sorts before '^', '.', digits and
//     letters.
//  5. '^' (epoch-ish patch separator) sorts before '.', digits and letters.
//  6. '.' sorts before digits and letters.
//  7. Digit runs are compared numerically, ignoring leading zeros (after
//     stripping zeros, longer run wins, then lexicographic). A digit run
//     compares NEWER than a letter at the same position (an empty digit run
//     loses to a non-empty one), matching rpm-style "numeric beats alpha".
//  8. Letter runs are compared bytewise (ASCII, case-sensitive); on a common
//     prefix the longer run is newer.
//
// Resulting per-position precedence:
//
//	'~'  <  end-of-string  <  '-'  <  '^'  <  '.'  <  letters  <  digits
//
// Note: this is the verified systemd ordering; '~' sorts below end-of-string
// (that is its purpose: pre-releases sort before the release), and digits
// sort above letters.
func CompareVersions(a, b string) int {
	i, j := 0, 0
loop:
	for {
		// 1. Skip invalid characters.
		for i < len(a) && !isVersionChar(a[i]) {
			i++
		}
		for j < len(b) && !isVersionChar(b[j]) {
			j++
		}

		// 2. '~' sorts before everything, including end-of-string.
		aTilde := i < len(a) && a[i] == '~'
		bTilde := j < len(b) && b[j] == '~'
		if aTilde || bTilde {
			if !aTilde {
				return 1
			}
			if !bTilde {
				return -1
			}
			i++
			j++
			continue
		}

		// 3. End-of-string: the string that still has characters is
		// newer; both ended means equal.
		aEnd := i >= len(a)
		bEnd := j >= len(b)
		if aEnd || bEnd {
			switch {
			case aEnd && bEnd:
				return 0
			case aEnd:
				return -1
			default:
				return 1
			}
		}

		// 4-6. Separators, in increasing precedence: '-' < '^' < '.'.
		// For each: if only one side has it, that side is older; if
		// both have it, consume and continue.
		for _, sep := range [...]byte{'-', '^', '.'} {
			as := a[i] == sep
			bs := b[j] == sep
			if as || bs {
				if !as {
					return 1
				}
				if !bs {
					return -1
				}
				i++
				j++
				continue loop
			}
		}

		// 7. Digit runs: compared numerically, leading zeros ignored.
		// An empty digit run (i.e. a letter on the other side) loses.
		if isDigit(a[i]) || isDigit(b[j]) {
			ai, bj := i, j
			for ai < len(a) && isDigit(a[ai]) {
				ai++
			}
			for bj < len(b) && isDigit(b[bj]) {
				bj++
			}
			// Strip leading zeros ("007" -> "7", "0" -> "").
			for i < ai && a[i] == '0' {
				i++
			}
			for j < bj && b[j] == '0' {
				j++
			}
			if d := (ai - i) - (bj - j); d != 0 {
				return sign(d)
			}
			if c := strings.Compare(a[i:ai], b[j:bj]); c != 0 {
				return c
			}
			i, j = ai, bj
			continue
		}

		// 8. Letter runs: bytewise on the common prefix, then the
		// longer run is newer.
		ai, bj := i, j
		for ai < len(a) && isAlpha(a[ai]) {
			ai++
		}
		for bj < len(b) && isAlpha(b[bj]) {
			bj++
		}
		n := min(ai-i, bj-j)
		if c := strings.Compare(a[i:i+n], b[j:j+n]); c != 0 {
			return c
		}
		if d := (ai - i) - (bj - j); d != 0 {
			return sign(d)
		}
		i, j = ai, bj
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isVersionChar(c byte) bool {
	return isDigit(c) || isAlpha(c) || c == '~' || c == '-' || c == '^' || c == '.'
}

func sign(d int) int {
	if d < 0 {
		return -1
	}
	return 1
}

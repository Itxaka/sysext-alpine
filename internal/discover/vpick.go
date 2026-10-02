package discover

import (
	"cmp"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// typeMask selects the inode types a versioned entry may have.
type typeMask int

const (
	typeReg typeMask = 1 << iota
	typeBlk
	typeDir
)

func (m typeMask) matches(mode fs.FileMode) bool {
	switch {
	case mode.IsRegular():
		return m&typeReg != 0
	case mode.IsDir():
		return m&typeDir != 0
	case mode&fs.ModeDevice != 0 && mode&fs.ModeCharDevice == 0:
		return m&typeBlk != 0
	}
	return false
}

// triesUnset marks an entry without a "+<left>[-<done>]" boot counter
// (systemd uses UINT_MAX).
const triesUnset = math.MaxUint32

type pickResult struct {
	path      string
	info      fs.FileInfo
	version   string
	arch      string
	triesLeft uint32
	triesDone uint32
}

// pickVersion is systemd's path_pick() with PICK_ARCHITECTURE|PICK_TRIES|
// PICK_RESOLVE for the versioned directory vdir (a path inside root) and one
// filter: entries named <basename>_<version>[_<arch>][+<left>[-<done>]]<suffix>
// whose inode type is in want, whose architecture is native, secondary or
// absent, and whose version is a valid UAPI.10 version. The best entry wins
// (see better). A nil result means no entry matched.
func pickVersion(root, vdir, basename, suffix string, want typeMask) (*pickResult, error) {
	dir, err := fsutil.Chase(root, vdir, 0)
	if err != nil {
		return nil, err
	}
	names, err := readDirNames(dir)
	if err != nil {
		return nil, err
	}
	native, secondary := release.NativeArchitecture(), release.SecondaryArchitecture()

	var best *pickResult
	for _, dname := range names {
		e, ok := strings.CutPrefix(dname, basename+"_")
		if !ok {
			continue
		}
		if suffix != "" {
			if e, ok = strings.CutSuffix(e, suffix); !ok {
				continue
			}
		}

		left, done := uint32(triesUnset), uint32(triesUnset)
		if plus := strings.LastIndexByte(e, '+'); plus >= 0 {
			if l, d, ok := parseTries(e[plus:]); ok {
				left, done = l, d
				e = e[:plus]
			}
		}

		arch := ""
		if underscore := strings.LastIndexByte(e, '_'); underscore >= 0 {
			if a := e[underscore+1:]; release.IsArchitecture(a) {
				arch = a
			}
			if arch != "" && arch != native && (secondary == "" || arch != secondary) {
				continue
			}
			e = e[:underscore]
		}

		if !versionIsValid(e) {
			continue
		}

		resolved, err := fsutil.Chase(root, filepath.Join(vdir, dname), 0)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(resolved)
		if err != nil {
			return nil, err
		}
		if !want.matches(fi.Mode()) {
			continue
		}

		c := &pickResult{path: resolved, info: fi, version: e, arch: arch, triesLeft: left, triesDone: done}
		if best == nil || better(c, best, native, secondary) > 0 {
			best = c
		}
	}
	return best, nil
}

// better is pick_result_compare(): >0 when a is the better pick. Entries
// with tries left beat exhausted ones, then the newer version wins, then
// native over secondary over no architecture, then more tries left, then
// fewer tries done, then the file name.
func better(a, b *pickResult, native, secondary string) int {
	d := cmpBool(a.triesLeft != 0, b.triesLeft != 0)
	if d == 0 {
		d = CompareVersions(a.version, b.version)
	}
	if d == 0 {
		d = cmpBool(a.arch == native, b.arch == native)
	}
	if d == 0 && secondary != "" {
		d = cmpBool(a.arch == secondary, b.arch == secondary)
	}
	if d == 0 {
		d = cmp.Compare(a.triesLeft, b.triesLeft)
	}
	if d == 0 {
		d = -cmp.Compare(a.triesDone, b.triesDone)
	}
	if d == 0 {
		d = strings.Compare(filepath.Base(a.path), filepath.Base(b.path))
	}
	return d
}

// parseTries parses a "+<left>" or "+<left>-<done>" boot counter suffix.
func parseTries(s string) (left, done uint32, ok bool) {
	s, ok = strings.CutPrefix(s, "+")
	if !ok {
		return 0, 0, false
	}
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	if n == 0 {
		return 0, 0, false
	}
	if n == len(s) {
		left, ok = parseUnsigned(s)
		return left, 0, ok
	}
	if s[n] != '-' {
		return 0, 0, false
	}
	if left, ok = parseUnsigned(s[:n]); !ok {
		return 0, 0, false
	}
	rest := s[n+1:]
	for i := range len(rest) {
		if !isDigit(rest[i]) {
			return 0, 0, false
		}
	}
	done, ok = parseUnsigned(rest)
	return left, done, ok
}

// parseUnsigned is safe_atou() on a digit string: strtoul() base 0, so a
// leading zero selects octal.
func parseUnsigned(s string) (uint32, bool) {
	if s == "" {
		return 0, false
	}
	base := 10
	if len(s) > 1 && s[0] == '0' {
		base = 8
	}
	v, err := strconv.ParseUint(s, base, 32)
	return uint32(v), err == nil
}

// versionIsValid is version_is_valid(s, 0): a non-empty file name part made
// of [0-9A-Za-z.~^-].
func versionIsValid(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !isDigit(c) && !isAlpha(c) && c != '.' && c != '-' && c != '~' && c != '^' {
			return false
		}
	}
	return true
}

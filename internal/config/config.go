// Package config loads sysext.conf(5) / confext.conf(5) configuration:
// the main configuration file plus *.conf.d drop-ins, with systemd's
// search-path precedence, lexicographic drop-in ordering, same-basename
// shadowing and /dev/null masking.
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// Config holds the recognized sysext.conf(5) options. An empty string means
// the option was not set by any configuration file.
type Config struct {
	Mutable     string // [SysExt]/[ConfExt] Mutable=
	ImagePolicy string // [SysExt]/[ConfExt] ImagePolicy=
}

// searchDirs is the main-file search order (first found wins) and, equally,
// the drop-in shadowing priority (earlier dir shadows later for the same
// basename): /etc > /run > /usr/local/lib > /usr/lib.
var searchDirs = []string{
	"/etc/systemd",
	"/run/systemd",
	"/usr/local/lib/systemd",
	"/usr/lib/systemd",
}

// fileName returns the main configuration file name for the class.
func fileName(class release.Class) string {
	if class == release.Confext {
		return "confext.conf"
	}
	return "sysext.conf"
}

// sectionName returns the configuration section applied for the class.
// Both [SysExt] and [ConfExt] are understood by both tools per the man
// page, but only the section matching the running class takes effect.
func sectionName(class release.Class) string {
	if class == release.Confext {
		return "ConfExt"
	}
	return "SysExt"
}

// isMasked reports whether path is masked, i.e. resolves (following
// symlinks) to a character device such as /dev/null, or is an empty file.
// Missing paths are not masked.
func isMasked(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0 || (fi.Mode().IsRegular() && fi.Size() == 0)
}

// exists reports whether path names an existing file (any type, symlinks
// followed). A dangling symlink does not count.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Load reads the main configuration file (first one found in the search
// path wins) and then every *.conf drop-in, sorted by basename across all
// directories, with same-basename files in higher-priority directories
// shadowing lower-priority ones. Drop-ins are applied after the main file;
// within the applied stream the last assignment wins. All paths are
// interpreted relative to root ("" = the real root). Missing files are not
// an error; unknown sections and keys are ignored silently.
func Load(class release.Class, root string) (Config, error) {
	var cfg Config
	name := fileName(class)
	section := sectionName(class)

	// Main file: first found wins, even when masked (a mask hides any
	// lower-priority main file too).
	for _, dir := range searchDirs {
		path := filepath.Join(root, dir, name)
		if !exists(path) {
			continue
		}
		if !isMasked(path) {
			if err := parseInto(&cfg, path, section); err != nil {
				return Config{}, err
			}
		}
		break
	}

	// Drop-ins: collect basenames across all <dir>/<name>.d directories;
	// the highest-priority directory owning a basename provides the file
	// (shadowing), then everything is applied in lexicographic basename
	// order, after the main file.
	dropins := map[string]string{} // basename -> chosen path
	for _, dir := range searchDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir, name+".d"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return Config{}, err
		}
		for _, e := range entries {
			base := e.Name()
			if !strings.HasSuffix(base, ".conf") {
				continue
			}
			if _, shadowed := dropins[base]; shadowed {
				continue // higher-priority dir already owns this basename
			}
			dropins[base] = filepath.Join(root, dir, name+".d", base)
		}
	}
	basenames := make([]string, 0, len(dropins))
	for base := range dropins {
		basenames = append(basenames, base)
	}
	sort.Strings(basenames)
	for _, base := range basenames {
		path := dropins[base]
		if !exists(path) || isMasked(path) {
			continue
		}
		if err := parseInto(&cfg, path, section); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// parseInto parses one configuration file and applies assignments from the
// wanted section onto cfg (last assignment wins; an empty assignment resets
// the option to unset). Unknown sections and keys, comments ('#' and ';')
// and malformed lines are ignored silently, like systemd's lenient loader.
func parseInto(cfg *Config, path, wantSection string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	current := "" // current section name; "" = before any section header
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.HasSuffix(line, "]") {
				current = line[1 : len(line)-1]
			} else {
				current = "\x00malformed" // keys until next header are ignored
			}
			continue
		}
		if current != wantSection {
			continue // keys in other (or unknown) sections are ignored
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue // not an assignment
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key { // section options are case-sensitive
		case "Mutable":
			cfg.Mutable = value
		case "ImagePolicy":
			cfg.ImagePolicy = value
		}
	}
	return nil
}

// Package release implements os-release(5) style file parsing and the
// extension-release compatibility matching algorithm described in
// docs/SPEC.md §2.
package release

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Class selects sysext vs confext behavior.
type Class int

const (
	Sysext Class = iota
	Confext
)

// ReleaseFileDir returns the directory inside an extension image that holds
// the extension-release file for the class:
//
//	Sysext:  usr/lib/extension-release.d
//	Confext: etc/extension-release.d
func (c Class) ReleaseFileDir() string {
	if c == Confext {
		return "etc/extension-release.d"
	}
	return "usr/lib/extension-release.d"
}

// LevelField returns the level field name for the class
// (SYSEXT_LEVEL or CONFEXT_LEVEL).
func (c Class) LevelField() string {
	if c == Confext {
		return "CONFEXT_LEVEL"
	}
	return "SYSEXT_LEVEL"
}

// ScopeField returns the scope field name for the class
// (SYSEXT_SCOPE or CONFEXT_SCOPE).
func (c Class) ScopeField() string {
	if c == Confext {
		return "CONFEXT_SCOPE"
	}
	return "SYSEXT_SCOPE"
}

// Fields is a parsed os-release / extension-release key=value map.
type Fields map[string]string

// Parse parses os-release(5) format content: KEY=VALUE lines, shell-style
// single/double quoting and escapes, '#' comments, blank lines ignored.
// Malformed lines are skipped silently. On repeated keys the last
// assignment wins, like a shell sourcing the file.
func Parse(content []byte) (Fields, error) {
	f := Fields{}
	for line := range strings.SplitSeq(string(content), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue // no assignment on this line
		}
		key := line[:eq]
		if !validKey(key) {
			continue
		}
		value, ok := parseValue(line[eq+1:])
		if !ok {
			continue
		}
		f[key] = value
	}
	return f, nil
}

// validKey reports whether key is a valid shell variable name
// ([A-Za-z_][A-Za-z0-9_]*).
func validKey(key string) bool {
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return len(key) > 0
}

// parseValue interprets the right-hand side of an assignment using the
// shell-compatible subset described in os-release(5). The second return
// value is false when the value is malformed (e.g. unterminated quote).
func parseValue(s string) (string, bool) {
	if s == "" {
		return "", true
	}
	switch s[0] {
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				if i+1 >= len(s) {
					return "", false // dangling backslash
				}
				next := s[i+1]
				switch next {
				case '\\', '$', '"', '`':
					b.WriteByte(next)
				default:
					// Not a recognized escape: keep both bytes,
					// matching shell behavior in double quotes.
					b.WriteByte('\\')
					b.WriteByte(next)
				}
				i++
			case '"':
				// Closing quote; only trailing whitespace may follow.
				if strings.TrimSpace(s[i+1:]) != "" {
					return "", false
				}
				return b.String(), true
			default:
				b.WriteByte(s[i])
			}
		}
		return "", false // unterminated double quote
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", false // unterminated single quote
		}
		if strings.TrimSpace(s[1+end+1:]) != "" {
			return "", false
		}
		return s[1 : 1+end], true
	default:
		return strings.TrimSpace(s), true
	}
}

// ParseFile reads and parses the file at path.
func ParseFile(path string) (Fields, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(content)
}

// HostOSRelease loads the host os-release relative to root (the --root
// option; "" or "/" means the real root). Tries <root>/etc/os-release first,
// then <root>/usr/lib/os-release.
func HostOSRelease(root string) (Fields, error) {
	if root == "" {
		root = "/"
	}
	etcPath := filepath.Join(root, "etc/os-release")
	fields, err := ParseFile(etcPath)
	if err == nil {
		return fields, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	libPath := filepath.Join(root, "usr/lib/os-release")
	fields, err = ParseFile(libPath)
	if err == nil {
		return fields, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no os-release file found (tried %s and %s)", etcPath, libPath)
	}
	return nil, err
}

// FindExtensionRelease locates and parses the extension-release file for
// image name inside the mounted/extracted image tree rooted at imageRoot.
// Match rule: file named extension-release.<name>; escape hatch: if exactly
// one file exists in the dir and it carries the user.extension-release.strict
// xattr set to a false value, it is accepted regardless of name.
func FindExtensionRelease(imageRoot, name string, class Class) (Fields, error) {
	dir := filepath.Join(imageRoot, class.ReleaseFileDir())
	primary := filepath.Join(dir, "extension-release."+name)
	if info, err := os.Stat(primary); err == nil && info.Mode().IsRegular() {
		return ParseFile(primary)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("image %q has no %s directory", name, class.ReleaseFileDir())
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		// Follow symlinks: only regular files qualify.
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, e.Name())
	}
	if len(files) == 1 {
		path := filepath.Join(dir, files[0])
		value, found, err := getxattr(path, "user.extension-release.strict")
		if err != nil {
			return nil, fmt.Errorf("reading user.extension-release.strict xattr of %s: %w", path, err)
		}
		if found && isFalsy(value) {
			return ParseFile(path)
		}
	}
	return nil, fmt.Errorf("no extension-release file named extension-release.%s found in %s", name, dir)
}

// getxattr reads the named extended attribute of path. found is false when
// the attribute does not exist or the filesystem does not support xattrs.
func getxattr(path, attr string) (value string, found bool, err error) {
	buf := make([]byte, 64)
	for {
		n, err := unix.Getxattr(path, attr, buf)
		switch {
		case err == nil:
			return string(buf[:n]), true, nil
		case errors.Is(err, unix.ERANGE):
			buf = make([]byte, len(buf)*2)
		case errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP):
			return "", false, nil
		default:
			return "", false, err
		}
	}
}

// isFalsy reports whether an xattr value counts as boolean false for the
// user.extension-release.strict escape hatch ("0", "no", "false", "off").
func isFalsy(v string) bool {
	switch strings.ToLower(strings.TrimRight(strings.TrimSpace(v), "\x00")) {
	case "0", "no", "false", "off":
		return true
	}
	return false
}

// Match implements the SPEC §2 algorithm. host is the host os-release,
// ext the extension-release fields. arch is the host architecture in
// systemd ConditionArchitecture notation (e.g. "x86-64", "arm64") — see
// HostArchitecture. Returns nil if compatible, descriptive error otherwise.
func Match(host, ext Fields, class Class, arch string) error {
	extID, ok := ext["ID"]
	if !ok || extID == "" {
		return errors.New("extension-release file is missing the ID field")
	}

	if extID != "_any" {
		hostID := host["ID"]
		if extID != hostID {
			return fmt.Errorf("extension ID %q does not match host ID %q", extID, hostID)
		}

		levelField := class.LevelField()
		if extLevel, defined := ext[levelField]; defined {
			hostLevel := host[levelField]
			if extLevel != hostLevel {
				return fmt.Errorf("extension %s %q does not match host %s %q",
					levelField, extLevel, levelField, hostLevel)
			}
		} else {
			extVersion, defined := ext["VERSION_ID"]
			if !defined {
				return fmt.Errorf("extension-release defines neither %s nor VERSION_ID", levelField)
			}
			hostVersion := host["VERSION_ID"]
			if extVersion != hostVersion {
				return fmt.Errorf("extension VERSION_ID %q does not match host VERSION_ID %q",
					extVersion, hostVersion)
			}
		}
	}

	if extArch, defined := ext["ARCHITECTURE"]; defined && extArch != "" && extArch != "_any" {
		if extArch != arch {
			return fmt.Errorf("extension ARCHITECTURE %q does not match host architecture %q",
				extArch, arch)
		}
	}

	// Scope check: we always run on a booted system, so a non-empty
	// SYSEXT_SCOPE/CONFEXT_SCOPE (whitespace-separated list) must contain
	// "system". An absent or empty field means no restriction.
	scopeField := class.ScopeField()
	if scopeValue, defined := ext[scopeField]; defined {
		scopes := strings.Fields(scopeValue)
		if len(scopes) > 0 && !slices.Contains(scopes, "system") {
			return fmt.Errorf("extension %s %q does not include the %q scope required on a booted system",
				scopeField, scopeValue, "system")
		}
	}

	return nil
}

// HostArchitecture maps runtime.GOARCH/uname to systemd architecture
// identifiers ("x86-64", "arm64", "x86", "arm", "riscv64", ...).
func HostArchitecture() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86-64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	case "arm":
		return "arm"
	case "riscv64":
		return "riscv64"
	case "ppc64le":
		return "ppc64-le"
	case "s390x":
		return "s390x"
	case "loong64":
		return "loongarch64"
	default:
		return runtime.GOARCH
	}
}

// Package release implements os-release(5) style file parsing and the
// extension-release compatibility matching algorithm described in
// docs/SPEC.md §2.
package release

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
	panic("unimplemented")
}

// LevelField returns the level field name for the class
// (SYSEXT_LEVEL or CONFEXT_LEVEL).
func (c Class) LevelField() string {
	panic("unimplemented")
}

// Fields is a parsed os-release / extension-release key=value map.
type Fields map[string]string

// Parse parses os-release(5) format content: KEY=VALUE lines, shell-style
// single/double quoting and escapes, '#' comments, blank lines ignored.
func Parse(content []byte) (Fields, error) {
	panic("unimplemented")
}

// ParseFile reads and parses the file at path.
func ParseFile(path string) (Fields, error) {
	panic("unimplemented")
}

// HostOSRelease loads the host os-release relative to root (the --root
// option; "" or "/" means the real root). Tries <root>/etc/os-release first,
// then <root>/usr/lib/os-release.
func HostOSRelease(root string) (Fields, error) {
	panic("unimplemented")
}

// FindExtensionRelease locates and parses the extension-release file for
// image name inside the mounted/extracted image tree rooted at imageRoot.
// Match rule: file named extension-release.<name>; escape hatch: if exactly
// one file exists in the dir and it carries the user.extension-release.strict
// xattr set to a false value, it is accepted regardless of name.
func FindExtensionRelease(imageRoot, name string, class Class) (Fields, error) {
	panic("unimplemented")
}

// Match implements the SPEC §2 algorithm. host is the host os-release,
// ext the extension-release fields. arch is the host architecture in
// systemd ConditionArchitecture notation (e.g. "x86-64", "arm64") — see
// HostArchitecture. Returns nil if compatible, descriptive error otherwise.
func Match(host, ext Fields, class Class, arch string) error {
	panic("unimplemented")
}

// HostArchitecture maps runtime.GOARCH/uname to systemd architecture
// identifiers ("x86-64", "arm64", "x86", "arm", "riscv64", ...).
func HostArchitecture() string {
	panic("unimplemented")
}

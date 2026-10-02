// Package release implements os-release(5) and extension-release parsing,
// lookup and the extension compatibility check of systemd-sysext 262
// (extension_release_validate() in src/shared/extension-util.c and the
// release file helpers in src/basic/os-util.c).
package release

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
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

// Validate is systemd's extension_release_validate(): it decides whether the
// extension called name, whose release file parsed to ext, may be merged on
// top of the host described by host. arch is the host architecture
// (HostArchitecture) and scope the host scope ("system" or "initrd", see
// HostScope; "" skips the scope check). An empty ext means the image carries
// no release data. reason is the message systemd logs at debug level for
// the decision, both for accepted and for ignored images.
//
// The host ID must be non-empty; callers refuse to merge otherwise, like
// systemd ("'ID' field not found or empty in 'os-release' data of OS tree
// '%s'.").
func Validate(name string, host, ext Fields, class Class, arch, scope string) (ok bool, reason string) {
	if len(ext) == 0 {
		return false, fmt.Sprintf("Extension '%s' carries no release data, ignoring.", name)
	}

	if scope != "" {
		scopes := []string{"system", "portable"}
		if v, present := ext[class.ScopeField()]; present {
			scopes = strings.Fields(v)
		}
		if !slices.Contains(scopes, scope) {
			return false, fmt.Sprintf("Extension '%s' is not suitable for scope %s, ignoring.", name, scope)
		}
	}

	if extArch := ext["ARCHITECTURE"]; extArch != "" && extArch != "_any" && extArch != arch {
		return false, fmt.Sprintf("Extension '%s' is for architecture '%s', but deployed on top of '%s'.",
			name, extArch, arch)
	}

	hostID := host["ID"]
	extID := ext["ID"]
	if extID == "" {
		return false, fmt.Sprintf("Extension '%s' does not contain ID in release file but requested to match '%s' or be '_any'",
			name, hostID)
	}
	if extID == "_any" {
		return true, fmt.Sprintf("Extension '%s' matches '_any' OS.", name)
	}

	idLike := host["ID_LIKE"]
	if extID != hostID && !slices.Contains(strings.Fields(idLike), extID) {
		like := ""
		if idLike != "" {
			like = fmt.Sprintf(" (like '%s')", idLike)
		}
		return false, fmt.Sprintf("Extension '%s' is for OS '%s', but deployed on top of '%s'%s.",
			name, extID, hostID, like)
	}

	levelField := class.LevelField()
	hostVersion, hostLevel := host["VERSION_ID"], host[levelField]
	if hostVersion == "" && hostLevel == "" {
		return true, fmt.Sprintf("No version info on the host (rolling release?), but ID in %s matched.", name)
	}

	if extLevel := ext[levelField]; hostLevel != "" && extLevel != "" {
		if extLevel != hostLevel {
			return false, fmt.Sprintf("Extension '%s' is for API level '%s', but running on API level '%s'",
				name, extLevel, hostLevel)
		}
	} else if hostVersion != "" {
		extVersion := ext["VERSION_ID"]
		if extVersion == "" {
			return false, fmt.Sprintf("Extension '%s' does not contain VERSION_ID in release file but requested to match '%s'",
				name, hostVersion)
		}
		if extVersion != hostVersion {
			return false, fmt.Sprintf("Extension '%s' is for OS '%s', but deployed on top of '%s'.",
				name, extVersion, hostVersion)
		}
	}

	return true, fmt.Sprintf("Version info of extension '%s' matches host.", name)
}

// HostScope returns the extension scope of the OS tree at root: "initrd"
// when <root>/etc/initrd-release exists, "system" otherwise.
func HostScope(root string) (string, error) {
	_, err := fsutil.Chase(root, "/etc/initrd-release", 0)
	switch {
	case err == nil:
		return "initrd", nil
	case errors.Is(err, fs.ErrNotExist):
		return "system", nil
	default:
		return "", err
	}
}

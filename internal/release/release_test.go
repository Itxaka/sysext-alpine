package release

import (
	"testing"
)

func TestClassReleaseFileDir(t *testing.T) {
	if got := Sysext.ReleaseFileDir(); got != "usr/lib/extension-release.d" {
		t.Errorf("Sysext.ReleaseFileDir() = %q, want usr/lib/extension-release.d", got)
	}
	if got := Confext.ReleaseFileDir(); got != "etc/extension-release.d" {
		t.Errorf("Confext.ReleaseFileDir() = %q, want etc/extension-release.d", got)
	}
}

func TestClassLevelField(t *testing.T) {
	if got := Sysext.LevelField(); got != "SYSEXT_LEVEL" {
		t.Errorf("Sysext.LevelField() = %q, want SYSEXT_LEVEL", got)
	}
	if got := Confext.LevelField(); got != "CONFEXT_LEVEL" {
		t.Errorf("Confext.LevelField() = %q, want CONFEXT_LEVEL", got)
	}
}

func TestClassScopeField(t *testing.T) {
	if got := Sysext.ScopeField(); got != "SYSEXT_SCOPE" {
		t.Errorf("Sysext.ScopeField() = %q, want SYSEXT_SCOPE", got)
	}
	if got := Confext.ScopeField(); got != "CONFEXT_SCOPE" {
		t.Errorf("Confext.ScopeField() = %q, want CONFEXT_SCOPE", got)
	}
}

// TestValidate mirrors the branches of systemd's
// extension_release_validate() and the e2e compatibility matrix.
func TestValidate(t *testing.T) {
	alpine := Fields{"ID": "alpine", "VERSION_ID": "3.24.2"}
	tests := []struct {
		name   string
		host   Fields
		ext    Fields
		class  Class
		arch   string
		scope  string // "" = "system"
		skip   bool   // pass no scope at all
		ok     bool
		reason string
	}{
		{name: "no release data", host: alpine, ext: Fields{}, ok: false,
			reason: "Extension 'e' carries no release data, ignoring."},
		{name: "nil release data", host: alpine, ext: nil, ok: false,
			reason: "Extension 'e' carries no release data, ignoring."},

		// ID and _any
		{name: "_any matches everything", host: alpine, ext: Fields{"ID": "_any"}, ok: true,
			reason: "Extension 'e' matches '_any' OS."},
		{name: "_any skips version checks", host: alpine, ext: Fields{"ID": "_any", "VERSION_ID": "1"}, ok: true},
		{name: "missing ext ID", host: alpine, ext: Fields{"VERSION_ID": "3.24.2"}, ok: false,
			reason: "Extension 'e' does not contain ID in release file but requested to match 'alpine' or be '_any'"},
		{name: "empty ext ID", host: alpine, ext: Fields{"ID": "", "VERSION_ID": "3.24.2"}, ok: false},
		{name: "ID mismatch", host: alpine, ext: Fields{"ID": "fedora", "VERSION_ID": "3.24.2"}, ok: false,
			reason: "Extension 'e' is for OS 'fedora', but deployed on top of 'alpine'."},
		{name: "ID mismatch names ID_LIKE", host: Fields{"ID": "pmos", "ID_LIKE": "alpine", "VERSION_ID": "1"},
			ext: Fields{"ID": "fedora", "VERSION_ID": "1"}, ok: false,
			reason: "Extension 'e' is for OS 'fedora', but deployed on top of 'pmos' (like 'alpine')."},
		{name: "ID_LIKE match", host: Fields{"ID": "postmarketos", "ID_LIKE": "alpine", "VERSION_ID": "3.20"},
			ext: Fields{"ID": "alpine", "VERSION_ID": "3.20"}, ok: true,
			reason: "Version info of extension 'e' matches host."},
		{name: "ID_LIKE list match", host: Fields{"ID": "testos", "ID_LIKE": "debian  ubuntu\t", "VERSION_ID": "1"},
			ext: Fields{"ID": "ubuntu", "VERSION_ID": "1"}, ok: true},
		{name: "ID_LIKE match still checks VERSION_ID", host: Fields{"ID": "pmos", "ID_LIKE": "alpine", "VERSION_ID": "3.20"},
			ext: Fields{"ID": "alpine", "VERSION_ID": "3.19"}, ok: false,
			reason: "Extension 'e' is for OS '3.19', but deployed on top of '3.20'."},
		{name: "ID_LIKE is not a substring match", host: Fields{"ID": "x", "ID_LIKE": "alpinelinux", "VERSION_ID": "1"},
			ext: Fields{"ID": "alpine", "VERSION_ID": "1"}, ok: false},

		// rolling release hosts
		{name: "rolling host matches ID only", host: Fields{"ID": "rolling"}, ext: Fields{"ID": "rolling"}, ok: true,
			reason: "No version info on the host (rolling release?), but ID in e matched."},
		{name: "rolling host ignores ext VERSION_ID", host: Fields{"ID": "arch"}, ext: Fields{"ID": "arch", "VERSION_ID": "9"}, ok: true},
		{name: "rolling host ignores ext level", host: Fields{"ID": "arch", "VERSION_ID": ""},
			ext: Fields{"ID": "arch", "SYSEXT_LEVEL": "1"}, ok: true},

		// levels vs VERSION_ID
		{name: "VERSION_ID equal", host: alpine, ext: Fields{"ID": "alpine", "VERSION_ID": "3.24.2"}, ok: true},
		{name: "VERSION_ID differs", host: alpine, ext: Fields{"ID": "alpine", "VERSION_ID": "3.24.1"}, ok: false,
			reason: "Extension 'e' is for OS '3.24.1', but deployed on top of '3.24.2'."},
		{name: "VERSION_ID missing", host: alpine, ext: Fields{"ID": "alpine"}, ok: false,
			reason: "Extension 'e' does not contain VERSION_ID in release file but requested to match '3.24.2'"},
		{name: "ext level without host level falls back to VERSION_ID", host: alpine,
			ext: Fields{"ID": "alpine", "SYSEXT_LEVEL": "1", "VERSION_ID": "3.24.2"}, ok: true},
		{name: "ext level without host level, VERSION_ID differs", host: alpine,
			ext: Fields{"ID": "alpine", "SYSEXT_LEVEL": "1", "VERSION_ID": "3.21"}, ok: false},
		{name: "ext level only, host has VERSION_ID only", host: alpine,
			ext: Fields{"ID": "alpine", "SYSEXT_LEVEL": "1.0"}, ok: false},
		{name: "both levels equal", host: Fields{"ID": "testos", "VERSION_ID": "1", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "testos", "SYSEXT_LEVEL": "2"}, ok: true},
		{name: "both levels equal, VERSION_ID ignored", host: Fields{"ID": "testos", "VERSION_ID": "1", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "testos", "SYSEXT_LEVEL": "2", "VERSION_ID": "7"}, ok: true},
		{name: "both levels differ", host: Fields{"ID": "testos", "VERSION_ID": "1", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "testos", "SYSEXT_LEVEL": "3"}, ok: false,
			reason: "Extension 'e' is for API level '3', but running on API level '2'"},
		{name: "host level, ext VERSION_ID", host: Fields{"ID": "testos", "VERSION_ID": "1", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "testos", "VERSION_ID": "1"}, ok: true},
		{name: "empty ext level is absent", host: Fields{"ID": "testos", "VERSION_ID": "1", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "testos", "SYSEXT_LEVEL": "", "VERSION_ID": "1"}, ok: true},
		{name: "host level only, ext without version info", host: Fields{"ID": "x", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "x"}, ok: true},
		{name: "host level only, ext level differs", host: Fields{"ID": "x", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "x", "SYSEXT_LEVEL": "1"}, ok: false},
		{name: "sysext ignores CONFEXT_LEVEL", host: Fields{"ID": "x", "VERSION_ID": "1", "CONFEXT_LEVEL": "2"},
			ext: Fields{"ID": "x", "CONFEXT_LEVEL": "2"}, ok: false},
		{name: "confext levels equal", class: Confext, host: Fields{"ID": "x", "VERSION_ID": "1", "CONFEXT_LEVEL": "2"},
			ext: Fields{"ID": "x", "CONFEXT_LEVEL": "2"}, ok: true},
		{name: "confext levels differ", class: Confext, host: Fields{"ID": "x", "VERSION_ID": "1", "CONFEXT_LEVEL": "2"},
			ext: Fields{"ID": "x", "CONFEXT_LEVEL": "3"}, ok: false},
		{name: "confext ignores SYSEXT_LEVEL", class: Confext, host: Fields{"ID": "x", "VERSION_ID": "1", "SYSEXT_LEVEL": "2"},
			ext: Fields{"ID": "x", "SYSEXT_LEVEL": "2"}, ok: false},

		// architecture
		{name: "arch matches", host: alpine, ext: Fields{"ID": "_any", "ARCHITECTURE": "x86-64"}, ok: true},
		{name: "arch _any", host: alpine, ext: Fields{"ID": "_any", "ARCHITECTURE": "_any"}, ok: true},
		{name: "arch empty", host: alpine, ext: Fields{"ID": "_any", "ARCHITECTURE": ""}, ok: true},
		{name: "arch mismatch", host: alpine, ext: Fields{"ID": "_any", "ARCHITECTURE": "s390x"}, ok: false,
			reason: "Extension 'e' is for architecture 's390x', but deployed on top of 'x86-64'."},
		{name: "arch checked before ID", host: alpine, arch: "arm64",
			ext: Fields{"ID": "alpine", "VERSION_ID": "3.24.2", "ARCHITECTURE": "x86-64"}, ok: false},

		// scope
		{name: "absent scope allows system", host: alpine, ext: Fields{"ID": "_any"}, scope: "system", ok: true},
		{name: "absent scope refuses initrd", host: alpine, ext: Fields{"ID": "_any"}, scope: "initrd", ok: false,
			reason: "Extension 'e' is not suitable for scope initrd, ignoring."},
		{name: "empty scope refused", host: alpine, ext: Fields{"ID": "_any", "SYSEXT_SCOPE": ""}, scope: "system", ok: false,
			reason: "Extension 'e' is not suitable for scope system, ignoring."},
		{name: "whitespace scope refused", host: alpine, ext: Fields{"ID": "_any", "SYSEXT_SCOPE": "  \t"}, scope: "system", ok: false},
		{name: "initrd-only scope refused on system", host: alpine, ext: Fields{"ID": "_any", "SYSEXT_SCOPE": "initrd"}, scope: "system", ok: false},
		{name: "initrd scope on initrd", host: alpine, ext: Fields{"ID": "_any", "SYSEXT_SCOPE": "initrd"}, scope: "initrd", ok: true},
		{name: "scope list", host: alpine, ext: Fields{"ID": "_any", "SYSEXT_SCOPE": "initrd system portable"}, scope: "system", ok: true},
		{name: "scope is checked first", host: alpine, ext: Fields{"ID": "fedora", "SYSEXT_SCOPE": "portable"}, scope: "system", ok: false,
			reason: "Extension 'e' is not suitable for scope system, ignoring."},
		{name: "confext uses CONFEXT_SCOPE", class: Confext, host: alpine,
			ext: Fields{"ID": "_any", "SYSEXT_SCOPE": "", "CONFEXT_SCOPE": "system"}, scope: "system", ok: true},
		{name: "no host scope skips the check", host: alpine, ext: Fields{"ID": "_any", "SYSEXT_SCOPE": ""}, skip: true, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arch := tc.arch
			if arch == "" {
				arch = "x86-64"
			}
			scope := tc.scope
			switch {
			case tc.skip:
				scope = ""
			case scope == "":
				scope = "system"
			}
			ok, reason := Validate("e", tc.host, tc.ext, tc.class, arch, scope)
			if ok != tc.ok {
				t.Errorf("Validate() ok = %v, want %v (reason %q)", ok, tc.ok, reason)
			}
			if tc.reason != "" && reason != tc.reason {
				t.Errorf("Validate() reason = %q, want %q", reason, tc.reason)
			}
			if reason == "" {
				t.Error("Validate() returned no reason")
			}
		})
	}
}

func TestHostScope(t *testing.T) {
	root := t.TempDir()
	if got, err := HostScope(root); err != nil || got != "system" {
		t.Errorf("HostScope() = %q, %v; want system", got, err)
	}
	mustWriteFile(t, root+"/etc/initrd-release", "ID=x\n")
	if got, err := HostScope(root); err != nil || got != "initrd" {
		t.Errorf("HostScope() = %q, %v; want initrd", got, err)
	}

	link := t.TempDir()
	mustSymlink(t, "/run/initrd-release", link+"/etc/initrd-release")
	if got, err := HostScope(link); err != nil || got != "system" {
		t.Errorf("dangling initrd-release: HostScope() = %q, %v; want system", got, err)
	}
	mustWriteFile(t, link+"/run/initrd-release", "")
	if got, err := HostScope(link); err != nil || got != "initrd" {
		t.Errorf("initrd-release symlink resolved in root: HostScope() = %q, %v; want initrd", got, err)
	}
}

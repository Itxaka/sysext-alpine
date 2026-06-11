package release

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Fields
	}{
		{
			name:    "plain assignment",
			content: "ID=alpine\nVERSION_ID=3.20\n",
			want:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
		},
		{
			name:    "double quoted",
			content: `NAME="Alpine Linux"`,
			want:    Fields{"NAME": "Alpine Linux"},
		},
		{
			name:    "single quoted",
			content: `PRETTY_NAME='Alpine Linux v3.20'`,
			want:    Fields{"PRETTY_NAME": "Alpine Linux v3.20"},
		},
		{
			name: "double quote escapes",
			// File text: MSG="a \"quoted\" \$var \`tick\\ backslash"
			content: "MSG=\"a \\\"quoted\\\" \\$var \\`tick\\\\ backslash\"",
			want:    Fields{"MSG": "a \"quoted\" $var `tick\\ backslash"},
		},
		{
			name:    "unrecognized escape kept literally in double quotes",
			content: `V="a\nb"`,
			want:    Fields{"V": `a\nb`},
		},
		{
			name:    "no escapes inside single quotes",
			content: `V='a\$b\\c'`,
			want:    Fields{"V": `a\$b\\c`},
		},
		{
			name:    "comments and blank lines",
			content: "# leading comment\n\nID=alpine\n   # indented comment\n\t\nVERSION_ID=3.20\n",
			want:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
		},
		{
			name:    "leading and trailing whitespace around assignment",
			content: "  ID=alpine   \n",
			want:    Fields{"ID": "alpine"},
		},
		{
			name:    "empty value",
			content: "EMPTY=\nQUOTED_EMPTY=\"\"\nSINGLE_EMPTY=''\n",
			want:    Fields{"EMPTY": "", "QUOTED_EMPTY": "", "SINGLE_EMPTY": ""},
		},
		{
			name:    "duplicate key last wins",
			content: "ID=first\nID=second\n",
			want:    Fields{"ID": "second"},
		},
		{
			name:    "malformed lines skipped",
			content: "noequals\n=value\n1BAD=x\nBAD-KEY=x\nGOOD=yes\n",
			want:    Fields{"GOOD": "yes"},
		},
		{
			name:    "unterminated double quote skipped",
			content: "BAD=\"unterminated\nGOOD=ok\n",
			want:    Fields{"GOOD": "ok"},
		},
		{
			name:    "unterminated single quote skipped",
			content: "BAD='unterminated\nGOOD=ok\n",
			want:    Fields{"GOOD": "ok"},
		},
		{
			name:    "trailing garbage after closing quote skipped",
			content: "BAD=\"value\" garbage\nGOOD=ok\n",
			want:    Fields{"GOOD": "ok"},
		},
		{
			name:    "dangling backslash skipped",
			content: "BAD=\"value\\\nGOOD=ok\n",
			want:    Fields{"GOOD": "ok"},
		},
		{
			name:    "trailing whitespace after quote allowed",
			content: "V=\"value\"   \n",
			want:    Fields{"V": "value"},
		},
		{
			name:    "CRLF line endings",
			content: "ID=alpine\r\nVERSION_ID=3.20\r\n",
			want:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
		},
		{
			name:    "hash inside unquoted value is literal",
			content: "V=foo#bar\n",
			want:    Fields{"V": "foo#bar"},
		},
		{
			name:    "underscore key and value with equals sign",
			content: "_KEY=a=b\n",
			want:    Fields{"_KEY": "a=b"},
		},
		{
			name:    "empty content",
			content: "",
			want:    Fields{},
		},
		{
			name:    "realistic os-release",
			content: "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.20.3\nPRETTY_NAME=\"Alpine Linux v3.20\"\nHOME_URL=\"https://alpinelinux.org/\"\n",
			want: Fields{
				"NAME":        "Alpine Linux",
				"ID":          "alpine",
				"VERSION_ID":  "3.20.3",
				"PRETTY_NAME": "Alpine Linux v3.20",
				"HOME_URL":    "https://alpinelinux.org/",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.content))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Parse() = %#v, want %#v", got, tc.want)
			}
			for k, v := range tc.want {
				gv, ok := got[k]
				if !ok || gv != v {
					t.Errorf("Parse()[%q] = %q (present=%v), want %q", k, gv, ok, v)
				}
			}
		})
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	if err := os.WriteFile(path, []byte("ID=alpine\nVERSION_ID=3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}
	if got["ID"] != "alpine" || got["VERSION_ID"] != "3.20" {
		t.Errorf("ParseFile() = %#v", got)
	}

	if _, err := ParseFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("ParseFile() on missing file: expected error, got nil")
	}
}

func TestHostOSRelease(t *testing.T) {
	t.Run("etc takes precedence", func(t *testing.T) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "etc"))
		mustMkdirAll(t, filepath.Join(root, "usr/lib"))
		mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=fromtetc\n")
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=fromusrlib\n")
		got, err := HostOSRelease(root)
		if err != nil {
			t.Fatalf("HostOSRelease() error = %v", err)
		}
		if got["ID"] != "fromtetc" {
			t.Errorf("ID = %q, want fromtetc", got["ID"])
		}
	})

	t.Run("falls back to usr/lib", func(t *testing.T) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "usr/lib"))
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=fromusrlib\n")
		got, err := HostOSRelease(root)
		if err != nil {
			t.Fatalf("HostOSRelease() error = %v", err)
		}
		if got["ID"] != "fromusrlib" {
			t.Errorf("ID = %q, want fromusrlib", got["ID"])
		}
	})

	t.Run("neither present", func(t *testing.T) {
		root := t.TempDir()
		if _, err := HostOSRelease(root); err == nil {
			t.Error("expected error when no os-release exists")
		}
	})
}

func TestMatch(t *testing.T) {
	const arch = "x86-64"

	tests := []struct {
		name    string
		host    Fields
		ext     Fields
		class   Class
		wantErr string // substring of expected error; "" means match must succeed
	}{
		{
			name:    "missing ID is an error",
			host:    Fields{"ID": "alpine"},
			ext:     Fields{"VERSION_ID": "3.20"},
			class:   Sysext,
			wantErr: "ID",
		},
		{
			name:  "ID _any skips ID and version checks",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "_any"},
			class: Sysext,
		},
		{
			name:    "ID mismatch",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "debian", "VERSION_ID": "3.20"},
			class:   Sysext,
			wantErr: `"debian"`,
		},
		{
			name:  "SYSEXT_LEVEL match",
			host:  Fields{"ID": "alpine", "SYSEXT_LEVEL": "2", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "SYSEXT_LEVEL": "2"},
			class: Sysext,
		},
		{
			name:    "SYSEXT_LEVEL mismatch",
			host:    Fields{"ID": "alpine", "SYSEXT_LEVEL": "2", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "SYSEXT_LEVEL": "1"},
			class:   Sysext,
			wantErr: "SYSEXT_LEVEL",
		},
		{
			name:    "SYSEXT_LEVEL defined on extension but missing on host",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "SYSEXT_LEVEL": "2"},
			class:   Sysext,
			wantErr: "SYSEXT_LEVEL",
		},
		{
			name: "SYSEXT_LEVEL defined ignores VERSION_ID mismatch",
			host: Fields{"ID": "alpine", "SYSEXT_LEVEL": "2", "VERSION_ID": "3.20"},
			ext:  Fields{"ID": "alpine", "SYSEXT_LEVEL": "2", "VERSION_ID": "999"},
		},
		{
			name:  "VERSION_ID fallback match",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			class: Sysext,
		},
		{
			name:    "VERSION_ID fallback mismatch",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "VERSION_ID": "3.19"},
			class:   Sysext,
			wantErr: "VERSION_ID",
		},
		{
			name:    "neither level nor VERSION_ID on extension",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine"},
			class:   Sysext,
			wantErr: "VERSION_ID",
		},
		{
			name:  "confext uses CONFEXT_LEVEL",
			host:  Fields{"ID": "alpine", "CONFEXT_LEVEL": "1", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "CONFEXT_LEVEL": "1"},
			class: Confext,
		},
		{
			name:    "confext CONFEXT_LEVEL mismatch",
			host:    Fields{"ID": "alpine", "CONFEXT_LEVEL": "1"},
			ext:     Fields{"ID": "alpine", "CONFEXT_LEVEL": "2"},
			class:   Confext,
			wantErr: "CONFEXT_LEVEL",
		},
		{
			name:    "confext ignores SYSEXT_LEVEL and falls back to VERSION_ID",
			host:    Fields{"ID": "alpine", "SYSEXT_LEVEL": "2", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "SYSEXT_LEVEL": "2", "VERSION_ID": "3.19"},
			class:   Confext,
			wantErr: "VERSION_ID",
		},
		{
			name:  "ARCHITECTURE match",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "ARCHITECTURE": "x86-64"},
			class: Sysext,
		},
		{
			name:  "ARCHITECTURE _any always matches",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "ARCHITECTURE": "_any"},
			class: Sysext,
		},
		{
			name:    "ARCHITECTURE mismatch",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "VERSION_ID": "3.20", "ARCHITECTURE": "arm64"},
			class:   Sysext,
			wantErr: "ARCHITECTURE",
		},
		{
			name:    "ARCHITECTURE checked even with ID _any",
			host:    Fields{"ID": "alpine"},
			ext:     Fields{"ID": "_any", "ARCHITECTURE": "arm64"},
			class:   Sysext,
			wantErr: "ARCHITECTURE",
		},
		{
			name:  "ARCHITECTURE absent skips check",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			class: Sysext,
		},
		{
			name:  "SYSEXT_SCOPE absent is OK",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			class: Sysext,
		},
		{
			name:  "SYSEXT_SCOPE empty is OK",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": ""},
			class: Sysext,
		},
		{
			name:  "SYSEXT_SCOPE system is OK",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "system"},
			class: Sysext,
		},
		{
			name:    "SYSEXT_SCOPE initrd portable rejected",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "initrd portable"},
			class:   Sysext,
			wantErr: "SYSEXT_SCOPE",
		},
		{
			name:  "SYSEXT_SCOPE system initrd is OK",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "system initrd"},
			class: Sysext,
		},
		{
			name:    "SYSEXT_SCOPE only portable rejected",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "portable"},
			class:   Sysext,
			wantErr: "SYSEXT_SCOPE",
		},
		{
			name:  "SYSEXT_SCOPE with extra whitespace is OK",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "  initrd \t system  "},
			class: Sysext,
		},
		{
			name:  "SYSEXT_SCOPE whitespace-only treated as empty",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "   \t "},
			class: Sysext,
		},
		{
			name:    "SYSEXT_SCOPE checked even with ID _any",
			host:    Fields{"ID": "alpine"},
			ext:     Fields{"ID": "_any", "SYSEXT_SCOPE": "initrd"},
			class:   Sysext,
			wantErr: "SYSEXT_SCOPE",
		},
		{
			name:    "confext uses CONFEXT_SCOPE",
			host:    Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:     Fields{"ID": "alpine", "VERSION_ID": "3.20", "CONFEXT_SCOPE": "initrd"},
			class:   Confext,
			wantErr: "CONFEXT_SCOPE",
		},
		{
			name:  "confext ignores SYSEXT_SCOPE",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "initrd"},
			class: Confext,
		},
		{
			name:  "sysext ignores CONFEXT_SCOPE",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "CONFEXT_SCOPE": "initrd"},
			class: Sysext,
		},
		{
			name:  "confext CONFEXT_SCOPE system is OK",
			host:  Fields{"ID": "alpine", "VERSION_ID": "3.20"},
			ext:   Fields{"ID": "alpine", "VERSION_ID": "3.20", "CONFEXT_SCOPE": "system portable"},
			class: Confext,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Match(tc.host, tc.ext, tc.class, arch)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Match() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Match() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Match() error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestMatchErrorNamesBothValues(t *testing.T) {
	err := Match(
		Fields{"ID": "alpine", "VERSION_ID": "3.20"},
		Fields{"ID": "debian", "VERSION_ID": "3.20"},
		Sysext, "x86-64")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"debian", "alpine", "ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestMatchScopeErrorNamesFieldAndValue(t *testing.T) {
	err := Match(
		Fields{"ID": "alpine", "VERSION_ID": "3.20"},
		Fields{"ID": "alpine", "VERSION_ID": "3.20", "SYSEXT_SCOPE": "initrd portable"},
		Sysext, "x86-64")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"SYSEXT_SCOPE", "initrd portable", "system"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestHostArchitecture(t *testing.T) {
	got := HostArchitecture()
	if got == "" {
		t.Fatal("HostArchitecture() returned empty string")
	}
	// Spot check the mapping for the architecture the tests run on.
	want := map[string]string{
		"amd64":   "x86-64",
		"arm64":   "arm64",
		"386":     "x86",
		"arm":     "arm",
		"riscv64": "riscv64",
		"ppc64le": "ppc64-le",
		"s390x":   "s390x",
		"loong64": "loongarch64",
	}[runtime.GOARCH]
	if want != "" && got != want {
		t.Errorf("HostArchitecture() = %q, want %q for GOARCH %s", got, want, runtime.GOARCH)
	}
}

// makeImage builds an image tree with the given release files in the
// class release dir and returns imageRoot.
func makeImage(t *testing.T, class Class, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, class.ReleaseFileDir())
	mustMkdirAll(t, dir)
	for name, content := range files {
		mustWriteFile(t, filepath.Join(dir, name), content)
	}
	return root
}

func TestFindExtensionRelease(t *testing.T) {
	for _, class := range []Class{Sysext, Confext} {
		className := map[Class]string{Sysext: "sysext", Confext: "confext"}[class]

		t.Run(className+"/primary file found", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{
				"extension-release.foo": "ID=alpine\nVERSION_ID=3.20\n",
			})
			got, err := FindExtensionRelease(root, "foo", class)
			if err != nil {
				t.Fatalf("FindExtensionRelease() error = %v", err)
			}
			if got["ID"] != "alpine" || got["VERSION_ID"] != "3.20" {
				t.Errorf("FindExtensionRelease() = %#v", got)
			}
		})

		t.Run(className+"/name mismatch without xattr", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{
				"extension-release.bar": "ID=alpine\n",
			})
			if _, err := FindExtensionRelease(root, "foo", class); err == nil {
				t.Error("expected error for mismatching release file name")
			}
		})

		t.Run(className+"/missing release dir", func(t *testing.T) {
			root := t.TempDir()
			_, err := FindExtensionRelease(root, "foo", class)
			if err == nil {
				t.Error("expected error for missing release dir")
			}
		})

		t.Run(className+"/empty release dir", func(t *testing.T) {
			root := makeImage(t, class, nil)
			if _, err := FindExtensionRelease(root, "foo", class); err == nil {
				t.Error("expected error for empty release dir")
			}
		})

		t.Run(className+"/wrong class dir", func(t *testing.T) {
			other := Sysext
			if class == Sysext {
				other = Confext
			}
			root := makeImage(t, other, map[string]string{
				"extension-release.foo": "ID=alpine\n",
			})
			if _, err := FindExtensionRelease(root, "foo", class); err == nil {
				t.Error("expected error: release file is in the other class's dir")
			}
		})

		t.Run(className+"/escape hatch single file with falsy xattr", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{
				"extension-release.othername": "ID=_any\n",
			})
			path := filepath.Join(root, class.ReleaseFileDir(), "extension-release.othername")
			setStrictXattr(t, path, "0")
			got, err := FindExtensionRelease(root, "foo", class)
			if err != nil {
				t.Fatalf("FindExtensionRelease() error = %v", err)
			}
			if got["ID"] != "_any" {
				t.Errorf("ID = %q, want _any", got["ID"])
			}
		})

		t.Run(className+"/escape hatch with truthy xattr rejected", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{
				"extension-release.othername": "ID=_any\n",
			})
			path := filepath.Join(root, class.ReleaseFileDir(), "extension-release.othername")
			setStrictXattr(t, path, "1")
			if _, err := FindExtensionRelease(root, "foo", class); err == nil {
				t.Error("expected error: strict xattr is truthy")
			}
		})

		t.Run(className+"/escape hatch refused with two files", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{
				"extension-release.one": "ID=_any\n",
				"extension-release.two": "ID=_any\n",
			})
			dir := filepath.Join(root, class.ReleaseFileDir())
			setStrictXattr(t, filepath.Join(dir, "extension-release.one"), "0")
			setStrictXattr(t, filepath.Join(dir, "extension-release.two"), "0")
			if _, err := FindExtensionRelease(root, "foo", class); err == nil {
				t.Error("expected error: more than one candidate file")
			}
		})
	}

	t.Run("falsy xattr variants", func(t *testing.T) {
		for _, v := range []string{"0", "no", "false", "off", "NO", "False", "OFF"} {
			root := makeImage(t, Sysext, map[string]string{
				"extension-release.other": "ID=_any\n",
			})
			path := filepath.Join(root, Sysext.ReleaseFileDir(), "extension-release.other")
			setStrictXattr(t, path, v)
			if _, err := FindExtensionRelease(root, "foo", Sysext); err != nil {
				t.Errorf("xattr value %q: FindExtensionRelease() error = %v, want nil", v, err)
			}
		}
	})

	t.Run("primary file preferred over escape hatch", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{
			"extension-release.foo": "ID=alpine\nVERSION_ID=3.20\n",
		})
		got, err := FindExtensionRelease(root, "foo", Sysext)
		if err != nil {
			t.Fatalf("FindExtensionRelease() error = %v", err)
		}
		if got["ID"] != "alpine" {
			t.Errorf("ID = %q, want alpine", got["ID"])
		}
	})
}

func TestIsFalsy(t *testing.T) {
	for _, v := range []string{"0", "no", "false", "off", "No", "FALSE", "Off", " 0 ", "0\x00"} {
		if !isFalsy(v) {
			t.Errorf("isFalsy(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "1", "yes", "true", "on", "n", "f", "anything"} {
		if isFalsy(v) {
			t.Errorf("isFalsy(%q) = true, want false", v)
		}
	}
}

// setStrictXattr sets the user.extension-release.strict xattr, skipping the
// test if the filesystem backing TMPDIR does not support user xattrs.
func setStrictXattr(t *testing.T, path, value string) {
	t.Helper()
	err := unix.Setxattr(path, "user.extension-release.strict", []byte(value), 0)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
			t.Skipf("filesystem does not support user xattrs: %v", err)
		}
		t.Fatalf("Setxattr: %v", err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

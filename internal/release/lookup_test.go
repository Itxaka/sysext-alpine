package release

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostOSRelease(t *testing.T) {
	t.Run("etc first", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=etc\n")
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=usrlib\n")
		got, err := HostOSRelease(root)
		if err != nil || got["ID"] != "etc" {
			t.Errorf("HostOSRelease() = %v, %v; want ID=etc", got, err)
		}
	})
	t.Run("usr/lib fallback", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=usrlib\n")
		got, err := HostOSRelease(root)
		if err != nil || got["ID"] != "usrlib" {
			t.Errorf("HostOSRelease() = %v, %v; want ID=usrlib", got, err)
		}
	})
	t.Run("dangling etc symlink falls back", func(t *testing.T) {
		root := t.TempDir()
		mustSymlink(t, "/nonexistent", filepath.Join(root, "etc/os-release"))
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=usrlib\n")
		got, err := HostOSRelease(root)
		if err != nil || got["ID"] != "usrlib" {
			t.Errorf("HostOSRelease() = %v, %v; want ID=usrlib", got, err)
		}
	})
	t.Run("absolute symlink resolved inside root", func(t *testing.T) {
		root := t.TempDir()
		mustSymlink(t, "/usr/lib/os-release", filepath.Join(root, "etc/os-release"))
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=fromroot\n")
		got, err := HostOSRelease(root)
		if err != nil || got["ID"] != "fromroot" {
			t.Errorf("HostOSRelease() = %v, %v; want ID=fromroot", got, err)
		}
	})
	t.Run("dotdot symlink clamped to root", func(t *testing.T) {
		root := t.TempDir()
		mustSymlink(t, "../../../../../../usr/lib/os-release", filepath.Join(root, "etc/os-release"))
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=clamped\n")
		got, err := HostOSRelease(root)
		if err != nil || got["ID"] != "clamped" {
			t.Errorf("HostOSRelease() = %v, %v; want ID=clamped", got, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := HostOSRelease(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want ErrNotExist", err)
		}
	})
	t.Run("etc error does not fall back", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "etc"), "not a dir")
		mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=usrlib\n")
		if _, err := HostOSRelease(root); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want ENOTDIR", err)
		}
	})
	t.Run("invalid UTF-8 fails", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=\xff\n")
		if _, err := HostOSRelease(root); !errors.Is(err, unix.EINVAL) {
			t.Errorf("err = %v, want EINVAL", err)
		}
	})
	t.Run("SYSTEMD_OS_RELEASE", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=etc\n")
		mustWriteFile(t, filepath.Join(root, "custom/os-release"), "ID=custom\n")
		t.Setenv("SYSTEMD_OS_RELEASE", "/custom/os-release")
		got, err := HostOSRelease(root)
		if err != nil || got["ID"] != "custom" {
			t.Errorf("HostOSRelease() = %v, %v; want ID=custom", got, err)
		}
		t.Setenv("SYSTEMD_OS_RELEASE", "/missing")
		if _, err := HostOSRelease(root); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("missing SYSTEMD_OS_RELEASE target: err = %v, want ErrNotExist without fallback", err)
		}
	})
}

// makeImage builds an image tree with the given release files in the class
// release dir and returns its root.
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

func releasePath(root string, class Class, name string) string {
	return filepath.Join(root, class.ReleaseFileDir(), name)
}

// setStrictXattr sets user.extension-release.strict, skipping the test when
// the filesystem backing TMPDIR does not support user xattrs.
func setStrictXattr(t *testing.T, path, value string) {
	t.Helper()
	if err := unix.Setxattr(path, strictXattr, []byte(value), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
			t.Skipf("filesystem does not support user xattrs: %v", err)
		}
		t.Fatalf("Setxattr: %v", err)
	}
}

func TestFindExtensionRelease(t *testing.T) {
	for _, class := range []Class{Sysext, Confext} {
		other := Confext
		if class == Confext {
			other = Sysext
		}
		className := map[Class]string{Sysext: "sysext", Confext: "confext"}[class]

		t.Run(className+"/primary", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{"extension-release.foo": "ID=alpine\nVERSION_ID=3.20\n"})
			got, err := FindExtensionRelease(root, "foo", class, false)
			if err != nil || got["ID"] != "alpine" || got["VERSION_ID"] != "3.20" {
				t.Errorf("FindExtensionRelease() = %v, %v", got, err)
			}
		})
		t.Run(className+"/wrong class dir", func(t *testing.T) {
			root := makeImage(t, other, map[string]string{"extension-release.foo": "ID=alpine\n"})
			if _, err := FindExtensionRelease(root, "foo", class, false); !errors.Is(err, ErrNoExtensionRelease) {
				t.Errorf("err = %v, want ErrNoExtensionRelease", err)
			}
		})
		t.Run(className+"/versioned name", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{"extension-release.foo": "ID=_any\n"})
			for _, name := range []string{"foo_1.2", "foo+3-1", "foo_1.2.raw", "foo.raw", "foo.sysext.raw", "foo.confext.raw", "foo_2+1"} {
				if got, err := FindExtensionRelease(root, name, class, false); err != nil || got["ID"] != "_any" {
					t.Errorf("%s: FindExtensionRelease() = %v, %v", name, got, err)
				}
			}
		})
		t.Run(className+"/name mismatch without xattr", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{"extension-release.bar": "ID=alpine\n"})
			if _, err := FindExtensionRelease(root, "foo_1.2", class, false); !errors.Is(err, ErrNoExtensionRelease) {
				t.Errorf("err = %v, want ErrNoExtensionRelease", err)
			}
		})
		t.Run(className+"/relax accepts any single file", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{"extension-release.bar": "ID=alpine\n"})
			if got, err := FindExtensionRelease(root, "foo", class, true); err != nil || got["ID"] != "alpine" {
				t.Errorf("FindExtensionRelease(relax) = %v, %v", got, err)
			}
		})
		t.Run(className+"/relax refuses two files", func(t *testing.T) {
			root := makeImage(t, class, map[string]string{
				"extension-release.bar": "ID=alpine\n",
				"extension-release.baz": "ID=alpine\n",
			})
			if _, err := FindExtensionRelease(root, "foo", class, true); !errors.Is(err, ErrExtensionReleaseNotUnique) {
				t.Errorf("err = %v, want ErrExtensionReleaseNotUnique", err)
			}
		})
	}

	t.Run("missing release dir", func(t *testing.T) {
		_, err := FindExtensionRelease(t.TempDir(), "foo", Sysext, false)
		if !errors.Is(err, ErrNoExtensionRelease) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want ErrNoExtensionRelease", err)
		}
	})
	t.Run("empty release dir", func(t *testing.T) {
		if _, err := FindExtensionRelease(makeImage(t, Sysext, nil), "foo", Sysext, false); !errors.Is(err, ErrNoExtensionRelease) {
			t.Errorf("err = %v, want ErrNoExtensionRelease", err)
		}
	})
	t.Run("invalid image name", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{"extension-release.foo": "ID=_any\n"})
		for _, name := range []string{"", ".", "..", "a/b", ".#foo", "nl\nx"} {
			if _, err := FindExtensionRelease(root, name, Sysext, false); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%q: err = %v, want EINVAL", name, err)
			}
		}
	})
	t.Run("absolute symlink resolved inside image", func(t *testing.T) {
		root := makeImage(t, Sysext, nil)
		mustSymlink(t, "/etc/os-release", releasePath(root, Sysext, "extension-release.foo"))
		if _, err := FindExtensionRelease(root, "foo", Sysext, false); !errors.Is(err, ErrNoExtensionRelease) {
			t.Errorf("symlink to a path missing in the image: err = %v, want ErrNoExtensionRelease", err)
		}
		mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=inimage\n")
		if got, err := FindExtensionRelease(root, "foo", Sysext, false); err != nil || got["ID"] != "inimage" {
			t.Errorf("FindExtensionRelease() = %v, %v; want ID=inimage", got, err)
		}
	})
	t.Run("dotdot symlink clamped", func(t *testing.T) {
		root := makeImage(t, Sysext, nil)
		mustSymlink(t, "../../../../../../../../etc/os-release", releasePath(root, Sysext, "extension-release.foo"))
		mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=clamped\n")
		if got, err := FindExtensionRelease(root, "foo", Sysext, false); err != nil || got["ID"] != "clamped" {
			t.Errorf("FindExtensionRelease() = %v, %v; want ID=clamped", got, err)
		}
	})
	t.Run("relative symlink followed", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{"real": "ID=real\n"})
		mustSymlink(t, "real", releasePath(root, Sysext, "extension-release.foo"))
		if got, err := FindExtensionRelease(root, "foo", Sysext, false); err != nil || got["ID"] != "real" {
			t.Errorf("FindExtensionRelease() = %v, %v; want ID=real", got, err)
		}
	})
	t.Run("symlinked release dir", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "opt/rel/extension-release.foo"), "ID=moved\n")
		mustSymlink(t, "/opt/rel", filepath.Join(root, Sysext.ReleaseFileDir()))
		if got, err := FindExtensionRelease(root, "foo", Sysext, false); err != nil || got["ID"] != "moved" {
			t.Errorf("FindExtensionRelease() = %v, %v; want ID=moved", got, err)
		}
	})
	t.Run("primary that is a directory", func(t *testing.T) {
		root := makeImage(t, Sysext, nil)
		mustMkdirAll(t, releasePath(root, Sysext, "extension-release.foo"))
		if _, err := FindExtensionRelease(root, "foo", Sysext, false); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want EISDIR", err)
		}
	})
	t.Run("fallback ignores symlinked candidate", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{"target": "ID=_any\n"})
		mustSymlink(t, "target", releasePath(root, Sysext, "extension-release.foo"))
		if got, err := FindExtensionRelease(root, "foo", Sysext, false); err != nil || got["ID"] != "_any" {
			t.Errorf("primary symlink: FindExtensionRelease() = %v, %v", got, err)
		}
		if _, err := FindExtensionRelease(root, "foo_1", Sysext, false); !errors.Is(err, ErrNoExtensionRelease) {
			t.Errorf("symlinked base-name candidate: err = %v, want ErrNoExtensionRelease", err)
		}
	})
	t.Run("fallback ignores backup files", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{"extension-release.foo~": "ID=_any\n", "extension-release.foo.bak": "ID=_any\n"})
		if _, err := FindExtensionRelease(root, "foo_1", Sysext, false); !errors.Is(err, ErrNoExtensionRelease) {
			t.Errorf("err = %v, want ErrNoExtensionRelease", err)
		}
	})
	t.Run("base name plus unrelated file", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{
			"extension-release.foo":   "ID=foo\n",
			"extension-release.other": "ID=other\n",
		})
		if got, err := FindExtensionRelease(root, "foo_2", Sysext, false); err != nil || got["ID"] != "foo" {
			t.Errorf("FindExtensionRelease() = %v, %v; want ID=foo", got, err)
		}
	})

	t.Run("xattr escape hatch", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{
			"README":                "not a release file",
			"extension-release.zzz": "ID=_any\n",
		})
		setStrictXattr(t, releasePath(root, Sysext, "extension-release.zzz"), "0")
		if got, err := FindExtensionRelease(root, "foo", Sysext, false); err != nil || got["ID"] != "_any" {
			t.Errorf("FindExtensionRelease() = %v, %v", got, err)
		}
	})
	t.Run("xattr on a file not named extension-release.*", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{"myrelease": "ID=_any\n"})
		setStrictXattr(t, releasePath(root, Sysext, "myrelease"), "0")
		if _, err := FindExtensionRelease(root, "foo", Sysext, false); !errors.Is(err, ErrNoExtensionRelease) {
			t.Errorf("err = %v, want ErrNoExtensionRelease", err)
		}
	})
	t.Run("xattr true is strict", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{"extension-release.zzz": "ID=_any\n"})
		setStrictXattr(t, releasePath(root, Sysext, "extension-release.zzz"), "1")
		if _, err := FindExtensionRelease(root, "foo", Sysext, false); !errors.Is(err, ErrNoExtensionRelease) {
			t.Errorf("err = %v, want ErrNoExtensionRelease", err)
		}
	})
	t.Run("two xattr candidates", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{
			"extension-release.one": "ID=_any\n",
			"extension-release.two": "ID=_any\n",
		})
		setStrictXattr(t, releasePath(root, Sysext, "extension-release.one"), "0")
		setStrictXattr(t, releasePath(root, Sysext, "extension-release.two"), "no")
		if _, err := FindExtensionRelease(root, "foo", Sysext, false); !errors.Is(err, ErrExtensionReleaseNotUnique) || !errors.Is(err, unix.ENOTUNIQ) {
			t.Errorf("err = %v, want ErrExtensionReleaseNotUnique", err)
		}
	})
	t.Run("base name and xattr candidate", func(t *testing.T) {
		root := makeImage(t, Sysext, map[string]string{
			"extension-release.foo":   "ID=_any\n",
			"extension-release.other": "ID=_any\n",
		})
		setStrictXattr(t, releasePath(root, Sysext, "extension-release.other"), "0")
		if _, err := FindExtensionRelease(root, "foo_1", Sysext, false); !errors.Is(err, ErrExtensionReleaseNotUnique) {
			t.Errorf("err = %v, want ErrExtensionReleaseNotUnique", err)
		}
		if _, err := FindExtensionRelease(root, "foo", Sysext, false); err != nil {
			t.Errorf("primary file wins without fallback: %v", err)
		}
	})
	t.Run("xattr values", func(t *testing.T) {
		for v, want := range map[string]bool{
			"0": true, "n": true, "f": true, "N": true, "no": true, "False": true, "OFF": true, "0\x00": true, "0\x00junk": true,
			"1": false, "yes": false, " 0 ": false, "0 ": false, "maybe": false, "": false,
		} {
			root := makeImage(t, Sysext, map[string]string{"extension-release.other": "ID=_any\n"})
			setStrictXattr(t, releasePath(root, Sysext, "extension-release.other"), v)
			_, err := FindExtensionRelease(root, "foo", Sysext, false)
			if got := err == nil; got != want {
				t.Errorf("xattr %q: found = %v (err %v), want %v", v, got, err, want)
			}
		}
	})
}

func TestImageBaseName(t *testing.T) {
	for in, want := range map[string]string{
		"foo":                "foo",
		"foo_1.2":            "foo",
		"foo+3-1":            "foo",
		"foo_1.2+3":          "foo",
		"foo.raw":            "foo",
		"foo_1.raw":          "foo",
		"foo.sysext.raw":     "foo",
		"foo.confext.raw":    "foo",
		"foo.sysext":         "foo.sysext",
		"foo.raw/":           "foo.raw",
		"/var/lib/foo_2.raw": "foo",
	} {
		got, ok := imageBaseName(in)
		if !ok || got != want {
			t.Errorf("imageBaseName(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"_1.2", "+3", ".raw", "", ".#x_1"} {
		if got, ok := imageBaseName(in); ok {
			t.Errorf("imageBaseName(%q) = %q, want invalid", in, got)
		}
	}
}

func TestHasForbiddenContent(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  bool
	}{
		{"absent", func(t *testing.T, root string) { mustMkdirAll(t, filepath.Join(root, "usr/lib")) }, false},
		{"regular file", func(t *testing.T, root string) {
			mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=evil\n")
		}, true},
		{"relative symlink inside root", func(t *testing.T, root string) {
			mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=evil\n")
			mustSymlink(t, "../../etc/os-release", filepath.Join(root, "usr/lib/os-release"))
		}, true},
		{"dangling symlink", func(t *testing.T, root string) {
			mustSymlink(t, "../../etc/os-release", filepath.Join(root, "usr/lib/os-release"))
		}, false},
		{"absolute symlink to a host-only path", func(t *testing.T, root string) {
			mustSymlink(t, "/etc/hostname-that-is-not-in-the-image", filepath.Join(root, "usr/lib/os-release"))
		}, false},
		{"etc/os-release is allowed", func(t *testing.T, root string) {
			mustWriteFile(t, filepath.Join(root, "etc/os-release"), "ID=fine\n")
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got, err := HasForbiddenContent(root)
			if err != nil || got != tc.want {
				t.Errorf("HasForbiddenContent() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestIsOSTree(t *testing.T) {
	root := t.TempDir()
	if ok, err := IsOSTree(root); err != nil || ok {
		t.Errorf("empty tree: %v, %v", ok, err)
	}
	mustWriteFile(t, filepath.Join(root, "usr/lib/os-release"), "ID=x\n")
	if ok, err := IsOSTree(root); err != nil || !ok {
		t.Errorf("usr/lib/os-release: %v, %v", ok, err)
	}
	if _, err := IsOSTree(filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing tree: err = %v", err)
	}
}

func TestIsExtensionTree(t *testing.T) {
	root := makeImage(t, Confext, map[string]string{"extension-release.foo": "ID=_any\n"})
	if ok, err := IsExtensionTree(root, "foo_1", Confext, false); err != nil || !ok {
		t.Errorf("confext: %v, %v", ok, err)
	}
	if ok, err := IsExtensionTree(root, "foo", Sysext, false); err != nil || ok {
		t.Errorf("sysext: %v, %v", ok, err)
	}
	mustMkdirAll(t, releasePath(root, Sysext, "extension-release.dir"))
	if ok, err := IsExtensionTree(root, "dir", Sysext, false); err != nil || !ok {
		t.Errorf("primary of any type exists: %v, %v", ok, err)
	}
	if _, err := IsExtensionTree(filepath.Join(root, "missing"), "foo", Sysext, false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing tree: err = %v", err)
	}
}

func TestCheckImageTree(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		acceptOS bool
		want     bool
	}{
		{"sysext release", map[string]string{"usr/lib/extension-release.d/extension-release.img": "ID=_any\n"}, false, true},
		{"confext release", map[string]string{"etc/extension-release.d/extension-release.img": "ID=_any\n"}, false, true},
		{"release of another image", map[string]string{"usr/lib/extension-release.d/extension-release.other": "ID=_any\n"}, false, false},
		{"no release file", map[string]string{"usr/share/x": ""}, false, false},
		{"no release file, OS tree", map[string]string{"usr/lib/os-release": "ID=x\n"}, true, true},
		{"forbidden content", map[string]string{
			"usr/lib/os-release": "ID=x\n",
			"usr/lib/extension-release.d/extension-release.img": "ID=_any\n",
		}, false, false},
		{"forbidden content but OS tree accepted", map[string]string{
			"usr/lib/os-release": "ID=x\n",
			"usr/lib/extension-release.d/extension-release.img": "ID=_any\n",
		}, true, true},
		{"etc os-release only is not forbidden", map[string]string{
			"etc/os-release": "ID=x\n",
			"usr/lib/extension-release.d/extension-release.img": "ID=_any\n",
		}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for p, c := range tc.files {
				mustWriteFile(t, filepath.Join(root, p), c)
			}
			got, err := CheckImageTree(root, "img", tc.acceptOS)
			if err != nil || got != tc.want {
				t.Errorf("CheckImageTree() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}

	t.Run("ambiguous fallback is an error", func(t *testing.T) {
		root := t.TempDir()
		for _, n := range []string{"a", "b"} {
			p := filepath.Join(root, "usr/lib/extension-release.d/extension-release."+n)
			mustWriteFile(t, p, "ID=_any\n")
			setStrictXattr(t, p, "0")
		}
		if _, err := CheckImageTree(root, "img", true); !errors.Is(err, ErrExtensionReleaseNotUnique) {
			t.Errorf("err = %v, want ErrExtensionReleaseNotUnique", err)
		}
	})
}

func TestImageNameIsValid(t *testing.T) {
	for _, s := range []string{"foo", "foo.raw", ".hidden", "foo_1.2", "ąęół", "a b", "x+1"} {
		if !ImageNameIsValid(s) {
			t.Errorf("ImageNameIsValid(%q) = false", s)
		}
	}
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	for _, s := range []string{"", ".", "..", "a/b", "nl\nx", "tab\tx", "del\x7f", "\xff", ".#tmp", string(long), "nul\x00"} {
		if ImageNameIsValid(s) {
			t.Errorf("ImageNameIsValid(%q) = true", s)
		}
	}
}

func TestHiddenOrBackupFile(t *testing.T) {
	for _, s := range []string{".x", "x~", "lost+found", "aquota.user", "a.rpmnew", "a.dpkg-old", "a.swp", "a.bak", "a.old", "a.new", "a.ignore"} {
		if !HiddenOrBackupFile(s) {
			t.Errorf("HiddenOrBackupFile(%q) = false", s)
		}
	}
	for _, s := range []string{"a", "a.conf", "extension-release.foo", "a.bak.conf", "new"} {
		if HiddenOrBackupFile(s) {
			t.Errorf("HiddenOrBackupFile(%q) = true", s)
		}
	}
}

func TestValidUTF8(t *testing.T) {
	for _, s := range []string{"", "abc", "ąęół", "�", "\U0010fffd"} {
		if !ValidUTF8(s) {
			t.Errorf("ValidUTF8(%q) = false", s)
		}
	}
	for _, s := range []string{"\xff", "a\x00b", "﷐", "﷯", "￾", "￿", "\U0001fffe", "\xed\xa0\x80", "\xc0\x80"} {
		if ValidUTF8(s) {
			t.Errorf("ValidUTF8(%q) = true", s)
		}
	}
}

func TestReadRegularFileRefusesSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := ParseFile(fifo); err == nil {
		t.Error("FIFO: want error")
	}
}

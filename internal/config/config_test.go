package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// write creates a config file at <root>/<rel> with the given content,
// creating parent directories as needed.
func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mask creates a /dev/null symlink at <root>/<rel>.
func mask(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", path); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, class release.Class, root string) Config {
	t.Helper()
	cfg, err := Load(class, root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadEmpty(t *testing.T) {
	cfg := load(t, release.Sysext, t.TempDir())
	if cfg.Mutable != "" || cfg.ImagePolicy != "" {
		t.Errorf("empty root should yield unset config, got %+v", cfg)
	}
}

func TestLoadMainFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf",
		"[SysExt]\nMutable=auto\nImagePolicy=root=verity\n")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q, want auto", cfg.Mutable)
	}
	if cfg.ImagePolicy != "root=verity" {
		t.Errorf("ImagePolicy = %q, want root=verity", cfg.ImagePolicy)
	}
}

// First main file found wins; lower-priority directories are not consulted.
func TestLoadMainFilePrecedence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf", "[SysExt]\nMutable=yes\nImagePolicy=usrlib\n")
	write(t, root, "usr/local/lib/systemd/sysext.conf", "[SysExt]\nMutable=import\n")
	write(t, root, "run/systemd/sysext.conf", "[SysExt]\nMutable=ephemeral\n")
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=auto\n")

	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q, want auto (/etc wins)", cfg.Mutable)
	}
	// /usr/lib's ImagePolicy must NOT leak through: only the first file is read.
	if cfg.ImagePolicy != "" {
		t.Errorf("ImagePolicy = %q, want unset (only first main file is used)", cfg.ImagePolicy)
	}

	// Remove /etc; /run is next.
	if err := os.Remove(filepath.Join(root, "etc/systemd/sysext.conf")); err != nil {
		t.Fatal(err)
	}
	if cfg := load(t, release.Sysext, root); cfg.Mutable != "ephemeral" {
		t.Errorf("Mutable = %q, want ephemeral (/run wins)", cfg.Mutable)
	}

	// Remove /run; /usr/local/lib is next.
	if err := os.Remove(filepath.Join(root, "run/systemd/sysext.conf")); err != nil {
		t.Fatal(err)
	}
	if cfg := load(t, release.Sysext, root); cfg.Mutable != "import" {
		t.Errorf("Mutable = %q, want import (/usr/local/lib wins)", cfg.Mutable)
	}
}

// A /dev/null symlink masks the main file, including lower-priority copies.
func TestLoadMainFileMasked(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf", "[SysExt]\nMutable=yes\n")
	mask(t, root, "etc/systemd/sysext.conf")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset (main file masked)", cfg.Mutable)
	}
}

// Drop-ins apply after the main file and therefore override it.
func TestLoadDropinOverridesMain(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=no\nImagePolicy=main\n")
	write(t, root, "usr/lib/systemd/sysext.conf.d/10-vendor.conf", "[SysExt]\nMutable=auto\n")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q, want auto (drop-in overrides main)", cfg.Mutable)
	}
	if cfg.ImagePolicy != "main" {
		t.Errorf("ImagePolicy = %q, want main (untouched by drop-in)", cfg.ImagePolicy)
	}
}

// Drop-ins are sorted by basename lexicographically across directories;
// the last one wins.
func TestLoadDropinOrdering(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf.d/90-late.conf", "[SysExt]\nMutable=ephemeral\n")
	write(t, root, "etc/systemd/sysext.conf.d/10-early.conf", "[SysExt]\nMutable=auto\n")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "ephemeral" {
		t.Errorf("Mutable = %q, want ephemeral (90- sorts after 10-, regardless of dir)", cfg.Mutable)
	}
}

// The same basename in a higher-priority directory shadows lower ones.
func TestLoadDropinShadowing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf.d/50-foo.conf", "[SysExt]\nMutable=yes\nImagePolicy=vendor\n")
	write(t, root, "etc/systemd/sysext.conf.d/50-foo.conf", "[SysExt]\nMutable=import\n")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "import" {
		t.Errorf("Mutable = %q, want import (/etc shadows /usr/lib)", cfg.Mutable)
	}
	if cfg.ImagePolicy != "" {
		t.Errorf("ImagePolicy = %q, want unset (shadowed file fully ignored)", cfg.ImagePolicy)
	}
}

// A /dev/null symlink in /etc masks a vendor drop-in of the same basename.
func TestLoadDropinMasked(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf.d/50-foo.conf", "[SysExt]\nMutable=yes\n")
	mask(t, root, "etc/systemd/sysext.conf.d/50-foo.conf")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset (drop-in masked)", cfg.Mutable)
	}
}

// Only *.conf files are drop-ins.
func TestLoadDropinSuffix(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf.d/50-foo.conf.bak", "[SysExt]\nMutable=yes\n")
	write(t, root, "etc/systemd/sysext.conf.d/README", "[SysExt]\nMutable=yes\n")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset (non-.conf files ignored)", cfg.Mutable)
	}
}

// Section selection: sysext reads [SysExt], confext reads [ConfExt]; the
// other section is recognized but ignored. confext uses confext.conf.
func TestLoadSectionPerClass(t *testing.T) {
	root := t.TempDir()
	both := "[SysExt]\nMutable=auto\n[ConfExt]\nMutable=ephemeral\n"
	write(t, root, "etc/systemd/sysext.conf", both)
	write(t, root, "etc/systemd/confext.conf", both)

	if cfg := load(t, release.Sysext, root); cfg.Mutable != "auto" {
		t.Errorf("sysext Mutable = %q, want auto", cfg.Mutable)
	}
	if cfg := load(t, release.Confext, root); cfg.Mutable != "ephemeral" {
		t.Errorf("confext Mutable = %q, want ephemeral", cfg.Mutable)
	}
}

// confext.conf and sysext.conf are independent files.
func TestLoadConfextFileName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[ConfExt]\nMutable=yes\n")
	cfg := load(t, release.Confext, root)
	if cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset (confext must not read sysext.conf)", cfg.Mutable)
	}
}

// Section names are case-sensitive.
func TestLoadSectionCaseSensitive(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[sysext]\nMutable=yes\n")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset ([sysext] != [SysExt])", cfg.Mutable)
	}
}

// Keys outside any section, in unknown sections, unknown keys, comments
// ('#' and ';') and malformed lines are all ignored silently.
func TestLoadLenientParsing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", `Mutable=orphan
# comment
; also a comment
[Bogus]
Mutable=bogus
[SysExt]
# Mutable=commented
Unknown=ignored
not an assignment
mutable=lowercase-key-ignored
Mutable=auto
[Broken
Mutable=after-malformed-header
`)
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q, want auto", cfg.Mutable)
	}
	if cfg.ImagePolicy != "" {
		t.Errorf("ImagePolicy = %q, want unset", cfg.ImagePolicy)
	}
}

// Within the applied stream the last assignment wins, and an empty
// assignment resets the option to unset.
func TestLoadLastAssignmentWins(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf",
		"[SysExt]\nMutable=yes\nMutable=auto\nImagePolicy=p1\nImagePolicy=\n")
	cfg := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q, want auto (last wins)", cfg.Mutable)
	}
	if cfg.ImagePolicy != "" {
		t.Errorf("ImagePolicy = %q, want unset (empty assignment resets)", cfg.ImagePolicy)
	}
}

// Everything is resolved relative to root: a populated host-style tree in
// one root must not affect another.
func TestLoadRootRelative(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	write(t, rootA, "etc/systemd/sysext.conf", "[SysExt]\nMutable=auto\n")
	if cfg := load(t, release.Sysext, rootB); cfg.Mutable != "" {
		t.Errorf("rootB Mutable = %q, want unset", cfg.Mutable)
	}
	if cfg := load(t, release.Sysext, rootA); cfg.Mutable != "auto" {
		t.Errorf("rootA Mutable = %q, want auto", cfg.Mutable)
	}
}

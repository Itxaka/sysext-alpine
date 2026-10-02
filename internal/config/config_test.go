package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

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

func symlink(t *testing.T, target, root, rel string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// devNullFIFO makes <root>/dev/null a non-regular file, standing in for the
// character device of a real root.
func devNullFIFO(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "dev/null"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
}

func load(t *testing.T, class release.Class, root string) (Config, []string) {
	t.Helper()
	return Load(class, root, nil)
}

func loadQuiet(t *testing.T, class release.Class, root string) Config {
	t.Helper()
	cfg, msgs := load(t, class, root)
	if len(msgs) != 0 {
		t.Errorf("unexpected messages: %q", msgs)
	}
	return cfg
}

func wantMessages(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("messages:\n got %q\nwant %q", got, want)
	}
}

func TestLoadEmpty(t *testing.T) {
	if cfg := loadQuiet(t, release.Sysext, t.TempDir()); cfg != (Config{}) {
		t.Errorf("empty root should yield unset config, got %+v", cfg)
	}
}

func TestLoadMainFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf", "[SysExt]\nMutable=auto\nImagePolicy=root=verity\n")
	cfg := loadQuiet(t, release.Sysext, root)
	if cfg.Mutable != "auto" || cfg.ImagePolicy != "root=verity" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// First main file found wins; lower-priority directories are not consulted.
func TestLoadMainFilePrecedence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf", "[SysExt]\nMutable=yes\nImagePolicy=usrlib\n")
	write(t, root, "usr/local/lib/systemd/sysext.conf", "[SysExt]\nMutable=import\n")
	write(t, root, "run/systemd/sysext.conf", "[SysExt]\nMutable=ephemeral\n")
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=auto\n")

	cfg := loadQuiet(t, release.Sysext, root)
	if cfg.Mutable != "auto" || cfg.ImagePolicy != "" {
		t.Errorf("cfg = %+v, want only /etc", cfg)
	}
	for _, next := range []struct{ remove, want string }{
		{"etc/systemd/sysext.conf", "ephemeral"},
		{"run/systemd/sysext.conf", "import"},
		{"usr/local/lib/systemd/sysext.conf", "yes"},
	} {
		if err := os.Remove(filepath.Join(root, next.remove)); err != nil {
			t.Fatal(err)
		}
		if cfg := loadQuiet(t, release.Sysext, root); cfg.Mutable != next.want {
			t.Errorf("after removing %s: Mutable = %q, want %q", next.remove, cfg.Mutable, next.want)
		}
	}
}

// An empty main file is read and stops the search.
func TestLoadEmptyMainFileHidesLowerOnes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf", "[SysExt]\nMutable=yes\n")
	write(t, root, "etc/systemd/sysext.conf", "")
	if cfg := loadQuiet(t, release.Sysext, root); cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset", cfg.Mutable)
	}
}

// Main files must be regular files. A /dev/null symlink resolves inside the
// root: when <root>/dev/null is missing the entry is skipped, when it is not
// a regular file (the real /dev/null) the whole configuration is ignored.
func TestLoadMainFileDevNull(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf", "[SysExt]\nMutable=yes\n")
	write(t, root, "etc/systemd/sysext.conf.d/10-a.conf", "[SysExt]\nImagePolicy=root=verity\n")
	symlink(t, "/dev/null", root, "etc/systemd/sysext.conf")
	if cfg := loadQuiet(t, release.Sysext, root); cfg.Mutable != "yes" || cfg.ImagePolicy != "root=verity" {
		t.Errorf("dangling mask: cfg = %+v, want the next main file", cfg)
	}

	devNullFIFO(t, root)
	cfg, msgs := load(t, release.Sysext, root)
	if cfg != (Config{}) {
		t.Errorf("cfg = %+v, want whole config ignored", cfg)
	}
	wantMessages(t, msgs,
		"Failed to open /etc/systemd/sysext.conf: File descriptor in bad state",
		"Failed to parse sysext config file, ignoring: File descriptor in bad state")
}

func TestLoadMainFileDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/sysext.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, msgs := load(t, release.Sysext, root)
	if cfg != (Config{}) {
		t.Errorf("cfg = %+v", cfg)
	}
	wantMessages(t, msgs,
		"Failed to open /etc/systemd/sysext.conf: Is a directory",
		"Failed to parse sysext config file, ignoring: Is a directory")
}

// Drop-ins apply after the main file and therefore override it.
func TestLoadDropinOverridesMain(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=no\nImagePolicy=main\n")
	write(t, root, "usr/lib/systemd/sysext.conf.d/10-vendor.conf", "[SysExt]\nMutable=auto\n")
	cfg := loadQuiet(t, release.Sysext, root)
	if cfg.Mutable != "auto" || cfg.ImagePolicy != "main" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// Drop-ins are sorted by file name across directories; the last one wins.
func TestLoadDropinOrdering(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf.d/90-late.conf", "[SysExt]\nMutable=ephemeral\n")
	write(t, root, "etc/systemd/sysext.conf.d/10-early.conf", "[SysExt]\nMutable=auto\n")
	write(t, root, "run/systemd/sysext.conf.d/50-mid.conf", "[SysExt]\nMutable=import\n")
	if cfg := loadQuiet(t, release.Sysext, root); cfg.Mutable != "ephemeral" {
		t.Errorf("Mutable = %q, want ephemeral", cfg.Mutable)
	}
}

// The same file name in a higher-priority directory shadows lower ones.
func TestLoadDropinShadowing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "usr/lib/systemd/sysext.conf.d/50-foo.conf", "[SysExt]\nMutable=yes\nImagePolicy=vendor\n")
	write(t, root, "etc/systemd/sysext.conf.d/50-foo.conf", "[SysExt]\nMutable=import\n")
	cfg := loadQuiet(t, release.Sysext, root)
	if cfg.Mutable != "import" || cfg.ImagePolicy != "" {
		t.Errorf("cfg = %+v, want /etc copy only", cfg)
	}
}

// A drop-in symlinked to /dev/null shadows the vendor file; inside a root
// without dev/null it is skipped, with a non-regular dev/null the whole
// configuration is ignored (systemd opens drop-ins with
// CHASE_MUST_BE_REGULAR).
func TestLoadDropinDevNull(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nImagePolicy=main\n")
	write(t, root, "usr/lib/systemd/sysext.conf.d/50-foo.conf", "[SysExt]\nMutable=yes\n")
	symlink(t, "/dev/null", root, "etc/systemd/sysext.conf.d/50-foo.conf")
	if cfg := loadQuiet(t, release.Sysext, root); cfg.Mutable != "" || cfg.ImagePolicy != "main" {
		t.Errorf("cfg = %+v, want vendor drop-in masked", cfg)
	}

	devNullFIFO(t, root)
	cfg, msgs := load(t, release.Sysext, root)
	if cfg != (Config{}) {
		t.Errorf("cfg = %+v, want whole config ignored", cfg)
	}
	wantMessages(t, msgs,
		"Failed to open etc/systemd/sysext.conf.d/50-foo.conf: File descriptor in bad state",
		"Failed to parse sysext config file, ignoring: File descriptor in bad state")
}

func TestLoadDropinDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=ephemeral\n")
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/sysext.conf.d/10-x.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, msgs := load(t, release.Sysext, root)
	if cfg != (Config{}) {
		t.Errorf("cfg = %+v, want whole config ignored", cfg)
	}
	wantMessages(t, msgs,
		"Failed to open etc/systemd/sysext.conf.d/10-x.conf: Is a directory",
		"Failed to parse sysext config file, ignoring: Is a directory")
}

// Only *.conf files count, hidden and backup files are skipped.
func TestLoadDropinNames(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"50-foo.conf.bak", "README", ".hidden.conf", "50-foo.conf~"} {
		write(t, root, "etc/systemd/sysext.conf.d/"+n, "[SysExt]\nMutable=yes\n")
	}
	if cfg := loadQuiet(t, release.Sysext, root); cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset", cfg.Mutable)
	}
}

// A main file that is the same inode as a drop-in is only read as the
// drop-in.
func TestLoadMainFileIsDropin(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf.d/50-a.conf", "[SysExt]\nMutable=auto\nBogus=1\n")
	symlink(t, "sysext.conf.d/50-a.conf", root, "etc/systemd/sysext.conf")
	cfg, msgs := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q", cfg.Mutable)
	}
	wantMessages(t, msgs, "etc/systemd/sysext.conf.d/50-a.conf:3: Unknown key 'Bogus' in section [SysExt], ignoring.")
}

// Only the section of the running class is applied; the other one is an
// unknown section. confext reads confext.conf.
func TestLoadSectionPerClass(t *testing.T) {
	root := t.TempDir()
	both := "[SysExt]\nMutable=auto\n[ConfExt]\nMutable=ephemeral\n"
	write(t, root, "etc/systemd/sysext.conf", both)
	write(t, root, "etc/systemd/confext.conf", both)

	cfg, msgs := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("sysext Mutable = %q, want auto", cfg.Mutable)
	}
	wantMessages(t, msgs, "/etc/systemd/sysext.conf:3: Unknown section 'ConfExt'. Ignoring.")

	cfg, msgs = load(t, release.Confext, root)
	if cfg.Mutable != "ephemeral" {
		t.Errorf("confext Mutable = %q, want ephemeral", cfg.Mutable)
	}
	wantMessages(t, msgs, "/etc/systemd/confext.conf:1: Unknown section 'SysExt'. Ignoring.")
}

func TestLoadConfextFileName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[ConfExt]\nMutable=yes\n")
	if cfg := loadQuiet(t, release.Confext, root); cfg.Mutable != "" {
		t.Errorf("Mutable = %q, want unset (confext must not read sysext.conf)", cfg.Mutable)
	}
}

func TestLoadLineDiagnostics(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", `Mutable=orphan
# comment
; also a comment
[sysext]
Mutable=lowercase-section-ignored
[X-Vendor]
Mutable=quietly-ignored
[SysExt]
# Mutable=commented
Unknown=ignored
X-Ours=fine
not an assignment
=novalue
mutable=lowercase-key
Mutable=auto
`)
	cfg, msgs := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" || cfg.ImagePolicy != "" {
		t.Errorf("cfg = %+v", cfg)
	}
	wantMessages(t, msgs,
		"/etc/systemd/sysext.conf:1: Assignment outside of section. Ignoring.",
		"/etc/systemd/sysext.conf:4: Unknown section 'sysext'. Ignoring.",
		"/etc/systemd/sysext.conf:10: Unknown key 'Unknown' in section [SysExt], ignoring.",
		"/etc/systemd/sysext.conf:12: Missing '=', ignoring line.",
		"/etc/systemd/sysext.conf:13: Missing key name before '=', ignoring line.",
		"/etc/systemd/sysext.conf:14: Unknown key 'mutable' in section [SysExt], ignoring.",
	)
}

// Invalid values are warned about and the previous value is kept.
func TestLoadInvalidMutable(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=auto\nMutable=bogus\n")
	write(t, root, "etc/systemd/sysext.conf.d/10-a.conf", "[SysExt]\nMutable=\nMutable=help\n")
	cfg, msgs := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q, want auto", cfg.Mutable)
	}
	wantMessages(t, msgs,
		"/etc/systemd/sysext.conf:3: Failed to parse Mutable=bogus, ignoring: Invalid argument",
		"etc/systemd/sysext.conf.d/10-a.conf:2: Failed to parse Mutable=, ignoring: Invalid argument",
		"etc/systemd/sysext.conf.d/10-a.conf:3: Failed to parse Mutable=help, ignoring: Invalid argument",
	)
}

func TestLoadMutableNormalized(t *testing.T) {
	for in, want := range map[string]string{
		"true": "yes", "1": "yes", "On": "yes", "0": "no", "off": "no", "N": "no",
		"auto": "auto", "import": "import", "ephemeral": "ephemeral", "ephemeral-import": "ephemeral-import",
	} {
		root := t.TempDir()
		write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable="+in+"\n")
		if cfg := loadQuiet(t, release.Sysext, root); cfg.Mutable != want {
			t.Errorf("Mutable=%s: got %q, want %q", in, cfg.Mutable, want)
		}
	}
}

// Within the applied stream the last assignment wins, and an empty
// ImagePolicy= resets it.
func TestLoadLastAssignmentWins(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf",
		"[SysExt]\nMutable=yes\nMutable=auto\nImagePolicy=p1\nImagePolicy=\n")
	cfg := loadQuiet(t, release.Sysext, root)
	if cfg.Mutable != "auto" || cfg.ImagePolicy != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadImagePolicyValidation(t *testing.T) {
	validate := func(s string) error {
		switch {
		case strings.Contains(s, "dup"):
			return fmt.Errorf("duplicate: %w", unix.ENOTUNIQ)
		case strings.Contains(s, "garbage"):
			return errors.New("bad policy")
		}
		return nil
	}

	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=ephemeral\nImagePolicy=root=verity\n")
	cfg, msgs := Load(release.Sysext, root, validate)
	if cfg.Mutable != "ephemeral" || cfg.ImagePolicy != "root=verity" || len(msgs) != 0 {
		t.Errorf("valid policy: cfg = %+v, msgs %q", cfg, msgs)
	}

	write(t, root, "etc/systemd/sysext.conf.d/10-a.conf", "[SysExt]\nImagePolicy=garbage\n")
	cfg, msgs = Load(release.Sysext, root, validate)
	if cfg != (Config{}) {
		t.Errorf("invalid policy: cfg = %+v, want whole config ignored", cfg)
	}
	wantMessages(t, msgs,
		"etc/systemd/sysext.conf.d/10-a.conf:2: Failed to parse image policy, refusing: garbage",
		"etc/systemd/sysext.conf.d/10-a.conf:2: Failed to parse file: Invalid argument",
		"Failed to parse sysext config file, ignoring: Invalid argument")

	write(t, root, "etc/systemd/sysext.conf.d/10-a.conf", "[SysExt]\nImagePolicy=root=verity:root=dup\n")
	_, msgs = Load(release.Sysext, root, validate)
	wantMessages(t, msgs,
		"etc/systemd/sysext.conf.d/10-a.conf:2: Duplicate rule in image policy, refusing: root=verity:root=dup",
		"etc/systemd/sysext.conf.d/10-a.conf:2: Failed to parse file: Name not unique on network",
		"Failed to parse sysext config file, ignoring: Name not unique on network")

	write(t, root, "etc/systemd/sysext.conf.d/10-a.conf", "[SysExt]\nImagePolicy=garbage\nImagePolicy=\n")
	if _, msgs = Load(release.Sysext, root, validate); len(msgs) != 3 {
		t.Errorf("a later reset does not rescue an invalid policy: msgs %q", msgs)
	}
}

func TestLoadFileLevelErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"unterminated section header", "[SysExt]\nMutable=ephemeral\n[SysExt\n", []string{
			"/etc/systemd/sysext.conf:3: Invalid section header '[SysExt'",
			"/etc/systemd/sysext.conf:3: Failed to parse file: Bad message",
			"Failed to parse sysext config file, ignoring: Bad message",
		}},
		{"unsafe section name", "[Sys\"Ext]\n", []string{
			"/etc/systemd/sysext.conf:1: Section header invalid '[Sys\"Ext]'",
			"/etc/systemd/sysext.conf:1: Failed to parse file: Bad message",
			"Failed to parse sysext config file, ignoring: Bad message",
		}},
		{"empty section name", "[]\n", []string{
			"/etc/systemd/sysext.conf:1: Section header invalid '[]'",
			"/etc/systemd/sysext.conf:1: Failed to parse file: Bad message",
			"Failed to parse sysext config file, ignoring: Bad message",
		}},
		{"invalid UTF-8", "[SysExt]\nMutable=ephemeral\nX-A=\xff\n", []string{
			"/etc/systemd/sysext.conf:3: String is not UTF-8 clean, ignoring assignment: X-A=�",
			"/etc/systemd/sysext.conf:3: Failed to parse file: Invalid argument",
			"Failed to parse sysext config file, ignoring: Invalid argument",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "etc/systemd/sysext.conf", tc.content)
			cfg, msgs := load(t, release.Sysext, root)
			if cfg != (Config{}) {
				t.Errorf("cfg = %+v, want whole config ignored", cfg)
			}
			wantMessages(t, msgs, tc.want...)
		})
	}
}

func TestLoadLexing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Config
	}{
		{"BOM", "\xef\xbb\xbf[SysExt]\nMutable=auto\n", Config{Mutable: "auto"}},
		{"CRLF", "[SysExt]\r\nMutable=auto\r\n", Config{Mutable: "auto"}},
		{"whitespace around key and value", "[SysExt]\n  Mutable \t=  auto  \n", Config{Mutable: "auto"}},
		{"continuation", "[SysExt]\nImagePolicy=root=verity:\\\nusr=signed\n", Config{ImagePolicy: "root=verity: usr=signed"}},
		{"continuation with leading value", "[SysExt]\nMutable=\\\nauto\n", Config{Mutable: "auto"}},
		{"comment inside continuation", "[SysExt]\nMutable=\\\n# comment\nauto\n", Config{Mutable: "auto"}},
		{"continuation at EOF", "[SysExt]\nMutable=auto\\", Config{Mutable: "auto"}},
		{"escaped backslash is no continuation", "[SysExt]\nImagePolicy=a\\\\\nMutable=auto\n", Config{Mutable: "auto", ImagePolicy: "a\\\\"}},
		{"NUL ends a line", "[SysExt]\x00Mutable=auto\x00", Config{Mutable: "auto"}},
		{"quotes are literal", "[SysExt]\nImagePolicy=\"root=verity\"\n", Config{ImagePolicy: "\"root=verity\""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "etc/systemd/sysext.conf", tc.content)
			if got := loadQuiet(t, release.Sysext, root); got != tc.want {
				t.Errorf("cfg = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoadPermissionWarnings(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/systemd/sysext.conf", "[SysExt]\nMutable=auto\n")
	if err := os.Chmod(filepath.Join(root, "etc/systemd/sysext.conf"), 0o757); err != nil {
		t.Fatal(err)
	}
	cfg, msgs := load(t, release.Sysext, root)
	if cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q", cfg.Mutable)
	}
	wantMessages(t, msgs,
		"Configuration file /etc/systemd/sysext.conf is marked executable. Please remove executable permission bits. Proceeding anyway.",
		"Configuration file /etc/systemd/sysext.conf is marked world-writable. Please remove world writability permission bits. Proceeding anyway.")
}

// Everything is resolved inside root: absolute symlinks restart at root and
// a populated tree in one root does not affect another.
func TestLoadRootRelative(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	write(t, rootA, "etc/systemd/sysext.conf", "[SysExt]\nMutable=auto\n")
	if cfg := loadQuiet(t, release.Sysext, rootB); cfg.Mutable != "" {
		t.Errorf("rootB Mutable = %q, want unset", cfg.Mutable)
	}
	if cfg := loadQuiet(t, release.Sysext, rootA); cfg.Mutable != "auto" {
		t.Errorf("rootA Mutable = %q, want auto", cfg.Mutable)
	}

	root := t.TempDir()
	write(t, root, "srv/conf/sysext.conf", "[SysExt]\nMutable=import\n")
	symlink(t, "/srv/conf/sysext.conf", root, "etc/systemd/sysext.conf")
	write(t, root, "srv/dropins/20-x.conf", "[SysExt]\nImagePolicy=root=verity\n")
	symlink(t, "/srv/dropins", root, "run/systemd/sysext.conf.d")
	cfg, msgs := load(t, release.Sysext, root)
	if cfg.Mutable != "import" || cfg.ImagePolicy != "root=verity" || len(msgs) != 0 {
		t.Errorf("absolute symlinks inside root: cfg = %+v, msgs %q", cfg, msgs)
	}

	escape := t.TempDir()
	symlink(t, "/etc/passwd", escape, "etc/systemd/sysext.conf")
	if cfg := loadQuiet(t, release.Sysext, escape); cfg != (Config{}) {
		t.Errorf("a symlink to a host-only path must not be followed out of the root: %+v", cfg)
	}

	loop := t.TempDir()
	symlink(t, "/etc/systemd/sysext.conf", loop, "etc/systemd/sysext.conf")
	cfg, msgs = load(t, release.Sysext, loop)
	if cfg != (Config{}) {
		t.Errorf("symlink loop: cfg = %+v", cfg)
	}
	wantMessages(t, msgs,
		"Failed to open /etc/systemd/sysext.conf: Too many levels of symbolic links",
		"Failed to parse sysext config file, ignoring: Too many levels of symbolic links")
}

func TestLoadDropinMessagesUseResolvedDir(t *testing.T) {
	root := t.TempDir()
	write(t, root, "srv/dropins/20-x.conf", "[SysExt]\nBogus=1\n")
	symlink(t, "/srv/dropins", root, "etc/systemd/sysext.conf.d")
	_, msgs := load(t, release.Sysext, root)
	wantMessages(t, msgs, "srv/dropins/20-x.conf:2: Unknown key 'Bogus' in section [SysExt], ignoring.")
}

func TestParseMutable(t *testing.T) {
	for in, want := range map[string]string{
		"no": "no", "yes": "yes", "auto": "auto", "import": "import", "ephemeral": "ephemeral",
		"ephemeral-import": "ephemeral-import", "1": "yes", "TRUE": "yes", "f": "no",
	} {
		if got, err := ParseMutable(in); err != nil || got != want {
			t.Errorf("ParseMutable(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "help", "Auto", "banana", " auto"} {
		if got, err := ParseMutable(in); err == nil {
			t.Errorf("ParseMutable(%q) = %q, want error", in, got)
		}
	}
}

func TestReadLine(t *testing.T) {
	split := func(s string) []string {
		var out []string
		data := []byte(s)
		for len(data) > 0 {
			line, rest, err := readLine(data)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(line))
			data = rest
		}
		return out
	}
	tests := []struct {
		in   string
		want []string
	}{
		{"a\nb", []string{"a", "b"}},
		{"a\n", []string{"a"}},
		{"a\r\nb", []string{"a", "b"}},
		{"a\n\rb", []string{"a", "b"}},
		{"a\n\nb", []string{"a", "", "b"}},
		{"a\r\rb", []string{"a", "", "b"}},
		{"a\r\n\r\nb", []string{"a", "", "b"}},
		{"a\x00b", []string{"a", "b"}},
		{"a\n\x00b", []string{"a", "b"}},
		{"a\r\n\x00b", []string{"a", "b"}},
		{"a\x00\x00b", []string{"a", "", "b"}},
		{"\n", []string{""}},
	}
	for _, tc := range tests {
		if got := split(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("readLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, _, err := readLine([]byte(strings.Repeat("x", longLineMax+1))); !errors.Is(err, unix.ENOBUFS) {
		t.Errorf("long line: err = %v, want ENOBUFS", err)
	}
}

func TestLoadMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	cfg, msgs := load(t, release.Sysext, root)
	if cfg != (Config{}) {
		t.Errorf("cfg = %+v", cfg)
	}
	wantMessages(t, msgs,
		"Failed to open root directory '"+root+"': No such file or directory",
		"Failed to parse sysext config file, ignoring: No such file or directory")
}

func TestLoadRelativeRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "r/etc/systemd/sysext.conf.d/10-a.conf", "[SysExt]\nMutable=auto\n")
	t.Chdir(dir)
	if cfg := loadQuiet(t, release.Sysext, "r"); cfg.Mutable != "auto" {
		t.Errorf("Mutable = %q, want auto", cfg.Mutable)
	}
}

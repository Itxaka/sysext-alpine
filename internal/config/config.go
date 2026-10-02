// Package config loads sysext.conf(5) / confext.conf(5) the way systemd-sysext
// 262 does (parse_config_file() in src/sysext/sysext.c on top of
// config_parse_standard_file_with_dropins_full() in src/shared/conf-parser.c):
// the first main file found in /etc, /run, /usr/local/lib, /usr/lib, then
// every *.conf drop-in sorted by file name, all resolved inside the root.
// Invalid values are warned about and ignored; file-level errors discard the
// whole configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// Config holds the recognized sysext.conf(5) options. An empty string means
// no configuration file set the option.
type Config struct {
	// Mutable is the normalized mode: no, yes, auto, import, ephemeral or
	// ephemeral-import.
	Mutable string
	// ImagePolicy is the image policy string as written.
	ImagePolicy string
}

// confDirs is systemd's CONF_PATHS_STRV(""), in priority order.
var confDirs = []string{"/etc/", "/run/", "/usr/local/lib/", "/usr/lib/"}

// longLineMax is systemd's LONG_LINE_MAX.
const longLineMax = 1 << 20

const utf8BOM = "\xef\xbb\xbf"

func shortIdentifier(class release.Class) string {
	if class == release.Confext {
		return "confext"
	}
	return "sysext"
}

func sectionName(class release.Class) string {
	if class == release.Confext {
		return "ConfExt"
	}
	return "SysExt"
}

// ParseMutable parses a Mutable= value like systemd's
// mutable_mode_from_string(): boolean spellings map to "yes"/"no", the
// named modes pass through.
func ParseMutable(s string) (string, error) {
	if b, err := release.ParseBoolean(s); err == nil {
		if b {
			return "yes", nil
		}
		return "no", nil
	}
	switch s {
	case "no", "yes", "auto", "import", "ephemeral", "ephemeral-import":
		return s, nil
	}
	return "", fmt.Errorf("invalid mutable mode %q: %w", s, unix.EINVAL)
}

// Load reads the configuration for class relative to root ("" = the real
// root). It never fails: the returned messages are what systemd logs while
// parsing (unknown sections and keys, invalid values, permission problems),
// in order. When a file cannot be opened or parsed as a whole, the entire
// configuration is discarded, a zero Config is returned and the last message
// is "Failed to parse <class> config file, ignoring: <error>".
//
// validatePolicy, when non-nil, validates each non-empty ImagePolicy=
// assignment the way image_policy_from_string(graceful=true) does; a
// rejected policy is a file-level error, as in systemd. Errors wrapping
// ENOTUNIQ, EBADSLT or EBADRQC select systemd's specific messages.
func Load(class release.Class, root string, validatePolicy func(string) error) (Config, []string) {
	l := &loader{
		section:        sectionName(class),
		validatePolicy: validatePolicy,
	}
	err := l.openRoot(root)
	if err == nil {
		err = l.load("systemd/" + shortIdentifier(class) + ".conf")
	}
	if err != nil {
		l.warnf("Failed to parse %s config file, ignoring: %s", shortIdentifier(class), strerror(err))
		return Config{}, l.messages
	}
	return l.cfg, l.messages
}

type loader struct {
	root           string
	section        string
	validatePolicy func(string) error
	cfg            Config
	messages       []string
}

// openRoot sets the root all paths are resolved in; a root other than "/"
// must be an existing directory.
func (l *loader) openRoot(root string) error {
	if root == "" || root == "/" {
		l.root = "/"
		return nil
	}
	abs, err := filepath.Abs(root)
	if err == nil {
		var fi fs.FileInfo
		if fi, err = os.Stat(abs); err == nil && !fi.IsDir() {
			err = unix.ENOTDIR
		}
	}
	if err != nil {
		l.warnf("Failed to open root directory '%s': %s", root, strerror(err))
		return err
	}
	l.root = abs
	return nil
}

func (l *loader) warnf(format string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}

type openedFile struct {
	name string // as systemd names it in messages
	data []byte
	info fs.FileInfo
}

func (l *loader) load(mainFile string) error {
	var dropins []openedFile
	for _, name := range l.listDropins(mainFile + ".d") {
		f, err := l.open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			l.warnf("Failed to open %s: %s", name, strerror(err))
			return err
		}
		dropins = append(dropins, f)
	}

	for _, dir := range confDirs {
		name := dir + mainFile
		f, err := l.open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			l.warnf("Failed to open %s: %s", name, strerror(err))
			return err
		}
		if !sameAsAny(f.info, dropins) {
			if err := l.parse(f); err != nil {
				return err
			}
		}
		break
	}

	for _, f := range dropins {
		if err := l.parse(f); err != nil {
			return err
		}
	}
	return nil
}

// listDropins is conf_files_list_dropins(): *.conf entries of every
// <confdir>/<dirname>, skipping hidden and backup files, a file name in a
// higher-priority directory shadowing the same name in later ones, sorted by
// file name. Entries are named relative to the root, without leading slash.
func (l *loader) listDropins(dirname string) []string {
	byName := map[string]string{}
	for _, dir := range confDirs {
		dirPath := dir + "/" + dirname
		resolved, err := fsutil.Chase(l.root, dirPath, 0)
		var entries []os.DirEntry
		if err == nil {
			entries, err = os.ReadDir(resolved)
		}
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				l.warnf("Failed to chase and open directory '%s', ignoring: %s", dirPath, strerror(err))
			}
			continue
		}
		rel := relativeToRoot(l.root, resolved)
		for _, e := range entries {
			n := e.Name()
			if release.HiddenOrBackupFile(n) || !strings.HasSuffix(n, ".conf") {
				continue
			}
			if _, shadowed := byName[n]; shadowed {
				continue
			}
			p := filepath.Join(rel, n)
			if _, err := fsutil.Chase(l.root, p, fsutil.ChaseNonexistent); err != nil {
				l.warnf("Failed to chase '%s/%s': %s", dirPath, n, strerror(err))
				continue
			}
			byName[n] = strings.TrimPrefix(p, "/")
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	files := make([]string, len(names))
	for i, n := range names {
		files[i] = byName[n]
	}
	return files
}

// open resolves name inside the root and reads it; it must be a regular
// file (systemd: CHASE_MUST_BE_REGULAR).
func (l *loader) open(name string) (openedFile, error) {
	resolved, err := fsutil.Chase(l.root, name, 0)
	if err != nil {
		return openedFile{}, err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return openedFile{}, err
	}
	switch {
	case fi.IsDir():
		return openedFile{}, unix.EISDIR
	case !fi.Mode().IsRegular():
		return openedFile{}, unix.EBADFD
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return openedFile{}, err
	}
	return openedFile{name: name, data: data, info: fi}, nil
}

func sameAsAny(fi fs.FileInfo, files []openedFile) bool {
	for _, f := range files {
		if os.SameFile(fi, f.info) {
			return true
		}
	}
	return false
}

// parse is config_parse(): it walks the logical lines of one file, joining
// backslash continuations, and applies [SysExt]/[ConfExt] assignments.
func (l *loader) parse(f openedFile) error {
	if f.info.Mode()&0o111 != 0 {
		l.warnf("Configuration file %s is marked executable. Please remove executable permission bits. Proceeding anyway.", f.name)
	}
	if f.info.Mode()&0o002 != 0 {
		l.warnf("Configuration file %s is marked world-writable. Please remove world writability permission bits. Proceeding anyway.", f.name)
	}

	p := &fileParser{loader: l, name: f.name}
	data := f.data
	line := 0
	bomSeen := false
	var continuation []byte
	haveContinuation := false
	for len(data) > 0 {
		buf, rest, err := readLine(data)
		if err != nil {
			l.warnf("%s:%d: Line too long", f.name, line)
			return err
		}
		data = rest
		line++

		if t := strings.TrimLeft(string(buf), " \t\n\r"); t != "" && (t[0] == '#' || t[0] == ';') {
			continue
		}

		cur := buf
		if !bomSeen {
			if s, ok := strings.CutPrefix(string(buf), utf8BOM); ok {
				cur = []byte(s)
				bomSeen = true
			}
		}

		if haveContinuation {
			if len(continuation)+len(cur) > longLineMax {
				l.warnf("%s:%d: Continuation line too long", f.name, line)
				return unix.ENOBUFS
			}
			continuation = append(continuation, cur...)
			cur = continuation
		}

		if endsWithEscape(cur) {
			cur[len(cur)-1] = ' '
			if !haveContinuation {
				continuation = append([]byte(nil), cur...)
				haveContinuation = true
			} else {
				continuation = cur
			}
			continue
		}

		if err := p.parseLine(line, string(cur)); err != nil {
			l.warnf("%s:%d: Failed to parse file: %s", f.name, line, strerror(err))
			return err
		}
		continuation, haveContinuation = nil, false
	}

	if haveContinuation {
		line++
		if err := p.parseLine(line, string(continuation)); err != nil {
			l.warnf("%s:%d: Failed to parse file: %s", f.name, line, strerror(err))
			return err
		}
	}
	return nil
}

// endsWithEscape reports whether s ends in an unescaped backslash.
func endsWithEscape(s []byte) bool {
	escaped := false
	for _, c := range s {
		if escaped {
			escaped = false
		} else if c == '\\' {
			escaped = true
		}
	}
	return escaped
}

// readLine is systemd's read_line(): it splits off one line, accepting
// "\n", "\r", "\0" and their combinations "\r\n", "\n\r", "\n\0", "\r\0",
// "\r\n\0", "\n\r\0" as one line ending.
func readLine(data []byte) (line, rest []byte, err error) {
	const (
		eolZero = 1 << iota
		eolTen
		eolThirteen
	)
	prev := 0
	i := 0
	n := 0
	for ; i < len(data); i++ {
		c := data[i]
		eol := 0
		switch c {
		case '\n':
			eol = eolTen
		case '\r':
			eol = eolThirteen
		case 0:
			eol = eolZero
		}
		if prev&eolZero != 0 || (eol == 0 && prev != 0) || (eol != 0 && prev&eol != 0) {
			break
		}
		if eol != 0 {
			prev |= eol
			continue
		}
		if n >= longLineMax {
			return nil, nil, unix.ENOBUFS
		}
		n++
	}
	line = make([]byte, 0, n)
	for _, c := range data[:i] {
		if c != '\n' && c != '\r' && c != 0 {
			line = append(line, c)
		}
	}
	return line, data[i:], nil
}

type fileParser struct {
	loader         *loader
	name           string
	section        string
	inSection      bool
	sectionIgnored bool
}

func (p *fileParser) warnf(line int, format string, args ...any) {
	p.loader.warnf("%s:%d: %s", p.name, line, fmt.Sprintf(format, args...))
}

// parseLine is parse_line() for one logical line.
func (p *fileParser) parseLine(line int, l string) error {
	l = strings.Trim(l, " \t\n\r")
	if l == "" {
		return nil
	}
	if !release.ValidUTF8(l) {
		p.warnf(line, "String is not UTF-8 clean, ignoring assignment: %s", escapeInvalidUTF8(l))
		return unix.EINVAL
	}

	if l[0] == '[' {
		if l[len(l)-1] != ']' {
			p.warnf(line, "Invalid section header '%s'", l)
			return unix.EBADMSG
		}
		n := l[1 : len(l)-1]
		if !sectionNameIsSafe(n) {
			p.warnf(line, "Section header invalid '%s'", l)
			return unix.EBADMSG
		}
		if n != p.loader.section {
			if !strings.HasPrefix(n, "X-") {
				p.warnf(line, "Unknown section '%s'. Ignoring.", n)
			}
			p.section, p.inSection, p.sectionIgnored = "", false, true
		} else {
			p.section, p.inSection, p.sectionIgnored = n, true, false
		}
		return nil
	}

	if !p.inSection {
		if !p.sectionIgnored {
			p.warnf(line, "Assignment outside of section. Ignoring.")
		}
		return nil
	}

	eq := strings.IndexByte(l, '=')
	if eq < 0 {
		p.warnf(line, "Missing '=', ignoring line.")
		return nil
	}
	if eq == 0 {
		p.warnf(line, "Missing key name before '=', ignoring line.")
		return nil
	}
	return p.assign(line, strings.Trim(l[:eq], " \t\n\r"), strings.Trim(l[eq+1:], " \t\n\r"))
}

func (p *fileParser) assign(line int, key, value string) error {
	cfg := &p.loader.cfg
	switch key {
	case "Mutable":
		m, err := ParseMutable(value)
		if err != nil {
			p.warnf(line, "Failed to parse Mutable=%s, ignoring: Invalid argument", value)
			return nil
		}
		cfg.Mutable = m
	case "ImagePolicy":
		if value == "" {
			cfg.ImagePolicy = ""
			return nil
		}
		if v := p.loader.validatePolicy; v != nil {
			if err := v(value); err != nil {
				errno := unix.EINVAL
				what := "Failed to parse image policy"
				switch {
				case errors.Is(err, unix.ENOTUNIQ):
					errno, what = unix.ENOTUNIQ, "Duplicate rule in image policy"
				case errors.Is(err, unix.EBADSLT):
					errno, what = unix.EBADSLT, "Unknown partition type in image policy"
				case errors.Is(err, unix.EBADRQC):
					errno, what = unix.EBADRQC, "Unknown partition policy flag in image policy"
				}
				p.warnf(line, "%s, refusing: %s", what, value)
				return errno
			}
		}
		cfg.ImagePolicy = value
	default:
		if !strings.HasPrefix(key, "X-") {
			p.warnf(line, "Unknown key '%s' in section [%s], ignoring.", key, p.section)
		}
	}
	return nil
}

// sectionNameIsSafe is string_is_safe(s, 0): non-empty, valid UTF-8, no
// control characters, backslashes, quotes or glob characters.
func sectionNameIsSafe(s string) bool {
	if s == "" || !release.ValidUTF8(s) {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c < ' ' || c == 0x7f || strings.IndexByte("\\\"'*?[", c) >= 0 {
			return false
		}
	}
	return true
}

// escapeInvalidUTF8 is utf8_escape_invalid(): every byte that does not
// start a valid character becomes U+FFFD.
func escapeInvalidUTF8(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		_, size := utf8.DecodeRuneInString(s)
		if c := s[:size]; release.ValidUTF8(c) {
			b.WriteString(c)
			s = s[size:]
			continue
		}
		b.WriteRune(utf8.RuneError)
		s = s[1:]
	}
	return b.String()
}

// strerror renders err like glibc's strerror() for errno values.
func strerror(err error) string {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		s := errno.Error()
		if s != "" {
			return strings.ToUpper(s[:1]) + s[1:]
		}
	}
	return err.Error()
}

// relativeToRoot returns the root-prefixed path p as a path inside the
// absolute root.
func relativeToRoot(root, p string) string {
	if root == "/" {
		return p
	}
	if rel, ok := strings.CutPrefix(p, root); ok && (rel == "" || rel[0] == '/') {
		if rel == "" {
			return "/"
		}
		return rel
	}
	return p
}

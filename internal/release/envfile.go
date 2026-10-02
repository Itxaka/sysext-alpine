package release

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/errno"
)

// maxEnvFileSize bounds how much of a release file is read, like systemd's
// READ_FULL_BYTES_MAX.
const maxEnvFileSize = 64 << 20

type envState int

const (
	envPreKey envState = iota
	envKey
	envPreValue
	envValue
	envValueEscape
	envSingleQuote
	envDoubleQuote
	envDoubleQuoteEscape
	envComment
	envCommentEscape
)

func isEnvWhitespace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isEnvNewline(c byte) bool    { return c == '\n' || c == '\r' }
func isEnvComment(c byte) bool    { return c == '#' || c == ';' }

// needsShellEscape reports whether c is one of systemd's SHELL_NEED_ESCAPE
// characters, which a backslash escapes inside double quotes.
func needsShellEscape(c byte) bool { return c == '"' || c == '\\' || c == '`' || c == '$' }

// Parse parses os-release(5) style KEY=VALUE content exactly like systemd's
// parse_env_file_internal(): whitespace around keys and values is dropped,
// '#' and ';' start comments, single and double quotes may span lines and
// concatenate with adjacent segments, a backslash escapes the next character
// (a backslash-newline is a line continuation) and input ends at the first
// NUL byte. The last assignment of a key wins. A key or value that is not
// valid UTF-8 fails the whole file.
func Parse(content []byte) (Fields, error) {
	if i := bytes.IndexByte(content, 0); i >= 0 {
		content = content[:i]
	}

	f := Fields{}
	var key, value []byte
	lastKeyWS, lastValueWS := -1, -1
	state := envPreKey

	push := func(chompValue bool) error {
		k := key
		if lastKeyWS >= 0 {
			k = k[:lastKeyWS]
		}
		v := value
		if chompValue && lastValueWS >= 0 {
			v = v[:lastValueWS]
		}
		if !validUTF8(k) {
			return errno.New(unix.EINVAL, "invalid UTF-8 in key %q", k)
		}
		if !validUTF8(v) {
			return errno.New(unix.EINVAL, "invalid UTF-8 value for key %s", k)
		}
		f[string(k)] = string(v)
		key, value = key[:0], value[:0]
		return nil
	}

	for _, c := range content {
		switch state {
		case envPreKey:
			if isEnvComment(c) {
				state = envComment
			} else if !isEnvWhitespace(c) {
				state = envKey
				lastKeyWS = -1
				key = append(key, c)
			}

		case envKey:
			switch {
			case isEnvNewline(c):
				state = envPreKey
				key = key[:0]
			case c == '=':
				state = envPreValue
				lastValueWS = -1
			default:
				if !isEnvWhitespace(c) {
					lastKeyWS = -1
				} else if lastKeyWS < 0 {
					lastKeyWS = len(key)
				}
				key = append(key, c)
			}

		case envPreValue:
			switch {
			case isEnvNewline(c):
				state = envPreKey
				if err := push(false); err != nil {
					return nil, err
				}
			case c == '\'':
				state = envSingleQuote
			case c == '"':
				state = envDoubleQuote
			case c == '\\':
				state = envValueEscape
			case !isEnvWhitespace(c):
				state = envValue
				value = append(value, c)
			}

		case envValue:
			switch {
			case isEnvNewline(c):
				state = envPreKey
				if err := push(true); err != nil {
					return nil, err
				}
			case c == '\\':
				state = envValueEscape
				lastValueWS = -1
			default:
				if !isEnvWhitespace(c) {
					lastValueWS = -1
				} else if lastValueWS < 0 {
					lastValueWS = len(value)
				}
				value = append(value, c)
			}

		case envValueEscape:
			state = envValue
			if !isEnvNewline(c) {
				value = append(value, c)
			}

		case envSingleQuote:
			if c == '\'' {
				state = envPreValue
			} else {
				value = append(value, c)
			}

		case envDoubleQuote:
			switch c {
			case '"':
				state = envPreValue
			case '\\':
				state = envDoubleQuoteEscape
			default:
				value = append(value, c)
			}

		case envDoubleQuoteEscape:
			state = envDoubleQuote
			if needsShellEscape(c) {
				value = append(value, c)
			} else if c != '\n' {
				value = append(value, '\\', c)
			}

		case envComment:
			if c == '\\' {
				state = envCommentEscape
			} else if isEnvNewline(c) {
				state = envPreKey
			}

		case envCommentEscape:
			if isEnvNewline(c) {
				state = envPreKey
			} else {
				state = envComment
			}
		}
	}

	switch state {
	case envPreValue, envValue, envValueEscape, envSingleQuote, envDoubleQuote, envDoubleQuoteEscape:
		if err := push(state == envValue); err != nil {
			return nil, err
		}
	case envPreKey, envKey, envComment, envCommentEscape:
	}
	return f, nil
}

// ParseFile reads and parses the file at path. Only regular files are read.
func ParseFile(path string) (Fields, error) {
	content, err := readRegularFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(content)
}

func readRegularFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case fi.IsDir():
		return nil, &os.PathError{Op: "read", Path: path, Err: unix.EISDIR}
	case !fi.Mode().IsRegular():
		return nil, &os.PathError{Op: "read", Path: path, Err: unix.EBADFD}
	}
	content, err := io.ReadAll(io.LimitReader(f, maxEnvFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxEnvFileSize {
		return nil, &os.PathError{Op: "read", Path: path, Err: unix.E2BIG}
	}
	return content, nil
}

// ParseBoolean implements systemd's parse_boolean(): "1", "yes", "y",
// "true", "t" and "on" are true, "0", "no", "n", "false", "f" and "off" are
// false, compared ASCII case-insensitively and without trimming.
func ParseBoolean(s string) (bool, error) {
	switch asciiLower(s) {
	case "1", "yes", "y", "true", "t", "on":
		return true, nil
	case "0", "no", "n", "false", "f", "off":
		return false, nil
	}
	return false, errInvalidBoolean
}

var errInvalidBoolean = errors.New("invalid boolean")

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

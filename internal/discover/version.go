package discover

import (
	"cmp"
	"strings"
)

// CompareVersions is systemd's strverscmp_improved()
// (src/fundamental/string-util.c), the UAPI.10 version comparison. It
// returns <0, 0 or >0.
//
// The strings are split into segments: characters outside [a-zA-Z0-9~^.-]
// are dropped and only separate segments. A segment prefixed with '~' is
// older than anything, including the end of the string; otherwise a string
// with more segments is newer. Prefixes rank '-' < '^' < '.' < none, numeric
// segments (compared by value, leading zeros ignored) are newer than
// alphabetic ones (compared bytewise, longer is newer). For example:
//
//	122.1 < 123~rc1-1 < 123 < 123-a < 123-a.1 < 123-1 < 123-1.1 < 123^post1
//	      < 123.a-1 < 123.1-1 < 123a-1 < 124-1
func CompareVersions(a, b string) int {
	if i := strings.IndexByte(a, 0); i >= 0 {
		a = a[:i]
	}
	if i := strings.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	at := func(s string, i int) byte {
		if i < len(s) {
			return s[i]
		}
		return 0
	}

	i, j := 0, 0
	for {
		for i < len(a) && !isVersionChar(a[i]) {
			i++
		}
		for j < len(b) && !isVersionChar(b[j]) {
			j++
		}

		if at(a, i) == '~' || at(b, j) == '~' {
			if r := cmpBool(at(a, i) != '~', at(b, j) != '~'); r != 0 {
				return r
			}
			i++
			j++
		}

		if at(a, i) == 0 || at(b, j) == 0 {
			return cmp.Compare(at(a, i), at(b, j))
		}

		for _, sep := range [...]byte{'-', '^', '.'} {
			if at(a, i) == sep || at(b, j) == sep {
				if r := cmpBool(at(a, i) != sep, at(b, j) != sep); r != 0 {
					return r
				}
				i++
				j++
			}
		}

		ai, bj := i, j
		if isDigit(at(a, i)) || isDigit(at(b, j)) {
			for isDigit(at(a, ai)) {
				ai++
			}
			for isDigit(at(b, bj)) {
				bj++
			}
			if r := cmpBool(ai != i, bj != j); r != 0 {
				return r
			}
			for at(a, i) == '0' {
				i++
			}
			for at(b, j) == '0' {
				j++
			}
			if r := cmp.Compare(ai-i, bj-j); r != 0 {
				return r
			}
			if r := strings.Compare(a[i:ai], b[j:bj]); r != 0 {
				return r
			}
		} else {
			for isAlpha(at(a, ai)) {
				ai++
			}
			for isAlpha(at(b, bj)) {
				bj++
			}
			n := min(ai-i, bj-j)
			if r := strings.Compare(a[i:i+n], b[j:j+n]); r != 0 {
				return r
			}
			if r := cmp.Compare(ai-i, bj-j); r != 0 {
				return r
			}
		}
		i, j = ai, bj
	}
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isVersionChar(c byte) bool {
	return isDigit(c) || isAlpha(c) || c == '~' || c == '-' || c == '^' || c == '.'
}

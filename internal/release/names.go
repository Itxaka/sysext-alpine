package release

import (
	"strings"
	"unicode/utf8"
)

// ImageNameIsValid is systemd's image_name_is_valid(): a valid file name
// (non-empty, not "." or "..", no '/', at most 255 bytes) without control
// characters, valid UTF-8 and not starting with ".#".
func ImageNameIsValid(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 255 || strings.ContainsRune(s, '/') {
		return false
	}
	for i := range len(s) {
		if s[i] < ' ' || s[i] == 0x7f {
			return false
		}
	}
	return validUTF8(s) && !strings.HasPrefix(s, ".#")
}

// ValidUTF8 is systemd's utf8_is_valid(): well-formed UTF-8 without NUL
// bytes and without the Unicode noncharacters U+FDD0..U+FDEF, U+xxFFFE and
// U+xxFFFF.
func ValidUTF8(s string) bool {
	return validUTF8(s)
}

func validUTF8[T string | []byte](s T) bool {
	b := []byte(s)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && size <= 1:
			return false
		case r == 0:
			return false
		case r >= 0xFDD0 && r <= 0xFDEF:
			return false
		case r&0xFFFE == 0xFFFE:
			return false
		}
		b = b[size:]
	}
	return true
}

// HiddenOrBackupFile is systemd's hidden_or_backup_file(), which directory
// iteration with FOREACH_DIRENT uses to skip entries: dot files, "~" backups,
// lost+found, quota files and package manager leftovers such as .rpmnew or
// .dpkg-old.
func HiddenOrBackupFile(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
		return true
	}
	switch name {
	case "lost+found", "aquota.user", "aquota.group":
		return true
	}
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return false
	}
	switch name[dot+1:] {
	case "ignore", "rpmnew", "rpmsave", "rpmorig", "dpkg-old", "dpkg-new", "dpkg-tmp", "dpkg-dist",
		"dpkg-bak", "dpkg-backup", "dpkg-remove", "ucf-new", "ucf-old", "ucf-dist", "swp", "bak",
		"old", "new":
		return true
	}
	return false
}

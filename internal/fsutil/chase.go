// Package fsutil holds filesystem helpers shared across packages: path
// resolution confined to a root directory (systemd's chase() with
// CHASE_PREFIX_ROOT), mount point detection and /proc/self/mountinfo parsing.
package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ChaseFlags tune Chase.
type ChaseFlags uint

const (
	// ChaseNonexistent accepts a path whose trailing components do not
	// exist: resolution stops at the first missing component and the
	// remaining components are appended unresolved.
	ChaseNonexistent ChaseFlags = 1 << iota
)

const maxSymlinkHops = 40

// Chase resolves path relative to root, following symlinks the way the
// kernel would if root were "/": absolute link targets restart at root and
// ".." never climbs above it. The result is the resolved path including the
// root prefix. An empty root means "/".
func Chase(root, path string, flags ChaseFlags) (string, error) {
	if root == "" {
		root = "/"
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		root = abs
	}

	done := "/"
	todo := path
	hops := 0
	for {
		todo = strings.TrimLeft(todo, "/")
		if todo == "" {
			break
		}
		comp, rest, _ := strings.Cut(todo, "/")
		todo = rest
		switch comp {
		case ".":
			continue
		case "..":
			done = filepath.Dir(done)
			continue
		}

		next := filepath.Join(done, comp)
		full := filepath.Join(root, next)
		fi, err := os.Lstat(full)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && flags&ChaseNonexistent != 0 {
				return filepath.Join(root, filepath.Join(next, filepath.Clean("/"+rest))), nil
			}
			return "", &fs.PathError{Op: "chase", Path: full, Err: unwrapErrno(err)}
		}

		if fi.Mode()&fs.ModeSymlink != 0 {
			hops++
			if hops > maxSymlinkHops {
				return "", &fs.PathError{Op: "chase", Path: full, Err: syscall.ELOOP}
			}
			target, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				done = "/"
			}
			todo = target + "/" + rest
			continue
		}

		if rest != "" && strings.Trim(rest, "/") != "" && !fi.IsDir() {
			return "", &fs.PathError{Op: "chase", Path: full, Err: syscall.ENOTDIR}
		}
		done = next
	}
	return filepath.Join(root, done), nil
}

func unwrapErrno(err error) error {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno
	}
	return err
}

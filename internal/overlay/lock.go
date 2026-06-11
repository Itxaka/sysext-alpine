// lock.go serializes mutating overlay operations across processes, like
// systemd-sysext, which serializes merge/unmerge/refresh per class.
package overlay

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// lockFileName returns the per-class lock file basename.
func lockFileName(class release.Class) string {
	if class == release.Confext {
		return "confext.lock"
	}
	return "sysext.lock"
}

// Lock takes a blocking exclusive flock(2) on the per-class lock file
// <root>/run/systemd/<sysext|confext>.lock, creating parent directories
// (0755) and the file (0600) as needed. It blocks until any other holder
// releases the lock. The returned unlock func releases the lock and closes
// the file descriptor; the lock file itself is intentionally never deleted —
// unlinking a lock file another process may be about to flock is racy.
//
// Merge and Unmerge each take this lock internally for the duration of the
// call. Callers performing an unmerge+merge sequence (refresh) therefore get
// two separate critical sections with a brief unmerged window in between;
// this matches the behavior systemd documents for refresh.
func Lock(class release.Class, root string) (unlock func(), err error) {
	dir := filepath.Join(root, "/run/systemd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating lock dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, lockFileName(class))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

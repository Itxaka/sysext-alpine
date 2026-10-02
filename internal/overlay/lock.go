package overlay

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// Lock takes the exclusive lock serializing merge, refresh and unmerge of
// class into root across processes: a blocking flock(2) on the lock file
// next to the workspace. systemd-sysext takes no such lock; it is needed
// here because the workspace outlives the process. The lock file is never
// deleted, since unlinking a file another process may be about to lock is
// racy.
func Lock(class release.Class, root string) (unlock func(), err error) {
	r, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	return lock(class, r)
}

func lock(class release.Class, root string) (func(), error) {
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", runtimeDir, err)
	}
	path := workspaceFor(class, root) + ".lock"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
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

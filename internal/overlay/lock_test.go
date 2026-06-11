package overlay

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itxaka/sysext-alpine/internal/release"
)

func TestLockPaths(t *testing.T) {
	root := t.TempDir()

	unlockS, err := Lock(release.Sysext, root)
	if err != nil {
		t.Fatalf("Lock(sysext): %v", err)
	}
	defer unlockS()
	unlockC, err := Lock(release.Confext, root)
	if err != nil {
		t.Fatalf("Lock(confext): %v", err)
	}
	defer unlockC()

	for _, name := range []string{"sysext.lock", "confext.lock"} {
		p := filepath.Join(root, "run/systemd", name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("lock file %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 0600", p, got)
		}
	}
}

func TestLockBlocksConcurrentHolder(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "run/systemd", "sysext.lock")

	unlock, err := Lock(release.Sysext, root)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	errCh := make(chan error, 1)
	acquired := make(chan struct{})
	go func() {
		u, err := Lock(release.Sysext, root)
		if err != nil {
			errCh <- err
			return
		}
		close(acquired)
		u()
		errCh <- nil
	}()

	// The second Lock must block while the first is held.
	select {
	case <-acquired:
		t.Fatal("second Lock acquired while first was held")
	case err := <-errCh:
		t.Fatalf("second Lock failed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	unlock()

	// After release it must go through promptly.
	select {
	case <-acquired:
	case err := <-errCh:
		t.Fatalf("second Lock failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("second Lock still blocked after release")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("second unlock path: %v", err)
	}

	// The lock file must survive unlock — deleting it would be racy.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file removed by unlock: %v", err)
	}
}

func TestLockSequentialReacquire(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		unlock, err := Lock(release.Confext, root)
		if err != nil {
			t.Fatalf("Lock round %d: %v", i, err)
		}
		unlock()
	}
}

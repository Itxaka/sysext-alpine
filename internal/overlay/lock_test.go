package overlay

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// withRuntimeDir points the workspaces and locks at a temporary directory.
func withRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "systemd")
	old := runtimeDir
	runtimeDir = dir
	t.Cleanup(func() { runtimeDir = old })
	return dir
}

func TestLockPaths(t *testing.T) {
	dir := withRuntimeDir(t)
	root := t.TempDir()

	for _, c := range []struct {
		class release.Class
		root  string
		want  string
	}{
		{release.Sysext, "", "sysext.lock"},
		{release.Confext, "/", "confext.lock"},
		{release.Sysext, root, filepath.Base(workspaceFor(release.Sysext, root)) + ".lock"},
	} {
		unlock, err := Lock(c.class, c.root)
		if err != nil {
			t.Fatalf("Lock(%v, %q): %v", c.class, c.root, err)
		}
		fi, err := os.Stat(filepath.Join(dir, c.want))
		if err != nil {
			t.Errorf("lock file %s: %v", c.want, err)
		} else if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 0600", c.want, got)
		}
		unlock()
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("Lock wrote into the root: %v", entries)
	}
}

func TestLockBlocksConcurrentHolder(t *testing.T) {
	withRuntimeDir(t)
	unlock, err := Lock(release.Sysext, "")
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	errCh := make(chan error, 1)
	acquired := make(chan struct{})
	go func() {
		u, err := Lock(release.Sysext, "")
		if err != nil {
			errCh <- err
			return
		}
		close(acquired)
		u()
		errCh <- nil
	}()

	select {
	case <-acquired:
		t.Fatal("second Lock acquired while first was held")
	case err := <-errCh:
		t.Fatalf("second Lock failed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	unlock()

	// On success the goroutine both closes acquired and sends nil to
	// errCh; select picks randomly among ready channels, so only acquired
	// is waited for here.
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		select {
		case err := <-errCh:
			t.Fatalf("second Lock failed: %v", err)
		default:
			t.Fatal("second Lock still blocked after release")
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("second unlock path: %v", err)
	}
}

func TestLockClassesIndependent(t *testing.T) {
	withRuntimeDir(t)
	unlock, err := Lock(release.Sysext, "")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := make(chan error, 1)
	go func() {
		u, err := Lock(release.Confext, "")
		if err == nil {
			u()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("confext lock blocked by the sysext lock")
	}
}

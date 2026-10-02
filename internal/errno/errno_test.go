package errno

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNew(t *testing.T) {
	err := New(unix.EINVAL, "Mutable directory '%s' has mode %04o", "/x", 0o700)
	if err.Error() != "Mutable directory '/x' has mode 0700" {
		t.Errorf("message = %q", err.Error())
	}
	wrapped := fmt.Errorf("context: %w", err)
	if !errors.Is(wrapped, unix.EINVAL) || errors.Is(wrapped, unix.ENOENT) {
		t.Errorf("errors.Is does not see the errno")
	}
	if e, ok := errors.AsType[unix.Errno](wrapped); !ok || e != unix.EINVAL {
		t.Errorf("errors.AsType = %v, %v", e, ok)
	}
}

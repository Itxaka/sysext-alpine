// Package errno attaches errno values to errors the way systemd does.
package errno

import (
	"fmt"

	"golang.org/x/sys/unix"
)

type synthetic struct {
	msg   string
	errno unix.Errno
}

func (e *synthetic) Error() string { return e.msg }
func (e *synthetic) Unwrap() error { return e.errno }

// New returns an error whose message is complete in itself and which
// matches errno, like systemd's log_error_errno(SYNTHETIC_ERRNO(errno), ...):
// the errno classifies the error, it is not part of the message.
func New(errno unix.Errno, format string, args ...any) error {
	return &synthetic{msg: fmt.Sprintf(format, args...), errno: errno}
}

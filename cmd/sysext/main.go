// sysext is a standalone reimplementation of systemd-sysext/-confext for
// systems without systemd (Alpine Linux). Behaves as confext when invoked
// through an argv[0] containing "confext" or with --confext.
//
// Usage: sysext [OPTIONS...] [status|merge|unmerge|refresh|list]
//
// Implemented per docs/SPEC.md §5. Stdlib only — no external dependencies.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "sysext: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches the CLI. Flags (SPEC §5): --root=, --force, --noexec=BOOL,
// --json=short|pretty|off, --no-reload, --always-refresh=yes|no, --confext,
// -h/--help, --version. Default verb: status.
func run(args []string) error {
	panic("unimplemented")
}

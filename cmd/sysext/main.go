// Command sysext is a reimplementation of systemd-sysext and systemd-confext
// 262 for systems without systemd, such as Alpine Linux with OpenRC. It
// operates on configuration extensions when invoked through a name
// containing "confext" or with --confext.
package main

import (
	"errors"
	"io"
	"os"

	"github.com/itxaka/sysext-alpine/internal/service"
)

// errLogged is returned for failures whose message was logged already.
var errLogged = errors.New("failure already logged")

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run executes the command line and returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	log := newLogger(stderr)
	c := &cli{stdout: stdout, log: log, rc: &service.OpenRC{Log: log}}
	if err := c.execute(args); err != nil {
		if !errors.Is(err, errLogged) {
			log.Errorf("%s", errorText(err))
		}
		return 1
	}
	return 0
}

func (c *cli) execute(args []string) error {
	done, err := c.parseArgs(args)
	if err != nil || done {
		return err
	}
	if c.disabledByCmdline() {
		return nil
	}
	return c.dispatch()
}

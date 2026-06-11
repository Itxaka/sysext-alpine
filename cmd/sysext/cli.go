// CLI argument parsing and verb dispatch for sysext/confext (SPEC §5).
//
// Flag parsing is hand-rolled (GNU getopt_long style) so that
// --flag=value and --flag value both work and error messages can mimic
// systemd/getopt output exactly.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/image"
	"github.com/itxaka/sysext-alpine/internal/overlay"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// version is the build version, injected via -ldflags "-X main.version=...".
var version = "0.1.0"

// JSON output modes (--json=).
const (
	jsonOff    = "off"
	jsonShort  = "short"
	jsonPretty = "pretty"
)

// config is the fully parsed command line.
type config struct {
	progName      string
	class         release.Class
	root          string // --root=
	force         bool   // --force
	noExec        bool   // --noexec= (default true; only meaningful for confext)
	jsonMode      string // --json=short|pretty|off
	noReload      bool   // --no-reload (accepted; reload is a no-op on Alpine)
	alwaysRefresh bool   // --always-refresh=yes|no
	noLegend      bool   // --no-legend
	showHelp      bool   // -h/--help
	showVersion   bool   // --version
	verb          string // status|merge|unmerge|refresh|list
}

// classFromArgv0 selects confext behavior when the binary is invoked through
// a name containing "confext" (e.g. a confext or systemd-confext symlink).
func classFromArgv0(argv0 string) release.Class {
	if strings.Contains(filepath.Base(argv0), "confext") {
		return release.Confext
	}
	return release.Sysext
}

// parseBool accepts systemd parse_boolean() spellings.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "1", "yes", "y", "true", "t", "on":
		return true, nil
	case "0", "no", "n", "false", "f", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean value '%s'", s)
}

// parseArgs parses the full argv (args[0] = program name). -h/--help and
// --version short-circuit, like getopt-based systemd tools.
func parseArgs(args []string) (*config, error) {
	cfg := &config{
		progName: "sysext",
		noExec:   true,
		jsonMode: jsonOff,
		verb:     "status",
	}
	if len(args) > 0 && args[0] != "" {
		cfg.progName = filepath.Base(args[0])
		cfg.class = classFromArgv0(args[0])
	}

	var positional []string
	for i := 1; i < len(args); i++ {
		arg := args[i]

		if arg == "--" { // end of options
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-h" {
			cfg.showHelp = true
			return cfg, nil
		}
		if !strings.HasPrefix(arg, "--") {
			if strings.HasPrefix(arg, "-") && len(arg) > 1 {
				return nil, fmt.Errorf("unrecognized option '%s'", arg)
			}
			positional = append(positional, arg)
			continue
		}

		name, value, hasValue := strings.Cut(arg[2:], "=")

		var takesValue bool
		switch name {
		case "root", "noexec", "json", "always-refresh":
			takesValue = true
		case "force", "no-reload", "no-pager", "no-legend", "confext",
			"help", "version":
			takesValue = false
		default:
			return nil, fmt.Errorf("unrecognized option '--%s'", name)
		}
		if takesValue && !hasValue {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("option '--%s' requires an argument", name)
			}
			value = args[i]
		}
		if !takesValue && hasValue {
			return nil, fmt.Errorf("option '--%s' doesn't allow an argument", name)
		}

		switch name {
		case "help":
			cfg.showHelp = true
			return cfg, nil
		case "version":
			cfg.showVersion = true
			return cfg, nil
		case "root":
			cfg.root = value
		case "force":
			cfg.force = true
		case "noexec":
			b, err := parseBool(value)
			if err != nil {
				return nil, fmt.Errorf("failed to parse --noexec= argument: %w", err)
			}
			cfg.noExec = b
		case "json":
			switch value {
			case jsonShort, jsonPretty, jsonOff:
				cfg.jsonMode = value
			default:
				return nil, fmt.Errorf("unknown JSON output format '%s'", value)
			}
		case "no-reload":
			cfg.noReload = true
		case "always-refresh":
			b, err := parseBool(value)
			if err != nil {
				return nil, fmt.Errorf("failed to parse --always-refresh= argument: %w", err)
			}
			cfg.alwaysRefresh = b
		case "no-pager":
			// Accepted for compatibility; we never page output.
		case "no-legend":
			cfg.noLegend = true
		case "confext":
			cfg.class = release.Confext
		}
	}

	switch len(positional) {
	case 0:
		// default verb: status
	case 1:
		switch positional[0] {
		case "status", "merge", "unmerge", "refresh", "list":
			cfg.verb = positional[0]
		default:
			return nil, fmt.Errorf("unknown command verb '%s'", positional[0])
		}
	default:
		return nil, errors.New("too many arguments")
	}
	return cfg, nil
}

// cli bundles parsed config with output streams for testability.
type cli struct {
	cfg    *config
	stdout io.Writer
	stderr io.Writer
}

// runWith is the testable core of run().
func runWith(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseArgs(args)
	if err != nil {
		return err
	}
	if cfg.showHelp {
		printUsage(stdout, cfg)
		return nil
	}
	if cfg.showVersion {
		fmt.Fprintf(stdout, "%s %s\n", cfg.progName, version)
		return nil
	}

	c := &cli{cfg: cfg, stdout: stdout, stderr: stderr}
	switch cfg.verb {
	case "status":
		return c.cmdStatus()
	case "merge":
		return c.cmdMerge()
	case "unmerge":
		return c.cmdUnmerge()
	case "refresh":
		return c.cmdRefresh()
	case "list":
		return c.cmdList()
	}
	// Unreachable: parseArgs validates the verb.
	return fmt.Errorf("unknown command verb '%s'", cfg.verb)
}

// printUsage emits help modeled after `systemd-sysext --help`.
func printUsage(w io.Writer, cfg *config) {
	what, hier := "extension images", "/usr/ and /opt/"
	if cfg.class == release.Confext {
		what, hier = "configuration extension images", "/etc/"
	}
	fmt.Fprintf(w, `%s [OPTIONS...] COMMAND

Merge %s into %s.

Commands:
  status                   Show current merge status (default)
  merge                    Merge extensions into %s
  unmerge                  Unmerge extensions from %s
  refresh                  Unmerge and merge extensions again
  list                     List installed extensions
  -h --help                Show this help
     --version             Show package version

Options:
     --root=PATH           Operate relative to root path
     --force               Ignore version incompatibilities
     --noexec=BOOL         Whether to mount extension overlay with noexec
     --no-reload           Do not reload the service manager (no-op here)
     --always-refresh=yes|no
                           Refresh even when the merged set is unchanged
     --confext             Operate on configuration extensions (/etc/)
     --no-pager            Do not pipe output into a pager
     --no-legend           Do not show the headers and footers
     --json=pretty|short|off
                           Generate JSON output
`, cfg.progName, what, hier, hier, hier)
}

// mustBeRoot guards verbs that mount/unmount.
func mustBeRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("need to be root")
	}
	return nil
}

// noteNoReload emits the debug note for the accepted-but-no-op --no-reload.
func (c *cli) noteNoReload() {
	if c.cfg.noReload {
		fmt.Fprintln(c.stderr,
			"Debug: --no-reload specified, but service manager reload is a no-op on this system.")
	}
}

// cmdStatus implements `status` (also the default verb).
func (c *cli) cmdStatus() error {
	statuses, err := overlay.CurrentStatus(c.cfg.class, c.cfg.root)
	if err != nil {
		return err
	}
	if c.cfg.jsonMode != jsonOff {
		out, err := renderJSON(statuses, c.cfg.jsonMode)
		if err != nil {
			return err
		}
		fmt.Fprint(c.stdout, out)
		return nil
	}
	fmt.Fprint(c.stdout, formatTable(
		[]string{"HIERARCHY", "EXTENSIONS", "SINCE"},
		statusRows(statuses), c.cfg.noLegend))
	return nil
}

// cmdList implements `list`.
func (c *cli) cmdList() error {
	images, err := discover.Discover(c.cfg.class, c.cfg.root)
	if err != nil {
		return err
	}
	entries := toListEntries(images)
	if c.cfg.jsonMode != jsonOff {
		out, err := renderJSON(entries, c.cfg.jsonMode)
		if err != nil {
			return err
		}
		fmt.Fprint(c.stdout, out)
		return nil
	}
	if len(entries) == 0 {
		fmt.Fprintln(c.stdout, "No extensions found.")
		return nil
	}
	fmt.Fprint(c.stdout, formatTable(
		[]string{"NAME", "TYPE", "PATH", "TIME"},
		listRows(entries), c.cfg.noLegend))
	return nil
}

// cmdMerge implements `merge`: discover, refuse if already merged, validate
// compatibility (unless --force), then hand over to overlay.Merge.
func (c *cli) cmdMerge() error {
	if err := mustBeRoot(); err != nil {
		return err
	}
	images, err := discover.Discover(c.cfg.class, c.cfg.root)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		fmt.Fprintln(c.stdout, "No extensions found.")
		return nil
	}
	for _, h := range overlay.Hierarchies(c.cfg.class) {
		merged, err := overlay.IsMergedByUs(c.cfg.class, c.cfg.root, h)
		if err != nil {
			return err
		}
		if merged {
			return fmt.Errorf("hierarchy '%s' is already merged", h)
		}
	}
	return c.merge(images)
}

// cmdUnmerge implements `unmerge`. overlay.Unmerge is idempotent.
func (c *cli) cmdUnmerge() error {
	if err := mustBeRoot(); err != nil {
		return err
	}
	if err := overlay.Unmerge(c.cfg.class, c.cfg.root); err != nil {
		return err
	}
	c.noteNoReload()
	return nil
}

// cmdRefresh implements `refresh`: no images -> plain unmerge; unchanged
// merged set (and not --always-refresh) -> skip; else unmerge + merge.
func (c *cli) cmdRefresh() error {
	if err := mustBeRoot(); err != nil {
		return err
	}
	images, err := discover.Discover(c.cfg.class, c.cfg.root)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		if err := overlay.Unmerge(c.cfg.class, c.cfg.root); err != nil {
			return err
		}
		c.noteNoReload()
		return nil
	}

	names := make([]string, 0, len(images))
	for _, img := range images {
		names = append(names, img.Name)
	}
	var mergedSets [][]string
	for _, h := range overlay.Hierarchies(c.cfg.class) {
		merged, err := overlay.MergedExtensions(c.cfg.class, c.cfg.root, h)
		if err != nil {
			return err
		}
		mergedSets = append(mergedSets, merged)
	}
	if shouldSkipRefresh(names, mergedSets, c.cfg.alwaysRefresh) {
		fmt.Fprintln(c.stdout, "Skipping refresh, extensions already merged.")
		return nil
	}

	// Unmerge first (no-op when nothing is merged), then merge fresh.
	if err := overlay.Unmerge(c.cfg.class, c.cfg.root); err != nil {
		return err
	}
	return c.merge(images)
}

// shouldSkipRefresh is the pure skip decision for `refresh`: skip when at
// least one hierarchy is merged and every merged hierarchy's recorded
// extension list matches the discovered image names exactly (in order).
// --always-refresh=yes disables skipping. mergedSets holds the per-hierarchy
// MergedExtensions results (empty/nil entries = hierarchy not merged).
func shouldSkipRefresh(discovered []string, mergedSets [][]string, alwaysRefresh bool) bool {
	if alwaysRefresh {
		return false
	}
	anyMerged := false
	for _, merged := range mergedSets {
		if len(merged) == 0 {
			continue
		}
		anyMerged = true
		if !slices.Equal(merged, discovered) {
			return false
		}
	}
	return anyMerged
}

// merge validates (unless --force) and mounts the overlays.
func (c *cli) merge(images []discover.Image) error {
	arch := release.HostArchitecture()
	if !c.cfg.force {
		if err := c.validateImages(images, arch); err != nil {
			return err
		}
	}
	err := overlay.Merge(c.cfg.class, images, overlay.MergeOptions{
		Root:   c.cfg.root,
		NoExec: c.cfg.noExec,
		Force:  c.cfg.force,
		Arch:   arch,
	})
	if err != nil {
		return err
	}
	c.noteNoReload()
	return nil
}

// validateImages checks every image's extension-release against the host
// os-release (SPEC §2). Directory images are inspected in place; raw images
// must be mounted first to expose their release file.
func (c *cli) validateImages(images []discover.Image, arch string) error {
	host, err := release.HostOSRelease(c.cfg.root)
	if err != nil {
		return fmt.Errorf("failed to read host os-release: %w", err)
	}
	for _, img := range images {
		var ext release.Fields
		switch img.Type {
		case discover.TypeRaw:
			ext, err = c.rawExtensionRelease(img, arch)
		default: // discover.TypeDirectory
			ext, err = release.FindExtensionRelease(img.Path, img.Name, c.cfg.class)
		}
		if err != nil {
			return fmt.Errorf("extension '%s': %w", img.Name, err)
		}
		if err := release.Match(host, ext, c.cfg.class, arch); err != nil {
			return fmt.Errorf("extension '%s' is not compatible with the host: %w",
				img.Name, err)
		}
	}
	return nil
}

// rawExtensionRelease mounts a raw image at a temporary mount point under
// <root>/run, reads its extension-release file and unmounts again.
//
// TODO(MVP): this means raw images are mounted twice — once here for
// validation and once inside overlay.Merge — because overlay.Merge takes
// already-validated images and exposes no post-mount validation hook.
// Acceptable cost for the MVP; revisit if overlay grows a hook.
func (c *cli) rawExtensionRelease(img discover.Image, arch string) (release.Fields, error) {
	runDir := filepath.Join(c.cfg.root, "/run")
	mountPoint, err := os.MkdirTemp(runDir, ".sysext-validate-")
	if err != nil {
		return nil, fmt.Errorf("failed to create validation mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)

	m, err := image.Mount(img, mountPoint, arch)
	if err != nil {
		return nil, fmt.Errorf("failed to mount for validation: %w", err)
	}
	defer func() {
		if err := m.Unmount(); err != nil {
			fmt.Fprintf(c.stderr, "Warning: failed to unmount '%s': %v\n", mountPoint, err)
		}
	}()
	return release.FindExtensionRelease(m.Root, img.Name, c.cfg.class)
}

// mutable.go implements the systemd-sysext --mutable= write-routing modes
// ("no", "auto", "yes", "import", "ephemeral", "ephemeral-import") per the
// systemd 260 man page MUTABILITY section. See docs/MUTABLE.md.
package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

const (
	// mutableRoutingBase is where per-hierarchy write-routing directories
	// live (relative to --root): /var/lib/extensions.mutable/usr etc.
	mutableRoutingBase = "/var/lib/extensions.mutable"

	// mutableMountOptions mirrors systemd's MUTABLE_EXTENSIONS_MOUNT_OPTIONS
	// ("redirect_dir=on,noatime,metacopy=off,index=off"), minus "noatime":
	// that one is a generic mount flag, not an overlayfs data option —
	// systemd feeds the string through libmount which splits it into
	// MS_NOATIME + fs options, while raw mount(2) data reaches overlayfs
	// verbatim and unknown options fail with EINVAL. MS_NOATIME is set as a
	// mount flag instead (see mergeHierarchy).
	mutableMountOptions = "redirect_dir=on,metacopy=off,index=off"

	// ephemeralWorkspaceDir is the workspace subdirectory holding the
	// per-hierarchy tmpfs mounts backing ephemeral upper/work dirs.
	ephemeralWorkspaceDir = "mh_workspace"
)

// normalizeMutableMode validates a --mutable= mode string and maps the empty
// string to "no". Unknown values are an error. Pure; unit-tested.
func normalizeMutableMode(mode string) (string, error) {
	switch mode {
	case "", "no":
		return "no", nil
	case "auto", "yes", "import", "ephemeral", "ephemeral-import":
		return mode, nil
	}
	return "", fmt.Errorf("invalid mutable mode %q", mode)
}

// modeUsesEphemeralUpper reports whether the mode backs the overlay upperdir
// with a fresh tmpfs private to the merge.
func modeUsesEphemeralUpper(mode string) bool {
	return mode == "ephemeral" || mode == "ephemeral-import"
}

// modeImportsRouting reports whether the mode inserts the write-routing
// directory as an extra lowerdir (directly below the meta dir).
func modeImportsRouting(mode string) bool {
	return mode == "import" || mode == "ephemeral-import"
}

// routingDir returns the write-routing directory for a hierarchy under root:
// /usr → <root>/var/lib/extensions.mutable/usr, etc.
func routingDir(root, hierarchy string) string {
	return filepath.Join(root, mutableRoutingBase, escapeHierarchy(hierarchy))
}

// workDirForUpper derives the overlayfs workdir for a resolved upperdir: a
// hidden sibling next to it named .<escaped-hierarchy>-workdir, so it always
// shares the upperdir's filesystem (an overlayfs requirement). E.g. upperdir
// /var/lib/extensions.mutable/usr → /var/lib/extensions.mutable/.usr-workdir;
// a routing symlink resolved to /usr → /.usr-workdir. Pure; unit-tested.
func workDirForUpper(hierarchy, resolvedUpper string) string {
	return filepath.Join(filepath.Dir(resolvedUpper), "."+escapeHierarchy(hierarchy)+"-workdir")
}

// buildOverlayData constructs the overlayfs mount data string. With an
// upperdir, the workdir and systemd's mutable mount options are appended;
// otherwise the result is the plain read-only "lowerdir=..." form. Paths are
// escaped per overlayfs option escaping. Pure; unit-tested.
func buildOverlayData(lower []string, upper, work string) string {
	data := "lowerdir=" + buildLowerdir(lower)
	if upper != "" {
		data += ",upperdir=" + escapeOverlayPath(upper) +
			",workdir=" + escapeOverlayPath(work) +
			"," + mutableMountOptions
	}
	return data
}

// hostLowerExcluded reports whether the host hierarchy must be left out of
// the lowerdir list because it already participates as the upperdir ("host
// hierarchy at the bottom unless serving as upperdir", e.g. a routing symlink
// /var/lib/extensions.mutable/usr → /usr), or because it is the imported
// routing dir itself (which already sits directly below the meta dir).
// Pure; unit-tested.
func hostLowerExcluded(hostDir, upperDir, importDir string) bool {
	hostDir = filepath.Clean(hostDir)
	if upperDir != "" && filepath.Clean(upperDir) == hostDir {
		return true
	}
	if importDir != "" && filepath.Clean(importDir) == hostDir {
		return true
	}
	return false
}

// buildLowerPaths assembles the lowerdir list for one hierarchy: meta dir
// (topmost), then the imported routing dir (import modes only), then the
// extension dirs newest-first, then the host hierarchy — unless includeHost
// is false (missing/empty) or the host already serves as upperdir/import dir.
// Pure; unit-tested.
func buildLowerPaths(metaDir, importDir string, extDirs []string, hostDir string, includeHost bool, upperDir string) []string {
	lower := []string{metaDir}
	if importDir != "" {
		lower = append(lower, importDir)
	}
	lower = append(lower, extDirs...)
	if includeHost && !hostLowerExcluded(hostDir, upperDir, importDir) {
		lower = append(lower, hostDir)
	}
	return lower
}

// hierMutable is the resolved mutability configuration for one hierarchy.
type hierMutable struct {
	upperDir  string // overlay upperdir; "" → read-only overlay
	workDir   string // overlay workdir; non-empty iff upperDir is
	importDir string // extra lowerdir directly below meta; "" if none
	tmpfs     string // mounted tmpfs backing ephemeral upper/work; "" if none
}

// resolveRoutingDir resolves the routing dir's symlinks and verifies the
// target is a directory. Returns "" (no error) when it does not exist or is
// not a directory.
func resolveRoutingDir(routing string) (string, error) {
	resolved, err := filepath.EvalSymlinks(routing)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("resolving write-routing dir %s: %w", routing, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return "", nil
	}
	return resolved, nil
}

// resolveHierarchyMutability computes the hierMutable config for one
// hierarchy, creating routing dirs ("yes"), workdirs, and ephemeral tmpfs
// mounts as the mode requires. Resources acquired (tmpfs mounts, workdirs)
// must be released by the caller on failure (mergeState rollback / Unmerge).
func resolveHierarchyMutability(mode, root, hierarchy, ws string) (hierMutable, error) {
	var hm hierMutable
	mode, err := normalizeMutableMode(mode)
	if err != nil {
		return hm, err
	}
	if mode == "no" {
		return hm, nil
	}

	routing := routingDir(root, hierarchy)

	if modeImportsRouting(mode) {
		resolved, err := resolveRoutingDir(routing)
		if err != nil {
			return hm, err
		}
		hm.importDir = resolved // "" when absent → nothing imported
	}

	if mode == "import" {
		return hm, nil // overlay stays read-only, no upperdir
	}

	if modeUsesEphemeralUpper(mode) {
		tdir := filepath.Join(ws, ephemeralWorkspaceDir, escapeHierarchy(hierarchy))
		if err := os.MkdirAll(tdir, 0o700); err != nil {
			return hm, fmt.Errorf("creating ephemeral workspace %s: %w", tdir, err)
		}
		if err := unix.Mount("tmpfs", tdir, "tmpfs", 0, "mode=0755"); err != nil {
			return hm, fmt.Errorf("mounting ephemeral tmpfs at %s: %w", tdir, err)
		}
		hm.tmpfs = tdir
		hm.upperDir = filepath.Join(tdir, "upper")
		hm.workDir = filepath.Join(tdir, "work")
		for _, d := range []string{hm.upperDir, hm.workDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return hm, fmt.Errorf("creating ephemeral dir %s: %w", d, err)
			}
		}
		return hm, nil
	}

	// "yes": create the routing dir when missing. "auto": only use it if it
	// (or its symlink target) already exists as a directory.
	if mode == "yes" {
		if err := os.MkdirAll(routing, 0o755); err != nil {
			return hm, fmt.Errorf("creating write-routing dir %s: %w", routing, err)
		}
	}
	resolved, err := resolveRoutingDir(routing)
	if err != nil {
		if mode == "auto" {
			return hierMutable{}, nil // skip-mutable
		}
		return hm, err
	}
	if resolved == "" {
		if mode == "auto" {
			return hierMutable{}, nil // no routing dir → hierarchy stays read-only
		}
		return hm, fmt.Errorf("write-routing dir %s is not a directory", routing)
	}

	hm.upperDir = resolved
	hm.workDir = workDirForUpper(hierarchy, resolved)
	if err := os.MkdirAll(hm.workDir, 0o755); err != nil {
		if mode == "auto" {
			return hierMutable{}, nil // skip-mutable when workdir can't be created
		}
		return hm, fmt.Errorf("creating overlay workdir %s: %w", hm.workDir, err)
	}
	return hm, nil
}

// safeWorkDirPath sanity-checks a work_dir marker value before removal:
// it must be absolute and look like one of the paths we generate (hidden
// "-workdir" sibling, or an ephemeral path inside the workspace).
func safeWorkDirPath(p, ws string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	p = filepath.Clean(p)
	return strings.HasSuffix(filepath.Base(p), "-workdir") ||
		strings.HasPrefix(p, filepath.Join(ws, ephemeralWorkspaceDir)+string(filepath.Separator))
}

// cleanupMutableLeftovers removes mutable-mode residue recorded in the
// workspace: the ephemeral tmpfs mounts under mh_workspace are unmounted, and
// the per-hierarchy `work_dir` marker paths (the hidden overlayfs workdirs)
// are removed. Best-effort and idempotent; called from Unmerge after the
// overlays themselves are unmounted (the tmpfs backs their upperdir).
func cleanupMutableLeftovers(class release.Class, ws string) {
	if entries, err := os.ReadDir(filepath.Join(ws, ephemeralWorkspaceDir)); err == nil {
		for _, e := range entries {
			_ = unmountWithRetry(filepath.Join(ws, ephemeralWorkspaceDir, e.Name()))
		}
	}
	if entries, err := os.ReadDir(filepath.Join(ws, "meta")); err == nil {
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(ws, "meta", e.Name(), MarkerDirName(class), "work_dir"))
			if err != nil {
				continue
			}
			p := strings.TrimSpace(string(data))
			if safeWorkDirPath(p, ws) {
				_ = os.RemoveAll(p)
			}
		}
	}
}

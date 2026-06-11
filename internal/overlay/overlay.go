// Package overlay assembles, mounts and dismantles the merged overlayfs
// hierarchies and the /run/systemd/{sysext,confext} workspace, mirroring
// systemd's runtime layout per docs/SPEC.md §4-5.
package overlay

import (
	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// Hierarchies returns the merge targets for the class:
// Sysext: ["/usr", "/opt"]; Confext: ["/etc"].
func Hierarchies(class release.Class) []string {
	panic("unimplemented")
}

// Workspace returns the runtime workspace dir for the class under root:
// /run/systemd/sysext or /run/systemd/confext. Subdirs used:
// extensions/<name>, meta/<escaped hierarchy>, overlay/<escaped hierarchy>.
func Workspace(class release.Class, root string) string {
	panic("unimplemented")
}

// MarkerDirName returns ".systemd-sysext" or ".systemd-confext".
func MarkerDirName(class release.Class) string {
	panic("unimplemented")
}

// Status describes one hierarchy's merge state for `status` output.
type Status struct {
	Hierarchy  string   `json:"hierarchy"`
	Merged     bool     `json:"merged"`
	Extensions []string `json:"extensions"` // names, merge order; nil if unmerged
	Since      int64    `json:"since"`      // unix mtime of marker dir; 0 if unmerged
}

// MergeOptions tunes Merge behavior.
type MergeOptions struct {
	Root   string // --root; "" = real root
	NoExec bool   // confext only; default true (apply MS_NOEXEC)
	Force  bool   // informational; version checks happen in caller
	Arch   string // host architecture (release.HostArchitecture())
}

// Merge mounts the merged overlays for the given already-validated images.
// Steps per SPEC §4:
//  1. create workspace dirs (0700)
//  2. mount every image under workspace/extensions/<name> (image.Mount)
//  3. per hierarchy: build lowerdir = meta : images reverse-sorted : host,
//     skipping images lacking the hierarchy and a missing/empty host dir
//  4. write marker meta dir (extensions list + origin JSON)
//  5. mount overlay at workspace/overlay/<h> with class mount flags,
//     stat it, write `dev` marker, then MS_MOVE onto the hierarchy
//
// Fails (ErrAlreadyMerged) if any target hierarchy is already merged by us.
// On error, everything mounted so far is rolled back.
func Merge(class release.Class, images []discover.Image, opts MergeOptions) error {
	panic("unimplemented")
}

// ErrAlreadyMerged is returned by Merge when a hierarchy is already merged.
var ErrAlreadyMerged error

// Unmerge detaches all merged hierarchies for the class: for each hierarchy
// merged by us, unmount the overlay (MNT_DETACH fallback), then unmount image
// mounts and detach loop devices, then remove workspace dirs. Idempotent —
// returns nil when nothing is merged.
func Unmerge(class release.Class, root string) error {
	panic("unimplemented")
}

// IsMergedByUs reports whether hierarchy (relative to root) is currently
// overmounted by our overlay: it is a mount point, carries
// <hierarchy>/<marker>/dev, and that dev equals stat(hierarchy).st_dev.
func IsMergedByUs(class release.Class, root, hierarchy string) (bool, error) {
	panic("unimplemented")
}

// CurrentStatus reads the per-hierarchy merge state from the markers.
func CurrentStatus(class release.Class, root string) ([]Status, error) {
	panic("unimplemented")
}

// MergedExtensions returns the extension names recorded in the marker of a
// merged hierarchy (empty if unmerged). Used by refresh change detection.
func MergedExtensions(class release.Class, root, hierarchy string) ([]string, error) {
	panic("unimplemented")
}

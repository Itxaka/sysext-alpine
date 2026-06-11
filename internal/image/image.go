// Package image provides access to extension image content: plain
// directories and raw disk images (bare-filesystem or GPT-partitioned),
// per docs/SPEC.md §3.
//
// Raw images are attached to a loop device (LOOP_CTL_GET_FREE +
// LOOP_CONFIGURE, read-only, partition scanning enabled for GPT) and
// mounted read-only at the supplied mount point.
package image

import (
	"github.com/itxaka/sysext-alpine/internal/discover"
)

// FSType is a detected filesystem or container format.
type FSType string

const (
	FSSquashfs FSType = "squashfs"
	FSErofs    FSType = "erofs"
	FSExt4     FSType = "ext4"
	FSGPT      FSType = "gpt" // GPT-partitioned DDI
	FSUnknown  FSType = "unknown"
)

// Mounted is an attached image whose root tree is accessible at Root.
type Mounted struct {
	// Root is the directory exposing the image's filesystem tree
	// (for TypeDirectory images this is the image path itself).
	Root string
	// LoopDevice is the backing loop device path ("" for directories).
	LoopDevice string
	// Partition is the mounted partition device for GPT images ("" otherwise).
	Partition string
	// FS is the detected payload filesystem.
	FS FSType
}

// Detect probes the file at path and returns the format: checks GPT header
// ("EFI PART" at LBA 1 for 512 and 4096 byte sectors), squashfs magic
// ("hsqs" at 0), erofs magic (0xE0F5E1E2 LE at 1024), ext4 magic
// (0xEF53 LE at 1080).
func Detect(path string) (FSType, error) {
	panic("unimplemented")
}

// Mount makes the image's tree available at mountPoint (which must exist)
// and returns a Mounted handle.
//
//   - TypeDirectory: no mount; Root = img.Path.
//   - TypeRaw bare filesystem: loop-attach read-only, mount at mountPoint.
//   - TypeRaw GPT: loop-attach with partscan, pick the root partition for
//     arch (SPEC §3 type GUIDs), else the usr partition (then the tree root
//     is synthesized so the payload appears under <mountPoint>/usr).
//
// On any error all intermediate resources are released.
func Mount(img discover.Image, mountPoint, arch string) (*Mounted, error) {
	panic("unimplemented")
}

// Unmount releases the mount and detaches the loop device. Safe to call on
// directory-backed images (no-op).
func (m *Mounted) Unmount() error {
	panic("unimplemented")
}

// DetachAllLoopsFor detaches any loop devices whose backing file is path.
// Used by unmerge cleanup when state was lost.
func DetachAllLoopsFor(path string) error {
	panic("unimplemented")
}

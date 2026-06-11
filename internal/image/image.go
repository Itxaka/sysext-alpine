// Package image provides access to extension image content: plain
// directories and raw disk images (bare-filesystem or GPT-partitioned),
// per docs/SPEC.md §3.
//
// Raw images are attached to a loop device (LOOP_CTL_GET_FREE +
// LOOP_CONFIGURE, read-only, partition scanning enabled for GPT) and
// mounted read-only at the supplied mount point.
package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

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

	// mountTarget is the directory we actually mounted on (Root, or
	// Root/usr for usr-only GPT images). Empty for directory images.
	mountTarget string
}

// Detect probes the file at path and returns the format: checks GPT header
// ("EFI PART" at LBA 1 for 512 and 4096 byte sectors), squashfs magic
// ("hsqs" at 0), erofs magic (0xE0F5E1E2 LE at 1024), ext4 magic
// (0xEF53 LE at 1080).
func Detect(path string) (FSType, error) {
	return detectPath(path)
}

// MountOpts tunes MountWithOpts.
type MountOpts struct {
	// Arch is the host architecture in systemd notation ("x86-64", ...);
	// selects the GPT partition type GUIDs.
	Arch string
	// Policy is the systemd.image-policy(7) string applied to disk images
	// ("" = the class default policy). Enforced for GPT DDIs with verity
	// partitions; bare-filesystem images count as "unprotected".
	Policy string
}

// Mount makes the image's tree available at mountPoint with the default
// image policy. See MountWithOpts.
func Mount(img discover.Image, mountPoint, arch string) (*Mounted, error) {
	return MountWithOpts(img, mountPoint, MountOpts{Arch: arch})
}

// MountWithOpts makes the image's tree available at mountPoint (which must
// exist) and returns a Mounted handle.
//
//   - TypeDirectory: no mount; Root = img.Path.
//   - TypeRaw bare filesystem: loop-attach read-only, mount at mountPoint.
//   - TypeRaw GPT: loop-attach with partscan, pick the root partition for
//     arch (SPEC §3 type GUIDs), else the usr partition (then the tree root
//     is synthesized so the payload appears under <mountPoint>/usr).
//
// On any error all intermediate resources are released.
func MountWithOpts(img discover.Image, mountPoint string, opts MountOpts) (*Mounted, error) {
	arch := opts.Arch
	if img.Type == discover.TypeDirectory {
		return &Mounted{Root: img.Path}, nil
	}

	fs, err := Detect(img.Path)
	if err != nil {
		return nil, fmt.Errorf("detecting format of %s: %w", img.Path, err)
	}

	switch fs {
	case FSSquashfs, FSErofs, FSExt4:
		return mountBareFS(img.Path, mountPoint, fs)
	case FSGPT:
		return mountGPT(img.Path, mountPoint, arch)
	default:
		return nil, fmt.Errorf("%s: unrecognized image format", img.Path)
	}
}

// mountBareFS loop-attaches a partition-table-less raw image and mounts it.
func mountBareFS(path, mountPoint string, fs FSType) (*Mounted, error) {
	loopDev, err := loopAttach(path, false)
	if err != nil {
		return nil, err
	}
	if err := mountRO(loopDev, mountPoint, fs); err != nil {
		_ = loopDetach(loopDev)
		return nil, err
	}
	return &Mounted{
		Root:        mountPoint,
		LoopDevice:  loopDev,
		FS:          fs,
		mountTarget: mountPoint,
	}, nil
}

// mountGPT parses the partition table, attaches the image with partition
// scanning, and mounts the selected payload partition.
func mountGPT(path, mountPoint, arch string) (*Mounted, error) {
	parts, err := parseGPTFile(path)
	if err != nil {
		return nil, fmt.Errorf("parsing GPT of %s: %w", path, err)
	}
	part, isUsr, err := selectPartition(parts, arch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	loopDev, err := loopAttach(path, true)
	if err != nil {
		return nil, err
	}

	cleanup := func() { _ = loopDetach(loopDev) }

	partDev, err := ensurePartitionNode(loopDev, part.Index)
	if err != nil {
		cleanup()
		return nil, err
	}

	fs, err := Detect(partDev)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("detecting filesystem on %s: %w", partDev, err)
	}
	if fs == FSUnknown || fs == FSGPT {
		cleanup()
		return nil, fmt.Errorf("%s: partition %d has unsupported filesystem", path, part.Index)
	}

	target := mountPoint
	if isUsr {
		// Only a usr partition: synthesize the tree root so the payload
		// shows up under <mountPoint>/usr.
		target = filepath.Join(mountPoint, "usr")
		if err := os.MkdirAll(target, 0o755); err != nil {
			cleanup()
			return nil, err
		}
	}

	if err := mountRO(partDev, target, fs); err != nil {
		cleanup()
		return nil, err
	}
	return &Mounted{
		Root:        mountPoint,
		LoopDevice:  loopDev,
		Partition:   partDev,
		FS:          fs,
		mountTarget: target,
	}, nil
}

// mountRO mounts a read-only nodev filesystem.
func mountRO(device, target string, fs FSType) error {
	err := unix.Mount(device, target, string(fs), unix.MS_RDONLY|unix.MS_NODEV, "")
	if err != nil {
		return fmt.Errorf("mounting %s (%s) at %s: %w", device, fs, target, err)
	}
	return nil
}

// Unmount releases the mount and detaches the loop device. Safe to call on
// directory-backed images (no-op).
func (m *Mounted) Unmount() error {
	if m == nil || (m.LoopDevice == "" && m.mountTarget == "") {
		return nil // directory image
	}

	targets := []string{m.mountTarget}
	if m.mountTarget == "" {
		// Handle reconstructed from lost state: try both possible targets.
		targets = []string{filepath.Join(m.Root, "usr"), m.Root}
	}

	var firstErr error
	for _, t := range targets {
		if t == "" {
			continue
		}
		if err := unmountTolerant(t); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if m.LoopDevice != "" {
		if err := loopDetach(m.LoopDevice); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// unmountTolerant unmounts target, ignoring "not mounted"/missing errors and
// falling back to a lazy detach when the mount is busy.
func unmountTolerant(target string) error {
	err := unix.Unmount(target, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOENT):
		// Not a mount point / already gone.
		return nil
	case errors.Is(err, unix.EBUSY):
		if err := unix.Unmount(target, unix.MNT_DETACH); err != nil &&
			!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("lazy unmount %s: %w", target, err)
		}
		return nil
	default:
		return fmt.Errorf("unmount %s: %w", target, err)
	}
}

// DetachAllLoopsFor detaches any loop devices whose backing file is path.
// Used by unmerge cleanup when state was lost.
func DetachAllLoopsFor(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs
	}

	backings, err := filepath.Glob("/sys/block/loop*/loop/backing_file")
	if err != nil {
		return err
	}

	var firstErr error
	for _, bf := range backings {
		data, err := os.ReadFile(bf)
		if err != nil {
			continue // device went away
		}
		backing := strings.TrimSpace(string(data))
		// The kernel appends " (deleted)" when the backing file was
		// unlinked while attached.
		backing = strings.TrimSuffix(backing, " (deleted)")
		if backing != abs && backing != resolved {
			continue
		}
		// /sys/block/loopN/loop/backing_file -> /dev/loopN
		devName := filepath.Base(filepath.Dir(filepath.Dir(bf)))
		if err := loopDetach("/dev/" + devName); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

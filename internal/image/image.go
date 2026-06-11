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
	// VerityDevice is the dm-verity device path (/dev/mapper/<name>) the
	// filesystem was mounted from, "" when no verity protection is active.
	VerityDevice string
	// FS is the detected payload filesystem.
	FS FSType

	// verityName is the device-mapper device name behind VerityDevice;
	// Unmount removes it after unmounting.
	verityName string
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
	// TrustDir is the directory holding trusted PEM certificates (*.crt)
	// for verity signature verification ("" = /etc/verity.d). See
	// internal/image/signature.go for the trust model.
	TrustDir string
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

	policy, err := parseImagePolicy(opts.Policy)
	if err != nil {
		return nil, err
	}

	switch fs {
	case FSSquashfs, FSErofs, FSExt4:
		return mountBareFS(img.Path, mountPoint, fs, policy)
	case FSGPT:
		return mountGPT(img.Path, mountPoint, arch, policy, opts.TrustDir)
	default:
		return nil, fmt.Errorf("%s: unrecognized image format", img.Path)
	}
}

// mountBareFS loop-attaches a partition-table-less raw image and mounts it.
// Bare-filesystem images carry no verity metadata: they classify as an
// unprotected root payload and the policy must allow that.
func mountBareFS(path, mountPoint string, fs FSType, policy *imagePolicy) (*Mounted, error) {
	if !policy.root[protUnprotected] {
		return nil, fmt.Errorf("%s: %w", path,
			policyError("root", protUnprotected, policy.root))
	}

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

// mountGPT parses the partition table, enforces the image policy, attaches
// the image with partition scanning, verifies the verity root-hash
// signature when present (and the policy accepts "signed"), sets up
// dm-verity when the image carries a verity partition (and the policy
// allows it), and mounts the payload read-only.
func mountGPT(path, mountPoint, arch string, policy *imagePolicy, trustDir string) (*Mounted, error) {
	parts, err := parseGPTFile(path)
	if err != nil {
		return nil, fmt.Errorf("parsing GPT of %s: %w", path, err)
	}
	part, isUsr, err := selectPartition(parts, arch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	guids := archGUIDs[arch] // selectPartition validated arch

	designator := "root"
	if isUsr {
		designator = "usr"
	}

	useVerity, checkSig, err := decideVerity(parts, guids, designator, policy)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	loopDev, err := loopAttach(path, true)
	if err != nil {
		return nil, err
	}

	verityName := "" // set once a dm device exists; cleaned up on error
	cleanup := func() {
		if verityName != "" {
			_ = verityRemove(verityName)
		}
		_ = loopDetach(loopDev)
	}

	partDev, err := ensurePartitionNode(loopDev, part.Index)
	if err != nil {
		cleanup()
		return nil, err
	}

	mountDev := partDev
	verityDevPath := ""
	if useVerity {
		verityType, sigType := guids.rootVerity, guids.rootVeritySig
		if isUsr {
			verityType, sigType = guids.usrVerity, guids.usrVeritySig
		}
		vpart := findByType(parts, verityType) // non-nil: decideVerity classified it

		if checkSig {
			// Image classified as signed and the policy accepts signed:
			// verify the PKCS#7 signature over the GUID-reconstructed
			// root hash before activating verity with it.
			if serr := verifyImageSignature(loopDev, parts, sigType, part, *vpart, trustDir); serr != nil {
				if policy.forDesignator(designator)[protVerity] {
					// Policy also accepts plain verity: degrade with a
					// warning, hash tree still enforced.
					fmt.Fprintf(os.Stderr,
						"Warning: %s: verity signature verification failed, continuing as unsigned verity (policy allows verity): %v\n",
						path, serr)
				} else {
					cleanup()
					return nil, fmt.Errorf("%s: verity signature verification failed: %w", path, serr)
				}
			}
		}

		verityDevPath, err = setupVerity(path, loopDev, part, *vpart, partDev)
		if err != nil {
			cleanup()
			return nil, err
		}
		verityName = verityDeviceName(path)
		mountDev = verityDevPath
	}

	fs, err := Detect(mountDev)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("detecting filesystem on %s: %w", mountDev, err)
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

	if err := mountRO(mountDev, target, fs); err != nil {
		cleanup()
		return nil, err
	}
	return &Mounted{
		Root:         mountPoint,
		LoopDevice:   loopDev,
		Partition:    partDev,
		VerityDevice: verityDevPath,
		FS:           fs,
		verityName:   verityName,
		mountTarget:  target,
	}, nil
}

// decideVerity applies the image policy to the actual protection level of
// the selected designator and reports whether dm-verity should be used
// (useVerity), whether the verity root-hash signature must be verified
// first (checkSig), or whether the image is rejected (error).
//
// Like systemd, the highest protection both sides accept is preferred:
// when the image carries a verity signature partition and the policy
// accepts "signed", the signature is verified (checkSig=true). If
// verification later fails, the caller degrades to plain verity when the
// policy also accepts "verity" (warning), otherwise the mount fails.
// Signed images facing a policy that accepts "verity" but not "signed" are
// mounted as plain verity without signature verification.
func decideVerity(parts []gptPartition, guids dpsGUIDs, designator string, policy *imagePolicy) (useVerity, checkSig bool, err error) {
	prot := classifyProtection(parts, guids, designator)
	allowed := policy.forDesignator(designator)

	switch prot {
	case protUnprotected:
		if !allowed[protUnprotected] {
			return false, false, policyError(designator, prot, allowed)
		}
		return false, false, nil
	case protVerity:
		switch {
		case allowed[protVerity]:
			return true, false, nil
		case allowed[protUnprotected]:
			return false, false, nil
		default:
			// Policy accepts only signed/encrypted/absent: a plain
			// verity image cannot satisfy it.
			return false, false, policyError(designator, prot, allowed)
		}
	case protSigned:
		switch {
		case allowed[protSigned]:
			// Verify the signature; on failure the caller degrades to
			// plain verity iff allowed[protVerity].
			return true, true, nil
		case allowed[protVerity]:
			// Treat the signed image as plain verity (signature not
			// verified, hash tree still enforced).
			return true, false, nil
		case allowed[protUnprotected]:
			return false, false, nil
		default:
			return false, false, policyError(designator, prot, allowed)
		}
	default: // protAbsent cannot happen for the selected partition
		return false, false, policyError(designator, prot, allowed)
	}
}

// setupVerity prepares and activates the dm-verity device for the selected
// data partition: resolves the verity partition node, reads the verity
// superblock, reconstructs the root hash from the unique partition GUIDs,
// and creates the device-mapper device. Returns the dm device node path.
func setupVerity(imagePath, loopDev string, data, verity gptPartition, dataDev string) (string, error) {
	hashDev, err := ensurePartitionNode(loopDev, verity.Index)
	if err != nil {
		return "", err
	}

	sb, err := readVeritySuperblock(hashDev)
	if err != nil {
		return "", err
	}
	// The GUID-embedded root hash is exactly 256 bits; only digest
	// algorithms with 32-byte output can be carried this way.
	if sb.Algorithm != "sha256" {
		return "", fmt.Errorf("%s: verity algorithm %q not supported for GUID root-hash discovery (need sha256)", imagePath, sb.Algorithm)
	}

	rootHash, err := rootHashFromGUIDs(data.UniqueGUID, verity.UniqueGUID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", imagePath, err)
	}

	name := verityDeviceName(imagePath)
	devPath, err := verityActivate(name, sb, dataDev, hashDev, rootHash)
	if err != nil {
		return "", fmt.Errorf("%s: activating dm-verity: %w", imagePath, err)
	}
	return devPath, nil
}

// mountRO mounts a read-only nodev filesystem.
func mountRO(device, target string, fs FSType) error {
	err := unix.Mount(device, target, string(fs), unix.MS_RDONLY|unix.MS_NODEV, "")
	if err != nil {
		return fmt.Errorf("mounting %s (%s) at %s: %w", device, fs, target, err)
	}
	return nil
}

// Unmount releases the mount, removes the dm-verity device (if any) and
// detaches the loop device. Safe to call on directory-backed images (no-op).
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

	if m.verityName != "" {
		if err := verityRemove(m.verityName); err != nil && firstErr == nil {
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

// RemoveVerityFor tears down the dm-verity device that MountWithOpts would
// have created for the image at path ("sysext-<name>-verity"). Used by
// unmerge cleanup, where the Mounted handle from the original merge process
// is gone. Idempotent: a missing device is not an error.
func RemoveVerityFor(path string) error {
	return verityRemove(verityDeviceName(path))
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

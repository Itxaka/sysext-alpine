// Package image makes extension images accessible: plain directories and
// raw disk images, either a single filesystem or a partitioned DDI per the
// UAPI Discoverable Partitions Specification, dissected like systemd v262
// (src/shared/dissect-image.c) with the flags systemd-sysext uses.
//
// Raw images are attached read-only to loop devices with
// LO_FLAGS_AUTOCLEAR, dm-verity devices are marked for deferred removal once
// mounted, so unmounting the image releases everything.
package image

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/errno"
	"github.com/itxaka/sysext-alpine/internal/fsutil"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// MountOpts tunes MountWithOpts.
type MountOpts struct {
	// Arch is the host architecture in systemd notation ("x86-64", ...);
	// partitions for it are preferred, then those of its secondary
	// architecture, then any other. "" is the architecture this program
	// was built for, like systemd's native_architecture().
	Arch string
	// Policy is the systemd.image-policy(7) string applied to raw images;
	// "" selects the default policy of Class.
	Policy string
	// Class selects the default image policy.
	Class release.Class
	// TrustDirs are the directories holding trusted verity signing
	// certificates (*.crt), in priority order; nil means TrustDirs("/").
	TrustDirs []string
	// Warnf, when set, receives non-fatal diagnostics (e.g. a signature
	// that could not be verified when the policy also accepts plain
	// verity).
	Warnf func(format string, args ...any)
}

// Mounted is an image whose tree is accessible at Root.
type Mounted struct {
	// Root is the directory exposing the image's filesystem tree (for
	// directory images the image path itself).
	Root string
	// RootHash is the lowercase hex dm-verity root hash the image was
	// mounted with, "" when no verity protection is in use.
	RootHash string

	mounts []string
}

// MountWithOpts makes the image's tree available at mountPoint, which must
// exist. Directory images are used in place. Raw images are dissected and
// their root partition (or a tmpfs when there is only a usr partition) is
// mounted at mountPoint and the usr partition, if any, at mountPoint/usr,
// both read-only. On error nothing stays mounted or attached; on success
// unmounting the mount points releases the loop and dm devices.
func MountWithOpts(img discover.Image, mountPoint string, opts MountOpts) (*Mounted, error) {
	if img.Type == discover.TypeDirectory {
		return &Mounted{Root: img.Path}, nil
	}
	policy, err := resolvePolicy(opts.Policy, opts.Class)
	if err != nil {
		return nil, err
	}
	if opts.Arch == "" {
		opts.Arch = release.NativeArchitecture()
	}
	if opts.TrustDirs == nil {
		opts.TrustDirs = TrustDirs("/")
	}
	m := &mounter{path: img.Path, mountPoint: mountPoint, opts: opts, policy: policy}
	return m.mount()
}

// mounter holds the state of one MountWithOpts call so that every error
// path (and panic) releases what was set up so far.
type mounter struct {
	path       string
	mountPoint string
	opts       MountOpts
	policy     *imagePolicy

	file *os.File
	size int64
	// block is set for block device images; sectorSize is then the
	// device's logical sector size, 512 for image files.
	block      bool
	sectorSize int64
	loops      []*loopDevice
	verity     *verityDevice
	mounts     []string
}

func (m *mounter) warnf(format string, args ...any) {
	if m.opts.Warnf != nil {
		m.opts.Warnf(format, args...)
	}
}

func (m *mounter) mount() (*Mounted, error) {
	f, err := os.OpenFile(m.path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	m.file, m.sectorSize = f, 512
	switch mode := fi.Mode(); {
	case mode.IsRegular():
		m.size = fi.Size()
	case mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0:
		m.block = true
		if m.size, m.sectorSize, err = blockDeviceGeometry(f); err != nil {
			return nil, fmt.Errorf("%s: %w", m.path, err)
		}
	default:
		return nil, fmt.Errorf("%s: neither a regular file nor a block device", m.path)
	}

	ok := false
	defer func() {
		if !ok {
			m.cleanup()
		}
	}()

	vs, err := loadVeritySidecars(m.path)
	if err != nil {
		return nil, err
	}
	var res *Mounted
	if vs.dataPath != "" {
		res, err = m.mountWithExternalHashTree(vs)
	} else {
		res, err = m.mountRaw(vs)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	if err := m.relinquish(); err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	ok = true
	return res, nil
}

// mountRaw handles images without an external hash tree: a partition table
// or a single filesystem.
func (m *mounter) mountRaw(vs *veritySettings) (*Mounted, error) {
	ss, err := probeSectorSize(m.file)
	if err != nil {
		return nil, err
	}
	pt, err := readPartitionTable(m.file, m.size, pick(ss != 0, ss, m.sectorSize))
	if err != nil && !errors.Is(err, errNoPartitionTable) {
		return nil, err
	}
	if pt != nil && pt.gpt {
		return m.mountPartitioned(pt, vs)
	}
	fs, err := detectFS(m.file)
	if err != nil {
		return nil, err
	}
	if fs != "" {
		return m.mountUnpartitioned(fs, vs)
	}
	if pt != nil {
		return m.mountPartitioned(pt, vs)
	}
	return nil, errno.New(unix.ENOPKG, "no suitable partition table or file system found")
}

// mountWithExternalHashTree handles images with a .verity sidecar, which
// must be a single filesystem.
func (m *mounter) mountWithExternalHashTree(vs *veritySettings) (*Mounted, error) {
	fs, err := detectFS(m.file)
	if err != nil {
		return nil, err
	}
	if fs == "" {
		return nil, fmt.Errorf("external verity data %s given for an image that is not a single filesystem", vs.dataPath)
	}
	return m.mountUnpartitioned(fs, vs)
}

// mountUnpartitioned mirrors dissect_image_from_unpartitioned(): the whole
// image is the root partition, verity protected when an external hash tree
// and root hash cover it.
func (m *mounter) mountUnpartitioned(fs fsType, vs *veritySettings) (*Mounted, error) {
	found := polUnprotected
	switch {
	case vs.covers(partRoot):
		found = pick(len(vs.sig) > 0, polSigned, polVerity)
	case fs == fsLUKS:
		found = polEncrypted | polEncryptedWithIntegrity
	}
	use, err := m.policy.mayUse(partRoot)
	if err != nil {
		return nil, err
	}
	if !use {
		return nil, errno.New(unix.ENOPKG, "the image policy ignores the root partition")
	}
	if err := m.policy.checkProtection(partRoot, found); err != nil {
		return nil, err
	}
	if err := m.policy.checkPartitionFlags(partRoot, 0); err != nil {
		return nil, err
	}

	loop, err := m.attach(m.file, 0, false)
	if err != nil {
		return nil, err
	}
	dev := loop.path
	var rootHash []byte
	if vs.covers(partRoot) && m.policy.exhaustive(partRoot)&(polVerity|polSigned) != 0 {
		hf, err := os.OpenFile(vs.dataPath, os.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		defer hf.Close()
		hashLoop, err := m.attach(hf, 0, false)
		if err != nil {
			return nil, err
		}
		if dev, err = m.setupVerity(partRoot, vs, hf, loop, loop.path, hashLoop.path); err != nil {
			return nil, err
		}
		rootHash = vs.rootHash
	}
	if err := m.mountDevice(partRoot, dev, m.mountPoint); err != nil {
		return nil, err
	}
	return m.result(rootHash), nil
}

// mountPartitioned dissects the partition table and mounts root and usr.
func (m *mounter) mountPartitioned(pt *partitionTable, vs *veritySettings) (*Mounted, error) {
	ds := &dissector{
		table:     pt,
		policy:    m.policy,
		verity:    vs,
		native:    m.opts.Arch,
		readSig:   func(p partition) (*veritySig, error) { return readVeritySig(m.file, p, pt.sectorSize) },
		machineID: hostMachineID,
	}
	d, err := ds.dissect()
	if err != nil {
		return nil, err
	}
	if err := m.checkFilesystems(d, pt.sectorSize); err != nil {
		return nil, err
	}
	if err := d.loadSigPartitionRootHash(vs, m.file, pt.sectorSize); err != nil {
		return nil, err
	}
	if err := d.guessRootHash(vs); err != nil {
		return nil, err
	}

	loop, err := m.attach(m.file, uint32(pt.sectorSize), true)
	if err != nil {
		return nil, err
	}

	var rootHash []byte
	devices := map[designator]string{}
	for _, x := range []designator{partRoot, partUsr} {
		if !d.found(x) {
			continue
		}
		node, err := loop.partitionNode(*d.parts[x], pt.sectorSize)
		if err != nil {
			return nil, err
		}
		devices[x] = node
		if !m.useVerity(x, vs, d) {
			continue
		}
		hash := d.parts[x.verityHash()]
		hashNode, err := loop.partitionNode(*hash, pt.sectorSize)
		if err != nil {
			return nil, err
		}
		dm, err := m.setupVerity(x, vs, sectionReader(m.file, *hash, pt.sectorSize), loop, node, hashNode)
		if err != nil {
			return nil, err
		}
		devices[x] = dm
		rootHash = vs.rootHash
	}

	if dev, ok := devices[partRoot]; ok {
		if err := m.mountDevice(partRoot, dev, m.mountPoint); err != nil {
			return nil, err
		}
	} else if err := m.mountRootTmpfs(); err != nil {
		return nil, err
	}
	if dev, ok := devices[partUsr]; ok {
		target, err := m.usrMountPoint()
		if err != nil {
			return nil, err
		}
		if err := m.mountDevice(partUsr, dev, target); err != nil {
			return nil, err
		}
	}
	return m.result(rootHash), nil
}

// checkFilesystems mirrors dissected_image_probe_filesystems(): every used
// partition is probed, encrypted ones are checked against the policy.
func (m *mounter) checkFilesystems(d *dissected, ss int64) error {
	for x := range numDesignators {
		if !d.found(x) {
			continue
		}
		found := polUnused | polUnprotected | polVerity | polSigned
		if !x.isVerityHash() && !x.isVeritySig() {
			fs, err := detectFS(sectionReader(m.file, *d.parts[x], ss))
			if err != nil {
				return fmt.Errorf("%s partition: %w", x, err)
			}
			if fs == fsLUKS {
				found = polUnused | polEncrypted | polEncryptedWithIntegrity
			}
		}
		if err := m.policy.checkProtection(x, found); err != nil {
			return err
		}
	}
	return nil
}

// useVerity mirrors the preconditions of systemd's verity_partition().
func (m *mounter) useVerity(x designator, vs *veritySettings, d *dissected) bool {
	if len(vs.rootHash) == 0 {
		return false
	}
	if vs.designator != x && (vs.designator != partInvalid || x != partRoot) {
		return false
	}
	if !d.found(x.verityHash()) {
		return false
	}
	return m.policy.exhaustive(x)&(polVerity|polSigned) != 0
}

// VerityError is the error of a dm-verity activation that failed,
// signature verification included (systemd's dissected_image_decrypt()).
type VerityError struct {
	// Partition is the designator of the data partition ("root", "usr").
	Partition string
	Err       error
}

func (e *VerityError) Error() string {
	return "activating dm-verity for the " + e.Partition + " partition: " + e.Err.Error()
}

func (e *VerityError) Unwrap() error { return e.Err }

// setupVerity activates dm-verity for designator x (systemd's
// verity_partition() and do_crypt_activate_verity()) and returns the device
// node to mount.
func (m *mounter) setupVerity(x designator, vs *veritySettings, hash io.ReaderAt, loop *loopDevice, dataNode, hashNode string) (node string, err error) {
	defer func() {
		if err != nil {
			err = &VerityError{Partition: x.String(), Err: err}
		}
	}()
	sb, err := readVeritySuperblock(hash)
	if err != nil {
		return "", err
	}
	if err := sb.checkRootHash(vs.rootHash); err != nil {
		return "", err
	}
	dataDev, err := nodeDevT(dataNode)
	if err != nil {
		return "", err
	}
	hashDev, err := nodeDevT(hashNode)
	if err != nil {
		return "", err
	}

	pol := m.policy.exhaustive(x)
	checkSig := len(vs.sig) > 0 && pol&polSigned != 0 && !envDisabled("SYSTEMD_DISSECT_VERITY_SIGNATURE")
	if !checkSig && pol&polVerity == 0 {
		return "", errno.New(unix.ERFKILL, "image does not satisfy image policy: activation of the %s partition without a verified signature is not allowed", x)
	}

	t := &verityTarget{
		name:     verityDMName(dataNode, loop.diskseq),
		sb:       sb,
		dataDev:  dataDev,
		hashDev:  hashDev,
		rootHash: vs.rootHash,
	}
	var sig []byte
	if checkSig {
		sig = vs.sig
	}
	dev, err := activateVerity(t, sig, func(kernelErr error) error {
		ok, reason, err := verifySignature(vs.rootHash, vs.sig, m.opts.TrustDirs)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if pol&polVerity == 0 {
			return fmt.Errorf("verity signature verification failed: %s (%w)", reason, kernelErr)
		}
		m.warnf("%s: verity signature verification failed, continuing as unsigned verity (policy allows verity): %s", m.path, reason)
		return nil
	})
	if err != nil {
		return "", err
	}
	m.verity = dev
	return dev.node, nil
}

// nodeDevT returns "major:minor" of a block device node.
func nodeDevT(node string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(node, &st); err != nil {
		return "", fmt.Errorf("stat %s: %w", node, err)
	}
	return strconv.FormatUint(uint64(unix.Major(st.Rdev)), 10) + ":" + strconv.FormatUint(uint64(unix.Minor(st.Rdev)), 10), nil
}

// attach provides a loop device for f with the given logical block size
// (0 = default) and partition scanning. A block device image is used as is
// when that gives the same view of it, like systemd's
// loop_device_make_internal() does.
func (m *mounter) attach(f *os.File, blockSize uint32, partscan bool) (*loopDevice, error) {
	var l *loopDevice
	var err error
	if f == m.file && m.useDeviceDirectly(blockSize, partscan) {
		l, err = openBlockDevice(f)
	} else {
		l, err = loopAttach(f, blockSize, partscan)
	}
	if err != nil {
		return nil, err
	}
	m.loops = append(m.loops, l)
	return l, nil
}

// useDeviceDirectly is loop_device_can_shortcut(): a block device image
// needs no loop device when the requested sector size is its own and, if
// partitions are wanted, the kernel scans its partition table already.
// Partitions are only wanted for images with a partition table, so the
// exception systemd makes for devices without one never applies.
func (m *mounter) useDeviceDirectly(blockSize uint32, partscan bool) bool {
	if !m.block || (blockSize != 0 && int64(blockSize) != m.sectorSize) {
		return false
	}
	if !partscan {
		return true
	}
	dir, err := blockSysDir(m.file)
	return err == nil && partscanEnabled(dir)
}

// mountDevice probes and mounts the filesystem on dev read-only at target.
func (m *mounter) mountDevice(x designator, dev, target string) error {
	f, err := os.OpenFile(dev, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	fs, err := detectFS(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s partition: %w", x, err)
	}
	switch {
	case fs == "":
		return errno.New(unix.EAFNOSUPPORT, "%s partition: file system type not supported or not known", x)
	case fs == fsLUKS:
		return errno.New(unix.EUNATCH, "%s partition: encrypted (LUKS) partitions are not supported", x)
	case !fsTypeAllowed(fs):
		return errno.New(unix.EIDRM, "%s partition: file system %s is not allowed for automatic mounting", x, fs)
	}
	if err := m.policy.checkFS(x, fs); err != nil {
		return err
	}
	if err := mountReadOnly(dev, target, fs); err != nil {
		return err
	}
	m.mounts = append(m.mounts, target)
	return nil
}

// mountRootTmpfs provides the tree root for images with only a usr
// partition, like systemd's mount_root_tmpfs().
func (m *mounter) mountRootTmpfs() error {
	if err := unix.Mount("rootfs", m.mountPoint, "tmpfs", unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mounting tmpfs at %s: %w", m.mountPoint, err)
	}
	m.mounts = append(m.mounts, m.mountPoint)
	return nil
}

// usrMountPoint resolves /usr inside the mounted root, creating it when the
// root is writable (the tmpfs of usr-only images).
func (m *mounter) usrMountPoint() (string, error) {
	p, err := fsutil.Chase(m.mountPoint, "/usr", 0)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(filepath.Join(m.mountPoint, "usr"), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("root partition has no /usr directory to mount the usr partition on: %w", err)
		}
		p, err = fsutil.Chase(m.mountPoint, "/usr", 0)
	}
	if err != nil {
		return "", err
	}
	return p, nil
}

// mountOptions mirrors fstype_norecovery_option(): read-only mounts must not
// replay journals, which would write to the image.
func mountOptions(fs fsType) []string {
	switch fs {
	case fsExt3, fsExt4, fsXFS:
		return []string{"norecovery"}
	case fsBtrfs:
		return []string{"rescue=nologreplay", "norecovery"}
	case fsF2FS:
		return []string{"norecovery", ""}
	default:
		return []string{""}
	}
}

// mountReadOnly mounts a filesystem read-only and nodev, trying the
// norecovery-style options in order until the kernel accepts one.
func mountReadOnly(device, target string, fs fsType) error {
	var err error
	for _, data := range mountOptions(fs) {
		err = unix.Mount(device, target, string(fs), unix.MS_RDONLY|unix.MS_NODEV, data)
		if !errors.Is(err, unix.EINVAL) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("mounting %s (%s) at %s: %w", device, fs, target, err)
	}
	return nil
}

func (m *mounter) result(rootHash []byte) *Mounted {
	res := &Mounted{Root: m.mountPoint, mounts: m.mounts}
	if len(rootHash) > 0 {
		res.RootHash = hex.EncodeToString(rootHash)
	}
	return res
}

// relinquish hands loop and dm lifetime to the kernel once mounted.
func (m *mounter) relinquish() error {
	if m.verity != nil {
		if err := m.verity.relinquish(); err != nil {
			return err
		}
		m.verity = nil
	}
	for _, l := range m.loops {
		l.relinquish()
	}
	m.loops = nil
	return nil
}

func (m *mounter) cleanup() {
	for _, t := range slices.Backward(m.mounts) {
		_ = unmountTolerant(t)
	}
	if m.verity != nil {
		m.verity.remove()
	}
	for _, l := range m.loops {
		l.detach()
	}
}

// Unmount unmounts everything MountWithOpts mounted, deepest first. The
// kernel then releases the dm-verity and loop devices. Directory images
// are a no-op.
func (m *Mounted) Unmount() error {
	if m == nil {
		return nil
	}
	var errs []error
	for _, t := range slices.Backward(m.mounts) {
		if err := unmountTolerant(t); err != nil {
			errs = append(errs, err)
		}
	}
	m.mounts = nil
	return errors.Join(errs...)
}

// unmountTolerant unmounts target, ignoring "not mounted"/missing errors and
// falling back to a lazy detach when the mount is busy.
func unmountTolerant(target string) error {
	err := unix.Unmount(target, 0)
	switch {
	case err == nil, errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOENT):
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

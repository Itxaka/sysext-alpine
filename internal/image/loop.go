package image

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	loopControlPath = "/dev/loop-control"
	sysBlock        = "/sys/block"
	sysDevBlock     = "/sys/dev/block"
	// loopAttachAttempts bounds retries when racing other processes for a
	// free loop device (LOOP_CTL_GET_FREE is not a reservation).
	loopAttachAttempts = 64
)

// errLoopRetry marks configure failures after which another free loop
// device should be tried.
var errLoopRetry = errors.New("loop device busy")

// loopDevice is an attached loop device. Like systemd's LoopDevice it is
// configured with LO_FLAGS_AUTOCLEAR: the kernel detaches it once the last
// opener is gone, so the fd is kept open until the filesystem (or dm device)
// on top holds its own reference, then released with relinquish().
type loopDevice struct {
	f       *os.File
	lock    *os.File
	path    string
	name    string
	diskseq uint64
	// borrowed is set when the image's own block device stands in for a
	// loop device; it is never detached.
	borrowed bool
}

// loopAttach attaches backing read-only to a free loop device with the
// given logical block size (0 = 512), enabling partition scanning when
// partscan is set. It follows systemd's loop_device_make_internal():
// direct I/O when possible, partscan enabled after LOOP_CONFIGURE, a lock on
// /dev/loop-control while allocating and a BSD lock on the device.
func loopAttach(backing *os.File, blockSize uint32, partscan bool) (*loopDevice, error) {
	if blockSize == 0 {
		blockSize = 512
	}

	directIO := !envDisabled("SYSTEMD_LOOP_DIRECT_IO")
	fd := backing
	if directIO {
		if direct, err := reopenFile(backing, unix.O_RDONLY|unix.O_DIRECT); err == nil {
			defer direct.Close()
			fd = direct
		} else {
			directIO = false
		}
	}

	ctl, err := os.OpenFile(loopControlPath, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", loopControlPath, err)
	}
	defer ctl.Close()

	var lastErr error
	for attempt := range loopAttachAttempts {
		if err := unix.Flock(int(ctl.Fd()), unix.LOCK_EX); err != nil {
			return nil, fmt.Errorf("locking %s: %w", loopControlPath, err)
		}
		nr, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			_ = unix.Flock(int(ctl.Fd()), unix.LOCK_UN)
			return nil, fmt.Errorf("LOOP_CTL_GET_FREE: %w", err)
		}
		dev, err := loopConfigure(nr, fd, backing.Name(), blockSize, directIO)
		_ = unix.Flock(int(ctl.Fd()), unix.LOCK_UN)
		if err == nil {
			if partscan {
				if err := dev.enablePartscan(); err != nil {
					dev.detach()
					return nil, err
				}
			}
			return dev, nil
		}
		if errors.Is(err, unix.ENOANO) && directIO {
			directIO = false
			fd = backing
		} else if !errors.Is(err, errLoopRetry) && !errors.Is(err, unix.EBUSY) &&
			!errors.Is(err, unix.ENXIO) && !errors.Is(err, unix.ENODEV) && !errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("attaching %s to /dev/loop%d: %w", backing.Name(), nr, err)
		}
		lastErr = err
		time.Sleep(time.Duration(10+attempt*4) * time.Millisecond)
	}
	return nil, fmt.Errorf("attaching %s: no usable loop device after %d attempts: %w", backing.Name(), loopAttachAttempts, lastErr)
}

// loopConfigure binds fd to /dev/loop<nr> (systemd's loop_configure()).
func loopConfigure(nr int, fd *os.File, backingPath string, blockSize uint32, directIO bool) (*loopDevice, error) {
	name := "loop" + strconv.Itoa(nr)
	path := "/dev/" + name
	if err := ensureBlockNode(path, filepath.Join(sysBlock, name, "dev")); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	dev := &loopDevice{f: f, path: path, name: name}
	ok := false
	defer func() {
		if !ok {
			dev.close()
		}
	}()

	dev.lock, err = os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(dev.lock.Fd()), unix.LOCK_EX); err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	if _, err := unix.IoctlLoopGetStatus64(dev.fd()); err == nil {
		return nil, errLoopRetry
	} else if !errors.Is(err, unix.ENXIO) {
		return nil, fmt.Errorf("LOOP_GET_STATUS64 %s: %w", path, err)
	}
	if removed, err := removeStalePartitions(dev); err != nil {
		return nil, err
	} else if removed {
		return nil, errLoopRetry
	}

	cfg := loopConfig(fd, backingPath, blockSize, directIO)
	if err := unix.IoctlLoopConfigure(dev.fd(), &cfg); err != nil {
		if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTTY) {
			return nil, err
		}
		if err := loopConfigureLegacy(dev.fd(), &cfg); err != nil {
			return nil, err
		}
	}
	if err := verifyLoopConfig(dev, &cfg); err != nil {
		_ = unix.IoctlSetInt(dev.fd(), unix.LOOP_CLR_FD, 0)
		return nil, err
	}

	dev.diskseq = blockDiskseq(dev.f, name)
	if err := unix.Flock(int(dev.lock.Fd()), unix.LOCK_SH); err != nil {
		_ = unix.IoctlSetInt(dev.fd(), unix.LOOP_CLR_FD, 0)
		return nil, fmt.Errorf("downgrading lock on %s: %w", path, err)
	}
	ok = true
	return dev, nil
}

// loopConfig builds the LOOP_CONFIGURE argument. Partition scanning is never
// requested here: like systemd, it is enabled afterwards (enablePartscan) to
// avoid a kernel race in which a concurrent opener triggers a first scan and
// the second one briefly drops all partitions.
func loopConfig(fd *os.File, backingPath string, blockSize uint32, directIO bool) unix.LoopConfig {
	cfg := unix.LoopConfig{Fd: uint32(fd.Fd()), Size: blockSize}
	cfg.Info.Flags = unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR
	if directIO {
		cfg.Info.Flags |= unix.LO_FLAGS_DIRECT_IO
	}
	setLoopName(&cfg.Info, backingPath)
	return cfg
}

// loopConfigureLegacy is the LOOP_SET_FD + LOOP_SET_STATUS64 fallback for
// kernels without LOOP_CONFIGURE.
func loopConfigureLegacy(fd int, cfg *unix.LoopConfig) error {
	if err := unix.IoctlSetInt(fd, unix.LOOP_SET_FD, int(cfg.Fd)); err != nil {
		return err
	}
	info := cfg.Info
	info.Flags &= unix.LO_FLAGS_AUTOCLEAR | unix.LO_FLAGS_PARTSCAN
	var err error
	for range 64 {
		if err = unix.IoctlLoopSetStatus64(fd, &info); !errors.Is(err, unix.EAGAIN) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil {
		_ = unix.IoctlSetInt(fd, unix.LOOP_SET_BLOCK_SIZE, int(cfg.Size))
		if cfg.Info.Flags&unix.LO_FLAGS_DIRECT_IO != 0 {
			_ = unix.IoctlSetInt(fd, unix.LOOP_SET_DIRECT_IO, 1)
		}
	}
	if err != nil {
		_ = unix.IoctlSetInt(fd, unix.LOOP_CLR_FD, 0)
	}
	return err
}

// verifyLoopConfig checks that the kernel honoured the requested block size
// and direct I/O mode (ENOANO asks the caller to retry without it).
func verifyLoopConfig(dev *loopDevice, cfg *unix.LoopConfig) error {
	ssz, err := unix.IoctlGetInt(dev.fd(), unix.BLKSSZGET)
	if err != nil {
		return fmt.Errorf("BLKSSZGET %s: %w", dev.path, err)
	}
	if uint32(ssz) != cfg.Size {
		if err = unix.IoctlSetInt(dev.fd(), unix.LOOP_SET_BLOCK_SIZE, int(cfg.Size)); err == nil {
			ssz, err = unix.IoctlGetInt(dev.fd(), unix.BLKSSZGET)
		}
		if err != nil || uint32(ssz) != cfg.Size {
			return fmt.Errorf("%s: sector size %d instead of the requested %d", dev.path, ssz, cfg.Size)
		}
	}
	if cfg.Info.Flags&unix.LO_FLAGS_DIRECT_IO != 0 {
		info, err := unix.IoctlLoopGetStatus64(dev.fd())
		if err != nil {
			return fmt.Errorf("LOOP_GET_STATUS64 %s: %w", dev.path, err)
		}
		if info.Flags&unix.LO_FLAGS_DIRECT_IO == 0 {
			return unix.ENOANO
		}
	}
	return nil
}

// enablePartscan turns on partition scanning after configuration, after
// draining the pending media change with a throwaway open.
func (l *loopDevice) enablePartscan() error {
	if tmp, err := os.OpenFile(l.path, os.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0); err == nil {
		tmp.Close()
	}
	info, err := unix.IoctlLoopGetStatus64(l.fd())
	if err != nil {
		return fmt.Errorf("LOOP_GET_STATUS64 %s: %w", l.path, err)
	}
	info.Flags |= unix.LO_FLAGS_PARTSCAN
	if err := unix.IoctlLoopSetStatus64(l.fd(), info); err != nil {
		return fmt.Errorf("enabling partition scanning on %s: %w", l.path, err)
	}
	return nil
}

func (l *loopDevice) fd() int { return int(l.f.Fd()) }

func (l *loopDevice) close() {
	if l.lock != nil {
		l.lock.Close()
		l.lock = nil
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// relinquish drops our references; LO_FLAGS_AUTOCLEAR detaches the device
// once whatever was set up on top of it is gone.
func (l *loopDevice) relinquish() { l.close() }

// detach explicitly unbinds the device (error paths before relinquish).
func (l *loopDevice) detach() {
	if l.f != nil && !l.borrowed {
		_ = unix.IoctlSetInt(l.fd(), unix.LOOP_CLR_FD, 0)
	}
	l.close()
}

// openBlockDevice is loop_device_open_from_fd(): the block device of an
// image, used in place of a loop device, with a shared BSD lock like
// systemd takes.
func openBlockDevice(f *os.File) (*loopDevice, error) {
	dir, err := blockSysDir(f)
	if err != nil {
		return nil, err
	}
	l := &loopDevice{path: f.Name(), name: filepath.Base(dir), borrowed: true}
	if l.f, err = reopenFile(f, unix.O_RDONLY|unix.O_NONBLOCK); err != nil {
		return nil, err
	}
	if l.lock, err = reopenFile(f, unix.O_RDONLY|unix.O_NONBLOCK); err != nil {
		l.close()
		return nil, err
	}
	if err := unix.Flock(int(l.lock.Fd()), unix.LOCK_SH); err != nil {
		l.close()
		return nil, fmt.Errorf("locking %s: %w", l.path, err)
	}
	l.diskseq = blockDiskseq(l.f, l.name)
	return l, nil
}

// blockSysDir returns the sysfs directory of the block device f.
func blockSysDir(f *os.File) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	link := fmt.Sprintf("%s/%d:%d", sysDevBlock, unix.Major(st.Rdev), unix.Minor(st.Rdev))
	dir, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", fmt.Errorf("%s: no sysfs entry for the block device: %w", f.Name(), err)
	}
	return dir, nil
}

// partscanEnabled is blockdev_partscan_enabled() for kernels that report
// it in "partscan" (6.10 and later) or through "ext_range" (5.17 and
// later): partitions have no partition scanning, loop devices only with
// LO_FLAGS_PARTSCAN. When nothing tells, scanning is taken as off.
func partscanEnabled(sysDir string) bool {
	if v, err := readSysUint(filepath.Join(sysDir, "partscan")); err == nil {
		return v != 0
	}
	if _, err := os.Stat(filepath.Join(sysDir, "partition")); err == nil {
		return false
	}
	if v, err := readSysUint(filepath.Join(sysDir, "loop", "partscan")); err == nil && v == 0 {
		return false
	}
	n, err := readSysUint(filepath.Join(sysDir, "ext_range"))
	return err == nil && n > 1
}

// blockDeviceGeometry returns the size in bytes and the logical sector
// size of the block device f.
func blockDeviceGeometry(f *os.File) (size, sectorSize int64, err error) {
	var n uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.BLKGETSIZE64), uintptr(unsafe.Pointer(&n))); errno != 0 {
		return 0, 0, fmt.Errorf("BLKGETSIZE64: %w", errno)
	}
	ss, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKSSZGET)
	if err != nil {
		return 0, 0, fmt.Errorf("BLKSSZGET: %w", err)
	}
	if n > math.MaxInt64 || ss <= 0 {
		return 0, 0, fmt.Errorf("implausible block device geometry (%d bytes, %d byte sectors)", n, ss)
	}
	return int64(n), int64(ss), nil
}

// partitionName is the kernel's name for partition n of disk: a "p"
// separates the number from disk names ending in a digit.
func partitionName(disk string, n int) string {
	if disk != "" && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return disk + "p" + strconv.Itoa(n)
	}
	return disk + strconv.Itoa(n)
}

func setLoopName(info *unix.LoopInfo64, path string) {
	n := copy(info.File_name[:len(info.File_name)-1], path)
	clear(info.File_name[n:])
}

// reopenFile opens the inode behind f again with different flags, like
// systemd's fd_reopen().
func reopenFile(f *os.File, flags int) (*os.File, error) {
	return os.OpenFile("/proc/self/fd/"+strconv.Itoa(int(f.Fd())), flags|unix.O_CLOEXEC, 0)
}

// blockDiskseq returns the disk sequence number of a whole block device
// (0 when the kernel does not provide one).
func blockDiskseq(f *os.File, name string) uint64 {
	var seq uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.BLKGETDISKSEQ), uintptr(unsafe.Pointer(&seq)))
	if errno == 0 {
		return seq
	}
	if data, err := os.ReadFile(filepath.Join(sysBlock, name, "diskseq")); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// removeStalePartitions deletes partition devices left on an unbound loop
// device (seen on some kernels); removed=true asks for a retry.
func removeStalePartitions(dev *loopDevice) (bool, error) {
	matches, _ := filepath.Glob(filepath.Join(sysBlock, dev.name, dev.name+"p*"))
	for _, m := range matches {
		pno, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), dev.name+"p"))
		if err != nil {
			continue
		}
		if err := blkpg(dev.fd(), unix.BLKPG_DEL_PARTITION, pno, 0, 0); err != nil && !errors.Is(err, unix.ENXIO) {
			return false, fmt.Errorf("removing stale partition %d of %s: %w", pno, dev.path, err)
		}
	}
	return len(matches) > 0, nil
}

func blkpg(fd, op, pno int, start, length int64) error {
	part := unix.BlkpgPartition{Start: start, Length: length, Pno: int32(pno)}
	arg := unix.BlkpgIoctlArg{
		Op:      int32(op),
		Datalen: int32(unsafe.Sizeof(part)),
		Data:    (*byte)(unsafe.Pointer(&part)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.BLKPG, uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		return errno
	}
	return nil
}

// partitionNode returns the device node of partition p of the device,
// adding the partition with BLKPG_ADD_PARTITION when the kernel's own scan
// did not (EBUSY means it raced us and won), and refusing when the kernel's
// partition does not cover exactly the parsed extent.
func (l *loopDevice) partitionNode(p partition, ss int64) (string, error) {
	pname := partitionName(l.name, p.Index)
	sysDir := filepath.Join(sysBlock, l.name, pname)
	if _, err := os.Stat(sysDir); errors.Is(err, os.ErrNotExist) {
		if err := blkpg(l.fd(), unix.BLKPG_ADD_PARTITION, p.Index, p.offset(ss), p.size(ss)); err != nil && !errors.Is(err, unix.EBUSY) {
			return "", fmt.Errorf("adding partition %d to %s: %w", p.Index, l.path, err)
		}
	}
	start, err1 := readSysUint(filepath.Join(sysDir, "start"))
	sectors, err2 := readSysUint(filepath.Join(sysDir, "size"))
	if err := errors.Join(err1, err2); err != nil {
		return "", fmt.Errorf("partition %d of %s not available: %w", p.Index, l.path, err)
	}
	if int64(start)*512 != p.offset(ss) || int64(sectors)*512 != p.size(ss) {
		return "", fmt.Errorf("partition %d of %s spans sectors %d+%d, partition table says bytes %d+%d",
			p.Index, l.path, start, sectors, p.offset(ss), p.size(ss))
	}
	node := "/dev/" + pname
	if err := ensureBlockNode(node, filepath.Join(sysDir, "dev")); err != nil {
		return "", err
	}
	return node, nil
}

func readSysUint(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

// ensureBlockNode makes path a block device node with the dev_t published
// in sysDevFile. sysfs is authoritative: without udev nothing removes nodes
// from earlier attach cycles, and with loop.max_part the minor numbers are
// not simply the device number.
func ensureBlockNode(path, sysDevFile string) error {
	data, err := os.ReadFile(sysDevFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", sysDevFile, err)
	}
	major, minor, err := parseDevT(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("parsing %s: %w", sysDevFile, err)
	}
	return ensureNode(path, unix.S_IFBLK, unix.Mkdev(major, minor))
}

// ensureNode creates or replaces path so that it is a device node of the
// given type and number.
func ensureNode(path string, typ uint32, dev uint64) error {
	var st unix.Stat_t
	switch err := unix.Stat(path, &st); {
	case err == nil:
		if st.Mode&unix.S_IFMT == typ && st.Rdev == dev {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing stale node %s: %w", path, err)
		}
	case errors.Is(err, unix.ENOENT):
	default:
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := unix.Mknod(path, typ|0o600, int(dev)); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("mknod %s: %w", path, err)
	}
	return nil
}

// parseDevT parses sysfs "MAJOR:MINOR".
func parseDevT(s string) (major, minor uint32, err error) {
	maj, mnr, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("malformed dev_t %q", s)
	}
	m1, err := strconv.ParseUint(maj, 10, 32)
	if err != nil {
		return 0, 0, err
	}
	m2, err := strconv.ParseUint(mnr, 10, 32)
	if err != nil {
		return 0, 0, err
	}
	return uint32(m1), uint32(m2), nil
}

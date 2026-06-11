package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	loopControlPath = "/dev/loop-control"
	loopMajor       = 7
	// loopAttachRetries bounds retries when racing other processes for a
	// free loop device (LOOP_CTL_GET_FREE is not a reservation).
	loopAttachRetries = 10
)

// loopAttach attaches path to a free loop device read-only (with autoclear,
// and partition scanning when partscan is true) and returns the device path
// (e.g. /dev/loop3).
func loopAttach(path string, partscan bool) (string, error) {
	backing, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("opening backing file: %w", err)
	}
	defer backing.Close()

	ctl, err := os.OpenFile(loopControlPath, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", loopControlPath, err)
	}
	defer ctl.Close()

	flags := uint32(unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR)
	if partscan {
		flags |= unix.LO_FLAGS_PARTSCAN
	}

	var lastErr error
	for attempt := 0; attempt < loopAttachRetries; attempt++ {
		num, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return "", fmt.Errorf("LOOP_CTL_GET_FREE: %w", err)
		}

		devPath := fmt.Sprintf("/dev/loop%d", num)
		if err := ensureLoopNode(devPath, num); err != nil {
			return "", err
		}

		dev, err := os.OpenFile(devPath, os.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			lastErr = fmt.Errorf("opening %s: %w", devPath, err)
			continue
		}

		err = configureLoop(int(dev.Fd()), int(backing.Fd()), path, flags)
		dev.Close()
		if err == nil {
			return devPath, nil
		}
		if errors.Is(err, unix.EBUSY) {
			// Another process grabbed the device between GET_FREE and
			// configure: try again with a fresh device.
			lastErr = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return "", fmt.Errorf("attaching %s to %s: %w", path, devPath, err)
	}
	return "", fmt.Errorf("attaching %s: no free loop device after %d attempts: %w", path, loopAttachRetries, lastErr)
}

// configureLoop binds backingFd to the loop device via LOOP_CONFIGURE,
// falling back to LOOP_SET_FD + LOOP_SET_STATUS64 on kernels without
// LOOP_CONFIGURE (< 5.8, EINVAL).
func configureLoop(loopFd, backingFd int, backingPath string, flags uint32) error {
	cfg := unix.LoopConfig{
		Fd: uint32(backingFd),
	}
	cfg.Info.Flags = flags
	setLoopName(&cfg.Info, backingPath)

	err := unix.IoctlLoopConfigure(loopFd, &cfg)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EINVAL) {
		return err
	}

	// Legacy two-step path.
	if err := unix.IoctlSetInt(loopFd, unix.LOOP_SET_FD, backingFd); err != nil {
		return err
	}
	info := unix.LoopInfo64{Flags: flags}
	setLoopName(&info, backingPath)
	if err := unix.IoctlLoopSetStatus64(loopFd, &info); err != nil {
		// Undo the bind so the device is not left half-configured.
		_ = unix.IoctlSetInt(loopFd, unix.LOOP_CLR_FD, 0)
		return err
	}
	return nil
}

func setLoopName(info *unix.LoopInfo64, path string) {
	name := path
	if len(name) > len(info.File_name)-1 {
		name = name[:len(info.File_name)-1]
	}
	copy(info.File_name[:], name)
}

// ensureLoopNode creates /dev/loopN if it does not exist. On systems without
// udev (Alpine without mdev triggers) the node is not created automatically
// after LOOP_CTL_GET_FREE extends the loop range.
func ensureLoopNode(devPath string, num int) error {
	if _, err := os.Stat(devPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	err := unix.Mknod(devPath, unix.S_IFBLK|0o660, int(unix.Mkdev(loopMajor, uint32(num))))
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("mknod %s: %w", devPath, err)
	}
	return nil
}

// ensurePartitionNode returns the device path for partition index of loopDev
// (/dev/loopNpM), creating the node via mknod from the dev_t published in
// sysfs when udev has not created it. It waits briefly for the kernel's
// partition scan to publish the sysfs entry.
func ensurePartitionNode(loopDev string, index int) (string, error) {
	partDev := fmt.Sprintf("%sp%d", loopDev, index)
	if _, err := os.Stat(partDev); err == nil {
		return partDev, nil
	}

	loopName := filepath.Base(loopDev)
	sysDev := fmt.Sprintf("/sys/block/%s/%sp%d/dev", loopName, loopName, index)

	var data []byte
	var err error
	for i := 0; i < 100; i++ { // up to ~1s for partscan to settle
		data, err = os.ReadFile(sysDev)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return "", fmt.Errorf("partition %d of %s not published by kernel (%s): %w", index, loopDev, sysDev, err)
	}

	major, minor, err := parseDevT(strings.TrimSpace(string(data)))
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", sysDev, err)
	}
	if err := unix.Mknod(partDev, unix.S_IFBLK|0o660, int(unix.Mkdev(major, minor))); err != nil && !errors.Is(err, unix.EEXIST) {
		return "", fmt.Errorf("mknod %s: %w", partDev, err)
	}
	return partDev, nil
}

// parseDevT parses sysfs "MAJOR:MINOR".
func parseDevT(s string) (major, minor uint32, err error) {
	maj, min, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("malformed dev_t %q", s)
	}
	m1, err := strconv.ParseUint(maj, 10, 32)
	if err != nil {
		return 0, 0, err
	}
	m2, err := strconv.ParseUint(min, 10, 32)
	if err != nil {
		return 0, 0, err
	}
	return uint32(m1), uint32(m2), nil
}

// loopDetach detaches the loop device via LOOP_CLR_FD. Already-detached
// devices are not an error.
func loopDetach(devPath string) error {
	dev, err := os.OpenFile(devPath, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENXIO) {
			return nil
		}
		return fmt.Errorf("opening %s: %w", devPath, err)
	}
	defer dev.Close()

	err = unix.IoctlSetInt(int(dev.Fd()), unix.LOOP_CLR_FD, 0)
	if err != nil && !errors.Is(err, unix.ENXIO) {
		// ENXIO: no backing file bound (already detached).
		return fmt.Errorf("LOOP_CLR_FD %s: %w", devPath, err)
	}
	return nil
}

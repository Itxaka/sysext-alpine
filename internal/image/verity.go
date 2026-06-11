package image

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// dm-verity activation for GPT DDIs, per the UAPI Discoverable Partitions
// Specification: the verity partition carries a veritysetup superblock +
// hash tree, and the verity root hash is reconstructed from the unique
// partition GUIDs (data partition GUID = first 128 bits, verity partition
// GUID = last 128 bits, both in canonical textual form with dashes
// stripped).
//
// The device-mapper device is created by talking to /dev/mapper/control
// directly (ioctl, manually packed buffers) — no libdevmapper, no udev.

// ---------------------------------------------------------------------------
// Verity superblock (written by veritysetup format at hash device offset 0)
// ---------------------------------------------------------------------------

const (
	veritySBSize  = 344 // superblock fields up to and including salt
	verityMagic   = "verity\x00\x00"
	verityMaxSalt = 256

	// Field offsets within the superblock.
	vsbOffVersion       = 8  // u32 LE
	vsbOffHashType      = 12 // u32 LE
	vsbOffUUID          = 16 // 16 bytes
	vsbOffAlgorithm     = 32 // 32 bytes, NUL-padded
	vsbOffDataBlockSize = 64 // u32 LE
	vsbOffHashBlockSize = 68 // u32 LE
	vsbOffDataBlocks    = 72 // u64 LE
	vsbOffSaltSize      = 80 // u16 LE
	vsbOffSalt          = 88 // 256 bytes (salt_size used)
)

// veritySuperblock is the parsed on-disk veritysetup superblock.
type veritySuperblock struct {
	Version       uint32
	HashType      uint32
	UUID          [16]byte
	Algorithm     string
	DataBlockSize uint32
	HashBlockSize uint32
	DataBlocks    uint64
	Salt          []byte
}

// parseVeritySuperblock parses the superblock from raw bytes (at least
// veritySBSize long).
func parseVeritySuperblock(b []byte) (*veritySuperblock, error) {
	if len(b) < veritySBSize {
		return nil, fmt.Errorf("verity superblock truncated (%d bytes)", len(b))
	}
	if string(b[:8]) != verityMagic {
		return nil, errors.New("no verity superblock magic")
	}

	sb := &veritySuperblock{
		Version:       binary.LittleEndian.Uint32(b[vsbOffVersion:]),
		HashType:      binary.LittleEndian.Uint32(b[vsbOffHashType:]),
		DataBlockSize: binary.LittleEndian.Uint32(b[vsbOffDataBlockSize:]),
		HashBlockSize: binary.LittleEndian.Uint32(b[vsbOffHashBlockSize:]),
		DataBlocks:    binary.LittleEndian.Uint64(b[vsbOffDataBlocks:]),
	}
	copy(sb.UUID[:], b[vsbOffUUID:vsbOffUUID+16])

	alg := b[vsbOffAlgorithm : vsbOffAlgorithm+32]
	if i := indexByte(alg, 0); i >= 0 {
		alg = alg[:i]
	}
	sb.Algorithm = string(alg)

	saltSize := binary.LittleEndian.Uint16(b[vsbOffSaltSize:])
	if int(saltSize) > verityMaxSalt {
		return nil, fmt.Errorf("verity superblock salt size %d exceeds maximum %d", saltSize, verityMaxSalt)
	}
	sb.Salt = append([]byte(nil), b[vsbOffSalt:vsbOffSalt+int(saltSize)]...)

	if sb.Version != 1 {
		return nil, fmt.Errorf("unsupported verity superblock version %d", sb.Version)
	}
	if sb.HashType != 1 {
		return nil, fmt.Errorf("unsupported verity hash type %d", sb.HashType)
	}
	if sb.DataBlockSize == 0 || sb.HashBlockSize == 0 ||
		sb.DataBlockSize%512 != 0 || sb.HashBlockSize%512 != 0 {
		return nil, fmt.Errorf("invalid verity block sizes %d/%d", sb.DataBlockSize, sb.HashBlockSize)
	}
	if sb.DataBlocks == 0 {
		return nil, errors.New("verity superblock declares zero data blocks")
	}
	return sb, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// readVeritySuperblock reads and parses the superblock from the start of
// the verity partition device.
func readVeritySuperblock(dev string) (*veritySuperblock, error) {
	f, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, veritySBSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("reading verity superblock from %s: %w", dev, err)
	}
	sb, err := parseVeritySuperblock(buf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dev, err)
	}
	return sb, nil
}

// rootHashFromGUIDs reconstructs the verity root hash from the unique
// partition GUIDs per the Discoverable Partitions Specification: the data
// partition's unique GUID holds the first 128 bits, the verity partition's
// the last 128 bits — both taken in canonical textual (big-endian display)
// form, i.e. exactly what guidString() returns, dashes stripped.
func rootHashFromGUIDs(dataGUID, verityGUID string) (string, error) {
	a := strings.ReplaceAll(dataGUID, "-", "")
	b := strings.ReplaceAll(verityGUID, "-", "")
	if len(a) != 32 || len(b) != 32 {
		return "", fmt.Errorf("malformed partition GUIDs %q / %q", dataGUID, verityGUID)
	}
	h := a + b
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("partition GUIDs do not form a hex root hash: %w", err)
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// device-mapper ioctl plumbing
// ---------------------------------------------------------------------------

// Layout constants from /usr/include/linux/dm-ioctl.h. struct dm_ioctl is
// 312 bytes; the target spec header is 40 bytes and the parameter string
// after it is NUL-terminated and padded to 8 bytes.
const (
	dmIoctlSize      = 312
	dmTargetSpecSize = 40
	dmNameLen        = 128
	dmUUIDLen        = 129
	dmMaxTypeName    = 16

	// Offsets within struct dm_ioctl.
	dmOffVersion     = 0   // __u32[3]
	dmOffDataSize    = 12  // __u32
	dmOffDataStart   = 16  // __u32
	dmOffTargetCount = 20  // __u32
	dmOffOpenCount   = 24  // __s32
	dmOffFlags       = 28  // __u32
	dmOffEventNr     = 32  // __u32
	dmOffPadding     = 36  // __u32
	dmOffDev         = 40  // __u64
	dmOffName        = 48  // char[128]
	dmOffUUID        = 176 // char[129]
	dmOffData        = 305 // char[7]

	// Offsets within struct dm_target_spec.
	dmSpecOffSectorStart = 0  // __u64
	dmSpecOffLength      = 8  // __u64
	dmSpecOffStatus      = 16 // __s32
	dmSpecOffNext        = 20 // __u32
	dmSpecOffTargetType  = 24 // char[16]

	// _IOWR(0xfd, cmd, struct dm_ioctl) with sizeof(struct dm_ioctl)==312:
	// 0xc138fd00 | cmd.
	dmDevCreate  = 0xc138fd03 // DM_DEV_CREATE
	dmDevRemove  = 0xc138fd04 // DM_DEV_REMOVE
	dmDevSuspend = 0xc138fd06 // DM_DEV_SUSPEND (resume when DM_SUSPEND_FLAG unset)
	dmTableLoad  = 0xc138fd09 // DM_TABLE_LOAD

	dmReadonlyFlag = 1 << 0 // DM_READONLY_FLAG

	dmVersionMajor = 4
	dmControlPath  = "/dev/mapper/control"
	dmDir          = "/dev/mapper"
	dmMiscMajor    = 10
)

// dmIoctlHeader packs a struct dm_ioctl with the given name, total buffer
// size and flags into buf (which must be at least dmIoctlSize long).
func dmIoctlHeader(buf []byte, name string, dataSize, flags uint32) error {
	if len(name) >= dmNameLen {
		return fmt.Errorf("device-mapper name %q too long", name)
	}
	binary.LittleEndian.PutUint32(buf[dmOffVersion:], dmVersionMajor)
	binary.LittleEndian.PutUint32(buf[dmOffVersion+4:], 0)
	binary.LittleEndian.PutUint32(buf[dmOffVersion+8:], 0)
	binary.LittleEndian.PutUint32(buf[dmOffDataSize:], dataSize)
	binary.LittleEndian.PutUint32(buf[dmOffDataStart:], dmIoctlSize)
	binary.LittleEndian.PutUint32(buf[dmOffFlags:], flags)
	copy(buf[dmOffName:dmOffName+dmNameLen], name)
	return nil
}

// dmSimpleIoctl issues a parameterless dm ioctl (create/remove/resume) for
// the named device and returns the reply buffer.
func dmSimpleIoctl(ctl *os.File, cmd uintptr, name string, flags uint32) ([]byte, error) {
	buf := make([]byte, dmIoctlSize)
	if err := dmIoctlHeader(buf, name, dmIoctlSize, flags); err != nil {
		return nil, err
	}
	if err := dmIoctl(ctl, cmd, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func dmIoctl(ctl *os.File, cmd uintptr, buf []byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, ctl.Fd(), cmd, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

// dmVerityTable packs a complete DM_TABLE_LOAD buffer for a single verity
// target: struct dm_ioctl + struct dm_target_spec + NUL-terminated params,
// padded to 8 bytes.
func dmVerityTable(name string, lengthSectors uint64, params string) ([]byte, error) {
	paramsLen := (len(params) + 1 + 7) &^ 7 // NUL + pad to 8
	total := dmIoctlSize + dmTargetSpecSize + paramsLen

	buf := make([]byte, total)
	if err := dmIoctlHeader(buf, name, uint32(total), dmReadonlyFlag); err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(buf[dmOffTargetCount:], 1)

	spec := buf[dmIoctlSize:]
	binary.LittleEndian.PutUint64(spec[dmSpecOffSectorStart:], 0)
	binary.LittleEndian.PutUint64(spec[dmSpecOffLength:], lengthSectors)
	binary.LittleEndian.PutUint32(spec[dmSpecOffNext:], uint32(dmTargetSpecSize+paramsLen))
	copy(spec[dmSpecOffTargetType:dmSpecOffTargetType+dmMaxTypeName], "verity")
	copy(spec[dmTargetSpecSize:], params)
	return buf, nil
}

// dmDecodeDev decodes the __u64 dev field of a dm_ioctl reply (kernel
// huge_encode_dev/new_encode_dev format) into major/minor.
func dmDecodeDev(dev uint64) (major, minor uint32) {
	major = uint32((dev >> 8) & 0xfff)
	minor = uint32(dev&0xff) | uint32((dev>>12)&0xfff00)
	return major, minor
}

// ensureDMControl makes sure /dev/mapper/control exists (no udev on Alpine:
// create the misc-device node from /proc/misc) and opens it.
func ensureDMControl() (*os.File, error) {
	f, err := os.OpenFile(dmControlPath, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("opening %s: %w", dmControlPath, err)
	}

	minor, merr := dmMiscMinor()
	if merr != nil {
		return nil, fmt.Errorf("%s missing and device-mapper minor unknown: %w", dmControlPath, merr)
	}
	if err := os.MkdirAll(dmDir, 0o755); err != nil {
		return nil, err
	}
	err = unix.Mknod(dmControlPath, unix.S_IFCHR|0o600, int(unix.Mkdev(dmMiscMajor, minor)))
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("mknod %s: %w", dmControlPath, err)
	}
	f, err = os.OpenFile(dmControlPath, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dmControlPath, err)
	}
	return f, nil
}

// dmMiscMinor finds the misc-device minor of device-mapper in /proc/misc.
func dmMiscMinor() (uint32, error) {
	f, err := os.Open("/proc/misc")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == "device-mapper" {
			n, err := strconv.ParseUint(fields[0], 10, 32)
			if err != nil {
				return 0, err
			}
			return uint32(n), nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("device-mapper not registered in /proc/misc (dm modules not loaded?)")
}

// ---------------------------------------------------------------------------
// Activation
// ---------------------------------------------------------------------------

// verityDeviceName derives the device-mapper name for an image:
// "sysext-<imagename>-verity", sanitized to dm-safe characters.
func verityDeviceName(imagePath string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(
		imagePath[strings.LastIndexByte(imagePath, '/')+1:], ".raw"), ".img")
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := "sysext-" + b.String() + "-verity"
	if len(name) >= dmNameLen {
		name = name[:dmNameLen-1]
	}
	return name
}

// verityParams builds the dm verity target parameter string:
// "<version> <datadev> <hashdev> <data_block_size> <hash_block_size>
//
//	<data_blocks> <hash_start_block> <algorithm> <roothash> <salt-hex>"
//
// hash_start_block is 1: the superblock occupies hash block 0 and the hash
// tree starts at the next hash block.
func verityParams(sb *veritySuperblock, dataDev, hashDev, rootHash string) string {
	salt := "-"
	if len(sb.Salt) > 0 {
		salt = hex.EncodeToString(sb.Salt)
	}
	return fmt.Sprintf("%d %s %s %d %d %d 1 %s %s %s",
		sb.Version, dataDev, hashDev,
		sb.DataBlockSize, sb.HashBlockSize, sb.DataBlocks,
		sb.Algorithm, rootHash, salt)
}

// verityActivate creates a read-only dm-verity device named name backed by
// dataDev/hashDev and returns the /dev/mapper node path (creating the node
// when udev is not around to do it).
func verityActivate(name string, sb *veritySuperblock, dataDev, hashDev, rootHash string) (string, error) {
	lengthSectors := sb.DataBlocks * uint64(sb.DataBlockSize) / 512

	ctl, err := ensureDMControl()
	if err != nil {
		return "", err
	}
	defer ctl.Close()

	if _, err := dmSimpleIoctl(ctl, dmDevCreate, name, 0); err != nil {
		if errors.Is(err, unix.EBUSY) {
			// Stale device from a previous run: remove and retry once.
			if _, rerr := dmSimpleIoctl(ctl, dmDevRemove, name, 0); rerr != nil {
				return "", fmt.Errorf("DM_DEV_CREATE %s: %w (stale device removal also failed: %v)", name, err, rerr)
			}
			if _, err = dmSimpleIoctl(ctl, dmDevCreate, name, 0); err != nil {
				return "", fmt.Errorf("DM_DEV_CREATE %s: %w", name, err)
			}
		} else {
			return "", fmt.Errorf("DM_DEV_CREATE %s: %w", name, err)
		}
	}

	fail := func(stage string, err error) (string, error) {
		_, _ = dmSimpleIoctl(ctl, dmDevRemove, name, 0)
		return "", fmt.Errorf("%s %s: %w", stage, name, err)
	}

	table, err := dmVerityTable(name, lengthSectors, verityParams(sb, dataDev, hashDev, rootHash))
	if err != nil {
		return fail("building table for", err)
	}
	if err := dmIoctl(ctl, dmTableLoad, table); err != nil {
		return fail("DM_TABLE_LOAD", err)
	}

	// DM_DEV_SUSPEND without DM_SUSPEND_FLAG = resume: swaps the inactive
	// table in and brings the device live.
	reply, err := dmSimpleIoctl(ctl, dmDevSuspend, name, 0)
	if err != nil {
		return fail("DM_DEV_RESUME", err)
	}

	major, minor := dmDecodeDev(binary.LittleEndian.Uint64(reply[dmOffDev:]))
	devPath, err := ensureDMNode(name, major, minor)
	if err != nil {
		_, _ = dmSimpleIoctl(ctl, dmDevRemove, name, 0)
		return "", err
	}
	return devPath, nil
}

// ensureDMNode creates /dev/mapper/<name> for the given dev numbers when
// udev has not (replacing a stale node if necessary).
func ensureDMNode(name string, major, minor uint32) (string, error) {
	path := dmDir + "/" + name
	want := unix.Mkdev(major, minor)

	var st unix.Stat_t
	switch err := unix.Stat(path, &st); {
	case err == nil:
		if st.Mode&unix.S_IFMT == unix.S_IFBLK && st.Rdev == want {
			return path, nil
		}
		if err := os.Remove(path); err != nil {
			return "", fmt.Errorf("removing stale node %s: %w", path, err)
		}
	case errors.Is(err, unix.ENOENT):
		if err := os.MkdirAll(dmDir, 0o755); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("stat %s: %w", path, err)
	}

	if err := unix.Mknod(path, unix.S_IFBLK|0o600, int(want)); err != nil && !errors.Is(err, unix.EEXIST) {
		return "", fmt.Errorf("mknod %s: %w", path, err)
	}
	return path, nil
}

// dmDeferredRemoveFlag requests removal once the last opener goes away
// (DM_DEFERRED_REMOVE in dm-ioctl.h).
const dmDeferredRemoveFlag = 1 << 17

// verityRemove tears down the named dm device. Missing devices are not an
// error. EBUSY is retried briefly — unmount(2) releases the device via a
// delayed fput, so the open count can lag the unmount — and falls back to a
// kernel deferred remove (drops the device as soon as the last reference is
// gone). The /dev/mapper node is removed best-effort.
func verityRemove(name string) error {
	ctl, err := ensureDMControl()
	if err != nil {
		return err
	}
	defer ctl.Close()

	for attempt := 0; ; attempt++ {
		_, err = dmSimpleIoctl(ctl, dmDevRemove, name, 0)
		if err == nil || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENODEV) {
			break
		}
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("DM_DEV_REMOVE %s: %w", name, err)
		}
		if attempt >= 20 { // ~1s of retries, then hand off to the kernel
			if _, derr := dmSimpleIoctl(ctl, dmDevRemove, name, dmDeferredRemoveFlag); derr != nil &&
				!errors.Is(derr, unix.ENXIO) && !errors.Is(derr, unix.ENODEV) {
				return fmt.Errorf("DM_DEV_REMOVE (deferred) %s: %w", name, derr)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(dmDir + "/" + name)
	return nil
}

package image

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// dm-verity activation without libcryptsetup/libdevmapper/udev: the verity
// superblock written by veritysetup is parsed here and the device-mapper
// device is created with raw ioctls on /dev/mapper/control.

const (
	veritySBSize  = 344 // superblock fields up to and including salt
	verityMagic   = "verity\x00\x00"
	verityMaxSalt = 256

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

// verityDigestSizes are the hash algorithms accepted from a superblock,
// with their digest sizes. The name ends up in the dm table, so anything
// else is refused.
var verityDigestSizes = map[string]int{
	"sha1":   20,
	"sha224": 28,
	"sha256": 32,
	"sha384": 48,
	"sha512": 64,
}

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

func validVerityBlockSize(n uint32) bool {
	return n >= 512 && n <= 1<<20 && n&(n-1) == 0
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
	if i := bytes.IndexByte(alg, 0); i >= 0 {
		alg = alg[:i]
	}
	sb.Algorithm = string(alg)
	saltSize := binary.LittleEndian.Uint16(b[vsbOffSaltSize:])
	if int(saltSize) > verityMaxSalt {
		return nil, fmt.Errorf("verity superblock salt size %d exceeds maximum %d", saltSize, verityMaxSalt)
	}
	sb.Salt = bytes.Clone(b[vsbOffSalt : vsbOffSalt+int(saltSize)])

	if sb.Version != 1 {
		return nil, fmt.Errorf("unsupported verity superblock version %d", sb.Version)
	}
	if sb.HashType > 1 {
		return nil, fmt.Errorf("unsupported verity hash type %d", sb.HashType)
	}
	if _, ok := verityDigestSizes[sb.Algorithm]; !ok {
		return nil, fmt.Errorf("unsupported verity hash algorithm %q", sb.Algorithm)
	}
	if !validVerityBlockSize(sb.DataBlockSize) || !validVerityBlockSize(sb.HashBlockSize) {
		return nil, fmt.Errorf("invalid verity block sizes %d/%d", sb.DataBlockSize, sb.HashBlockSize)
	}
	if sb.DataBlocks == 0 || sb.DataBlocks > (1<<63-1)/uint64(sb.DataBlockSize) {
		return nil, fmt.Errorf("invalid verity data block count %d", sb.DataBlocks)
	}
	return sb, nil
}

// readVeritySuperblock reads the superblock at the start of r.
func readVeritySuperblock(r io.ReaderAt) (*veritySuperblock, error) {
	buf := make([]byte, veritySBSize)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("reading verity superblock: %w", err)
	}
	return parseVeritySuperblock(buf)
}

// checkRootHash verifies that the root hash length matches the digest size
// of the superblock's algorithm.
func (sb *veritySuperblock) checkRootHash(rootHash []byte) error {
	if want := verityDigestSizes[sb.Algorithm]; len(rootHash) != want {
		return fmt.Errorf("root hash is %d bytes, but %s digests are %d bytes", len(rootHash), sb.Algorithm, want)
	}
	return nil
}

// verityParams builds the dm verity target parameter string:
//
//	<hash_type> <data_dev> <hash_dev> <data_block_size> <hash_block_size>
//	<data_blocks> <hash_start_block> <algorithm> <root_hash> <salt>
//	[2 root_hash_sig_key_desc <key description>]
//
// hash_start_block is 1: the superblock occupies hash block 0.
func verityParams(sb *veritySuperblock, dataDev, hashDev string, rootHash []byte, sigKeyDesc string) string {
	salt := "-"
	if len(sb.Salt) > 0 {
		salt = hex.EncodeToString(sb.Salt)
	}
	s := fmt.Sprintf("%d %s %s %d %d %d 1 %s %s %s",
		sb.HashType, dataDev, hashDev,
		sb.DataBlockSize, sb.HashBlockSize, sb.DataBlocks,
		sb.Algorithm, hex.EncodeToString(rootHash), salt)
	if sigKeyDesc != "" {
		s += " 2 root_hash_sig_key_desc " + sigKeyDesc
	}
	return s
}

// verityDMName names the dm device like systemd does without
// DISSECT_IMAGE_VERITY_SHARE: after the data device node and the disk
// sequence number of the loop device, which is unique per attachment.
func verityDMName(dataNode string, diskseq uint64) string {
	base := dataNode[strings.LastIndexByte(dataNode, '/')+1:]
	if diskseq != 0 {
		return base + "-" + strconv.FormatUint(diskseq, 10) + "-verity"
	}
	return base + "-verity"
}

// ---------------------------------------------------------------------------
// device-mapper ioctls
// ---------------------------------------------------------------------------

const (
	dmDir       = "/dev/mapper"
	dmMiscMajor = 10
)

var (
	dmControlPath = "/dev/mapper/control"
	procMiscPath  = "/proc/misc"
)

// dmHeader encodes a struct dm_ioctl into buf in native byte order.
func dmHeader(buf []byte, name, uuid string, dataSize, targets, flags uint32) error {
	if len(name) >= unix.DM_NAME_LEN || strings.ContainsRune(name, '/') {
		return fmt.Errorf("invalid device-mapper name %q", name)
	}
	if len(uuid) >= unix.DM_UUID_LEN {
		return fmt.Errorf("device-mapper uuid %q too long", uuid)
	}
	h := unix.DmIoctl{
		Version:      [3]uint32{unix.DM_VERSION_MAJOR, 0, 0},
		Data_size:    dataSize,
		Data_start:   unix.SizeofDmIoctl,
		Target_count: targets,
		Flags:        flags,
	}
	copy(h.Name[:], name)
	copy(h.Uuid[:], uuid)
	_, err := binary.Encode(buf, binary.NativeEndian, &h)
	return err
}

// dmVerityTable packs a complete DM_TABLE_LOAD buffer for a single verity
// target: struct dm_ioctl, struct dm_target_spec, NUL-terminated params,
// padded to 8 bytes.
func dmVerityTable(name string, lengthSectors uint64, params string) ([]byte, error) {
	paramsLen := (len(params) + 1 + 7) &^ 7
	total := unix.SizeofDmIoctl + unix.SizeofDmTargetSpec + paramsLen
	buf := make([]byte, total)
	if err := dmHeader(buf, name, "", uint32(total), 1, unix.DM_READONLY_FLAG); err != nil {
		return nil, err
	}
	spec := unix.DmTargetSpec{
		Length: lengthSectors,
		Next:   uint32(unix.SizeofDmTargetSpec + paramsLen),
	}
	copy(spec.Target_type[:], "verity")
	if _, err := binary.Encode(buf[unix.SizeofDmIoctl:], binary.NativeEndian, &spec); err != nil {
		return nil, err
	}
	copy(buf[unix.SizeofDmIoctl+unix.SizeofDmTargetSpec:], params)
	return buf, nil
}

func dmIoctl(ctl *os.File, cmd uint, buf []byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, ctl.Fd(), uintptr(cmd), uintptr(unsafe.Pointer(&buf[0])))
	runtime.KeepAlive(buf)
	if errno != 0 {
		return errno
	}
	return nil
}

// dmSimple issues a dm ioctl that carries no payload and returns the reply
// header.
func dmSimple(ctl *os.File, cmd uint, name, uuid string, flags uint32) (*unix.DmIoctl, error) {
	buf := make([]byte, unix.SizeofDmIoctl)
	if err := dmHeader(buf, name, uuid, unix.SizeofDmIoctl, 0, flags); err != nil {
		return nil, err
	}
	if err := dmIoctl(ctl, cmd, buf); err != nil {
		return nil, err
	}
	var reply unix.DmIoctl
	if _, err := binary.Decode(buf, binary.NativeEndian, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// openDMControl opens /dev/mapper/control, creating the node from
// /proc/misc when it is missing (no udev).
func openDMControl() (*os.File, error) {
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
	if err := ensureNode(dmControlPath, unix.S_IFCHR, unix.Mkdev(dmMiscMajor, minor)); err != nil {
		return nil, err
	}
	f, err = os.OpenFile(dmControlPath, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dmControlPath, err)
	}
	return f, nil
}

var errNoDeviceMapper = errors.New("device-mapper not registered in /proc/misc (dm modules not loaded?)")

// dmMiscMinor finds the misc-device minor of device-mapper in /proc/misc.
func dmMiscMinor() (uint32, error) {
	f, err := os.Open(procMiscPath)
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
	return 0, errNoDeviceMapper
}

// verityDevice is an activated dm-verity device.
type verityDevice struct {
	name string
	node string
	// createdNode is set when node was created by us (no udev), so it can
	// be removed again once the device is in use.
	createdNode bool
}

// verityTarget describes a verity device to activate.
type verityTarget struct {
	name     string
	sb       *veritySuperblock
	dataDev  string // "major:minor"
	hashDev  string // "major:minor"
	rootHash []byte
}

// dmUUID follows libcryptsetup: CRYPT-VERITY-<superblock uuid>-<name>.
func (t *verityTarget) dmUUID() string {
	u := "CRYPT-VERITY-" + hex.EncodeToString(t.sb.UUID[:]) + "-" + t.name
	if len(u) >= unix.DM_UUID_LEN {
		u = u[:unix.DM_UUID_LEN-1]
	}
	return u
}

// kernelSignatureError wraps the reason the kernel refused to verify a root
// hash signature itself.
type kernelSignatureError struct{ err error }

func (e *kernelSignatureError) Error() string {
	return "kernel signature verification: " + e.err.Error()
}
func (e *kernelSignatureError) Unwrap() error { return e.err }

// activateVerity creates the dm-verity device. With sig set, the signature
// is first handed to the kernel (root_hash_sig_key_desc, checked against the
// kernel's trusted keyrings); when the kernel refuses, the device is removed
// again and fallback decides whether to create it with a table that carries
// no signature. No device exists while fallback runs, so nothing is left
// behind whatever happens to it.
func activateVerity(t *verityTarget, sig []byte, fallback func(kernelErr error) error) (*verityDevice, error) {
	ctl, err := openDMControl()
	if err != nil {
		return nil, err
	}
	defer ctl.Close()

	if sig != nil {
		dev, err := createVerityDevice(ctl, t, sig)
		kerr, refused := errors.AsType[*kernelSignatureError](err)
		if !refused {
			return dev, err
		}
		if err := fallback(kerr); err != nil {
			return nil, err
		}
	}
	return createVerityDevice(ctl, t, nil)
}

// createVerityDevice creates, loads and resumes the device, with the
// signature in the table when sig is set (a refusal of the kernel is a
// *kernelSignatureError). On error the device is removed again, waiting
// briefly for a transient opener like libdevmapper does, so that its name
// is free for another attempt.
func createVerityDevice(ctl *os.File, t *verityTarget, sig []byte) (*verityDevice, error) {
	if _, err := dmSimple(ctl, unix.DM_DEV_CREATE, t.name, t.dmUUID(), 0); err != nil {
		return nil, fmt.Errorf("DM_DEV_CREATE %s: %w", t.name, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = dmRemove(t.name)
		}
	}()

	length := t.sb.DataBlocks * uint64(t.sb.DataBlockSize) / 512
	if sig != nil {
		if err := loadVerityTableSigned(ctl, t, length, sig); err != nil {
			return nil, &kernelSignatureError{err}
		}
	} else {
		table, err := dmVerityTable(t.name, length, verityParams(t.sb, t.dataDev, t.hashDev, t.rootHash, ""))
		if err != nil {
			return nil, err
		}
		if err := dmIoctl(ctl, unix.DM_TABLE_LOAD, table); err != nil {
			return nil, fmt.Errorf("DM_TABLE_LOAD %s: %w", t.name, err)
		}
	}

	reply, err := dmSimple(ctl, unix.DM_DEV_SUSPEND, t.name, "", 0)
	if err != nil {
		return nil, fmt.Errorf("resuming %s: %w", t.name, err)
	}
	dev := &verityDevice{name: t.name, node: dmDir + "/" + t.name}
	created, err := ensureDMNode(dev.node, unix.Mkdev(unix.Major(reply.Dev), unix.Minor(reply.Dev)))
	if err != nil {
		return nil, err
	}
	dev.createdNode = created
	ok = true
	return dev, nil
}

// loadVerityTableSigned loads the table with the root hash signature in a
// thread keyring, so dm-verity verifies it against the kernel keyrings.
func loadVerityTableSigned(ctl *os.File, t *verityTarget, length uint64, sig []byte) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	desc := "sysext:" + t.name
	key, err := unix.AddKey("user", desc, sig, unix.KEY_SPEC_THREAD_KEYRING)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = unix.KeyctlInt(unix.KEYCTL_UNLINK, key, unix.KEY_SPEC_THREAD_KEYRING, 0, 0)
	}()
	table, err := dmVerityTable(t.name, length, verityParams(t.sb, t.dataDev, t.hashDev, t.rootHash, desc))
	if err != nil {
		return err
	}
	return dmIoctl(ctl, unix.DM_TABLE_LOAD, table)
}

// ensureDMNode makes path a block node for dev; created reports whether we
// made it (as opposed to udev or an earlier run).
func ensureDMNode(path string, dev uint64) (created bool, err error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK && st.Rdev == dev {
		return false, nil
	}
	if err := ensureNode(path, unix.S_IFBLK, dev); err != nil {
		return false, err
	}
	return true, nil
}

// releaseNode removes the /dev/mapper node we created once the device is
// held by a mount; the mount does not need it and nothing else would remove
// it after the kernel drops the device.
func (v *verityDevice) releaseNode() {
	if !v.createdNode {
		return
	}
	var st unix.Stat_t
	if err := unix.Lstat(v.node, &st); err == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK {
		_ = os.Remove(v.node)
	}
	v.createdNode = false
}

// relinquish schedules the device for removal when its last user (the
// mount) goes away, like systemd's dissected_image_relinquish().
func (v *verityDevice) relinquish() error {
	v.releaseNode()
	ctl, err := openDMControl()
	if err != nil {
		return err
	}
	defer ctl.Close()
	if _, err := dmSimple(ctl, unix.DM_DEV_REMOVE, v.name, "", unix.DM_DEFERRED_REMOVE); err != nil {
		return fmt.Errorf("marking %s for deferred removal: %w", v.name, err)
	}
	return nil
}

// remove tears the device down now (error paths).
func (v *verityDevice) remove() {
	v.releaseNode()
	_ = dmRemove(v.name)
}

// dmRemove removes the named dm device. Missing devices and missing
// device-mapper support are not an error. EBUSY is retried briefly (unmount
// releases the device asynchronously) before falling back to a deferred
// removal.
func dmRemove(name string) error {
	if _, err := os.Stat(dmControlPath); errors.Is(err, os.ErrNotExist) {
		if _, merr := dmMiscMinor(); merr != nil {
			return nil
		}
	}
	ctl, err := openDMControl()
	if err != nil {
		return err
	}
	defer ctl.Close()
	for attempt := 0; ; attempt++ {
		_, err = dmSimple(ctl, unix.DM_DEV_REMOVE, name, "", 0)
		if err == nil || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENODEV) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("DM_DEV_REMOVE %s: %w", name, err)
		}
		if attempt >= 20 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := dmSimple(ctl, unix.DM_DEV_REMOVE, name, "", unix.DM_DEFERRED_REMOVE); err != nil &&
		!errors.Is(err, unix.ENXIO) && !errors.Is(err, unix.ENODEV) {
		return fmt.Errorf("DM_DEV_REMOVE (deferred) %s: %w", name, err)
	}
	return nil
}

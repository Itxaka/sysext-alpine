package image

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/errno"
)

// fsType is a filesystem type name as passed to mount(2).
type fsType string

const (
	fsBtrfs    fsType = "btrfs"
	fsErofs    fsType = "erofs"
	fsExt2     fsType = "ext2"
	fsExt3     fsType = "ext3"
	fsExt4     fsType = "ext4"
	fsF2FS     fsType = "f2fs"
	fsSquashfs fsType = "squashfs"
	fsVfat     fsType = "vfat"
	fsXFS      fsType = "xfs"
	fsLUKS     fsType = "crypto_LUKS"
)

// defaultAllowedFSTypes is systemd's allowed_fstypes() default list.
var defaultAllowedFSTypes = []string{"btrfs", "erofs", "ext4", "f2fs", "squashfs", "vfat", "xfs"}

// fsTypeAllowed mirrors dissect_fstype_ok(): only common filesystems are
// mounted automatically, overridable with $SYSTEMD_DISSECT_FILE_SYSTEMS.
func fsTypeAllowed(fs fsType) bool {
	allowed := defaultAllowedFSTypes
	if e, ok := os.LookupEnv("SYSTEMD_DISSECT_FILE_SYSTEMS"); ok {
		allowed = strings.Split(e, ":")
	}
	return slices.Contains(allowed, string(fs))
}

var errAmbiguousFS = errno.New(unix.EUCLEAN, "ambiguous filesystem superblock signatures")

// ext superblock feature bits used to tell ext2/ext3/ext4 apart, as libblkid
// does.
const (
	extCompatHasJournal  = 0x0004
	extIncompatJournalDv = 0x0008
	ext3IncompatSupp     = 0x0002 | 0x0004 | 0x0010 // filetype, recover, meta_bg
	ext3RoCompatSupp     = 0x0001 | 0x0002 | 0x0004 // sparse_super, large_file, btree_dir
)

// probe is one superblock signature check. It returns ok=false when the
// signature does not match; short reads count as no match.
type probe func(r io.ReaderAt) (fsType, bool, error)

var fsProbes = []probe{
	probeSquashfs, probeErofs, probeExt, probeXFS, probeBtrfs, probeF2FS, probeVfat, probeLUKS,
}

// detectFS identifies the filesystem at the start of r. It returns "" when
// nothing is recognized and errAmbiguousFS when several signatures match
// (libblkid's safe probing refuses those).
func detectFS(r io.ReaderAt) (fsType, error) {
	var found fsType
	for _, p := range fsProbes {
		fs, ok, err := p(r)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("%w (%s and %s)", errAmbiguousFS, found, fs)
		}
		found = fs
	}
	return found, nil
}

// readAt reads len(buf) bytes at off; ok=false when the data is too short.
func readAt(r io.ReaderAt, buf []byte, off int64) (bool, error) {
	n, err := r.ReadAt(buf, off)
	if n == len(buf) {
		return true, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return false, nil
	}
	return false, err
}

func probeSquashfs(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 30)
	ok, err := readAt(r, b, 0)
	if !ok || string(b[:4]) != "hsqs" {
		return "", false, err
	}
	return fsSquashfs, binary.LittleEndian.Uint16(b[28:]) == 4, nil
}

func probeErofs(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 16)
	ok, err := readAt(r, b, 1024)
	if !ok || binary.LittleEndian.Uint32(b) != 0xe0f5e1e2 {
		return "", false, err
	}
	bits := b[12]
	return fsErofs, bits >= 9 && bits <= 16, nil
}

func probeExt(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 104)
	ok, err := readAt(r, b, 1024)
	if !ok || binary.LittleEndian.Uint16(b[56:]) != 0xef53 {
		return "", false, err
	}
	if binary.LittleEndian.Uint32(b[24:]) > 6 {
		return "", false, nil
	}
	compat := binary.LittleEndian.Uint32(b[92:])
	incompat := binary.LittleEndian.Uint32(b[96:])
	roCompat := binary.LittleEndian.Uint32(b[100:])
	switch {
	case incompat&extIncompatJournalDv != 0:
		return "", false, nil
	case roCompat&^ext3RoCompatSupp != 0 || incompat&^ext3IncompatSupp != 0:
		return fsExt4, true, nil
	case compat&extCompatHasJournal != 0:
		return fsExt3, true, nil
	default:
		return fsExt2, true, nil
	}
}

func probeXFS(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 8)
	ok, err := readAt(r, b, 0)
	if !ok || string(b[:4]) != "XFSB" {
		return "", false, err
	}
	bs := binary.BigEndian.Uint32(b[4:])
	return fsXFS, bs >= 512 && bs <= 65536 && bs&(bs-1) == 0, nil
}

func probeBtrfs(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 8)
	ok, err := readAt(r, b, 0x10040)
	if !ok || string(b) != "_BHRfS_M" {
		return "", false, err
	}
	return fsBtrfs, true, nil
}

func probeF2FS(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 4)
	ok, err := readAt(r, b, 1024)
	if !ok || binary.LittleEndian.Uint32(b) != 0xf2f52010 {
		return "", false, err
	}
	return fsF2FS, true, nil
}

func probeVfat(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 512)
	ok, err := readAt(r, b, 0)
	if !ok || binary.LittleEndian.Uint16(b[510:]) != 0xaa55 {
		return "", false, err
	}
	if string(b[54:57]) != "FAT" && string(b[82:87]) != "FAT32" {
		return "", false, nil
	}
	sectorSize := binary.LittleEndian.Uint16(b[11:])
	perCluster := b[13]
	reserved := binary.LittleEndian.Uint16(b[14:])
	fats := b[16]
	valid := sectorSize >= 512 && sectorSize <= 4096 && sectorSize&(sectorSize-1) == 0 &&
		perCluster != 0 && perCluster&(perCluster-1) == 0 && reserved != 0 && fats != 0
	return fsVfat, valid, nil
}

func probeLUKS(r io.ReaderAt) (fsType, bool, error) {
	b := make([]byte, 6)
	ok, err := readAt(r, b, 0)
	if !ok || string(b) != "LUKS\xba\xbe" {
		return "", false, err
	}
	return fsLUKS, true, nil
}

// sectionReader exposes a partition of an image as an io.ReaderAt.
func sectionReader(r io.ReaderAt, p partition, ss int64) *io.SectionReader {
	return io.NewSectionReader(r, p.offset(ss), p.size(ss))
}

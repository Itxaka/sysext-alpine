package image

import (
	"encoding/binary"
	"io"
	"os"
)

// Magic constants per docs/SPEC.md §3.
const (
	gptSignature  = "EFI PART" // at LBA 1 (offset 512 or 4096)
	squashfsMagic = "hsqs"     // at offset 0
	erofsMagic    = 0xE0F5E1E2 // LE uint32 at offset 1024
	ext4Magic     = 0xEF53     // LE uint16 at offset 1080 (ext2/3/4)

	erofsMagicOffset = 1024
	ext4MagicOffset  = 1080
)

// detectReaderAt probes magics on any io.ReaderAt (regular file or block
// device). Short reads (file smaller than a probe offset) simply skip that
// probe.
func detectReaderAt(r io.ReaderAt) (FSType, error) {
	// GPT first: a partitioned image may contain filesystems whose magics
	// would otherwise match at fixed offsets.
	for _, off := range []int64{512, 4096} {
		buf := make([]byte, len(gptSignature))
		if ok, err := readFull(r, buf, off); err != nil {
			return FSUnknown, err
		} else if ok && string(buf) == gptSignature {
			return FSGPT, nil
		}
	}

	buf := make([]byte, 4)
	if ok, err := readFull(r, buf, 0); err != nil {
		return FSUnknown, err
	} else if ok && string(buf) == squashfsMagic {
		return FSSquashfs, nil
	}

	if ok, err := readFull(r, buf, erofsMagicOffset); err != nil {
		return FSUnknown, err
	} else if ok && binary.LittleEndian.Uint32(buf) == erofsMagic {
		return FSErofs, nil
	}

	if ok, err := readFull(r, buf[:2], ext4MagicOffset); err != nil {
		return FSUnknown, err
	} else if ok && binary.LittleEndian.Uint16(buf[:2]) == ext4Magic {
		return FSExt4, nil
	}

	return FSUnknown, nil
}

// readFull reads len(buf) bytes at off. Returns false (no error) when the
// underlying file is too small for the probe.
func readFull(r io.ReaderAt, buf []byte, off int64) (bool, error) {
	_, err := r.ReadAt(buf, off)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// detectPath opens path read-only and probes it.
func detectPath(path string) (FSType, error) {
	f, err := os.Open(path)
	if err != nil {
		return FSUnknown, err
	}
	defer f.Close()
	return detectReaderAt(f)
}

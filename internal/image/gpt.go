package image

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Discoverable Partitions Specification type GUIDs (canonical string form).
// See docs/SPEC.md §3.
var archGUIDs = map[string]struct{ root, usr string }{
	"x86-64":  {"4f68bce3-e8cd-4db1-96e7-fbcaf984b709", "8484680c-9521-48c6-9c11-b0720656f69e"},
	"arm64":   {"b921b045-1df0-41c3-af44-4c6f280d3fae", "b0e01050-ee5f-4390-949a-9101b17104e9"},
	"riscv64": {"72ec70a6-cf74-40e6-bd49-4bda08e8f224", "b6ed5582-440b-4209-b8da-5ff7c419ea3d"},
}

// gptPartition is one parsed partition table entry.
type gptPartition struct {
	// Index is the 1-based partition number (kernel naming: loopNp<Index>).
	Index int
	// TypeGUID is the canonical lowercase string form of the type GUID.
	TypeGUID string
	FirstLBA uint64
	LastLBA  uint64
}

const (
	gptHeaderEntryLBAOff   = 72 // uint64: starting LBA of entry array
	gptHeaderEntryCountOff = 80 // uint32: number of entries
	gptHeaderEntrySizeOff  = 84 // uint32: size of one entry
	gptHeaderMinLen        = 92

	gptEntryMinSize = 128
	gptMaxEntries   = 4096 // sanity bound
)

// guidString decodes a 16-byte on-disk GPT GUID (mixed-endian: the first
// three groups are little-endian, the last two big-endian) into canonical
// lowercase 8-4-4-4-12 form.
func guidString(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.LittleEndian.Uint32(b[0:4]),
		binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]),
		b[8:10],
		b[10:16])
}

// parseGPT reads the GPT header at LBA 1, trying 512-byte then 4096-byte
// sectors, and returns the non-empty partition entries.
func parseGPT(r io.ReaderAt) ([]gptPartition, error) {
	var lastErr error
	for _, sector := range []int64{512, 4096} {
		parts, err := parseGPTAt(r, sector)
		if err == nil {
			return parts, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func parseGPTAt(r io.ReaderAt, sectorSize int64) ([]gptPartition, error) {
	hdr := make([]byte, gptHeaderMinLen)
	if _, err := r.ReadAt(hdr, sectorSize); err != nil {
		return nil, fmt.Errorf("reading GPT header at offset %d: %w", sectorSize, err)
	}
	if string(hdr[:8]) != gptSignature {
		return nil, fmt.Errorf("no GPT signature at offset %d", sectorSize)
	}

	entryLBA := binary.LittleEndian.Uint64(hdr[gptHeaderEntryLBAOff:])
	count := binary.LittleEndian.Uint32(hdr[gptHeaderEntryCountOff:])
	entrySize := binary.LittleEndian.Uint32(hdr[gptHeaderEntrySizeOff:])

	if entrySize < gptEntryMinSize {
		return nil, fmt.Errorf("GPT entry size %d too small", entrySize)
	}
	if count > gptMaxEntries {
		return nil, fmt.Errorf("GPT entry count %d exceeds sanity bound", count)
	}

	var parts []gptPartition
	entry := make([]byte, entrySize)
	base := int64(entryLBA) * sectorSize
	for i := uint32(0); i < count; i++ {
		off := base + int64(i)*int64(entrySize)
		if _, err := r.ReadAt(entry, off); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // truncated entry array: stop, keep what we have
			}
			return nil, fmt.Errorf("reading GPT entry %d: %w", i+1, err)
		}
		if isZero(entry[:16]) {
			continue // unused slot
		}
		parts = append(parts, gptPartition{
			Index:    int(i) + 1,
			TypeGUID: guidString(entry[:16]),
			FirstLBA: binary.LittleEndian.Uint64(entry[32:40]),
			LastLBA:  binary.LittleEndian.Uint64(entry[40:48]),
		})
	}
	return parts, nil
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// selectPartition picks the payload partition for arch per SPEC §3: prefer
// the root partition, else the usr partition (isUsr=true then).
func selectPartition(parts []gptPartition, arch string) (p gptPartition, isUsr bool, err error) {
	guids, ok := archGUIDs[arch]
	if !ok {
		return gptPartition{}, false, fmt.Errorf("unsupported architecture %q (no known partition type GUIDs)", arch)
	}
	var usrPart *gptPartition
	for i := range parts {
		switch parts[i].TypeGUID {
		case guids.root:
			return parts[i], false, nil
		case guids.usr:
			if usrPart == nil {
				usrPart = &parts[i]
			}
		}
	}
	if usrPart != nil {
		return *usrPart, true, nil
	}
	return gptPartition{}, false, fmt.Errorf("no root or usr partition for architecture %q found in GPT", arch)
}

// parseGPTFile is parseGPT over a file path.
func parseGPTFile(path string) ([]gptPartition, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseGPT(f)
}

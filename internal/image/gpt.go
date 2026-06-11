package image

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// dpsGUIDs holds the Discoverable Partitions Specification type GUIDs for
// one architecture (canonical lowercase string form).
type dpsGUIDs struct {
	root          string // root partition
	rootVerity    string // dm-verity hash data for root
	rootVeritySig string // PKCS#7 signature of root verity root hash
	usr           string // /usr partition
	usrVerity     string // dm-verity hash data for usr
	usrVeritySig  string // PKCS#7 signature of usr verity root hash
}

// archGUIDs maps systemd architecture identifiers to their partition type
// GUIDs, per the UAPI Discoverable Partitions Specification.
var archGUIDs = map[string]dpsGUIDs{
	"x86-64": {
		root:          "4f68bce3-e8cd-4db1-96e7-fbcaf984b709",
		rootVerity:    "2c7357ed-ebd2-46d9-aec1-23d437ec2bf5",
		rootVeritySig: "41092b05-9fc8-4523-994f-2def0408b176",
		usr:           "8484680c-9521-48c6-9c11-b0720656f69e",
		usrVerity:     "77ff5f63-e7b6-4633-acf4-1565b864c0e6",
		usrVeritySig:  "e7bb33fb-06cf-4e81-8273-e543b413e2e2",
	},
	"arm64": {
		root:          "b921b045-1df0-41c3-af44-4c6f280d3fae",
		rootVerity:    "df3300ce-d69f-4c92-978c-9bfb0f38d820",
		rootVeritySig: "6db69de6-29f4-4758-a7a5-962190f00ce3",
		usr:           "b0e01050-ee5f-4390-949a-9101b17104e9",
		usrVerity:     "6e11a4e7-fbca-4ded-b9e9-e1a512bb664e",
		usrVeritySig:  "c23ce4ff-44bd-4b00-b2d4-b41b3419e02a",
	},
	"riscv64": {
		root:          "72ec70a6-cf74-40e6-bd49-4bda08e8f224",
		rootVerity:    "b6ed5582-440b-4209-b8da-5ff7c419ea3d",
		rootVeritySig: "efe0f087-ea8d-4469-821a-4c2a96a8386a",
		usr:           "beaec34b-8442-439b-a40b-984381ed097d",
		usrVerity:     "8f1056be-9b05-47c4-81d6-be53128e5b54",
		usrVeritySig:  "d2f9000a-7a18-453f-b5cd-4d32f77a7b32",
	},
}

// gptPartition is one parsed partition table entry.
type gptPartition struct {
	// Index is the 1-based partition number (kernel naming: loopNp<Index>).
	Index int
	// TypeGUID is the canonical lowercase string form of the type GUID.
	TypeGUID string
	// UniqueGUID is the canonical lowercase string form of the unique
	// (per-partition) GUID — entry bytes 16:32. For verity-protected DDIs
	// the data and verity partitions' unique GUIDs encode the verity root
	// hash (see verity.go).
	UniqueGUID string
	FirstLBA   uint64
	LastLBA    uint64
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
			Index:      int(i) + 1,
			TypeGUID:   guidString(entry[:16]),
			UniqueGUID: guidString(entry[16:32]),
			FirstLBA:   binary.LittleEndian.Uint64(entry[32:40]),
			LastLBA:    binary.LittleEndian.Uint64(entry[40:48]),
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

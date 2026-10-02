package image

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

// guidBytes encodes a canonical GUID string into the GPT on-disk
// mixed-endian 16-byte form (inverse of guidString).
func guidBytes(t testing.TB, guid string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.ReplaceAll(guid, "-", ""))
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad guid %q", guid)
	}
	return []byte{
		raw[3], raw[2], raw[1], raw[0],
		raw[5], raw[4],
		raw[7], raw[6],
		raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15],
	}
}

// testPart describes one partition for buildGPT. Start and Sectors of zero
// get an automatic layout; an empty UUID gets a unique one.
type testPart struct {
	typ     string
	uuid    string
	start   uint64
	sectors uint64
	attrs   uint64
	label   string
}

const testEntries = 128

// gptLayout returns the sector numbers buildGPT uses.
func gptLayout(ss int64, nparts int) (entrySectors, firstUsable, total int64) {
	entrySectors = (testEntries*gptEntrySize + ss - 1) / ss
	firstUsable = 2 + entrySectors
	total = firstUsable + int64(nparts+1)*8 + entrySectors + 1
	return entrySectors, firstUsable, total
}

// buildGPT constructs a valid GPT image (protective MBR, primary and
// alternate headers and entry arrays with correct CRCs) for sector size ss.
func buildGPT(t testing.TB, ss int64, parts ...testPart) []byte {
	t.Helper()
	entrySectors, firstUsable, total := gptLayout(ss, len(parts))
	lastLBA := total - 1
	lastUsable := lastLBA - entrySectors - 1
	img := make([]byte, total*ss)

	mbr := img[:512]
	rec := mbr[mbrPartitionOffset:]
	rec[4] = mbrTypeGPTProtected
	binary.LittleEndian.PutUint32(rec[8:], 1)
	binary.LittleEndian.PutUint32(rec[12:], uint32(lastLBA))
	binary.LittleEndian.PutUint16(mbr[mbrSignatureOffset:], 0xaa55)

	entries := make([]byte, testEntries*gptEntrySize)
	next := uint64(firstUsable)
	for i, p := range parts {
		e := entries[i*gptEntrySize:]
		copy(e[0:16], guidBytes(t, p.typ))
		u := p.uuid
		if u == "" {
			u = uuidString([]byte{byte(i + 1), 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
		}
		copy(e[16:32], guidBytes(t, u))
		start, n := p.start, p.sectors
		if start == 0 {
			start, n = next, 8
			next += 8
		}
		binary.LittleEndian.PutUint64(e[32:], start)
		binary.LittleEndian.PutUint64(e[40:], start+n-1)
		binary.LittleEndian.PutUint64(e[48:], p.attrs)
		for j, c := range utf16.Encode([]rune(p.label)) {
			binary.LittleEndian.PutUint16(e[56+2*j:], c)
		}
	}
	copy(img[2*ss:], entries)
	copy(img[(lastUsable+1)*ss:], entries)

	writeHeader := func(lba, alt, entryLBA int64) {
		h := img[lba*ss:]
		copy(h, gptSignature)
		binary.LittleEndian.PutUint32(h[8:], gptRevision)
		binary.LittleEndian.PutUint32(h[12:], gptHeaderSize)
		binary.LittleEndian.PutUint64(h[24:], uint64(lba))
		binary.LittleEndian.PutUint64(h[32:], uint64(alt))
		binary.LittleEndian.PutUint64(h[40:], uint64(firstUsable))
		binary.LittleEndian.PutUint64(h[48:], uint64(lastUsable))
		binary.LittleEndian.PutUint64(h[72:], uint64(entryLBA))
		binary.LittleEndian.PutUint32(h[80:], testEntries)
		binary.LittleEndian.PutUint32(h[84:], gptEntrySize)
		binary.LittleEndian.PutUint32(h[88:], crc32.ChecksumIEEE(entries))
		fixHeaderCRC(img, ss, lba)
	}
	writeHeader(1, lastLBA, 2)
	writeHeader(lastLBA, 1, lastUsable+1)
	return img
}

// fixHeaderCRC recomputes the header CRC at lba after a test mutation.
func fixHeaderCRC(img []byte, ss, lba int64) {
	h := img[lba*ss:]
	hsz := binary.LittleEndian.Uint32(h[12:])
	clear(h[16:20])
	binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(h[:hsz]))
}

func parseTestGPT(img []byte, ss int64) ([]partition, error) {
	return parseGPT(bytes.NewReader(img), int64(len(img)), ss)
}

var x86 = archPartitionTypes[len(archPartitionTypes)-1].types

func TestParseGPTSectorSizes(t *testing.T) {
	for _, ss := range []int64{512, 1024, 2048, 4096} {
		img := buildGPT(t, ss,
			testPart{typ: x86[3], label: "usr_1.0", attrs: gptFlagReadOnly},
			testPart{typ: x86[0], label: "root"})
		got, err := probeSectorSize(bytes.NewReader(img))
		if err != nil || got != ss {
			t.Fatalf("probeSectorSize = %d, %v; want %d", got, err, ss)
		}
		pt, err := readPartitionTable(bytes.NewReader(img), int64(len(img)), ss)
		if err != nil {
			t.Fatalf("ss %d: %v", ss, err)
		}
		if !pt.gpt || len(pt.parts) != 2 {
			t.Fatalf("ss %d: got %+v", ss, pt)
		}
		p := pt.parts[0]
		if p.Index != 1 || p.TypeGUID != x86[3] || p.Label != "usr_1.0" || p.Attrs != gptFlagReadOnly || p.Sectors != 8 {
			t.Errorf("ss %d: partition 1 = %+v", ss, p)
		}
		if pt.parts[1].Index != 2 || pt.parts[1].TypeGUID != x86[0] {
			t.Errorf("ss %d: partition 2 = %+v", ss, pt.parts[1])
		}
	}
}

func TestProbeSectorSize(t *testing.T) {
	img := buildGPT(t, 512, testPart{typ: x86[0]})
	if len(img) < 8192 {
		img = append(img, make([]byte, 8192-len(img))...)
	}
	both := bytes.Clone(img)
	copy(both[4096:], img[512:1024])
	if _, err := probeSectorSize(bytes.NewReader(both)); !errors.Is(err, errAmbiguousSectorSize) {
		t.Errorf("headers at 512 and 4096: err = %v, want ambiguity", err)
	}

	for name, mutate := range map[string]func([]byte){
		"revision":    func(b []byte) { binary.LittleEndian.PutUint32(b[512+8:], 0x00020000) },
		"my_lba":      func(b []byte) { binary.LittleEndian.PutUint64(b[512+24:], 2) },
		"header_size": func(b []byte) { binary.LittleEndian.PutUint32(b[512+12:], 91) },
		"too large":   func(b []byte) { binary.LittleEndian.PutUint32(b[512+12:], 4097) },
	} {
		b := bytes.Clone(img)
		mutate(b)
		if got, err := probeSectorSize(bytes.NewReader(b)); got != 0 || err != nil {
			t.Errorf("%s: probeSectorSize = %d, %v; want 0", name, got, err)
		}
	}
	if got, err := probeSectorSize(bytes.NewReader(img[:4000])); got != 0 || err != nil {
		t.Errorf("short image: %d, %v", got, err)
	}
}

func TestParseGPTFallsBackToAlternateHeader(t *testing.T) {
	img := buildGPT(t, 512, testPart{typ: x86[0], label: "a"})
	img[512+16] ^= 0xff
	parts, err := parseTestGPT(img, 512)
	if err != nil || len(parts) != 1 || parts[0].Label != "a" {
		t.Fatalf("primary header CRC broken: %v, %+v; want alternate table", err, parts)
	}
	last := int64(len(img))/512 - 1
	img[last*512+16] ^= 0xff
	if _, err := parseTestGPT(img, 512); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("both headers broken: err = %v, want CRC error", err)
	}
}

func TestParseGPTRejectsInvalidTables(t *testing.T) {
	const ss = 512
	cases := map[string]func(img []byte){
		"entry array CRC": func(img []byte) { img[2*ss+100] ^= 1 },
		"entry size 256": func(img []byte) {
			binary.LittleEndian.PutUint32(img[ss+84:], 256)
		},
		"entry count 0": func(img []byte) {
			binary.LittleEndian.PutUint32(img[ss+80:], 0)
		},
		"entry count 2000": func(img []byte) {
			binary.LittleEndian.PutUint32(img[ss+80:], 2000)
		},
		"huge entry size": func(img []byte) {
			binary.LittleEndian.PutUint32(img[ss+84:], 0xf0000000)
		},
		"entry LBA overflow": func(img []byte) {
			binary.LittleEndian.PutUint64(img[ss+72:], 1<<62)
		},
		"usable range": func(img []byte) {
			binary.LittleEndian.PutUint64(img[ss+48:], 1<<40)
		},
		"truncated array": func(img []byte) {
			binary.LittleEndian.PutUint64(img[ss+72:], uint64(len(img)/ss-2))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			img := buildGPT(t, ss, testPart{typ: x86[0]})
			// Break the alternate header so the fallback cannot mask the
			// primary's problem.
			last := int64(len(img))/ss - 1
			img[last*ss] = 0
			mutate(img)
			fixHeaderCRC(img, ss, 1)
			if _, err := parseTestGPT(img, ss); err == nil {
				t.Fatal("invalid table accepted")
			}
		})
	}
}

func TestParseGPTBoundedAllocation(t *testing.T) {
	img := buildGPT(t, 512, testPart{typ: x86[0]})
	binary.LittleEndian.PutUint32(img[512+84:], 0xf0000000)
	fixHeaderCRC(img, 512, 1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _ = parseTestGPT(img, 512)
	runtime.ReadMemStats(&after)
	if d := after.TotalAlloc - before.TotalAlloc; d > 1<<20 {
		t.Errorf("parsing allocated %d bytes", d)
	}
}

func TestParseGPTSkipsInvalidEntries(t *testing.T) {
	_, firstUsable, total := gptLayout(512, 3)
	img := buildGPT(t, 512,
		testPart{typ: x86[0], start: 40, sectors: 0},
		testPart{typ: x86[0], start: uint64(firstUsable - 1), sectors: 4},
		testPart{typ: x86[3], start: uint64(total), sectors: 8},
		testPart{typ: x86[0], label: "ok"})
	// Entry 1 has last < first (sectors 0 → last = start-1).
	parts, err := parseTestGPT(img, 512)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Label != "ok" || parts[0].Index != 4 {
		t.Fatalf("got %+v, want only partition 4", parts)
	}
}

func TestReadPartitionTable(t *testing.T) {
	img := buildGPT(t, 512, testPart{typ: x86[0]})

	noPMBR := bytes.Clone(img)
	noPMBR[mbrPartitionOffset+4] = 0x83
	pt, err := readPartitionTable(bytes.NewReader(noPMBR), int64(len(img)), 512)
	if err != nil || pt.gpt {
		t.Errorf("GPT without protective MBR: %+v, %v; want a plain MBR table", pt, err)
	}

	noSig := bytes.Clone(img)
	noSig[mbrSignatureOffset] = 0
	if _, err := readPartitionTable(bytes.NewReader(noSig), int64(len(img)), 512); !errors.Is(err, errNoPartitionTable) {
		t.Errorf("no MBR signature: err = %v", err)
	}

	mbr := make([]byte, 4096)
	binary.LittleEndian.PutUint16(mbr[mbrSignatureOffset:], 0xaa55)
	e := mbr[mbrPartitionOffset+16:]
	e[0], e[4] = 0x80, 0x83
	binary.LittleEndian.PutUint32(e[8:], 2)
	binary.LittleEndian.PutUint32(e[12:], 4)
	pt, err = readPartitionTable(bytes.NewReader(mbr), int64(len(mbr)), 512)
	if err != nil || pt.gpt || len(pt.parts) != 1 || pt.parts[0].Index != 2 || pt.parts[0].Attrs != 0x80 {
		t.Errorf("MBR table: %+v, %v", pt, err)
	}
	e[0] = 0x12
	if _, err := readPartitionTable(bytes.NewReader(mbr), int64(len(mbr)), 512); !errors.Is(err, errNoPartitionTable) {
		t.Errorf("bogus boot indicator: err = %v", err)
	}
}

func TestGUIDStringMixedEndian(t *testing.T) {
	want := "4f68bce3-e8cd-4db1-96e7-fbcaf984b709"
	onDisk := []byte{0xe3, 0xbc, 0x68, 0x4f, 0xcd, 0xe8, 0xb1, 0x4d, 0x96, 0xe7, 0xfb, 0xca, 0xf9, 0x84, 0xb7, 0x09}
	if got := guidString(onDisk); got != want {
		t.Errorf("guidString = %q, want %q", got, want)
	}
	if !bytes.Equal(guidBytes(t, want), onDisk) {
		t.Errorf("guidBytes(%q) = %x", want, guidBytes(t, want))
	}
}

func TestGPTTypeTable(t *testing.T) {
	// Spot checks against the UAPI Discoverable Partitions Specification.
	for guid, want := range map[string]gptType{
		"44479540-f297-41b2-9af7-d131d5f0458a": {partRoot, "x86"},
		"69dad710-2ce4-4e3c-b16c-21a1d49abed3": {partRoot, "arm"},
		"c31c45e6-3f39-412e-80fb-4809c4980599": {partRoot, "ppc64-le"},
		"5eead9a9-fe09-4a1e-a1d7-520d00531306": {partRoot, "s390x"},
		"77055800-792c-4f94-b39a-98c91b762bb6": {partRoot, "loongarch64"},
		"4f68bce3-e8cd-4db1-96e7-fbcaf984b709": {partRoot, "x86-64"},
		"2c7357ed-ebd2-46d9-aec1-23d437ec2bf5": {partRootVerity, "x86-64"},
		"41092b05-9fc8-4523-994f-2def0408b176": {partRootVeritySig, "x86-64"},
		"8484680c-9521-48c6-9c11-b0720656f69e": {partUsr, "x86-64"},
		"77ff5f63-e7b6-4633-acf4-1565b864c0e6": {partUsrVerity, "x86-64"},
		"e7bb33fb-06cf-4e81-8273-e543b413e2e2": {partUsrVeritySig, "x86-64"},
		"b921b045-1df0-41c3-af44-4c6f280d3fae": {partRoot, "arm64"},
		"beaec34b-8442-439b-a40b-984381ed097d": {partUsr, "riscv64"},
		"c12a7328-f81f-11d2-ba4b-00a0c93ec93b": {partESP, ""},
		"0657fd6d-a4ab-43c4-84e5-0933c84b4f4f": {partSwap, ""},
		"4d21b016-b534-45c2-a9fb-5c16e091fd2d": {partVar, ""},
	} {
		if got := lookupGPTType(guid); got != want {
			t.Errorf("lookupGPTType(%s) = %+v, want %+v", guid, got, want)
		}
	}
	if got := lookupGPTType(gptTypeLinuxGeneric); got.designator != partInvalid {
		t.Errorf("generic Linux type must not map to a designator, got %+v", got)
	}
	if n := len(gptTypes); n != 6*len(archPartitionTypes)+7 {
		t.Errorf("gptTypes has %d entries; type GUIDs must be unique", n)
	}
}

func TestCompareArch(t *testing.T) {
	for _, tc := range []struct {
		a, b, native string
		want         int
	}{
		{"x86-64", "x86", "x86-64", 1},
		{"x86", "x86-64", "x86-64", -1},
		{"x86", "arm64", "x86-64", 1},
		{"arm64", "x86", "x86-64", -1},
		{"arm64", "riscv64", "x86-64", 0},
		{"arm", "arm64", "arm64", -1},
		{"s390", "x86", "s390x", 1},
		{"", "", "x86-64", 0},
	} {
		if got := compareArch(tc.a, tc.b, tc.native); got != tc.want {
			t.Errorf("compareArch(%q, %q, %q) = %d, want %d", tc.a, tc.b, tc.native, got, tc.want)
		}
	}
}

func TestDecodeGPTLabel(t *testing.T) {
	b := make([]byte, 72)
	for i, c := range utf16.Encode([]rune("µroot_1.2")) {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	if got := decodeGPTLabel(b); got != "µroot_1.2" {
		t.Errorf("decodeGPTLabel = %q", got)
	}
	full := bytes.Repeat([]byte{'a', 0}, 36)
	if got := decodeGPTLabel(full); got != strings.Repeat("a", 36) {
		t.Errorf("unterminated label = %q", got)
	}
}

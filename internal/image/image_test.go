package image

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp creates a temp file with the given content and returns its path.
func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "img-*.raw")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// guidBytes encodes a canonical GUID string into the GPT on-disk
// mixed-endian 16-byte form (inverse of guidString).
func guidBytes(t *testing.T, guid string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.ReplaceAll(guid, "-", ""))
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad guid %q", guid)
	}
	out := make([]byte, 16)
	// First three groups little-endian.
	out[0], out[1], out[2], out[3] = raw[3], raw[2], raw[1], raw[0]
	out[4], out[5] = raw[5], raw[4]
	out[6], out[7] = raw[7], raw[6]
	// Last two groups big-endian (as-is).
	copy(out[8:], raw[8:])
	return out
}

// buildGPT constructs a minimal synthetic GPT image: protective space at
// LBA 0, header at LBA 1, partition entries at entryLBA, for the given
// sector size. Each partition is laid out with a non-zero unique GUID.
func buildGPT(t *testing.T, sectorSize int, typeGUIDs []string) []byte {
	t.Helper()
	const entrySize = 128
	entryLBA := uint64(2)
	count := uint32(len(typeGUIDs))

	img := make([]byte, int(entryLBA)*sectorSize+len(typeGUIDs)*entrySize)

	// Header at LBA 1.
	hdr := img[sectorSize:]
	copy(hdr[0:8], "EFI PART")
	binary.LittleEndian.PutUint32(hdr[8:], 0x00010000) // revision 1.0
	binary.LittleEndian.PutUint32(hdr[12:], 92)        // header size
	binary.LittleEndian.PutUint64(hdr[72:], entryLBA)
	binary.LittleEndian.PutUint32(hdr[80:], count)
	binary.LittleEndian.PutUint32(hdr[84:], entrySize)

	// Entries.
	for i, g := range typeGUIDs {
		e := img[int(entryLBA)*sectorSize+i*entrySize:]
		copy(e[0:16], guidBytes(t, g))
		e[16] = byte(i + 1) // non-zero unique GUID
		binary.LittleEndian.PutUint64(e[32:], uint64(2048*(i+1)))
		binary.LittleEndian.PutUint64(e[40:], uint64(2048*(i+2)-1))
	}
	return img
}

func TestDetect(t *testing.T) {
	mk := func(size int, plant func([]byte)) []byte {
		b := make([]byte, size)
		plant(b)
		return b
	}

	cases := []struct {
		name string
		data []byte
		want FSType
	}{
		{"squashfs", mk(2048, func(b []byte) { copy(b, "hsqs") }), FSSquashfs},
		{"erofs", mk(2048, func(b []byte) {
			binary.LittleEndian.PutUint32(b[1024:], 0xE0F5E1E2)
		}), FSErofs},
		{"ext4", mk(2048, func(b []byte) {
			binary.LittleEndian.PutUint16(b[1080:], 0xEF53)
		}), FSExt4},
		{"gpt-512", mk(8192, func(b []byte) { copy(b[512:], "EFI PART") }), FSGPT},
		{"gpt-4096", mk(8192, func(b []byte) { copy(b[4096:], "EFI PART") }), FSGPT},
		{"gpt-wins-over-fs-magic", mk(8192, func(b []byte) {
			// A GPT image that also carries fs-looking bytes must be GPT.
			copy(b[512:], "EFI PART")
			copy(b, "hsqs")
			binary.LittleEndian.PutUint16(b[1080:], 0xEF53)
		}), FSGPT},
		{"unknown-zeros", mk(8192, func(b []byte) {}), FSUnknown},
		{"tiny-file", []byte{0x42}, FSUnknown},
		{"empty-file", nil, FSUnknown},
		{"erofs-wrong-endianness", mk(2048, func(b []byte) {
			binary.BigEndian.PutUint32(b[1024:], 0xE0F5E1E2)
		}), FSUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.data)
			got, err := Detect(path)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got != tc.want {
				t.Errorf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectMissingFile(t *testing.T) {
	if _, err := Detect(filepath.Join(t.TempDir(), "nope.raw")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestGUIDStringMixedEndian(t *testing.T) {
	// root x86-64 GUID in canonical form and its on-disk byte sequence.
	want := "4f68bce3-e8cd-4db1-96e7-fbcaf984b709"
	onDisk := []byte{
		0xe3, 0xbc, 0x68, 0x4f, // 4f68bce3 LE
		0xcd, 0xe8, // e8cd LE
		0xb1, 0x4d, // 4db1 LE
		0x96, 0xe7, // 96e7 BE
		0xfb, 0xca, 0xf9, 0x84, 0xb7, 0x09, // fbcaf984b709 BE
	}
	if got := guidString(onDisk); got != want {
		t.Errorf("guidString = %q, want %q", got, want)
	}
	// And the test helper must be its exact inverse.
	if !bytes.Equal(guidBytes(t, want), onDisk) {
		t.Errorf("guidBytes(%q) = %x, want %x", want, guidBytes(t, want), onDisk)
	}
}

func TestParseGPT(t *testing.T) {
	root := archGUIDs["x86-64"].root
	usr := archGUIDs["x86-64"].usr

	for _, sector := range []int{512, 4096} {
		t.Run(map[int]string{512: "sector512", 4096: "sector4096"}[sector], func(t *testing.T) {
			img := buildGPT(t, sector, []string{usr, root})
			path := writeTemp(t, img)

			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			parts, err := parseGPT(f)
			if err != nil {
				t.Fatalf("parseGPT: %v", err)
			}
			if len(parts) != 2 {
				t.Fatalf("got %d partitions, want 2", len(parts))
			}
			if parts[0].TypeGUID != usr || parts[0].Index != 1 {
				t.Errorf("part 0 = %+v, want usr GUID index 1", parts[0])
			}
			if parts[1].TypeGUID != root || parts[1].Index != 2 {
				t.Errorf("part 1 = %+v, want root GUID index 2", parts[1])
			}
			if parts[0].FirstLBA != 2048 || parts[0].LastLBA != 4095 {
				t.Errorf("part 0 LBAs = %d..%d, want 2048..4095",
					parts[0].FirstLBA, parts[0].LastLBA)
			}
			// buildGPT plants byte(i+1) as the first on-disk byte of the
			// unique GUID; that byte is the LSB of the LE first group.
			if parts[0].UniqueGUID != "00000001-0000-0000-0000-000000000000" ||
				parts[1].UniqueGUID != "00000002-0000-0000-0000-000000000000" {
				t.Errorf("unique GUIDs = %q, %q",
					parts[0].UniqueGUID, parts[1].UniqueGUID)
			}
		})
	}
}

func TestParseGPTEmptySlots(t *testing.T) {
	root := archGUIDs["arm64"].root
	img := buildGPT(t, 512, []string{root})

	// Extend the entry array with an all-zero (unused) slot and bump count.
	img = append(img, make([]byte, 128)...)
	binary.LittleEndian.PutUint32(img[512+80:], 2)

	f, err := os.Open(writeTemp(t, img))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	parts, err := parseGPT(f)
	if err != nil {
		t.Fatalf("parseGPT: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("got %d partitions, want 1 (zero slot skipped)", len(parts))
	}
	if parts[0].TypeGUID != root {
		t.Errorf("TypeGUID = %q, want %q", parts[0].TypeGUID, root)
	}
}

func TestParseGPTNoSignature(t *testing.T) {
	f, err := os.Open(writeTemp(t, make([]byte, 16384)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := parseGPT(f); err == nil {
		t.Fatal("expected error for missing GPT signature")
	}
}

func TestParseGPTBogusEntrySize(t *testing.T) {
	img := buildGPT(t, 512, []string{archGUIDs["x86-64"].root})
	binary.LittleEndian.PutUint32(img[512+84:], 16) // entry size < 128
	f, err := os.Open(writeTemp(t, img))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := parseGPT(f); err == nil {
		t.Fatal("expected error for undersized GPT entries")
	}
}

func TestSelectPartition(t *testing.T) {
	x := archGUIDs["x86-64"]
	a := archGUIDs["arm64"]

	t.Run("prefers root over usr", func(t *testing.T) {
		parts := []gptPartition{
			{Index: 1, TypeGUID: x.usr},
			{Index: 2, TypeGUID: x.root},
		}
		p, isUsr, err := selectPartition(parts, "x86-64")
		if err != nil {
			t.Fatal(err)
		}
		if isUsr || p.Index != 2 {
			t.Errorf("got index %d isUsr=%v, want root partition index 2", p.Index, isUsr)
		}
	})

	t.Run("falls back to usr", func(t *testing.T) {
		parts := []gptPartition{
			{Index: 1, TypeGUID: "deadbeef-0000-0000-0000-000000000000"},
			{Index: 2, TypeGUID: x.usr},
		}
		p, isUsr, err := selectPartition(parts, "x86-64")
		if err != nil {
			t.Fatal(err)
		}
		if !isUsr || p.Index != 2 {
			t.Errorf("got index %d isUsr=%v, want usr partition index 2", p.Index, isUsr)
		}
	})

	t.Run("arch GUIDs do not cross-match", func(t *testing.T) {
		parts := []gptPartition{{Index: 1, TypeGUID: a.root}}
		if _, _, err := selectPartition(parts, "x86-64"); err == nil {
			t.Error("arm64 root must not match x86-64 selection")
		}
	})

	t.Run("no match", func(t *testing.T) {
		if _, _, err := selectPartition(nil, "x86-64"); err == nil {
			t.Error("expected error with no partitions")
		}
	})

	t.Run("unknown arch", func(t *testing.T) {
		parts := []gptPartition{{Index: 1, TypeGUID: x.root}}
		if _, _, err := selectPartition(parts, "m68k"); err == nil {
			t.Error("expected error for unknown architecture")
		}
	})
}

func TestArchGUIDMapping(t *testing.T) {
	// Per the UAPI Discoverable Partitions Specification:
	// root, root-verity, root-verity-sig, usr, usr-verity, usr-verity-sig.
	want := map[string]dpsGUIDs{
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
	if len(archGUIDs) != len(want) {
		t.Errorf("archGUIDs has %d entries, want %d", len(archGUIDs), len(want))
	}
	for arch, w := range want {
		got, ok := archGUIDs[arch]
		if !ok {
			t.Errorf("missing arch %q", arch)
			continue
		}
		if got != w {
			t.Errorf("%s = %+v, want %+v", arch, got, w)
		}
	}
}

func TestParseDevT(t *testing.T) {
	maj, min, err := parseDevT("259:3")
	if err != nil || maj != 259 || min != 3 {
		t.Errorf("parseDevT(259:3) = %d,%d,%v; want 259,3,nil", maj, min, err)
	}
	if _, _, err := parseDevT("garbage"); err == nil {
		t.Error("expected error for malformed dev_t")
	}
}

func TestEndToEndGPTSelection(t *testing.T) {
	// Full Detect + parse + select flow over a synthetic image holding a
	// usr partition only (the <mountPoint>/usr case), at 4096-byte sectors.
	usr := archGUIDs["riscv64"].usr
	path := writeTemp(t, buildGPT(t, 4096, []string{usr}))

	fs, err := Detect(path)
	if err != nil || fs != FSGPT {
		t.Fatalf("Detect = %v, %v; want gpt", fs, err)
	}
	parts, err := parseGPTFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, isUsr, err := selectPartition(parts, "riscv64")
	if err != nil {
		t.Fatal(err)
	}
	if !isUsr || p.Index != 1 {
		t.Errorf("got index %d isUsr=%v, want usr partition index 1", p.Index, isUsr)
	}
}

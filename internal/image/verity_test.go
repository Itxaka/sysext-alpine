package image

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// mkVeritySB crafts an on-disk verity superblock and lets the caller
// mutate it before parsing.
func mkVeritySB(mutate func([]byte)) []byte {
	b := make([]byte, veritySBSize)
	copy(b, verityMagic)
	binary.LittleEndian.PutUint32(b[vsbOffVersion:], 1)
	binary.LittleEndian.PutUint32(b[vsbOffHashType:], 1)
	for i := 0; i < 16; i++ {
		b[vsbOffUUID+i] = byte(i)
	}
	copy(b[vsbOffAlgorithm:], "sha256") // rest stays NUL
	binary.LittleEndian.PutUint32(b[vsbOffDataBlockSize:], 4096)
	binary.LittleEndian.PutUint32(b[vsbOffHashBlockSize:], 4096)
	binary.LittleEndian.PutUint64(b[vsbOffDataBlocks:], 2048)
	binary.LittleEndian.PutUint16(b[vsbOffSaltSize:], 32)
	for i := 0; i < 32; i++ {
		b[vsbOffSalt+i] = byte(0xA0 + i)
	}
	if mutate != nil {
		mutate(b)
	}
	return b
}

func TestParseVeritySuperblock(t *testing.T) {
	sb, err := parseVeritySuperblock(mkVeritySB(nil))
	if err != nil {
		t.Fatal(err)
	}
	if sb.Version != 1 || sb.HashType != 1 {
		t.Errorf("version/hash_type = %d/%d, want 1/1", sb.Version, sb.HashType)
	}
	if sb.Algorithm != "sha256" {
		t.Errorf("algorithm = %q, want sha256", sb.Algorithm)
	}
	if sb.DataBlockSize != 4096 || sb.HashBlockSize != 4096 {
		t.Errorf("block sizes = %d/%d, want 4096/4096", sb.DataBlockSize, sb.HashBlockSize)
	}
	if sb.DataBlocks != 2048 {
		t.Errorf("data blocks = %d, want 2048", sb.DataBlocks)
	}
	if len(sb.Salt) != 32 || sb.Salt[0] != 0xA0 || sb.Salt[31] != 0xBF {
		t.Errorf("salt = %x, want 32 bytes a0..bf", sb.Salt)
	}
	if !bytes.Equal(sb.UUID[:4], []byte{0, 1, 2, 3}) {
		t.Errorf("uuid = %x", sb.UUID)
	}
}

func TestParseVeritySuperblockErrors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"truncated", mkVeritySB(nil)[:100]},
		{"bad magic", mkVeritySB(func(b []byte) { b[0] = 'X' })},
		{"version 2", mkVeritySB(func(b []byte) {
			binary.LittleEndian.PutUint32(b[vsbOffVersion:], 2)
		})},
		{"hash type 0", mkVeritySB(func(b []byte) {
			binary.LittleEndian.PutUint32(b[vsbOffHashType:], 0)
		})},
		{"oversized salt", mkVeritySB(func(b []byte) {
			binary.LittleEndian.PutUint16(b[vsbOffSaltSize:], 300)
		})},
		{"zero data blocks", mkVeritySB(func(b []byte) {
			binary.LittleEndian.PutUint64(b[vsbOffDataBlocks:], 0)
		})},
		{"unaligned data block size", mkVeritySB(func(b []byte) {
			binary.LittleEndian.PutUint32(b[vsbOffDataBlockSize:], 1000)
		})},
		{"zero hash block size", mkVeritySB(func(b []byte) {
			binary.LittleEndian.PutUint32(b[vsbOffHashBlockSize:], 0)
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseVeritySuperblock(tc.data); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestVeritySuperblockOffsets(t *testing.T) {
	// Layout per veritysetup's on-disk format: magic 8, version 4,
	// hash_type 4, uuid 16, algorithm 32, data_block_size 4,
	// hash_block_size 4, data_blocks 8, salt_size 2, pad 6, salt 256.
	if vsbOffVersion != 8 || vsbOffHashType != 12 || vsbOffUUID != 16 ||
		vsbOffAlgorithm != 32 || vsbOffDataBlockSize != 64 ||
		vsbOffHashBlockSize != 68 || vsbOffDataBlocks != 72 ||
		vsbOffSaltSize != 80 || vsbOffSalt != 88 {
		t.Error("verity superblock field offsets do not match the on-disk layout")
	}
	if veritySBSize != vsbOffSalt+verityMaxSalt {
		t.Errorf("veritySBSize = %d, want %d", veritySBSize, vsbOffSalt+verityMaxSalt)
	}
}

func TestRootHashFromGUIDs(t *testing.T) {
	data := "12345678-9abc-def0-1234-56789abcdef0"
	verity := "0fedcba9-8765-4321-0fed-cba987654321"
	got, err := rootHashFromGUIDs(data, verity)
	if err != nil {
		t.Fatal(err)
	}
	want := "123456789abcdef0123456789abcdef0" + "0fedcba987654321" + "0fedcba987654321"
	if got != want {
		t.Errorf("rootHashFromGUIDs = %q, want %q", got, want)
	}

	if _, err := rootHashFromGUIDs("short", verity); err == nil {
		t.Error("expected error for malformed data GUID")
	}
	if _, err := rootHashFromGUIDs(data, "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"); err == nil {
		t.Error("expected error for non-hex GUID")
	}
}

// TestRootHashGUIDRoundTrip validates the byte-order assumption end to end:
// a root hash split into two canonical UUID strings, written in GPT on-disk
// mixed-endian form (as sfdisk/sgdisk would), must decode via guidString
// back to the same hash halves.
func TestRootHashGUIDRoundTrip(t *testing.T) {
	rootHash := "8e0e58c326cc91a263d734e84d1023ef505566e10183c456da5ee45c92d44777"
	asUUID := func(h string) string {
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	}
	dataUUID := asUUID(rootHash[:32])
	verityUUID := asUUID(rootHash[32:])

	// Encode to on-disk form and decode again (what parseGPT does).
	gotData := guidString(guidBytes(t, dataUUID))
	gotVerity := guidString(guidBytes(t, verityUUID))

	rebuilt, err := rootHashFromGUIDs(gotData, gotVerity)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != rootHash {
		t.Errorf("round-tripped root hash = %q, want %q", rebuilt, rootHash)
	}
}

func TestDMIoctlLayout(t *testing.T) {
	// struct dm_ioctl per /usr/include/linux/dm-ioctl.h:
	// __u32 version[3]; __u32 data_size; __u32 data_start;
	// __u32 target_count; __s32 open_count; __u32 flags; __u32 event_nr;
	// __u32 padding; __u64 dev; char name[128]; char uuid[129]; char data[7];
	if dmOffDataSize != 12 || dmOffDataStart != 16 || dmOffTargetCount != 20 ||
		dmOffOpenCount != 24 || dmOffFlags != 28 || dmOffEventNr != 32 ||
		dmOffPadding != 36 || dmOffDev != 40 || dmOffName != 48 ||
		dmOffUUID != 176 || dmOffData != 305 {
		t.Error("dm_ioctl field offsets do not match linux/dm-ioctl.h")
	}
	if dmIoctlSize != dmOffData+7 {
		t.Errorf("dmIoctlSize = %d, want %d", dmIoctlSize, dmOffData+7)
	}

	// struct dm_target_spec: __u64 sector_start; __u64 length;
	// __s32 status; __u32 next; char target_type[16];
	if dmSpecOffSectorStart != 0 || dmSpecOffLength != 8 ||
		dmSpecOffStatus != 16 || dmSpecOffNext != 20 || dmSpecOffTargetType != 24 {
		t.Error("dm_target_spec field offsets do not match linux/dm-ioctl.h")
	}
	if dmTargetSpecSize != dmSpecOffTargetType+dmMaxTypeName {
		t.Errorf("dmTargetSpecSize = %d, want %d", dmTargetSpecSize, dmSpecOffTargetType+dmMaxTypeName)
	}

	// _IOWR(0xfd, cmd, struct dm_ioctl): dir=3<<30, size=312<<16, type
	// 0xfd<<8, nr=cmd.
	iowr := func(cmd uint32) uint32 {
		return 3<<30 | uint32(dmIoctlSize)<<16 | 0xfd<<8 | cmd
	}
	if dmDevCreate != iowr(3) || dmDevRemove != iowr(4) ||
		dmDevSuspend != iowr(6) || dmTableLoad != iowr(9) {
		t.Errorf("dm ioctl numbers mismatch: create=%#x remove=%#x suspend=%#x load=%#x",
			dmDevCreate, dmDevRemove, dmDevSuspend, dmTableLoad)
	}
	if dmDevCreate != 0xc138fd03 || dmDevRemove != 0xc138fd04 ||
		dmDevSuspend != 0xc138fd06 || dmTableLoad != 0xc138fd09 {
		t.Error("dm ioctl constants drifted from verified values")
	}
}

func TestDMVerityTablePacking(t *testing.T) {
	params := "1 /dev/loop0p1 /dev/loop0p2 4096 4096 2048 1 sha256 aabb ccdd"
	buf, err := dmVerityTable("sysext-x-verity", 16384, params)
	if err != nil {
		t.Fatal(err)
	}

	paramsLen := (len(params) + 1 + 7) &^ 7
	wantTotal := dmIoctlSize + dmTargetSpecSize + paramsLen
	if len(buf) != wantTotal {
		t.Fatalf("buffer length = %d, want %d", len(buf), wantTotal)
	}
	if len(buf)%8 != 0 {
		t.Errorf("buffer length %d not 8-byte aligned", len(buf))
	}

	if v := binary.LittleEndian.Uint32(buf[dmOffVersion:]); v != 4 {
		t.Errorf("version major = %d, want 4", v)
	}
	if v := binary.LittleEndian.Uint32(buf[dmOffDataSize:]); v != uint32(wantTotal) {
		t.Errorf("data_size = %d, want %d", v, wantTotal)
	}
	if v := binary.LittleEndian.Uint32(buf[dmOffDataStart:]); v != dmIoctlSize {
		t.Errorf("data_start = %d, want %d", v, dmIoctlSize)
	}
	if v := binary.LittleEndian.Uint32(buf[dmOffTargetCount:]); v != 1 {
		t.Errorf("target_count = %d, want 1", v)
	}
	if v := binary.LittleEndian.Uint32(buf[dmOffFlags:]); v&dmReadonlyFlag == 0 {
		t.Errorf("flags = %#x, want DM_READONLY_FLAG set", v)
	}
	name := buf[dmOffName : dmOffName+dmNameLen]
	if got := string(name[:bytes.IndexByte(name, 0)]); got != "sysext-x-verity" {
		t.Errorf("name = %q", got)
	}

	spec := buf[dmIoctlSize:]
	if v := binary.LittleEndian.Uint64(spec[dmSpecOffSectorStart:]); v != 0 {
		t.Errorf("sector_start = %d, want 0", v)
	}
	if v := binary.LittleEndian.Uint64(spec[dmSpecOffLength:]); v != 16384 {
		t.Errorf("length = %d, want 16384", v)
	}
	if v := binary.LittleEndian.Uint32(spec[dmSpecOffNext:]); v != uint32(dmTargetSpecSize+paramsLen) {
		t.Errorf("next = %d, want %d", v, dmTargetSpecSize+paramsLen)
	}
	tt := spec[dmSpecOffTargetType : dmSpecOffTargetType+dmMaxTypeName]
	if got := string(tt[:bytes.IndexByte(tt, 0)]); got != "verity" {
		t.Errorf("target_type = %q, want verity", got)
	}
	gotParams := spec[dmTargetSpecSize:]
	if got := string(gotParams[:bytes.IndexByte(gotParams, 0)]); got != params {
		t.Errorf("params = %q, want %q", got, params)
	}
}

func TestDMIoctlHeaderNameTooLong(t *testing.T) {
	buf := make([]byte, dmIoctlSize)
	if err := dmIoctlHeader(buf, strings.Repeat("x", dmNameLen), dmIoctlSize, 0); err == nil {
		t.Error("expected error for over-long dm name")
	}
}

func TestDMDecodeDev(t *testing.T) {
	// new_encode_dev: (minor & 0xff) | (major << 8) | ((minor & ~0xff) << 12)
	enc := func(major, minor uint32) uint64 {
		return uint64(minor&0xff) | uint64(major)<<8 | uint64(minor&^0xff)<<12
	}
	for _, tc := range []struct{ major, minor uint32 }{
		{253, 0}, {253, 3}, {252, 300}, {10, 236},
	} {
		gotMaj, gotMin := dmDecodeDev(enc(tc.major, tc.minor))
		if gotMaj != tc.major || gotMin != tc.minor {
			t.Errorf("dmDecodeDev(enc(%d,%d)) = %d,%d", tc.major, tc.minor, gotMaj, gotMin)
		}
	}
}

func TestVerityDeviceName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/var/lib/extensions/myext.raw", "sysext-myext-verity"},
		{"/var/lib/extensions/my ext!.raw", "sysext-my_ext_-verity"},
		{"plain", "sysext-plain-verity"},
		{"/x/weird/.raw", "sysext--verity"},
	}
	for _, tc := range cases {
		if got := verityDeviceName(tc.in); got != tc.want {
			t.Errorf("verityDeviceName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := verityDeviceName("/p/" + strings.Repeat("a", 200) + ".raw"); len(got) >= dmNameLen {
		t.Errorf("long name not truncated: %d bytes", len(got))
	}
}

func TestVerityParams(t *testing.T) {
	sb := &veritySuperblock{
		Version:       1,
		DataBlockSize: 4096,
		HashBlockSize: 4096,
		DataBlocks:    2048,
		Algorithm:     "sha256",
		Salt:          []byte{0xaa, 0xbb},
	}
	got := verityParams(sb, "/dev/loop0p1", "/dev/loop0p2", "deadbeef")
	want := "1 /dev/loop0p1 /dev/loop0p2 4096 4096 2048 1 sha256 deadbeef aabb"
	if got != want {
		t.Errorf("verityParams = %q, want %q", got, want)
	}

	sb.Salt = nil
	got = verityParams(sb, "a", "b", "00")
	if !strings.HasSuffix(got, " -") {
		t.Errorf("empty salt must encode as '-': %q", got)
	}
}

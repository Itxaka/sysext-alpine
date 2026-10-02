package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"unicode/utf16"
)

// designator is a partition designator as defined by the UAPI
// Discoverable Partitions Specification (systemd's PartitionDesignator,
// same order).
type designator int

const (
	partRoot designator = iota
	partUsr
	partHome
	partSrv
	partESP
	partXBootLdr
	partSwap
	partRootVerity
	partUsrVerity
	partRootVeritySig
	partUsrVeritySig
	partTmp
	partVar
	numDesignators

	partInvalid designator = -1
)

var designatorNames = [numDesignators]string{
	"root", "usr", "home", "srv", "esp", "xbootldr", "swap",
	"root-verity", "usr-verity", "root-verity-sig", "usr-verity-sig",
	"tmp", "var",
}

func (d designator) String() string {
	if d < 0 || d >= numDesignators {
		return "invalid"
	}
	return designatorNames[d]
}

func designatorFromString(s string) (designator, bool) {
	for i, n := range designatorNames {
		if n == s {
			return designator(i), true
		}
	}
	return partInvalid, false
}

// verityHash returns the verity hash designator protecting d.
func (d designator) verityHash() designator {
	switch d {
	case partRoot:
		return partRootVerity
	case partUsr:
		return partUsrVerity
	default:
		return partInvalid
	}
}

// veritySig returns the verity signature designator for d.
func (d designator) veritySig() designator {
	switch d {
	case partRoot:
		return partRootVeritySig
	case partUsr:
		return partUsrVeritySig
	default:
		return partInvalid
	}
}

// verityData maps a verity hash or signature designator to the data
// designator it protects.
func (d designator) verityData() designator {
	switch d {
	case partRootVerity, partRootVeritySig:
		return partRoot
	case partUsrVerity, partUsrVeritySig:
		return partUsr
	default:
		return partInvalid
	}
}

func (d designator) isVerityHash() bool { return d == partRootVerity || d == partUsrVerity }
func (d designator) isVeritySig() bool  { return d == partRootVeritySig || d == partUsrVeritySig }

// versioned designators pick the newest partition by label when several
// candidates for the same architecture exist (A/B schemes).
func (d designator) versioned() bool {
	switch d {
	case partRoot, partUsr, partRootVerity, partUsrVerity, partRootVeritySig, partUsrVeritySig:
		return true
	default:
		return false
	}
}

// GPT attribute bits (UEFI generic and DPS specific).
const (
	gptFlagNoBlockIOProtocol = uint64(1) << 1
	gptFlagGrowFS            = uint64(1) << 59
	gptFlagReadOnly          = uint64(1) << 60
	gptFlagNoAuto            = uint64(1) << 63
)

// archTypes holds the type GUIDs of one architecture in the order root,
// root-verity, root-verity-sig, usr, usr-verity, usr-verity-sig.
type archTypes struct {
	arch  string
	types [6]string
}

// archPartitionTypes is generated from systemd v262 src/systemd/sd-gpt.h and
// matches the UAPI Discoverable Partitions Specification.
var archPartitionTypes = []archTypes{
	{"alpha", [6]string{"6523f8ae-3eb1-4e2a-a05a-18b695ae656f", "fc56d9e9-e6e5-4c06-be32-e74407ce09a5", "d46495b7-a053-414f-80f7-700c99921ef8", "e18cf08c-33ec-4c0d-8246-c6c6fb3da024", "8cce0d25-c0d0-4a44-bd87-46331bf1df67", "5c6e1c76-076a-457a-a0fe-f3b4cd21ce6e"}},
	{"arc", [6]string{"d27f46ed-2919-4cb8-bd25-9531f3c16534", "24b2d975-0f97-4521-afa1-cd531e421b8d", "143a70ba-cbd3-4f06-919f-6c05683a78bc", "7978a683-6316-4922-bbee-38bff5a2fecc", "fca0598c-d880-4591-8c16-4eda05c7347c", "94f9a9a1-9971-427a-a400-50cb297f0f35"}},
	{"arm", [6]string{"69dad710-2ce4-4e3c-b16c-21a1d49abed3", "7386cdf2-203c-47a9-a498-f2ecce45a2d6", "42b0455f-eb11-491d-98d3-56145ba9d037", "7d0359a3-02b3-4f0a-865c-654403e70625", "c215d751-7bcd-4649-be90-6627490a4c05", "d7ff812f-37d1-4902-a810-d76ba57b975a"}},
	{"arm64", [6]string{"b921b045-1df0-41c3-af44-4c6f280d3fae", "df3300ce-d69f-4c92-978c-9bfb0f38d820", "6db69de6-29f4-4758-a7a5-962190f00ce3", "b0e01050-ee5f-4390-949a-9101b17104e9", "6e11a4e7-fbca-4ded-b9e9-e1a512bb664e", "c23ce4ff-44bd-4b00-b2d4-b41b3419e02a"}},
	{"ia64", [6]string{"993d8d3d-f80e-4225-855a-9daf8ed7ea97", "86ed10d5-b607-45bb-8957-d350f23d0571", "e98b36ee-32ba-4882-9b12-0ce14655f46a", "4301d2a6-4e3b-4b2a-bb94-9e0b2c4225ea", "6a491e03-3be7-4545-8e38-83320e0ea880", "8de58bc2-2a43-460d-b14e-a76e4a17b47f"}},
	{"loongarch64", [6]string{"77055800-792c-4f94-b39a-98c91b762bb6", "f3393b22-e9af-4613-a948-9d3bfbd0c535", "5afb67eb-ecc8-4f85-ae8e-ac1e7c50e7d0", "e611c702-575c-4cbe-9a46-434fa0bf7e3f", "f46b2c26-59ae-48f0-9106-c50ed47f673d", "b024f315-d330-444c-8461-44bbde524e99"}},
	{"mips", [6]string{"e9434544-6e2c-47cc-bae2-12d6deafb44c", "7a430799-f711-4c7e-8e5b-1d685bd48607", "bba210a2-9c5d-45ee-9e87-ff2ccbd002d0", "773b2abc-2a99-4398-8bf5-03baac40d02b", "6e5a1bc8-d223-49b7-bca8-37a5fcceb996", "97ae158d-f216-497b-8057-f7f905770f54"}},
	{"mips64", [6]string{"d113af76-80ef-41b4-bdb6-0cff4d3d4a25", "579536f8-6a33-4055-a95a-df2d5e2c42a8", "43ce94d4-0f3d-4999-8250-b9deafd98e6e", "57e13958-7331-4365-8e6e-35eeee17c61b", "81cf9d90-7458-4df4-8dcf-c8a3a404f09b", "05816ce2-dd40-4ac6-a61d-37d32dc1ba7d"}},
	{"mips-le", [6]string{"37c58c8a-d913-4156-a25f-48b1b64e07f0", "d7d150d2-2a04-4a33-8f12-16651205ff7b", "c919cc1f-4456-4eff-918c-f75e94525ca5", "0f4868e9-9952-4706-979f-3ed3a473e947", "46b98d8d-b55c-4e8f-aab3-37fca7f80752", "3e23ca0b-a4bc-4b4e-8087-5ab6a26aa8a9"}},
	{"mips64-le", [6]string{"700bda43-7a34-4507-b179-eeb93d7a7ca3", "16b417f8-3e06-4f57-8dd2-9b5232f41aa6", "904e58ef-5c65-4a31-9c57-6af5fc7c5de7", "c97c1f32-ba06-40b4-9f22-236061b08aa8", "3c3d61fe-b5f3-414d-bb71-8739a694a4ef", "f2c2c7ee-adcc-4351-b5c6-ee9816b66e16"}},
	{"parisc", [6]string{"1aacdb3b-5444-4138-bd9e-e5c2239b2346", "d212a430-fbc5-49f9-a983-a7feef2b8d0e", "15de6170-65d3-431c-916e-b0dcd8393f25", "dc4a4480-6917-4262-a4ec-db9384949f25", "5843d618-ec37-48d7-9f12-cea8e08768b2", "450dd7d1-3224-45ec-9cf2-a43a346d71ee"}},
	{"ppc", [6]string{"1de3f1ef-fa98-47b5-8dcd-4a860a654d78", "98cfe649-1588-46dc-b2f0-add147424925", "1b31b5aa-add9-463a-b2ed-bd467fc857e7", "7d14fec5-cc71-415d-9d6c-06bf0b3c3eaf", "df765d00-270e-49e5-bc75-f47bb2118b09", "7007891d-d371-4a80-86a4-5cb875b9302e"}},
	{"ppc64", [6]string{"912ade1d-a839-4913-8964-a10eee08fbd2", "9225a9a3-3c19-4d89-b4f6-eeff88f17631", "f5e2c20c-45b2-4ffa-bce9-2a60737e1aaf", "2c9739e2-f068-46b3-9fd0-01c5a9afbcca", "bdb528a5-a259-475f-a87d-da53fa736a07", "0b888863-d7f8-4d9e-9766-239fce4d58af"}},
	{"ppc64-le", [6]string{"c31c45e6-3f39-412e-80fb-4809c4980599", "906bd944-4589-4aae-a4e4-dd983917446a", "d4a236e7-e873-4c07-bf1d-bf6cf7f1c3c6", "15bb03af-77e7-4d4a-b12b-c0d084f7491c", "ee2b9983-21e8-4153-86d9-b6901a54d1ce", "c8bfbd1e-268e-4521-8bba-bf314c399557"}},
	{"riscv32", [6]string{"60d5a7fe-8e7d-435c-b714-3dd8162144e1", "ae0253be-1167-4007-ac68-43926c14c5de", "3a112a75-8729-4380-b4cf-764d79934448", "b933fb22-5c3f-4f91-af90-e2bb0fa50702", "cb1ee4e3-8cd0-4136-a0a4-aa61a32e8730", "c3836a13-3137-45ba-b583-b16c50fe5eb4"}},
	{"riscv64", [6]string{"72ec70a6-cf74-40e6-bd49-4bda08e8f224", "b6ed5582-440b-4209-b8da-5ff7c419ea3d", "efe0f087-ea8d-4469-821a-4c2a96a8386a", "beaec34b-8442-439b-a40b-984381ed097d", "8f1056be-9b05-47c4-81d6-be53128e5b54", "d2f9000a-7a18-453f-b5cd-4d32f77a7b32"}},
	{"s390", [6]string{"08a7acea-624c-4a20-91e8-6e0fa67d23f9", "7ac63b47-b25c-463b-8df8-b4a94e6c90e1", "3482388e-4254-435a-a241-766a065f9960", "cd0f869b-d0fb-4ca0-b141-9ea87cc78d66", "b663c618-e7bc-4d6d-90aa-11b756bb1797", "17440e4f-a8d0-467f-a46e-3912ae6ef2c5"}},
	{"s390x", [6]string{"5eead9a9-fe09-4a1e-a1d7-520d00531306", "b325bfbe-c7be-4ab8-8357-139e652d2f6b", "c80187a5-73a3-491a-901a-017c3fa953e9", "8a4f5770-50aa-4ed3-874a-99b710db6fea", "31741cc4-1a2a-4111-a581-e00b447d2d06", "3f324816-667b-46ae-86ee-9b0c0c6c11b4"}},
	{"tilegx", [6]string{"c50cdd70-3862-4cc3-90e1-809a8c93ee2c", "966061ec-28e4-4b2e-b4a5-1f0a825a1d84", "b3671439-97b0-4a53-90f7-2d5a8f3ad47b", "55497029-c7c1-44cc-aa39-815ed1558630", "2fb4bf56-07fa-42da-8132-6b139f2026ae", "4ede75e2-6ccc-4cc8-b9c7-70334b087510"}},
	{"x86", [6]string{"44479540-f297-41b2-9af7-d131d5f0458a", "d13c5d3b-b5d1-422a-b29f-9454fdc89d76", "5996fc05-109c-48de-808b-23fa0830b676", "75250d76-8cc6-458e-bd66-bd47cc81a812", "8f461b0d-14ee-4e81-9aa9-049b6fb97abd", "974a71c0-de41-43c3-be5d-5c5ccd1ad2c0"}},
	{"x86-64", [6]string{"4f68bce3-e8cd-4db1-96e7-fbcaf984b709", "2c7357ed-ebd2-46d9-aec1-23d437ec2bf5", "41092b05-9fc8-4523-994f-2def0408b176", "8484680c-9521-48c6-9c11-b0720656f69e", "77ff5f63-e7b6-4633-acf4-1565b864c0e6", "e7bb33fb-06cf-4e81-8273-e543b413e2e2"}},
}

// Architecture independent DPS type GUIDs.
const (
	gptTypeESP          = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	gptTypeXBootLdr     = "bc13c2ff-59e6-4262-a352-b275fd6f7172"
	gptTypeSwap         = "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f"
	gptTypeHome         = "933ac7e1-2eb4-4f13-b844-0e14e2aef915"
	gptTypeSrv          = "3b8f8425-20e0-4f3b-907f-1a25a76f98e8"
	gptTypeVar          = "4d21b016-b534-45c2-a9fb-5c16e091fd2d"
	gptTypeTmp          = "7ec6f557-3bc5-4aca-b293-16ef5df639d1"
	gptTypeLinuxGeneric = "0fc63daf-8483-4772-8e79-3d69d8477de4"
)

// gptType is what a partition type GUID maps to: a designator and, for
// root/usr and their verity partitions, an architecture.
type gptType struct {
	designator designator
	arch       string
}

var gptTypes = buildGPTTypes()

func buildGPTTypes() map[string]gptType {
	m := map[string]gptType{
		gptTypeESP:      {partESP, ""},
		gptTypeXBootLdr: {partXBootLdr, ""},
		gptTypeSwap:     {partSwap, ""},
		gptTypeHome:     {partHome, ""},
		gptTypeSrv:      {partSrv, ""},
		gptTypeVar:      {partVar, ""},
		gptTypeTmp:      {partTmp, ""},
	}
	sextet := [6]designator{partRoot, partRootVerity, partRootVeritySig, partUsr, partUsrVerity, partUsrVeritySig}
	for _, a := range archPartitionTypes {
		for i, g := range a.types {
			m[g] = gptType{sextet[i], a.arch}
		}
	}
	return m
}

// lookupGPTType classifies a type GUID; unknown types get partInvalid.
func lookupGPTType(guid string) gptType {
	if t, ok := gptTypes[guid]; ok {
		return t
	}
	return gptType{designator: partInvalid}
}

// secondaryArch is systemd's ARCHITECTURE_SECONDARY: the 32-bit
// architecture a 64-bit host can also execute.
func secondaryArch(native string) string {
	switch native {
	case "x86-64":
		return "x86"
	case "arm64":
		return "arm"
	case "s390x":
		return "s390"
	case "ppc64":
		return "ppc"
	case "ppc64-le":
		return "ppc-le"
	default:
		return ""
	}
}

// compareArch ranks two partition architectures for the host (systemd
// compare_arch): native beats secondary beats anything else.
func compareArch(a, b, native string) int {
	if a == b {
		return 0
	}
	if a == native {
		return 1
	}
	if b == native {
		return -1
	}
	if sec := secondaryArch(native); sec != "" {
		if a == sec {
			return 1
		}
		if b == sec {
			return -1
		}
	}
	return 0
}

// partition is one entry of a GPT or MBR partition table.
type partition struct {
	// Index is the kernel partition number (loopNp<Index>).
	Index int
	// TypeGUID and UUID are canonical lowercase GUID strings (GPT only).
	TypeGUID string
	UUID     string
	// MBRType is the MBR partition type byte (MBR only).
	MBRType byte
	// Start and Sectors are in units of the table's sector size.
	Start   uint64
	Sectors uint64
	// Attrs holds the GPT attribute bits, or the MBR boot indicator.
	Attrs uint64
	Label string
}

// partitionTable is a parsed GPT or MBR partition table.
type partitionTable struct {
	gpt        bool
	sectorSize int64
	parts      []partition
}

func (p partition) offset(ss int64) int64 { return int64(p.Start) * ss }
func (p partition) size(ss int64) int64   { return int64(p.Sectors) * ss }

const (
	gptSignature        = "EFI PART"
	gptRevision         = 0x00010000
	gptHeaderSize       = 92
	gptEntrySize        = 128
	gptMaxEntries       = 1024
	gptMaxSectorSize    = 4096
	mbrPartitionOffset  = 446
	mbrSignatureOffset  = 510
	mbrTypeGPTProtected = 0xee
)

var (
	errNoPartitionTable    = errors.New("no partition table")
	errAmbiguousSectorSize = errors.New("valid GPT headers found at offsets matching multiple sector sizes")
)

// gptHeaderHasSignature mirrors systemd's gpt_header_has_signature().
func gptHeaderHasSignature(h []byte) bool {
	if len(h) < gptHeaderSize || string(h[:8]) != gptSignature {
		return false
	}
	if binary.LittleEndian.Uint32(h[8:]) != gptRevision {
		return false
	}
	hsz := binary.LittleEndian.Uint32(h[12:])
	if hsz < gptHeaderSize || hsz > gptMaxSectorSize {
		return false
	}
	return binary.LittleEndian.Uint64(h[24:]) == 1
}

// probeSectorSize mirrors systemd's gpt_probe(): it looks for a GPT header
// at LBA 1 for sector sizes 512…4096 and returns the matching size, 0 when
// none matches, or errAmbiguousSectorSize when more than one does.
func probeSectorSize(r io.ReaderAt) (int64, error) {
	buf := make([]byte, 2*gptMaxSectorSize)
	if n, err := r.ReadAt(buf, 0); n < len(buf) {
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return 0, nil
	}
	var found int64
	for sz := int64(512); sz <= gptMaxSectorSize; sz <<= 1 {
		if !gptHeaderHasSignature(buf[sz:]) {
			continue
		}
		if found != 0 {
			return 0, errAmbiguousSectorSize
		}
		found = sz
	}
	return found, nil
}

// readPartitionTable parses the GPT (sector size ss) or, failing that, an
// MBR partition table from an image of the given size in bytes. It returns
// errNoPartitionTable when neither is present.
func readPartitionTable(r io.ReaderAt, size, ss int64) (*partitionTable, error) {
	mbr := make([]byte, 512)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errNoPartitionTable
		}
		return nil, err
	}
	if binary.LittleEndian.Uint16(mbr[mbrSignatureOffset:]) != 0xaa55 {
		return nil, errNoPartitionTable
	}
	if mbrHasProtectiveEntry(mbr) {
		parts, err := parseGPT(r, size, ss)
		if err != nil {
			return nil, err
		}
		return &partitionTable{gpt: true, sectorSize: ss, parts: parts}, nil
	}
	parts, ok := parseMBR(mbr)
	if !ok {
		return nil, errNoPartitionTable
	}
	return &partitionTable{sectorSize: 512, parts: parts}, nil
}

func mbrHasProtectiveEntry(mbr []byte) bool {
	for i := range 4 {
		if mbr[mbrPartitionOffset+16*i+4] == mbrTypeGPTProtected {
			return true
		}
	}
	return false
}

// parseMBR returns the primary partitions of a DOS partition table.
func parseMBR(mbr []byte) ([]partition, bool) {
	var parts []partition
	for i := range 4 {
		e := mbr[mbrPartitionOffset+16*i : mbrPartitionOffset+16*(i+1)]
		if e[0] != 0 && e[0] != 0x80 {
			return nil, false
		}
		start := binary.LittleEndian.Uint32(e[8:])
		n := binary.LittleEndian.Uint32(e[12:])
		if e[4] == 0 || n == 0 {
			continue
		}
		parts = append(parts, partition{
			Index:   i + 1,
			MBRType: e[4],
			Start:   uint64(start),
			Sectors: uint64(n),
			Attrs:   uint64(e[0]),
		})
	}
	return parts, len(parts) > 0
}

// parseGPT validates and parses the GPT of an image of size bytes with
// sector size ss, falling back to the alternate header at the last LBA when
// the primary one is invalid (like libblkid). Validation follows the kernel
// and libblkid: header CRC32, my_lba, usable range, 128-byte entries and the
// entry array CRC32; the entry count is bounded like systemd's gpt_probe().
func parseGPT(r io.ReaderAt, size, ss int64) ([]partition, error) {
	if ss < 512 || ss > gptMaxSectorSize || ss&(ss-1) != 0 {
		return nil, fmt.Errorf("invalid sector size %d", ss)
	}
	sectors := size / ss
	if sectors < 3 {
		return nil, errors.New("image too small for a GPT")
	}
	lastLBA := uint64(sectors - 1)

	parts, err := parseGPTAt(r, 1, lastLBA, ss)
	if err == nil {
		return parts, nil
	}
	if alt, aerr := parseGPTAt(r, lastLBA, lastLBA, ss); aerr == nil {
		return alt, nil
	}
	return nil, err
}

func parseGPTAt(r io.ReaderAt, lba, lastLBA uint64, ss int64) ([]partition, error) {
	which := "primary"
	if lba != 1 {
		which = "alternate"
	}
	hdr := make([]byte, ss)
	if _, err := r.ReadAt(hdr, int64(lba)*ss); err != nil {
		return nil, fmt.Errorf("reading %s GPT header: %w", which, err)
	}
	if string(hdr[:8]) != gptSignature {
		return nil, fmt.Errorf("no %s GPT header", which)
	}
	hsz := binary.LittleEndian.Uint32(hdr[12:])
	if hsz < gptHeaderSize || int64(hsz) > ss {
		return nil, fmt.Errorf("invalid %s GPT header size %d", which, hsz)
	}
	stored := binary.LittleEndian.Uint32(hdr[16:])
	h := bytes.Clone(hdr[:hsz])
	clear(h[16:20])
	if crc32.ChecksumIEEE(h) != stored {
		return nil, fmt.Errorf("invalid %s GPT header CRC", which)
	}
	if my := binary.LittleEndian.Uint64(hdr[24:]); my != lba {
		return nil, fmt.Errorf("%s GPT header claims LBA %d, found at %d", which, my, lba)
	}
	firstUsable := binary.LittleEndian.Uint64(hdr[40:])
	lastUsable := binary.LittleEndian.Uint64(hdr[48:])
	if firstUsable > lastUsable || lastUsable > lastLBA {
		return nil, fmt.Errorf("invalid %s GPT usable range %d..%d", which, firstUsable, lastUsable)
	}
	entryLBA := binary.LittleEndian.Uint64(hdr[72:])
	count := binary.LittleEndian.Uint32(hdr[80:])
	entrySize := binary.LittleEndian.Uint32(hdr[84:])
	arrayCRC := binary.LittleEndian.Uint32(hdr[88:])
	if entrySize != gptEntrySize {
		return nil, fmt.Errorf("unsupported GPT partition entry size %d", entrySize)
	}
	if count == 0 || count > gptMaxEntries {
		return nil, fmt.Errorf("invalid GPT partition entry count %d", count)
	}
	if entryLBA > lastLBA || entryLBA > uint64(math.MaxInt64/ss) {
		return nil, fmt.Errorf("GPT partition entry array LBA %d out of range", entryLBA)
	}

	entries := make([]byte, int(count)*gptEntrySize)
	if _, err := r.ReadAt(entries, int64(entryLBA)*ss); err != nil {
		return nil, fmt.Errorf("reading GPT partition entries: %w", err)
	}
	if crc32.ChecksumIEEE(entries) != arrayCRC {
		return nil, errors.New("invalid GPT partition entry array CRC")
	}

	var parts []partition
	for i := range int(count) {
		e := entries[i*gptEntrySize : (i+1)*gptEntrySize]
		if isZero(e[:16]) {
			continue
		}
		first := binary.LittleEndian.Uint64(e[32:])
		last := binary.LittleEndian.Uint64(e[40:])
		if first > last || first < firstUsable || last > lastUsable {
			continue
		}
		parts = append(parts, partition{
			Index:    i + 1,
			TypeGUID: guidString(e[0:16]),
			UUID:     guidString(e[16:32]),
			Start:    first,
			Sectors:  last - first + 1,
			Attrs:    binary.LittleEndian.Uint64(e[48:]),
			Label:    decodeGPTLabel(e[56:128]),
		})
	}
	return parts, nil
}

// decodeGPTLabel decodes the NUL-terminated UTF-16LE partition name.
func decodeGPTLabel(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

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

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

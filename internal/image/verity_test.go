package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// mkVeritySB crafts an on-disk verity superblock and lets the caller
// mutate it before parsing.
func mkVeritySB(mutate func([]byte)) []byte {
	b := make([]byte, veritySBSize)
	copy(b, verityMagic)
	binary.LittleEndian.PutUint32(b[vsbOffVersion:], 1)
	binary.LittleEndian.PutUint32(b[vsbOffHashType:], 1)
	for i := range 16 {
		b[vsbOffUUID+i] = byte(i)
	}
	copy(b[vsbOffAlgorithm:], "sha256")
	binary.LittleEndian.PutUint32(b[vsbOffDataBlockSize:], 4096)
	binary.LittleEndian.PutUint32(b[vsbOffHashBlockSize:], 4096)
	binary.LittleEndian.PutUint64(b[vsbOffDataBlocks:], 2048)
	binary.LittleEndian.PutUint16(b[vsbOffSaltSize:], 32)
	for i := range 32 {
		b[vsbOffSalt+i] = byte(0xa0 + i)
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
	if sb.Version != 1 || sb.HashType != 1 || sb.Algorithm != "sha256" ||
		sb.DataBlockSize != 4096 || sb.HashBlockSize != 4096 || sb.DataBlocks != 2048 {
		t.Errorf("unexpected superblock %+v", sb)
	}
	if len(sb.Salt) != 32 || sb.Salt[0] != 0xa0 || sb.Salt[31] != 0xbf {
		t.Errorf("salt = %x", sb.Salt)
	}
	if !bytes.Equal(sb.UUID[:4], []byte{0, 1, 2, 3}) {
		t.Errorf("uuid = %x", sb.UUID)
	}

	sb, err = parseVeritySuperblock(mkVeritySB(func(b []byte) {
		clear(b[vsbOffAlgorithm : vsbOffAlgorithm+32])
		copy(b[vsbOffAlgorithm:], "sha512")
		binary.LittleEndian.PutUint32(b[vsbOffHashType:], 0)
	}))
	if err != nil || sb.Algorithm != "sha512" || sb.HashType != 0 {
		t.Errorf("sha512/hash type 0: %+v, %v", sb, err)
	}
}

func TestParseVeritySuperblockErrors(t *testing.T) {
	setAlg := func(alg string) func([]byte) {
		return func(b []byte) {
			clear(b[vsbOffAlgorithm : vsbOffAlgorithm+32])
			copy(b[vsbOffAlgorithm:], alg)
		}
	}
	cases := map[string][]byte{
		"truncated":   mkVeritySB(nil)[:100],
		"bad magic":   mkVeritySB(func(b []byte) { b[0] = 'X' }),
		"version 2":   mkVeritySB(func(b []byte) { binary.LittleEndian.PutUint32(b[vsbOffVersion:], 2) }),
		"hash type 2": mkVeritySB(func(b []byte) { binary.LittleEndian.PutUint32(b[vsbOffHashType:], 2) }),
		"salt size":   mkVeritySB(func(b []byte) { binary.LittleEndian.PutUint16(b[vsbOffSaltSize:], 300) }),
		"no blocks":   mkVeritySB(func(b []byte) { binary.LittleEndian.PutUint64(b[vsbOffDataBlocks:], 0) }),
		"huge blocks": mkVeritySB(func(b []byte) { binary.LittleEndian.PutUint64(b[vsbOffDataBlocks:], 1<<62) }),
		"block size":  mkVeritySB(func(b []byte) { binary.LittleEndian.PutUint32(b[vsbOffDataBlockSize:], 1000) }),
		"zero hash":   mkVeritySB(func(b []byte) { binary.LittleEndian.PutUint32(b[vsbOffHashBlockSize:], 0) }),
		"algorithm":   mkVeritySB(setAlg("md5")),
		"injection":   mkVeritySB(setAlg("sha256 1 ignore_corruption")),
	}
	for name, data := range cases {
		if _, err := parseVeritySuperblock(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVeritySuperblockRootHashLength(t *testing.T) {
	sb, err := parseVeritySuperblock(mkVeritySB(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.checkRootHash(make([]byte, 32)); err != nil {
		t.Errorf("sha256 with 32 bytes: %v", err)
	}
	if err := sb.checkRootHash(make([]byte, 64)); err == nil {
		t.Error("sha256 with a 64-byte root hash accepted")
	}
	sb.Algorithm = "sha512"
	if err := sb.checkRootHash(make([]byte, 64)); err != nil {
		t.Errorf("sha512 with 64 bytes: %v", err)
	}
	if err := sb.checkRootHash(make([]byte, 32)); err == nil {
		t.Error("sha512 with a GUID-sized root hash accepted")
	}
}

func TestVerityParams(t *testing.T) {
	sb := &veritySuperblock{HashType: 1, DataBlockSize: 4096, HashBlockSize: 4096, DataBlocks: 2048, Algorithm: "sha256", Salt: []byte{0xaa, 0xbb}}
	got := verityParams(sb, "7:1", "7:2", []byte{0xde, 0xad, 0xbe, 0xef}, "")
	if want := "1 7:1 7:2 4096 4096 2048 1 sha256 deadbeef aabb"; got != want {
		t.Errorf("verityParams = %q, want %q", got, want)
	}
	sb.Salt = nil
	got = verityParams(sb, "7:1", "7:2", []byte{0xAB}, "sysext:loop0p1-verity")
	if want := "1 7:1 7:2 4096 4096 2048 1 sha256 ab - 2 root_hash_sig_key_desc sysext:loop0p1-verity"; got != want {
		t.Errorf("verityParams = %q, want %q", got, want)
	}
}

func TestDMHeaderNativeEndian(t *testing.T) {
	if unix.SizeofDmIoctl != 312 || unix.SizeofDmTargetSpec != 40 {
		t.Fatalf("struct sizes %d/%d", unix.SizeofDmIoctl, unix.SizeofDmTargetSpec)
	}
	params := "1 7:1 7:2 4096 4096 2048 1 sha256 aabb ccdd"
	buf, err := dmVerityTable("loop0p1-5-verity", 16384, params)
	if err != nil {
		t.Fatal(err)
	}
	paramsLen := (len(params) + 1 + 7) &^ 7
	if len(buf) != 312+40+paramsLen || len(buf)%8 != 0 {
		t.Fatalf("buffer length %d", len(buf))
	}

	var h unix.DmIoctl
	if _, err := binary.Decode(buf, binary.NativeEndian, &h); err != nil {
		t.Fatal(err)
	}
	if h.Version[0] != 4 || h.Data_size != uint32(len(buf)) || h.Data_start != 312 ||
		h.Target_count != 1 || h.Flags&unix.DM_READONLY_FLAG == 0 {
		t.Errorf("header %+v", h)
	}
	if name := string(h.Name[:bytes.IndexByte(h.Name[:], 0)]); name != "loop0p1-5-verity" {
		t.Errorf("name = %q", name)
	}

	var spec unix.DmTargetSpec
	if _, err := binary.Decode(buf[312:], binary.NativeEndian, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Sector_start != 0 || spec.Length != 16384 || spec.Next != uint32(40+paramsLen) {
		t.Errorf("target spec %+v", spec)
	}
	if tt := string(spec.Target_type[:bytes.IndexByte(spec.Target_type[:], 0)]); tt != "verity" {
		t.Errorf("target type %q", tt)
	}
	p := buf[352:]
	if got := string(p[:bytes.IndexByte(p, 0)]); got != params {
		t.Errorf("params %q", got)
	}

	hdr := make([]byte, 312)
	if err := dmHeader(hdr, strings.Repeat("x", 128), "", 312, 0, 0); err == nil {
		t.Error("over-long dm name accepted")
	}
	if err := dmHeader(hdr, "a/b", "", 312, 0, 0); err == nil {
		t.Error("dm name with '/' accepted")
	}
}

func TestDMIoctlNumbers(t *testing.T) {
	iowr := func(cmd uint32) uint32 { return 3<<30 | 312<<16 | 0xfd<<8 | cmd }
	for name, got := range map[string]uint32{
		"create":  uint32(unix.DM_DEV_CREATE),
		"remove":  uint32(unix.DM_DEV_REMOVE),
		"suspend": uint32(unix.DM_DEV_SUSPEND),
		"load":    uint32(unix.DM_TABLE_LOAD),
	} {
		want := map[string]uint32{"create": iowr(3), "remove": iowr(4), "suspend": iowr(6), "load": iowr(9)}[name]
		if got != want {
			t.Errorf("%s = %#x, want %#x", name, got, want)
		}
	}
}

func TestVerityDMName(t *testing.T) {
	for _, tc := range []struct {
		node    string
		diskseq uint64
		want    string
	}{
		{"/dev/loop3p1", 42, "loop3p1-42-verity"},
		{"/dev/loop3p1", 0, "loop3p1-verity"},
		{"/dev/loop7", 9, "loop7-9-verity"},
	} {
		if got := verityDMName(tc.node, tc.diskseq); got != tc.want {
			t.Errorf("verityDMName(%q, %d) = %q, want %q", tc.node, tc.diskseq, got, tc.want)
		}
	}
	if verityDMName("/dev/loop3p1", 1) == verityDMName("/dev/loop4p1", 1) {
		t.Error("different loop devices must get different names")
	}
}

func TestDMRemoveWithoutDeviceMapper(t *testing.T) {
	dir := t.TempDir()
	misc := filepath.Join(dir, "misc")
	if err := os.WriteFile(misc, []byte(" 1 psaux\n200 tun\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCtl, oldMisc := dmControlPath, procMiscPath
	dmControlPath, procMiscPath = filepath.Join(dir, "control"), misc
	t.Cleanup(func() { dmControlPath, procMiscPath = oldCtl, oldMisc })

	if err := dmRemove("sysext-x-verity"); err != nil {
		t.Errorf("dmRemove without device-mapper: %v", err)
	}
	if _, err := dmMiscMinor(); err == nil {
		t.Error("dmMiscMinor found device-mapper in a misc file without it")
	}
	if err := os.WriteFile(misc, []byte(" 1 psaux\n236 device-mapper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := dmMiscMinor(); err != nil || m != 236 {
		t.Errorf("dmMiscMinor = %d, %v", m, err)
	}
}

func TestSetupVerityErrors(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(node, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rootHash := bytes.Repeat([]byte{1}, 32)
	cases := []struct {
		name   string
		policy string
		hash   []byte
		vs     *veritySettings
		want   string
		errno  unix.Errno
	}{
		{"broken superblock", "root=verity", make([]byte, veritySBSize), &veritySettings{rootHash: rootHash}, "activating dm-verity for the root partition: ", 0},
		{"signature required", "root=signed", mkVeritySB(nil), &veritySettings{rootHash: rootHash},
			"activating dm-verity for the root partition: image does not satisfy image policy: activation of the root partition without a verified signature is not allowed", unix.ERFKILL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseImagePolicy(tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			m := &mounter{path: "/x.raw", policy: p}
			_, err = m.setupVerity(partRoot, tc.vs, bytes.NewReader(tc.hash), &loopDevice{}, node, node)
			ve, ok := errors.AsType[*VerityError](err)
			if !ok || ve.Partition != "root" || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a VerityError %q", err, tc.want)
			}
			if tc.errno != 0 && !errors.Is(err, tc.errno) {
				t.Errorf("err = %v, want errno %v", err, tc.errno)
			}
		})
	}
}

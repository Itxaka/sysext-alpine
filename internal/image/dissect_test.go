package image

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

func archTypesOf(t *testing.T, arch string) [6]string {
	t.Helper()
	for _, a := range archPartitionTypes {
		if a.arch == arch {
			return a.types
		}
	}
	t.Fatalf("no types for %s", arch)
	return [6]string{}
}

// gp builds a GPT partition entry for dissector tests.
func gp(index int, typ, uuid, label string, attrs uint64) partition {
	if uuid == "" {
		uuid = uuidString([]byte{byte(index), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	}
	return partition{Index: index, TypeGUID: typ, UUID: uuid, Label: label, Attrs: attrs, Start: uint64(index * 100), Sectors: 50}
}

func dissectTest(t *testing.T, policy string, vs *veritySettings, parts ...partition) (*dissected, error) {
	t.Helper()
	pol, err := resolvePolicy(policy, release.Sysext)
	if err != nil {
		t.Fatal(err)
	}
	if vs == nil {
		vs = &veritySettings{designator: partInvalid}
	}
	ds := &dissector{
		table:  &partitionTable{gpt: true, sectorSize: 512, parts: parts},
		policy: pol,
		verity: vs,
		native: "x86-64",
		readSig: func(partition) (*veritySig, error) {
			return nil, errors.New("unexpected signature read")
		},
		machineID: func() ([16]byte, error) { return [16]byte{1, 2, 3}, nil },
	}
	return ds.dissect()
}

func TestDissectArchitecturePreference(t *testing.T) {
	x64, x32, arm := archTypesOf(t, "x86-64"), archTypesOf(t, "x86"), archTypesOf(t, "arm64")
	for _, tc := range []struct {
		name  string
		parts []partition
		want  int
	}{
		{"native over secondary", []partition{gp(1, x32[0], "", "", 0), gp(2, x64[0], "", "", 0)}, 2},
		{"secondary over foreign", []partition{gp(1, arm[0], "", "", 0), gp(2, x32[0], "", "", 0)}, 2},
		{"only secondary", []partition{gp(1, x32[0], "", "", 0)}, 1},
		{"only foreign", []partition{gp(1, arm[0], "", "", 0)}, 1},
		{"native wins over later foreign", []partition{gp(1, x64[0], "", "", 0), gp(2, arm[0], "", "", 0)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := dissectTest(t, "", nil, tc.parts...)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.parts[partRoot].Index; got != tc.want {
				t.Errorf("root = partition %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDissectSelectionRules(t *testing.T) {
	x := archTypesOf(t, "x86-64")
	cases := []struct {
		name   string
		policy string
		parts  []partition
		root   int
		usr    int
	}{
		{"newest label wins", "", []partition{gp(1, x[0], "", "foo_1.10", 0), gp(2, x[0], "", "foo_1.2", 0)}, 1, 0},
		{"newest label wins, reverse order", "", []partition{gp(1, x[0], "", "foo_1.2", 0), gp(2, x[0], "", "foo_1.10", 0)}, 2, 0},
		{"_empty skipped", "", []partition{gp(1, x[0], "", "_empty", 0), gp(2, x[0], "", "", 0)}, 2, 0},
		{"no-auto skipped", "", []partition{gp(1, x[0], "", "", gptFlagNoAuto), gp(2, x[0], "", "", 0)}, 2, 0},
		{"generic fallback", "", []partition{gp(1, gptTypeLinuxGeneric, "", "", 0)}, 1, 0},
		{"generic ignored when root exists", "", []partition{gp(1, gptTypeLinuxGeneric, "", "", 0), gp(2, x[0], "", "", 0)}, 2, 0},
		{"root and usr", "", []partition{gp(1, x[0], "", "", 0), gp(2, x[3], "", "", 0)}, 1, 2},
		{"usr only", "", []partition{gp(1, x[3], "", "", 0)}, 0, 1},
		{"root ignored, usr used", "root=ignore:usr=open", []partition{gp(1, x[0], "", "", 0), gp(2, x[3], "", "", 0)}, 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := dissectTest(t, tc.policy, nil, tc.parts...)
			if err != nil {
				t.Fatal(err)
			}
			idx := func(x designator) int {
				if d.parts[x] == nil {
					return 0
				}
				return d.parts[x].Index
			}
			if idx(partRoot) != tc.root || idx(partUsr) != tc.usr {
				t.Errorf("root=%d usr=%d, want root=%d usr=%d", idx(partRoot), idx(partUsr), tc.root, tc.usr)
			}
		})
	}
}

func TestDissectVersionedVerityPairing(t *testing.T) {
	x := archTypesOf(t, "x86-64")
	d, err := dissectTest(t, "", nil,
		gp(1, x[0], "", "foo_1.2", 0), gp(2, x[1], "", "foo_1.2", 0),
		gp(3, x[0], "", "foo_1.10", 0), gp(4, x[1], "", "foo_1.10", 0))
	if err != nil {
		t.Fatal(err)
	}
	if d.parts[partRoot].Index != 3 || d.parts[partRootVerity].Index != 4 {
		t.Errorf("root=%d verity=%d, want 3/4", d.parts[partRoot].Index, d.parts[partRootVerity].Index)
	}
}

func TestDissectErrors(t *testing.T) {
	x, arm := archTypesOf(t, "x86-64"), archTypesOf(t, "arm64")
	cases := []struct {
		name   string
		policy string
		parts  []partition
		want   string
		errno  unix.Errno
		class  release.Class
	}{
		{"nothing usable", "", []partition{gp(1, gptTypeSwap, "", "", 0)}, "found neither", unix.ENXIO, release.Sysext},
		{"policy ignores everything", "root=ignore", []partition{gp(1, x[0], "", "", 0), gp(2, x[3], "", "", 0)}, "found neither", unix.ENXIO, release.Sysext},
		{"confext default ignores usr", "", []partition{gp(1, x[3], "", "", 0)}, "found neither", unix.ENXIO, release.Confext},
		{"two generic", "", []partition{gp(1, gptTypeLinuxGeneric, "", "", 0), gp(2, gptTypeLinuxGeneric, "", "", 0)}, "multiple generic", unix.ENOTUNIQ, release.Sysext},
		{"verity without data", "", []partition{gp(1, x[3], "", "", 0), gp(2, x[1], "", "", 0)}, "without matching root data", unix.EADDRNOTAVAIL, release.Sysext},
		{"sig without verity", "", []partition{gp(1, x[0], "", "", 0), gp(2, x[2], "", "", 0)}, "without matching root verity hash", unix.EADDRNOTAVAIL, release.Sysext},
		{"arch mismatch", "", []partition{gp(1, arm[0], "", "", 0), gp(2, x[3], "", "", 0)}, "different architectures", unix.EREMOTE, release.Sysext},
		{"root verity with usr", "", []partition{gp(1, x[0], "", "", 0), gp(2, x[1], "", "", 0), gp(3, x[3], "", "", 0)}, "split usr", unix.EADDRNOTAVAIL, release.Sysext},
		{"root and usr verity", "", []partition{gp(1, x[0], "", "", 0), gp(2, x[1], "", "", 0), gp(3, x[3], "", "", 0), gp(4, x[4], "", "", 0)}, "not supported", unix.ENOTUNIQ, release.Sysext},
		{"esp must be absent", "root=open:esp=absent", []partition{gp(1, x[0], "", "", 0), gp(2, gptTypeESP, "", "", 0)}, "absent", unix.ERFKILL, release.Sysext},
		{"swap must be absent by default rule", "root=open:=absent", []partition{gp(1, x[0], "", "", 0), gp(2, gptTypeSwap, "", "", 0)}, "absent", unix.ERFKILL, release.Sysext},
		{"sig partition must be absent", "root=open:root-verity-sig=absent", []partition{gp(1, x[0], "", "", 0), gp(2, x[1], "", "", 0), gp(3, x[2], "", "", 0)}, "absent", unix.ERFKILL, release.Sysext},
		{"read-only flag required", "root=read-only-on", []partition{gp(1, x[0], "", "", 0)}, "read-only", unix.ERFKILL, release.Sysext},
		{"verity required", "root=verity", []partition{gp(1, x[0], "", "", 0)}, "does not satisfy", unix.ERFKILL, release.Sysext},
		{"signature required", "root=signed", []partition{gp(1, x[0], "", "", 0), gp(2, x[1], "", "", 0)}, "does not satisfy", unix.ERFKILL, release.Sysext},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := resolvePolicy(tc.policy, tc.class)
			if err != nil {
				t.Fatal(err)
			}
			ds := &dissector{
				table:     &partitionTable{gpt: true, sectorSize: 512, parts: tc.parts},
				policy:    p,
				verity:    &veritySettings{designator: partInvalid},
				native:    "x86-64",
				machineID: hostMachineID,
			}
			_, err = ds.dissect()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if errno, _ := errors.AsType[unix.Errno](err); errno != tc.errno {
				t.Errorf("errno = %v, want %v", errno, tc.errno)
			}
		})
	}
}

func TestDissectReadOnlyFlagSatisfied(t *testing.T) {
	x := archTypesOf(t, "x86-64")
	if _, err := dissectTest(t, "root=read-only-on", nil, gp(1, x[0], "", "", gptFlagReadOnly)); err != nil {
		t.Fatal(err)
	}
}

func TestDissectGenericRootHasNoFlags(t *testing.T) {
	generic := gp(1, gptTypeLinuxGeneric, "", "", gptFlagReadOnly|gptFlagGrowFS)
	if _, err := dissectTest(t, "root=unprotected+read-only-off+growfs-off", nil, generic); err != nil {
		t.Errorf("generic root checked with its own flags: %v", err)
	}
	if _, err := dissectTest(t, "root=unprotected+read-only-on", nil, generic); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("generic root satisfied read-only-on: %v", err)
	}
}

func TestDissectExternalRootHashSelectsByUUID(t *testing.T) {
	x := archTypesOf(t, "x86-64")
	rootHash, _ := hex.DecodeString(strings.Repeat("ab", 16) + strings.Repeat("cd", 16))
	dataUUID, verityUUID, err := rootHashUUIDs(rootHash)
	if err != nil {
		t.Fatal(err)
	}
	vs := &veritySettings{rootHash: rootHash, designator: partRoot}
	d, err := dissectTest(t, "", vs,
		gp(1, x[0], "", "", 0), gp(2, x[1], "", "", 0),
		gp(3, x[0], dataUUID, "", 0), gp(4, x[1], verityUUID, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if d.parts[partRoot].Index != 3 || d.parts[partRootVerity].Index != 4 {
		t.Errorf("root=%d verity=%d, want 3/4", d.parts[partRoot].Index, d.parts[partRootVerity].Index)
	}

	if _, err := dissectTest(t, "", vs, gp(1, x[0], dataUUID, "", 0)); err == nil {
		t.Error("external root hash without matching verity partition accepted")
	}
	if _, _, err := rootHashUUIDs(make([]byte, 32)); err == nil {
		t.Error("all-zero root hash accepted")
	}
}

func TestDissectVarPartitionBinding(t *testing.T) {
	x := archTypesOf(t, "x86-64")
	mid := [16]byte{1, 2, 3}
	app, _ := uuidBytes(gptTypeVar)
	mac := hmac.New(sha256.New, mid[:])
	mac.Write(app)
	id := mac.Sum(nil)[:16]
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	bound := uuidString(id)

	d, err := dissectTest(t, "*", nil, gp(1, x[0], "", "", 0), gp(2, gptTypeVar, bound, "", 0))
	if err != nil || d.parts[partVar] == nil {
		t.Fatalf("bound /var partition not used: %v", err)
	}
	d, err = dissectTest(t, "*", nil, gp(1, x[0], "", "", 0), gp(2, gptTypeVar, "", "", 0))
	if err != nil || d.parts[partVar] != nil {
		t.Fatalf("foreign /var partition used: %v", err)
	}
}

func TestDissectMBR(t *testing.T) {
	pol, _ := resolvePolicy("", release.Sysext)
	ds := &dissector{
		table: &partitionTable{sectorSize: 512, parts: []partition{
			{Index: 1, MBRType: 0x83, Start: 2048, Sectors: 100},
			{Index: 2, MBRType: 0x83, Start: 4096, Sectors: 100, Attrs: 0x80},
		}},
		policy: pol,
		verity: &veritySettings{designator: partInvalid},
		native: "x86-64",
	}
	d, err := ds.dissect()
	if err != nil {
		t.Fatal(err)
	}
	if d.parts[partRoot] == nil || d.parts[partRoot].Index != 2 {
		t.Errorf("bootable 0x83 partition not used as root: %+v", d.parts[partRoot])
	}
}

func TestRootHashSources(t *testing.T) {
	x := archTypesOf(t, "x86-64")
	rootHash := bytes.Repeat([]byte{0x5a}, 32)
	sigJSON, _ := json.Marshal(map[string]string{
		"rootHash":  hex.EncodeToString(rootHash),
		"signature": "c2ln",
	})
	img := buildGPT(t, 512,
		testPart{typ: x[0]}, testPart{typ: x[1]}, testPart{typ: x[2], sectors: 8})
	pt, err := readPartitionTable(bytes.NewReader(img), int64(len(img)), 512)
	if err != nil {
		t.Fatal(err)
	}
	copy(img[pt.parts[2].offset(512):], sigJSON)

	d, err := dissectTest(t, "", nil, pt.parts...)
	if err != nil {
		t.Fatal(err)
	}
	vs := &veritySettings{designator: partInvalid}
	if err := d.loadSigPartitionRootHash(vs, bytes.NewReader(img), 512); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vs.rootHash, rootHash) || string(vs.sig) != "sig" || vs.designator != partRoot {
		t.Errorf("signature partition: hash %x sig %q designator %s", vs.rootHash, vs.sig, vs.designator)
	}
	if err := d.guessRootHash(vs); err != nil || !bytes.Equal(vs.rootHash, rootHash) {
		t.Errorf("guess must not override the signature's root hash: %x, %v", vs.rootHash, err)
	}

	d, err = dissectTest(t, "root=verity", nil, pt.parts...)
	if err != nil {
		t.Fatal(err)
	}
	if d.found(partRootVeritySig) {
		t.Fatal("root=verity must ignore the signature partition")
	}
	vs = &veritySettings{designator: partInvalid}
	if err := d.loadSigPartitionRootHash(vs, bytes.NewReader(img), 512); err != nil || vs.rootHash != nil {
		t.Fatalf("ignored signature partition loaded: %x, %v", vs.rootHash, err)
	}
	if err := d.guessRootHash(vs); err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(pt.parts[0].UUID+pt.parts[1].UUID, "-", "")
	if hex.EncodeToString(vs.rootHash) != want || vs.designator != partRoot {
		t.Errorf("guessed root hash %x, want %s", vs.rootHash, want)
	}

	vs = &veritySettings{rootHash: bytes.Repeat([]byte{1}, 32), designator: partRoot}
	d, err = dissectTest(t, "", nil, pt.parts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.loadSigPartitionRootHash(vs, bytes.NewReader(img), 512); err == nil {
		t.Error("signature root hash differing from the configured one accepted")
	}
}

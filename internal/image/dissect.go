package image

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/errno"
)

// dissected is the result of dissecting a partition table: the partition
// used for each designator, following systemd v262 dissect_image() with the
// flags systemd-sysext passes (GENERIC_ROOT, REQUIRE_ROOT, USR_NO_ROOT).
type dissected struct {
	parts [numDesignators]*partition
	arch  [numDesignators]string
	// gptFlags are the GPT attributes of the partitions picked by type;
	// like systemd, the generic root fallback and MBR partitions have none.
	gptFlags [numDesignators]uint64
	ignored  [numDesignators]bool
}

func (d *dissected) found(x designator) bool { return x >= 0 && d.parts[x] != nil }

// dissector carries the inputs of a dissection.
type dissector struct {
	table  *partitionTable
	policy *imagePolicy
	verity *veritySettings
	native string
	// readSig decodes a signature partition; needed only when an external
	// root hash has to be matched against signature partitions.
	readSig func(partition) (*veritySig, error)
	// machineID returns the host machine ID, for /var partition matching.
	machineID func() ([16]byte, error)
}

func (ds *dissector) dissect() (*dissected, error) {
	var d dissected
	var rootUUID, rootVerityUUID, usrUUID, usrVerityUUID string
	if v := ds.verity; len(v.rootHash) > 0 {
		data, hash, err := rootHashUUIDs(v.rootHash)
		if err != nil {
			return nil, err
		}
		if v.designator == partUsr {
			usrUUID, usrVerityUUID = data, hash
		} else {
			rootUUID, rootVerityUUID = data, hash
		}
	}

	var generic *partition
	multipleGeneric := false
	for i := range ds.table.parts {
		p := &ds.table.parts[i]
		if !ds.table.gpt {
			if err := ds.dissectMBR(&d, p, &generic, &multipleGeneric); err != nil {
				return nil, err
			}
			continue
		}

		typ := lookupGPTType(p.TypeGUID)
		if p.Label == "_empty" {
			continue
		}
		if p.Attrs&gptFlagNoAuto != 0 && typ.designator != partESP {
			continue
		}
		if vd := typ.designator.verityData(); ds.verity.designator != partInvalid && vd != partInvalid && vd != ds.verity.designator {
			continue
		}

		switch typ.designator {
		case partESP:
			if p.Attrs&gptFlagNoBlockIOProtocol != 0 {
				continue
			}
		case partRoot, partUsr:
			if want := pick(typ.designator == partRoot, rootUUID, usrUUID); want != "" && want != p.UUID {
				continue
			}
		case partRootVerity, partUsrVerity:
			if want := pick(typ.designator == partRootVerity, rootVerityUUID, usrVerityUUID); want != "" && want != p.UUID {
				continue
			}
		case partRootVeritySig, partUsrVeritySig:
			if len(ds.verity.rootHash) > 0 {
				sig, err := ds.readSig(*p)
				if err != nil {
					return nil, err
				}
				if string(sig.RootHash) != string(ds.verity.rootHash) {
					continue
				}
			}
		case partVar:
			ok, err := ds.varMatches(p)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		case partInvalid:
			if p.TypeGUID == gptTypeLinuxGeneric {
				if generic != nil {
					multipleGeneric = true
				} else {
					generic = p
				}
			}
			continue
		default:
		}

		if err := ds.consider(&d, typ, p); err != nil {
			return nil, err
		}
	}

	if err := ds.finish(&d, generic, multipleGeneric); err != nil {
		return nil, err
	}
	return &d, nil
}

// consider applies the policy to a candidate partition and keeps the best
// candidate per designator: by architecture, then (for versioned
// designators) by label version.
func (ds *dissector) consider(d *dissected, typ gptType, p *partition) error {
	use, err := ds.policy.mayUse(typ.designator)
	if err != nil {
		return err
	}
	x := typ.designator
	if !use {
		if d.parts[x] == nil {
			d.ignored[x] = true
		}
		return nil
	}
	if cur := d.parts[x]; cur != nil {
		c := compareArch(typ.arch, d.arch[x], ds.native)
		if c < 0 {
			return nil
		}
		if c == 0 && (!x.versioned() || discover.CompareVersions(p.Label, cur.Label) <= 0) {
			return nil
		}
	}
	d.parts[x] = p
	d.arch[x] = typ.arch
	d.gptFlags[x] = p.Attrs
	d.ignored[x] = false
	return nil
}

func (ds *dissector) dissectMBR(d *dissected, p *partition, generic **partition, multiple *bool) error {
	switch p.MBRType {
	case 0x83:
		if p.Attrs != 0x80 {
			return nil
		}
		if *generic != nil {
			*multiple = true
		} else {
			*generic = p
		}
	case 0xea:
		use, err := ds.policy.mayUse(partXBootLdr)
		if err != nil {
			return err
		}
		if !use {
			if d.parts[partXBootLdr] == nil {
				d.ignored[partXBootLdr] = true
			}
			return nil
		}
		if d.parts[partXBootLdr] == nil {
			d.parts[partXBootLdr] = p
		}
	}
	return nil
}

// finish runs the consistency checks and policy enforcement that follow
// partition discovery in dissect_image().
func (ds *dissector) finish(d *dissected, generic *partition, multipleGeneric bool) error {
	for _, x := range []designator{partRoot, partUsr} {
		if !d.found(x) && (d.found(x.verityHash()) || d.found(x.veritySig())) {
			return errno.New(unix.EADDRNOTAVAIL, "found %s verity partition without matching %s data partition", x, x)
		}
		if d.found(x.veritySig()) && !d.found(x.verityHash()) {
			return errno.New(unix.EADDRNOTAVAIL, "found %s verity signature partition without matching %s verity hash partition", x, x)
		}
	}

	if d.found(partRoot) && d.found(partUsr) && d.arch[partRoot] != "" && d.arch[partUsr] != "" && d.arch[partRoot] != d.arch[partUsr] {
		return errno.New(unix.EREMOTE, "found root and usr partitions with different architectures (%s vs %s)", d.arch[partRoot], d.arch[partUsr])
	}

	v := ds.verity
	if !d.found(partRoot) && !d.found(partUsr) && (len(v.rootHash) == 0 || v.designator != partUsr) {
		if multipleGeneric {
			return errno.New(unix.ENOTUNIQ, "multiple generic Linux partitions found, cannot pick a root partition")
		}
		if generic != nil {
			use, err := ds.policy.mayUse(partRoot)
			if err != nil {
				return err
			}
			if use {
				d.parts[partRoot] = generic
			} else {
				d.ignored[partRoot] = true
			}
		}
	}

	if !d.found(partRoot) && !d.found(partUsr) {
		return errno.New(unix.ENXIO, "root or usr partition requested but found neither")
	}

	if d.found(partRootVerity) {
		if d.found(partUsrVerity) {
			return errno.New(unix.ENOTUNIQ, "found both root and usr verity partitions, which is not supported")
		}
		if d.found(partUsr) {
			return errno.New(unix.EADDRNOTAVAIL, "found verity protected root partition with a split usr partition, which is not supported")
		}
	}

	if v.designator != partInvalid && !d.found(v.designator) {
		return errno.New(unix.EADDRNOTAVAIL, "verity data was provided for the %s partition, but none was found", v.designator)
	}
	if len(v.rootHash) > 0 {
		x := pick(v.designator == partUsr, partUsr, partRoot)
		if !d.found(x) {
			return errno.New(unix.EADDRNOTAVAIL, "no %s partition matching the external root hash found", x)
		}
		if !d.found(x.verityHash()) {
			return errno.New(unix.EADDRNOTAVAIL, "no %s verity partition matching the external root hash found", x)
		}
	}

	for x := range numDesignators {
		var found policyFlags
		switch {
		case d.found(x):
			found = polEncrypted | polEncryptedWithIntegrity | polUnprotected | polUnused
			if vh := x.verityHash(); vh != partInvalid && d.found(vh) {
				found |= polVerity
				if d.found(x.veritySig()) {
					found |= polSigned
				}
			}
		case d.ignored[x]:
			found = polUnused
		default:
			found = polAbsent
		}
		if err := ds.policy.checkProtection(x, found); err != nil {
			return err
		}
		if d.found(x) {
			if err := ds.policy.checkPartitionFlags(x, d.gptFlags[x]); err != nil {
				return err
			}
		}
	}
	return nil
}

// rootHashUUIDs splits a root hash into the data and verity partition
// UUIDs the DPS derives from it (first and last 128 bits).
func rootHashUUIDs(rootHash []byte) (data, verity string, err error) {
	if len(rootHash) < 16 {
		return "", "", errors.New("verity root hash too short")
	}
	data, verity = uuidString(rootHash[:16]), uuidString(rootHash[len(rootHash)-16:])
	if data == zeroUUID || verity == zeroUUID {
		return "", "", errors.New("verity root hash yields an all-zero partition UUID")
	}
	return data, verity, nil
}

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// uuidString formats 16 bytes in canonical (big-endian) UUID form.
func uuidString(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func uuidBytes(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		return nil, fmt.Errorf("malformed UUID %q", s)
	}
	return b, nil
}

// varMatches implements the /var partition binding: its UUID must be the
// HMAC-SHA256 of the /var type UUID keyed by the machine ID
// (sd_id128_get_machine_app_specific()).
func (ds *dissector) varMatches(p *partition) (bool, error) {
	mid, err := ds.machineID()
	if err != nil {
		return false, fmt.Errorf("image has a /var partition, but the machine ID is unavailable: %w", err)
	}
	app, _ := uuidBytes(gptTypeVar)
	mac := hmac.New(sha256.New, mid[:])
	mac.Write(app)
	id := mac.Sum(nil)[:16]
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return uuidString(id) == p.UUID, nil
}

// hostMachineID reads /etc/machine-id.
func hostMachineID() ([16]byte, error) {
	var id [16]byte
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return id, err
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(b) != 16 {
		return id, errors.New("malformed /etc/machine-id")
	}
	copy(id[:], b)
	return id, nil
}

// loadSigPartitionRootHash mirrors dissected_image_load_verity_sig_partition():
// the root hash and signature from the signature partition, when one is in
// use for the verity protected designator.
func (d *dissected) loadSigPartitionRootHash(v *veritySettings, r io.ReaderAt, ss int64) error {
	if len(v.rootHash) > 0 && len(v.sig) > 0 {
		return nil
	}
	if envDisabled("SYSTEMD_DISSECT_VERITY_EMBEDDED") {
		return nil
	}
	x := v.designator
	if x == partInvalid {
		switch {
		case d.found(partRootVerity):
			x = partRoot
		case d.found(partUsrVerity):
			x = partUsr
		default:
			return nil
		}
	}
	if !d.found(x) || !d.found(x.verityHash()) || !d.found(x.veritySig()) {
		return nil
	}
	sig, err := readVeritySig(r, *d.parts[x.veritySig()], ss)
	if err != nil {
		return err
	}
	if len(v.rootHash) > 0 && string(v.rootHash) != string(sig.RootHash) {
		return fmt.Errorf("root hash in signature partition (%x) does not match the configured one (%x)", sig.RootHash, v.rootHash)
	}
	v.rootHash, v.sig, v.designator = sig.RootHash, sig.Signature, x
	return nil
}

// guessRootHash mirrors dissected_image_guess_verity_roothash(): the root
// hash from the data and verity partition UUIDs.
func (d *dissected) guessRootHash(v *veritySettings) error {
	if len(v.rootHash) > 0 || envDisabled("SYSTEMD_DISSECT_VERITY_GUESS") {
		return nil
	}
	x := v.designator
	if x == partInvalid {
		switch {
		case d.found(partRootVerity):
			x = partRoot
		case d.found(partUsrVerity):
			x = partUsr
		default:
			return nil
		}
	}
	if !d.found(x) || !d.found(x.verityHash()) {
		return nil
	}
	a, err := uuidBytes(d.parts[x].UUID)
	if err != nil {
		return err
	}
	b, err := uuidBytes(d.parts[x.verityHash()].UUID)
	if err != nil {
		return err
	}
	v.rootHash, v.designator = slices.Concat(a, b), x
	return nil
}

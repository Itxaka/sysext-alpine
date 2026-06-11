package image

import (
	"fmt"
	"strings"
)

// This file implements a subset of systemd.image-policy(7) sufficient for
// sysext/confext DDIs: per-designator lists of acceptable protection levels.
//
// Grammar (subset):
//
//	policy       := part (':' part)*
//	part         := designator '=' alternatives
//	alternatives := protection ('+' protection)*
//	protection   := "verity" | "signed" | "encrypted" | "unprotected" | "absent"
//
// Only the "root" and "usr" designators are meaningful here; other
// designators (esp, home, srv, ...) parse but are ignored. Designators not
// mentioned in the policy keep the class default (everything allowed) —
// systemd's per-designator fallback machinery ("default" pseudo-designator,
// "ignore"/"unused") is not implemented.

// protection is a way a partition's payload may be protected.
type protection string

const (
	// protAbsent: the partition does not exist in the image.
	protAbsent protection = "absent"
	// protUnprotected: data partition without verity or LUKS.
	protUnprotected protection = "unprotected"
	// protVerity: data partition with a matching dm-verity partition.
	protVerity protection = "verity"
	// protSigned: verity plus a verity-signature partition.
	protSigned protection = "signed"
	// protEncrypted: LUKS — recognized in policies but never satisfiable
	// (LUKS images are unsupported).
	protEncrypted protection = "encrypted"
)

// protectionSet is the set of protection levels a policy accepts for one
// designator.
type protectionSet map[protection]bool

func (s protectionSet) clone() protectionSet {
	out := make(protectionSet, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// allProtections is the class-default set: everything allowed.
func allProtections() protectionSet {
	return protectionSet{
		protAbsent:      true,
		protUnprotected: true,
		protVerity:      true,
		protSigned:      true,
		protEncrypted:   true,
	}
}

// imagePolicy is a parsed image policy restricted to the designators we
// enforce.
type imagePolicy struct {
	root protectionSet
	usr  protectionSet
}

// forDesignator returns the allowed set for "root" or "usr".
func (p *imagePolicy) forDesignator(d string) protectionSet {
	if d == "usr" {
		return p.usr
	}
	return p.root
}

// parseImagePolicy parses a systemd.image-policy(7) string (subset, see
// file comment). The empty string is the class default: everything allowed
// for every designator.
func parseImagePolicy(s string) (*imagePolicy, error) {
	pol := &imagePolicy{root: allProtections(), usr: allProtections()}
	if s == "" {
		return pol, nil
	}

	for _, part := range strings.Split(s, ":") {
		designator, alts, ok := strings.Cut(part, "=")
		if !ok || designator == "" {
			return nil, fmt.Errorf("invalid image policy component %q (want designator=protection[+protection...])", part)
		}

		set := make(protectionSet)
		for _, alt := range strings.Split(alts, "+") {
			switch p := protection(alt); p {
			case protAbsent, protUnprotected, protVerity, protSigned, protEncrypted:
				set[p] = true
			default:
				return nil, fmt.Errorf("invalid protection level %q in image policy component %q", alt, part)
			}
		}

		switch designator {
		case "root":
			pol.root = set
		case "usr":
			pol.usr = set
		default:
			// Other designators (esp, home, srv, xbootldr, swap, tmp,
			// var, ...) are validated above but not enforced: sysext
			// DDIs only carry root/usr payloads.
		}
	}
	return pol, nil
}

// classifyProtection determines the actual protection level the image
// offers for one designator, from its partition list:
//
//	absent       — no data partition of the designator's type
//	unprotected  — data partition, but no usable verity partition
//	verity       — data + verity partition (with non-zero unique GUIDs)
//	signed       — data + verity + verity-signature partition
//
// A verity partition whose unique GUID (or whose data partition's unique
// GUID) is all-zero cannot convey a root hash and is treated as missing
// (the spec's root-hash discovery needs both halves).
func classifyProtection(parts []gptPartition, guids dpsGUIDs, designator string) protection {
	dataType, verityType, sigType := guids.root, guids.rootVerity, guids.rootVeritySig
	if designator == "usr" {
		dataType, verityType, sigType = guids.usr, guids.usrVerity, guids.usrVeritySig
	}

	data := findByType(parts, dataType)
	if data == nil {
		return protAbsent
	}
	verity := findByType(parts, verityType)
	if verity == nil || !verityUsable(*data, *verity) {
		return protUnprotected
	}
	if findByType(parts, sigType) != nil {
		return protSigned
	}
	return protVerity
}

// findByType returns the first partition with the given type GUID, or nil.
func findByType(parts []gptPartition, typeGUID string) *gptPartition {
	for i := range parts {
		if parts[i].TypeGUID == typeGUID {
			return &parts[i]
		}
	}
	return nil
}

// zeroGUID is the canonical form of the all-zero GUID.
const zeroGUID = "00000000-0000-0000-0000-000000000000"

// verityUsable reports whether the data/verity partition pair can convey a
// verity root hash via their unique partition GUIDs.
func verityUsable(data, verity gptPartition) bool {
	return data.UniqueGUID != zeroGUID && verity.UniqueGUID != zeroGUID &&
		data.UniqueGUID != "" && verity.UniqueGUID != ""
}

// errPolicy is the canonical policy rejection error text (matches what the
// task spec and systemd's messages convey).
func policyError(designator string, actual protection, allowed protectionSet) error {
	var names []string
	for _, p := range []protection{protVerity, protSigned, protEncrypted, protUnprotected, protAbsent} {
		if allowed[p] {
			names = append(names, string(p))
		}
	}
	hint := ""
	if allowed[protEncrypted] {
		hint = " (note: encrypted/LUKS images are not supported)"
	}
	return fmt.Errorf("image does not satisfy image policy: %s partition is %s, policy allows %s%s",
		designator, actual, strings.Join(names, "+"), hint)
}

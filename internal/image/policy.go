package image

import (
	"fmt"
	"sort"
	"strings"
)

// This file implements the systemd.image-policy(7) grammar (see
// docs/reference/systemd.image-policy.7.txt, dumped from systemd 260) for
// sysext/confext DDIs.
//
// Grammar:
//
//	policy       := special | rule (':' rule)*
//	special      := "" | "*" | "-" | "~"
//	rule         := designator? '=' flags?
//	flags        := flag ('+' flag)*
//	flag         := protection | shortcut | fstype | gptflag
//	designator   := "root" | "usr" | "home" | "srv" | "esp" | "xbootldr" |
//	                "swap" | "root-verity" | "root-verity-sig" |
//	                "usr-verity" | "usr-verity-sig" | "tmp" | "var"
//	protection   := "verity" | "signed" | "encrypted" |
//	                "encryptedwithintegrity" | "unprotected" | "unused" |
//	                "absent"
//	shortcut     := "open" | "ignore"
//	fstype       := "btrfs" | "erofs" | "ext4" | "f2fs" | "squashfs" |
//	                "vfat" | "xfs"
//	gptflag      := "read-only-on" | "read-only-off" |
//	                "growfs-on" | "growfs-off"
//
// Semantics, per the man page (verified against systemd-analyze
// image-policy from systemd 260):
//
//   - An empty designator ("=flags") sets the default policy for
//     designators not explicitly listed. There is no "default=" spelling.
//   - Designators listed without any protection flag (e.g. "root=" or
//     "root=erofs") get the "open" protection set (everything allowed).
//   - Designators not listed, with no "=" default rule, fall back to
//     "unused+absent" (i.e. the partition may exist but must not be used).
//   - "open"  = verity+signed+encrypted+encryptedwithintegrity+
//     unprotected+unused+absent; "ignore" = unused+absent.
//   - Whole-policy specials: "*" = "=open" (use everything),
//     "-" = "=unused+absent" (use nothing), "~" = "=absent"
//     (everything must be absent). A bare flag list without "=" (e.g.
//     just "verity") is invalid, as is "default=...".
//   - Filesystem-type flags restrict the allowed filesystem of the
//     partition; no fstype flag means all types are allowed.
//   - read-only-on/-off and growfs-on/-off dictate GPT partition flag
//     state; setting neither (or both) of a pair leaves it undictated.
//   - Duplicate rules for the same designator are an error; duplicate
//     flags within one rule are fine. Flag/designator names are
//     case-sensitive; whitespace around rules, designators and flags is
//     tolerated (matching systemd's word extraction).
//
// Enforcement scope: sysext DDIs only carry root/usr payloads, so
// enforcement consults the "root" and "usr" designators (protection level
// and filesystem type). Rules for all other designators parse and are
// retained in imagePolicy.rules, but are not enforced.
//
// Deliberate divergences from systemd:
//
//   - The empty policy string "" is this class's default and allows
//     everything (systemd parses "" like "-", i.e. "use nothing"). Pass
//     "-" explicitly for systemd's deny-by-default behavior.
//   - "encryptedwithintegrity" is accepted because systemd 260's parser
//     accepts it, although the man page's flag list omits it. Like
//     "encrypted" it is never satisfiable here (no LUKS support).
//   - read-only-on/-off and growfs-on/-off parse and are retained but are
//     not enforced: our GPT parser does not read partition attribute
//     flags, and images are always mounted read-only and never grown.

// protection is a partition policy flag dictating existence/use/protection.
type protection string

const (
	// protAbsent: the partition shall not exist on the image.
	protAbsent protection = "absent"
	// protUnused: the partition may exist but shall not be used.
	protUnused protection = "unused"
	// protUnprotected: data partition without verity or LUKS.
	protUnprotected protection = "unprotected"
	// protVerity: data partition with a matching dm-verity partition.
	protVerity protection = "verity"
	// protSigned: verity plus a verity-signature partition.
	protSigned protection = "signed"
	// protEncrypted: LUKS — recognized in policies but never satisfiable
	// (LUKS images are unsupported).
	protEncrypted protection = "encrypted"
	// protEncryptedWithIntegrity: LUKS with dm-integrity — like
	// protEncrypted, recognized but never satisfiable.
	protEncryptedWithIntegrity protection = "encryptedwithintegrity"
)

// protectionSet is the set of protection levels a policy accepts for one
// designator.
type protectionSet map[protection]bool

// openProtections is the "open" shortcut set: everything allowed.
func openProtections() protectionSet {
	return protectionSet{
		protAbsent:                 true,
		protUnused:                 true,
		protUnprotected:            true,
		protVerity:                 true,
		protSigned:                 true,
		protEncrypted:              true,
		protEncryptedWithIntegrity: true,
	}
}

// ignoreProtections is the "ignore" shortcut set ("unused+absent"), also
// the fallback for designators that are neither listed nor covered by a
// default rule.
func ignoreProtections() protectionSet {
	return protectionSet{protUnused: true, protAbsent: true}
}

// allProtections is the class-default set used for the empty policy
// string: everything allowed (identical to "open").
func allProtections() protectionSet { return openProtections() }

// validDesignators are the partition identifiers the man page defines.
var validDesignators = map[string]bool{
	"root": true, "usr": true, "home": true, "srv": true, "esp": true,
	"xbootldr": true, "swap": true, "root-verity": true,
	"root-verity-sig": true, "usr-verity": true, "usr-verity-sig": true,
	"tmp": true, "var": true,
}

// validFSTypes are the filesystem policy flags the man page defines.
var validFSTypes = map[string]bool{
	"btrfs": true, "erofs": true, "ext4": true, "f2fs": true,
	"squashfs": true, "vfat": true, "xfs": true,
}

// partitionRule is the parsed policy for one designator (or the default).
type partitionRule struct {
	// protections is never empty: a rule listing no protection flag gets
	// the "open" set, per the man page.
	protections protectionSet
	// fs restricts the allowed filesystem types; nil = all allowed.
	fs map[string]bool
	// readOnly/growfs dictate GPT partition flag state; nil = undictated
	// (neither or both of the on/off pair was given). Retained but not
	// enforced — see the divergence notes above.
	readOnly *bool
	growfs   *bool
}

// imagePolicy is a parsed image policy.
type imagePolicy struct {
	// rules maps explicitly listed designators to their policy.
	rules map[string]*partitionRule
	// def is the empty-designator default rule, nil when not given.
	def *partitionRule
	// allowAll marks the class default (empty policy string): everything
	// is allowed for every designator.
	allowAll bool
}

// ruleFor resolves the effective rule for a designator: explicit rule,
// else the default rule, else nil (the man page "unused+absent" fallback).
func (p *imagePolicy) ruleFor(d string) *partitionRule {
	if r, ok := p.rules[d]; ok {
		return r
	}
	return p.def
}

// forDesignator returns the allowed protection set for a designator
// (enforcement uses "root" and "usr" only).
func (p *imagePolicy) forDesignator(d string) protectionSet {
	if p.allowAll {
		return allProtections()
	}
	if r := p.ruleFor(d); r != nil {
		return r.protections
	}
	return ignoreProtections()
}

// checkFS enforces the filesystem-type policy flags for a designator
// against the detected payload filesystem.
func (p *imagePolicy) checkFS(designator string, fs FSType) error {
	if p.allowAll {
		return nil
	}
	r := p.ruleFor(designator)
	if r == nil || r.fs == nil || r.fs[string(fs)] {
		return nil
	}
	names := make([]string, 0, len(r.fs))
	for n := range r.fs {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("image does not satisfy image policy: %s partition filesystem is %s, policy allows %s",
		designator, fs, strings.Join(names, "+"))
}

// parseImagePolicy parses a systemd.image-policy(7) string (see the file
// comment for grammar, semantics and divergences). The empty string is the
// class default: everything allowed for every designator.
func parseImagePolicy(s string) (*imagePolicy, error) {
	pol := &imagePolicy{rules: make(map[string]*partitionRule)}

	switch strings.TrimSpace(s) {
	case "":
		if s == "" {
			pol.allowAll = true
			return pol, nil
		}
		return nil, fmt.Errorf("invalid image policy %q", s)
	case "*": // "use everything"
		s = "=verity+signed+encrypted+encryptedwithintegrity+unprotected+unused+absent"
	case "-": // "use nothing"
		s = "=unused+absent"
	case "~": // "everything must be absent"
		s = "=absent"
	}

	for _, comp := range strings.Split(s, ":") {
		comp = strings.TrimSpace(comp)
		if comp == "" {
			return nil, fmt.Errorf("invalid image policy: empty rule (leading, trailing or doubled %q)", ":")
		}

		designator, flags, ok := strings.Cut(comp, "=")
		if !ok {
			return nil, fmt.Errorf("invalid image policy rule %q (want [designator]=[flag[+flag...]])", comp)
		}
		designator = strings.TrimSpace(designator)
		if designator != "" && !validDesignators[designator] {
			return nil, fmt.Errorf("unknown partition designator %q in image policy rule %q", designator, comp)
		}

		rule, err := parsePartitionRule(flags, comp)
		if err != nil {
			return nil, err
		}

		if designator == "" {
			if pol.def != nil {
				return nil, fmt.Errorf("duplicate default rule in image policy (rule %q)", comp)
			}
			pol.def = rule
			continue
		}
		if _, dup := pol.rules[designator]; dup {
			return nil, fmt.Errorf("duplicate rule for designator %q in image policy", designator)
		}
		pol.rules[designator] = rule
	}
	return pol, nil
}

// parsePartitionRule parses the flag list of one rule and normalizes it:
// no protection flags ⇒ "open"; neither/both of an on/off GPT-flag pair ⇒
// undictated.
func parsePartitionRule(flags, comp string) (*partitionRule, error) {
	rule := &partitionRule{protections: make(protectionSet)}
	var roOn, roOff, gfOn, gfOff bool

	if flags != "" {
		for _, f := range strings.Split(flags, "+") {
			f = strings.TrimSpace(f)
			switch {
			case f == "":
				return nil, fmt.Errorf("empty flag in image policy rule %q", comp)
			case f == string(protAbsent), f == string(protUnused),
				f == string(protUnprotected), f == string(protVerity),
				f == string(protSigned), f == string(protEncrypted),
				f == string(protEncryptedWithIntegrity):
				rule.protections[protection(f)] = true
			case f == "open":
				for p := range openProtections() {
					rule.protections[p] = true
				}
			case f == "ignore":
				for p := range ignoreProtections() {
					rule.protections[p] = true
				}
			case validFSTypes[f]:
				if rule.fs == nil {
					rule.fs = make(map[string]bool)
				}
				rule.fs[f] = true
			case f == "read-only-on":
				roOn = true
			case f == "read-only-off":
				roOff = true
			case f == "growfs-on":
				gfOn = true
			case f == "growfs-off":
				gfOff = true
			default:
				return nil, fmt.Errorf("invalid flag %q in image policy rule %q", f, comp)
			}
		}
	}

	// "if none of the [protection] flags are set for a listed partition
	// identifier, the default policy of open is implied".
	if len(rule.protections) == 0 {
		rule.protections = openProtections()
	}
	// "Setting neither flag is equivalent to setting both."
	if roOn != roOff {
		rule.readOnly = &roOn
	}
	if gfOn != gfOff {
		rule.growfs = &gfOn
	}
	return rule, nil
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

// policyError is the canonical policy rejection error text (matches what
// the task spec and systemd's messages convey).
func policyError(designator string, actual protection, allowed protectionSet) error {
	var names []string
	for _, p := range []protection{
		protVerity, protSigned, protEncrypted, protEncryptedWithIntegrity,
		protUnprotected, protUnused, protAbsent,
	} {
		if allowed[p] {
			names = append(names, string(p))
		}
	}
	hint := ""
	if allowed[protEncrypted] || allowed[protEncryptedWithIntegrity] {
		hint = " (note: encrypted/LUKS images are not supported)"
	}
	return fmt.Errorf("image does not satisfy image policy: %s partition is %s, policy allows %s%s",
		designator, actual, strings.Join(names, "+"), hint)
}

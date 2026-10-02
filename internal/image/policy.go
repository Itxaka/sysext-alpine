package image

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/errno"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// This file implements systemd.image-policy(7) (see
// docs/reference/systemd.image-policy.7.txt) with the semantics of systemd
// v262 src/shared/image-policy.c:
//
//	policy     := "*" | "-" | "~" | rule (':' rule)*
//	rule       := designator? '=' flags
//	flags      := "-" | flag ('+' flag)*
//	flag       := verity | signed | encrypted | encryptedwithintegrity |
//	              unprotected | unused | absent | open | ignore |
//	              read-only-on | read-only-off | growfs-on | growfs-off |
//	              btrfs | erofs | ext4 | f2fs | squashfs | vfat | xfs
//
// The empty designator sets the default for unlisted designators, which is
// otherwise "ignore". Rules for verity and signature partitions that are not
// listed explicitly are derived from their data partition's rule.
//
// The empty policy string selects the class default (systemd's
// image_policy_sysext / image_policy_confext). systemd parses an explicit
// empty string like "-"; callers wanting that pass "-".

// policyFlags is systemd's PartitionPolicyFlags bit set.
type policyFlags uint32

const (
	polVerity policyFlags = 1 << iota
	polSigned
	polEncrypted
	polEncryptedWithIntegrity
	polUnprotected
	polUnused
	polAbsent
	polReadOnlyOff
	polReadOnlyOn
	polGrowFSOff
	polGrowFSOn
	polBtrfs
	polErofs
	polExt4
	polF2FS
	polSquashfs
	polVfat
	polXFS

	polOpen   = polVerity | polSigned | polEncrypted | polEncryptedWithIntegrity | polUnprotected | polUnused | polAbsent
	polIgnore = polUnused | polAbsent

	polUseMask      = polOpen
	polReadOnlyMask = polReadOnlyOff | polReadOnlyOn
	polGrowFSMask   = polGrowFSOff | polGrowFSOn
	polPFlagsMask   = polReadOnlyMask | polGrowFSMask
	polFSTypeMask   = polBtrfs | polErofs | polExt4 | polF2FS | polSquashfs | polVfat | polXFS
)

var policyFlagNames = []struct {
	flag policyFlags
	name string
}{
	{polVerity, "verity"},
	{polSigned, "signed"},
	{polEncrypted, "encrypted"},
	{polEncryptedWithIntegrity, "encryptedwithintegrity"},
	{polUnprotected, "unprotected"},
	{polUnused, "unused"},
	{polAbsent, "absent"},
	{polReadOnlyOn, "read-only-on"},
	{polReadOnlyOff, "read-only-off"},
	{polGrowFSOn, "growfs-on"},
	{polGrowFSOff, "growfs-off"},
	{polBtrfs, "btrfs"},
	{polErofs, "erofs"},
	{polExt4, "ext4"},
	{polF2FS, "f2fs"},
	{polSquashfs, "squashfs"},
	{polVfat, "vfat"},
	{polXFS, "xfs"},
}

func policyFlagFromString(s string) (policyFlags, bool) {
	switch s {
	case "open":
		return polOpen, true
	case "ignore":
		return polIgnore, true
	}
	for _, f := range policyFlagNames {
		if f.name == s {
			return f.flag, true
		}
	}
	return 0, false
}

// String renders the flags like systemd's partition_policy_flags_to_string()
// with simplify=true.
func (f policyFlags) String() string {
	var l []string
	switch f & polUseMask {
	case polOpen:
		l = append(l, "open")
	case polIgnore:
		l = append(l, "ignore")
	default:
		for _, n := range policyFlagNames[:7] {
			if f&n.flag != 0 {
				l = append(l, n.name)
			}
		}
	}
	if (f&polReadOnlyOn == 0) != (f&polReadOnlyOff == 0) {
		if f&polReadOnlyOn != 0 {
			l = append(l, "read-only-on")
		} else {
			l = append(l, "read-only-off")
		}
	}
	if (f&polGrowFSOn == 0) != (f&polGrowFSOff == 0) {
		if f&polGrowFSOn != 0 {
			l = append(l, "growfs-on")
		} else {
			l = append(l, "growfs-off")
		}
	}
	for _, n := range policyFlagNames[11:] {
		if f&n.flag != 0 {
			l = append(l, n.name)
		}
	}
	if len(l) == 0 {
		return "-"
	}
	return strings.Join(l, "+")
}

// imagePolicy is a parsed image policy (systemd's ImagePolicy).
type imagePolicy struct {
	rules map[designator]policyFlags
	def   policyFlags
}

const classPolicyProtection = polVerity | polSigned | polEncrypted | polEncryptedWithIntegrity | polUnprotected | polAbsent

// classDefaultPolicy returns image_policy_sysext or image_policy_confext.
func classDefaultPolicy(class release.Class) *imagePolicy {
	p := &imagePolicy{rules: map[designator]policyFlags{partRoot: classPolicyProtection}, def: polIgnore}
	if class != release.Confext {
		p.rules[partUsr] = classPolicyProtection
	}
	return p
}

// resolvePolicy parses s, or returns the class default for the empty string.
func resolvePolicy(s string, class release.Class) (*imagePolicy, error) {
	if s == "" {
		return classDefaultPolicy(class), nil
	}
	return parseImagePolicy(s)
}

// ValidatePolicy reports whether s is a valid systemd.image-policy(7)
// string, like image_policy_from_string() with graceful=false. The empty
// string (the class default) is valid. Errors wrap unix.ENOTUNIQ (duplicate
// rule), unix.EBADSLT (unknown partition designator), unix.EBADRQC (unknown
// policy flag) or unix.EINVAL.
func ValidatePolicy(s string) error {
	if s == "" {
		return nil
	}
	_, err := parseImagePolicy(s)
	return err
}

// NormalizePolicy parses s like image_policy_from_string() with
// graceful=true, as configuration files are parsed: unknown partition
// designators and policy flags are dropped. It returns the policy in a form
// the strict parser accepts; "" stays "".
func NormalizePolicy(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	pol, err := parsePolicy(s, true)
	if err != nil {
		return "", err
	}
	rules := []string{"=" + pol.def.String()}
	for d := range numDesignators {
		if f, ok := pol.rules[d]; ok {
			rules = append(rules, d.String()+"="+f.String())
		}
	}
	return strings.Join(rules, ":"), nil
}

// parseImagePolicy mirrors systemd's image_policy_from_string() with
// graceful=false.
func parseImagePolicy(s string) (*imagePolicy, error) {
	return parsePolicy(s, false)
}

func parsePolicy(s string, graceful bool) (*imagePolicy, error) {
	pol := &imagePolicy{rules: make(map[designator]policyFlags), def: polIgnore}
	switch s {
	case "", "-":
		return pol, nil
	case "*":
		pol.def = polOpen
		return pol, nil
	case "~":
		pol.def = polAbsent
		return pol, nil
	}

	defaultSet := false
	for rule := range strings.SplitSeq(s, ":") {
		name, flags, ok := strings.Cut(rule, "=")
		if !ok {
			return nil, fmt.Errorf("invalid image policy %q: missing '=' in %q: %w", s, rule, unix.EINVAL)
		}
		name = strings.TrimSpace(name)
		var d designator
		if name != "" {
			if d, ok = designatorFromString(name); !ok {
				if graceful {
					continue
				}
				return nil, fmt.Errorf("invalid image policy %q: unknown partition designator %q: %w", s, name, unix.EBADSLT)
			}
			if _, dup := pol.rules[d]; dup {
				return nil, fmt.Errorf("invalid image policy %q: partition designator %q specified more than once: %w", s, name, unix.ENOTUNIQ)
			}
		} else if defaultSet {
			return nil, fmt.Errorf("invalid image policy %q: default partition policy specified more than once: %w", s, unix.ENOTUNIQ)
		}
		f, err := parsePolicyFlags(strings.TrimSpace(flags), graceful)
		if err != nil {
			return nil, fmt.Errorf("invalid image policy %q: %w", s, err)
		}
		if name == "" {
			defaultSet = true
			pol.def = f
			continue
		}
		pol.rules[d] = f
	}
	return pol, nil
}

func parsePolicyFlags(s string, graceful bool) (policyFlags, error) {
	if s == "" || s == "-" {
		return 0, nil
	}
	var flags policyFlags
	for w := range strings.SplitSeq(s, "+") {
		f, ok := policyFlagFromString(strings.TrimSpace(w))
		if !ok {
			if graceful {
				continue
			}
			return 0, fmt.Errorf("unknown partition policy flag %q: %w", w, unix.EBADRQC)
		}
		flags |= f
	}
	return flags, nil
}

// extendPolicyFlags fills unspecified aspects with "don't care"
// (partition_policy_flags_extend).
func extendPolicyFlags(f policyFlags) policyFlags {
	if f&polUseMask == 0 {
		f |= polOpen
	}
	if f&polReadOnlyMask == 0 {
		f |= polReadOnlyMask
	}
	if f&polGrowFSMask == 0 {
		f |= polGrowFSMask
	}
	return f
}

// normalizePolicyFlags mirrors partition_policy_normalized_flags().
func normalizePolicyFlags(f policyFlags, d designator) policyFlags {
	f = extendPolicyFlags(f)
	if d.verityData() != partInvalid {
		f &^= polVerity | polSigned | polEncrypted | polEncryptedWithIntegrity
	}
	if d.verityHash() == partInvalid {
		f &^= polVerity | polSigned
	}
	if f&polUseMask == polAbsent {
		f &^= polPFlagsMask
	}
	return f
}

// get mirrors image_policy_get(): the explicit rule, or one derived from the
// data partition for verity/signature designators; ok=false when neither.
func (p *imagePolicy) get(d designator) (policyFlags, bool) {
	if f, ok := p.rules[d]; ok {
		return normalizePolicyFlags(f, d), true
	}
	data := d.verityData()
	if data == partInvalid {
		return 0, false
	}
	df, ok := p.get(data)
	if !ok {
		return 0, false
	}
	need := polVerity | polSigned
	if d.isVeritySig() {
		need = polSigned
	}
	if df&need == 0 {
		return 0, false
	}
	return normalizePolicyFlags(polUnprotected|df&(polUnused|polAbsent)|df&polPFlagsMask, d), true
}

// exhaustive mirrors image_policy_get_exhaustively().
func (p *imagePolicy) exhaustive(d designator) policyFlags {
	if f, ok := p.get(d); ok {
		return f
	}
	return normalizePolicyFlags(p.def, d)
}

// mayUse mirrors image_policy_may_use(): an error when the partition must be
// absent, false when it shall be ignored.
func (p *imagePolicy) mayUse(d designator) (bool, error) {
	f := p.exhaustive(d) & polUseMask
	if f == polAbsent {
		return false, errno.New(unix.ERFKILL, "image does not satisfy image policy: %s partition exists, but the policy requires it to be absent", d)
	}
	if f&^polAbsent == polUnused {
		return false, nil
	}
	return true, nil
}

// checkProtection mirrors image_policy_check_protection().
func (p *imagePolicy) checkProtection(d designator, found policyFlags) error {
	want := p.exhaustive(d)
	if found&want != 0 {
		return nil
	}
	hint := ""
	if want&polUseMask&^(polEncrypted|polEncryptedWithIntegrity) == 0 && want&(polEncrypted|polEncryptedWithIntegrity) != 0 {
		hint = " (encrypted images are not supported)"
	}
	return errno.New(unix.ERFKILL, "image does not satisfy image policy: %s partition is %s, but policy requires %s%s",
		d, found&polUseMask, want&polUseMask, hint)
}

// checkPartitionFlags mirrors image_policy_check_partition_flags().
func (p *imagePolicy) checkPartitionFlags(d designator, attrs uint64) error {
	want := p.exhaustive(d)
	if ro := attrs&gptFlagReadOnly != 0; want&polReadOnlyMask == pick(ro, polReadOnlyOff, polReadOnlyOn) {
		return errno.New(unix.ERFKILL, "image does not satisfy image policy: %s partition has the read-only flag incorrectly %s", d, pick(ro, "set", "unset"))
	}
	if grow := attrs&gptFlagGrowFS != 0; want&polGrowFSMask == pick(grow, polGrowFSOff, polGrowFSOn) {
		return errno.New(unix.ERFKILL, "image does not satisfy image policy: %s partition has the growfs flag incorrectly %s", d, pick(grow, "set", "unset"))
	}
	return nil
}

func pick[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

// checkFS enforces the filesystem type flags of a designator: when any are
// given, the detected filesystem must be one of them.
func (p *imagePolicy) checkFS(d designator, fs fsType) error {
	want := p.exhaustive(d) & polFSTypeMask
	if want == 0 {
		return nil
	}
	if f, ok := policyFlagFromString(string(fs)); ok && want&f != 0 {
		return nil
	}
	return errno.New(unix.ERFKILL, "image does not satisfy image policy: %s partition filesystem is %s, policy allows %s", d, fs, want)
}

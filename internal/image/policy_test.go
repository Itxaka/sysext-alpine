package image

import (
	"strings"
	"testing"
)

func setEq(s protectionSet, want ...protection) bool {
	if len(s) != len(want) {
		return false
	}
	for _, p := range want {
		if !s[p] {
			return false
		}
	}
	return true
}

// open is the full "open" protection list, for setEq.
var openList = []protection{
	protAbsent, protUnused, protUnprotected, protVerity, protSigned,
	protEncrypted, protEncryptedWithIntegrity,
}

func TestParseImagePolicyDefault(t *testing.T) {
	pol, err := parseImagePolicy("")
	if err != nil {
		t.Fatal(err)
	}
	if !pol.allowAll {
		t.Error("empty policy must be the allow-all class default")
	}
	if !setEq(pol.forDesignator("root"), openList...) || !setEq(pol.forDesignator("usr"), openList...) {
		t.Errorf("empty policy must allow everything, got root=%v usr=%v",
			pol.forDesignator("root"), pol.forDesignator("usr"))
	}
	if err := pol.checkFS("root", FSExt4); err != nil {
		t.Errorf("empty policy must allow any filesystem: %v", err)
	}
}

func TestParseImagePolicySubsets(t *testing.T) {
	pol, err := parseImagePolicy("root=verity")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), protVerity) {
		t.Errorf("root = %v, want {verity}", pol.forDesignator("root"))
	}
	// usr unmentioned, no default rule: man-page fallback is unused+absent.
	if !setEq(pol.forDesignator("usr"), protUnused, protAbsent) {
		t.Errorf("unmentioned usr must fall back to unused+absent, got %v", pol.forDesignator("usr"))
	}

	pol, err = parseImagePolicy("root=verity+signed:usr=unprotected+absent")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), protVerity, protSigned) {
		t.Errorf("root = %v, want {verity,signed}", pol.forDesignator("root"))
	}
	if !setEq(pol.forDesignator("usr"), protUnprotected, protAbsent) {
		t.Errorf("usr = %v, want {unprotected,absent}", pol.forDesignator("usr"))
	}
}

func TestParseImagePolicyOtherDesignatorsRetained(t *testing.T) {
	// Non-enforced designators must parse and be retained.
	pol, err := parseImagePolicy("home=encrypted:esp=unprotected:root=signed:root-verity-sig=absent")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), protSigned) {
		t.Errorf("root = %v, want {signed}", pol.forDesignator("root"))
	}
	for d, want := range map[string]protection{
		"home": protEncrypted, "esp": protUnprotected, "root-verity-sig": protAbsent,
	} {
		r, ok := pol.rules[d]
		if !ok {
			t.Errorf("rule for %q not retained", d)
			continue
		}
		if !setEq(r.protections, want) {
			t.Errorf("%s = %v, want {%s}", d, r.protections, want)
		}
	}
}

func TestParseImagePolicyDefaultRule(t *testing.T) {
	// "=verity" (empty designator): default for unlisted designators.
	pol, err := parseImagePolicy("usr=unprotected:=verity")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("usr"), protUnprotected) {
		t.Errorf("usr = %v, want {unprotected}", pol.forDesignator("usr"))
	}
	if !setEq(pol.forDesignator("root"), protVerity) {
		t.Errorf("root must use the default rule, got %v", pol.forDesignator("root"))
	}
	if !setEq(pol.forDesignator("home"), protVerity) {
		t.Errorf("home must use the default rule, got %v", pol.forDesignator("home"))
	}
}

func TestParseImagePolicySpecials(t *testing.T) {
	// "*" = use everything.
	pol, err := parseImagePolicy("*")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), openList...) || !setEq(pol.forDesignator("usr"), openList...) {
		t.Errorf("\"*\" must allow everything, got root=%v usr=%v",
			pol.forDesignator("root"), pol.forDesignator("usr"))
	}

	// "-" = use nothing.
	pol, err = parseImagePolicy("-")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), protUnused, protAbsent) {
		t.Errorf("\"-\" root = %v, want {unused,absent}", pol.forDesignator("root"))
	}

	// "~" = everything must be absent.
	pol, err = parseImagePolicy("~")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("usr"), protAbsent) {
		t.Errorf("\"~\" usr = %v, want {absent}", pol.forDesignator("usr"))
	}
}

func TestParseImagePolicyShortcutsAndEmptyFlags(t *testing.T) {
	// "open" shortcut.
	pol, err := parseImagePolicy("root=open")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), openList...) {
		t.Errorf("root=open = %v, want full open set", pol.forDesignator("root"))
	}

	// "ignore" shortcut = unused+absent.
	pol, err = parseImagePolicy("root=ignore")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), protUnused, protAbsent) {
		t.Errorf("root=ignore = %v, want {unused,absent}", pol.forDesignator("root"))
	}

	// Listed designator with no flags at all: open is implied.
	pol, err = parseImagePolicy("root=")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), openList...) {
		t.Errorf("root= must imply open, got %v", pol.forDesignator("root"))
	}

	// Only non-protection flags listed: open protections implied too.
	pol, err = parseImagePolicy("root=erofs")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), openList...) {
		t.Errorf("root=erofs must imply open protections, got %v", pol.forDesignator("root"))
	}

	// Duplicate flags are fine.
	if _, err := parseImagePolicy("root=verity+verity"); err != nil {
		t.Errorf("duplicate flags must parse: %v", err)
	}

	// encryptedwithintegrity is accepted (systemd 260 parser; not in the
	// man page flag table).
	pol, err = parseImagePolicy("root=encryptedwithintegrity")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.forDesignator("root"), protEncryptedWithIntegrity) {
		t.Errorf("root = %v, want {encryptedwithintegrity}", pol.forDesignator("root"))
	}
}

func TestParseImagePolicyFSFlags(t *testing.T) {
	pol, err := parseImagePolicy("root=erofs+squashfs:usr=verity")
	if err != nil {
		t.Fatal(err)
	}
	if err := pol.checkFS("root", FSErofs); err != nil {
		t.Errorf("erofs must be allowed: %v", err)
	}
	if err := pol.checkFS("root", FSSquashfs); err != nil {
		t.Errorf("squashfs must be allowed: %v", err)
	}
	if err := pol.checkFS("root", FSExt4); err == nil {
		t.Error("ext4 must be rejected for root=erofs+squashfs")
	} else if !strings.Contains(err.Error(), "erofs+squashfs") {
		t.Errorf("fs error must list allowed types, got %v", err)
	}
	// No fs flags on usr: all filesystems allowed.
	if err := pol.checkFS("usr", FSExt4); err != nil {
		t.Errorf("usr without fs flags must allow any filesystem: %v", err)
	}
	// Unlisted designator without default: fs unrestricted.
	if err := pol.checkFS("home", FSExt4); err != nil {
		t.Errorf("unlisted designator must not restrict fs: %v", err)
	}

	// All man-page fs flags parse.
	if _, err := parseImagePolicy("root=btrfs+erofs+ext4+f2fs+squashfs+vfat+xfs"); err != nil {
		t.Errorf("all man-page fs flags must parse: %v", err)
	}
}

func TestParseImagePolicyGPTFlags(t *testing.T) {
	pol, err := parseImagePolicy("root=verity+read-only-on:usr=growfs-off")
	if err != nil {
		t.Fatal(err)
	}
	r := pol.rules["root"]
	if r.readOnly == nil || !*r.readOnly {
		t.Errorf("root read-only flag = %v, want dictated on", r.readOnly)
	}
	if r.growfs != nil {
		t.Errorf("root growfs must be undictated, got %v", *r.growfs)
	}
	u := pol.rules["usr"]
	if u.growfs == nil || *u.growfs {
		t.Errorf("usr growfs flag = %v, want dictated off", u.growfs)
	}
	if !setEq(u.protections, openList...) {
		t.Errorf("usr=growfs-off must imply open protections, got %v", u.protections)
	}

	// Setting both of a pair is equivalent to setting neither.
	pol, err = parseImagePolicy("root=read-only-on+read-only-off+growfs-on+growfs-off")
	if err != nil {
		t.Fatal(err)
	}
	r = pol.rules["root"]
	if r.readOnly != nil || r.growfs != nil {
		t.Errorf("both-of-pair must be undictated, got readOnly=%v growfs=%v", r.readOnly, r.growfs)
	}
}

func TestParseImagePolicyWhitespace(t *testing.T) {
	for _, s := range []string{" root=verity", "root =verity", "root= verity ", " root=verity : usr=signed "} {
		pol, err := parseImagePolicy(s)
		if err != nil {
			t.Errorf("parseImagePolicy(%q): %v (systemd tolerates whitespace around words)", s, err)
			continue
		}
		if !setEq(pol.forDesignator("root"), protVerity) {
			t.Errorf("parseImagePolicy(%q) root = %v, want {verity}", s, pol.forDesignator("root"))
		}
	}
}

func TestParseImagePolicyMalformed(t *testing.T) {
	for _, s := range []string{
		"root",                    // no '='
		"verity",                  // bare flag list: no '=' (not a valid shorthand)
		"root=banana",             // unknown protection
		"root=ntfs",               // unknown filesystem
		"root=verity+",            // trailing '+' -> empty flag
		"root=+verity",            // leading '+' -> empty flag
		"root=verity:",            // trailing ':' -> empty rule
		":root=verity",            // leading ':'
		"root=verity::usr=open",   // doubled ':'
		"root=verity=x",           // junk after flags ('verity=x' is not a flag)
		"banana=verity",           // unknown designator
		"default=verity",          // 'default' is not a designator (use '=...')
		"ROOT=verity",             // designators are case-sensitive
		"root=Verity",             // flags are case-sensitive
		"root=verity:root=signed", // duplicate designator
		"=verity:=signed",         // duplicate default rule
		"   ",                     // whitespace-only
		"**",                      // not a special policy
	} {
		if _, err := parseImagePolicy(s); err == nil {
			t.Errorf("parseImagePolicy(%q) = nil error, want failure", s)
		}
	}
}

func TestClassifyProtection(t *testing.T) {
	g := archGUIDs["x86-64"]
	const uA = "11111111-1111-1111-1111-111111111111"
	const uB = "22222222-2222-2222-2222-222222222222"

	mk := func(types ...string) []gptPartition {
		var parts []gptPartition
		for i, ty := range types {
			u := uA
			if i%2 == 1 {
				u = uB
			}
			parts = append(parts, gptPartition{Index: i + 1, TypeGUID: ty, UniqueGUID: u})
		}
		return parts
	}

	cases := []struct {
		name       string
		parts      []gptPartition
		designator string
		want       protection
	}{
		{"no partitions", nil, "root", protAbsent},
		{"data only", mk(g.root), "root", protUnprotected},
		{"data+verity", mk(g.root, g.rootVerity), "root", protVerity},
		{"data+verity+sig", mk(g.root, g.rootVerity, g.rootVeritySig), "root", protSigned},
		{"sig without verity is unprotected", mk(g.root, g.rootVeritySig), "root", protUnprotected},
		{"usr data+verity", mk(g.usr, g.usrVerity), "usr", protVerity},
		{"usr absent when only root present", mk(g.root, g.rootVerity), "usr", protAbsent},
		{"root unaffected by usr verity", mk(g.root, g.usrVerity), "root", protUnprotected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProtection(tc.parts, g, tc.designator); got != tc.want {
				t.Errorf("classifyProtection = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("zero unique GUIDs degrade to unprotected", func(t *testing.T) {
		parts := []gptPartition{
			{Index: 1, TypeGUID: g.root, UniqueGUID: zeroGUID},
			{Index: 2, TypeGUID: g.rootVerity, UniqueGUID: uB},
		}
		if got := classifyProtection(parts, g, "root"); got != protUnprotected {
			t.Errorf("zero data GUID: got %v, want unprotected", got)
		}
		parts[0].UniqueGUID = uA
		parts[1].UniqueGUID = zeroGUID
		if got := classifyProtection(parts, g, "root"); got != protUnprotected {
			t.Errorf("zero verity GUID: got %v, want unprotected", got)
		}
	})
}

// testPartitionSets builds the standard partition layouts used by the
// decideVerity tests: root-designator (unprotected/verity/signed) and
// usr-designator (unprotected/verity) variants.
func testPartitionSets(g dpsGUIDs) (rootUnprot, rootVerity, rootSigned, usrUnprot, usrVerity []gptPartition) {
	const uA = "11111111-1111-1111-1111-111111111111"
	const uB = "22222222-2222-2222-2222-222222222222"

	rootUnprot = []gptPartition{{Index: 1, TypeGUID: g.root, UniqueGUID: uA}}
	rootVerity = append(append([]gptPartition(nil), rootUnprot...),
		gptPartition{Index: 2, TypeGUID: g.rootVerity, UniqueGUID: uB})
	rootSigned = append(append([]gptPartition(nil), rootVerity...),
		gptPartition{Index: 3, TypeGUID: g.rootVeritySig, UniqueGUID: uA})
	usrUnprot = []gptPartition{{Index: 1, TypeGUID: g.usr, UniqueGUID: uA}}
	usrVerity = append(append([]gptPartition(nil), usrUnprot...),
		gptPartition{Index: 2, TypeGUID: g.usrVerity, UniqueGUID: uB})
	return
}

func TestDecideVerity(t *testing.T) {
	g := archGUIDs["x86-64"]
	unprotected, verity, signed, _, _ := testPartitionSets(g)

	cases := []struct {
		name      string
		parts     []gptPartition
		policy    string
		useVerity bool
		checkSig  bool
		errSubstr string // "" = no error expected
	}{
		{"unprotected, default policy", unprotected, "", false, false, ""},
		{"unprotected vs root=verity", unprotected, "root=verity", false, false, "does not satisfy image policy"},
		{"unprotected vs root=unprotected", unprotected, "root=unprotected", false, false, ""},
		{"verity, default policy prefers verity", verity, "", true, false, ""},
		{"verity vs root=verity", verity, "root=verity", true, false, ""},
		{"verity vs root=unprotected mounts directly", verity, "root=unprotected", false, false, ""},
		{"verity vs root=signed", verity, "root=signed", false, false, "does not satisfy image policy"},
		{"verity vs root=absent", verity, "root=absent", false, false, "does not satisfy image policy"},
		{"verity vs root=encrypted", verity, "root=encrypted", false, false, "not supported"},
		{"verity vs root=unused", verity, "root=unused", false, false, "does not satisfy image policy"},
		{"verity vs root=ignore", verity, "root=ignore", false, false, "does not satisfy image policy"},
		{"verity vs root=open", verity, "root=open", true, false, ""},
		{"signed, default policy verifies signature", signed, "", true, true, ""},
		{"signed vs root=verity+signed verifies signature", signed, "root=verity+signed", true, true, ""},
		{"signed-only policy verifies signature", signed, "root=signed", true, true, ""},
		{"signed vs root=verity treated as plain verity", signed, "root=verity", true, false, ""},
		{"signed vs root=unprotected mounts directly", signed, "root=unprotected", false, false, ""},
		{"signed vs root=absent", signed, "root=absent", false, false, "does not satisfy image policy"},
		{"signed vs special * verifies signature", signed, "*", true, true, ""},
		{"unprotected vs special * accepted", unprotected, "*", false, false, ""},
		{"verity vs special - rejected", verity, "-", false, false, "does not satisfy image policy"},
		{"unprotected vs special ~ rejected", unprotected, "~", false, false, "does not satisfy image policy"},
		{"verity vs default rule =verity", verity, "=verity", true, false, ""},
		{"unprotected vs default rule =verity", unprotected, "=verity", false, false, "does not satisfy image policy"},
		{"root unlisted, no default: rejected", verity, "usr=verity", false, false, "does not satisfy image policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol, err := parseImagePolicy(tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			useVerity, checkSig, err := decideVerity(tc.parts, g, "root", pol)
			if tc.errSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("err = %v, want substring %q", err, tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if useVerity != tc.useVerity {
				t.Errorf("useVerity = %v, want %v", useVerity, tc.useVerity)
			}
			if checkSig != tc.checkSig {
				t.Errorf("checkSig = %v, want %v", checkSig, tc.checkSig)
			}
		})
	}
}

// TestManPageExamples checks every policy string from the EXAMPLES section
// of systemd.image-policy(7) (docs/reference/systemd.image-policy.7.txt):
// each parses and yields the documented enforcement decision for the
// root/usr designators.
func TestManPageExamples(t *testing.T) {
	g := archGUIDs["x86-64"]
	rootUnprot, rootVerity, _, usrUnprot, usrVerity := testPartitionSets(g)

	t.Run("usr=verity+read-only-on:root=encrypted:swap=encrypted", func(t *testing.T) {
		pol, err := parseImagePolicy("usr=verity+read-only-on:root=encrypted:swap=encrypted")
		if err != nil {
			t.Fatal(err)
		}
		// Verity-enabled usr partition is accepted.
		useVerity, checkSig, err := decideVerity(usrVerity, g, "usr", pol)
		if err != nil || !useVerity || checkSig {
			t.Errorf("usr verity: useVerity=%v checkSig=%v err=%v, want true/false/nil", useVerity, checkSig, err)
		}
		// Unprotected usr is rejected.
		if _, _, err := decideVerity(usrUnprot, g, "usr", pol); err == nil {
			t.Error("unprotected usr must be rejected by usr=verity")
		}
		// root requires encryption, which is never satisfiable here.
		if _, _, err := decideVerity(rootUnprot, g, "root", pol); err == nil ||
			!strings.Contains(err.Error(), "not supported") {
			t.Errorf("root=encrypted must reject with LUKS hint, got %v", err)
		}
		if _, _, err := decideVerity(rootVerity, g, "root", pol); err == nil {
			t.Error("verity root must be rejected by root=encrypted")
		}
	})

	t.Run("root=encrypted+read-only-off:srv=encrypted+absent:swap=absent", func(t *testing.T) {
		pol, err := parseImagePolicy("root=encrypted+read-only-off:srv=encrypted+absent:swap=absent")
		if err != nil {
			t.Fatal(err)
		}
		if r := pol.rules["root"]; r.readOnly == nil || *r.readOnly {
			t.Errorf("root read-only flag = %v, want dictated off", r.readOnly)
		}
		if !setEq(pol.rules["srv"].protections, protEncrypted, protAbsent) {
			t.Errorf("srv = %v, want {encrypted,absent}", pol.rules["srv"].protections)
		}
		// Unprotected root cannot satisfy root=encrypted.
		if _, _, err := decideVerity(rootUnprot, g, "root", pol); err == nil {
			t.Error("unprotected root must be rejected by root=encrypted")
		}
		// usr is unlisted with no default rule: unused+absent fallback, so
		// a usr payload can never be used.
		if _, _, err := decideVerity(usrVerity, g, "usr", pol); err == nil {
			t.Error("usr payload must be rejected when usr is unlisted without a default")
		}
	})

	t.Run("root=unprotected+encrypted:swap=absent+unused:=unprotected+encrypted+absent", func(t *testing.T) {
		pol, err := parseImagePolicy("root=unprotected+encrypted:swap=absent+unused:=unprotected+encrypted+absent")
		if err != nil {
			t.Fatal(err)
		}
		// Unprotected root accepted as-is.
		useVerity, _, err := decideVerity(rootUnprot, g, "root", pol)
		if err != nil || useVerity {
			t.Errorf("unprotected root: useVerity=%v err=%v, want false/nil", useVerity, err)
		}
		// Verity root downgraded to a direct mount (unprotected allowed,
		// verity not).
		useVerity, _, err = decideVerity(rootVerity, g, "root", pol)
		if err != nil || useVerity {
			t.Errorf("verity root: useVerity=%v err=%v, want false/nil", useVerity, err)
		}
		// usr handled by the "=unprotected+encrypted+absent" default rule.
		if !setEq(pol.forDesignator("usr"), protUnprotected, protEncrypted, protAbsent) {
			t.Errorf("usr = %v, want default-rule set", pol.forDesignator("usr"))
		}
		useVerity, _, err = decideVerity(usrVerity, g, "usr", pol)
		if err != nil || useVerity {
			t.Errorf("verity usr via default: useVerity=%v err=%v, want false/nil", useVerity, err)
		}
	})

	t.Run("root=erofs+squashfs:swap=absent+unused:=unprotected+encrypted+absent", func(t *testing.T) {
		pol, err := parseImagePolicy("root=erofs+squashfs:swap=absent+unused:=unprotected+encrypted+absent")
		if err != nil {
			t.Fatal(err)
		}
		// fs-only flags imply the open protection set: verity is used.
		useVerity, _, err := decideVerity(rootVerity, g, "root", pol)
		if err != nil || !useVerity {
			t.Errorf("verity root: useVerity=%v err=%v, want true/nil", useVerity, err)
		}
		if err := pol.checkFS("root", FSErofs); err != nil {
			t.Errorf("erofs root must be allowed: %v", err)
		}
		if err := pol.checkFS("root", FSSquashfs); err != nil {
			t.Errorf("squashfs root must be allowed: %v", err)
		}
		if err := pol.checkFS("root", FSExt4); err == nil {
			t.Error("ext4 root must be rejected by root=erofs+squashfs")
		}
		// The default rule carries no fs flags: usr fs unrestricted.
		if err := pol.checkFS("usr", FSExt4); err != nil {
			t.Errorf("usr fs must be unrestricted: %v", err)
		}
	})
}

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

func TestParseImagePolicyDefault(t *testing.T) {
	pol, err := parseImagePolicy("")
	if err != nil {
		t.Fatal(err)
	}
	all := []protection{protAbsent, protUnprotected, protVerity, protSigned, protEncrypted}
	if !setEq(pol.root, all...) || !setEq(pol.usr, all...) {
		t.Errorf("empty policy must allow everything, got root=%v usr=%v", pol.root, pol.usr)
	}
}

func TestParseImagePolicySubsets(t *testing.T) {
	pol, err := parseImagePolicy("root=verity")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.root, protVerity) {
		t.Errorf("root = %v, want {verity}", pol.root)
	}
	// usr unmentioned: keeps the class default.
	if !pol.usr[protUnprotected] || !pol.usr[protAbsent] {
		t.Errorf("unmentioned usr must keep default, got %v", pol.usr)
	}

	pol, err = parseImagePolicy("root=verity+signed:usr=unprotected+absent")
	if err != nil {
		t.Fatal(err)
	}
	if !setEq(pol.root, protVerity, protSigned) {
		t.Errorf("root = %v, want {verity,signed}", pol.root)
	}
	if !setEq(pol.usr, protUnprotected, protAbsent) {
		t.Errorf("usr = %v, want {unprotected,absent}", pol.usr)
	}
}

func TestParseImagePolicyUnknownDesignators(t *testing.T) {
	// Unknown-but-well-formed designators are ignored gracefully.
	pol, err := parseImagePolicy("home=encrypted:esp=unprotected:root=signed")
	if err != nil {
		t.Fatalf("unknown designators must be ignored: %v", err)
	}
	if !setEq(pol.root, protSigned) {
		t.Errorf("root = %v, want {signed}", pol.root)
	}
}

func TestParseImagePolicyMalformed(t *testing.T) {
	for _, s := range []string{
		"root",          // no '='
		"=verity",       // empty designator
		"root=banana",   // unknown protection
		"root=",         // empty alternatives
		"root=verity+",  // trailing '+' -> empty protection
		"root=verity:",  // trailing ':' -> empty component
		":root=verity",  // leading ':'
		"root=verity=x", // junk after alternatives
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

func TestDecideVerity(t *testing.T) {
	g := archGUIDs["x86-64"]
	const uA = "11111111-1111-1111-1111-111111111111"
	const uB = "22222222-2222-2222-2222-222222222222"

	unprotected := []gptPartition{{Index: 1, TypeGUID: g.root, UniqueGUID: uA}}
	verity := append(append([]gptPartition(nil), unprotected...),
		gptPartition{Index: 2, TypeGUID: g.rootVerity, UniqueGUID: uB})
	signed := append(append([]gptPartition(nil), verity...),
		gptPartition{Index: 3, TypeGUID: g.rootVeritySig, UniqueGUID: uA})

	cases := []struct {
		name      string
		parts     []gptPartition
		policy    string
		useVerity bool
		errSubstr string // "" = no error expected
	}{
		{"unprotected, default policy", unprotected, "", false, ""},
		{"unprotected vs root=verity", unprotected, "root=verity", false, "does not satisfy image policy"},
		{"unprotected vs root=unprotected", unprotected, "root=unprotected", false, ""},
		{"verity, default policy prefers verity", verity, "", true, ""},
		{"verity vs root=verity", verity, "root=verity", true, ""},
		{"verity vs root=unprotected mounts directly", verity, "root=unprotected", false, ""},
		{"verity vs root=signed", verity, "root=signed", false, "does not satisfy image policy"},
		{"verity vs root=absent", verity, "root=absent", false, "does not satisfy image policy"},
		{"verity vs root=encrypted", verity, "root=encrypted", false, "not supported"},
		{"signed treated as verity when verity allowed", signed, "root=verity+signed", true, ""},
		{"signed-only policy unimplemented", signed, "root=signed", false, "signature verification not implemented"},
		{"signed vs root=unprotected mounts directly", signed, "root=unprotected", false, ""},
		{"signed vs root=absent", signed, "root=absent", false, "does not satisfy image policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol, err := parseImagePolicy(tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decideVerity(tc.parts, g, "root", pol)
			if tc.errSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("err = %v, want substring %q", err, tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.useVerity {
				t.Errorf("useVerity = %v, want %v", got, tc.useVerity)
			}
		})
	}
}

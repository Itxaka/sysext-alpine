package image

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// analyzeRow renders an effective policy like the columns of
// "systemd-analyze image-policy".
func analyzeRow(f policyFlags) string {
	tri := func(on, off policyFlags) string {
		switch f & (on | off) {
		case on:
			return "yes"
		case off:
			return "no"
		default:
			return "-"
		}
	}
	fstype := "-"
	if f&polFSTypeMask != 0 {
		fstype = (f & polFSTypeMask).String()
	}
	return strings.Join([]string{
		(f & polUseMask).String(),
		tri(polReadOnlyOn, polReadOnlyOff),
		tri(polGrowFSOn, polGrowFSOff),
		fstype,
	}, " ")
}

func TestPolicyMatchesSystemdAnalyze(t *testing.T) {
	for _, v := range systemdAnalyzeVectors {
		pol, err := parseImagePolicy(v.policy)
		if err != nil {
			t.Fatalf("parseImagePolicy(%q): %v", v.policy, err)
		}
		if len(v.rows) != int(numDesignators) {
			t.Fatalf("%q: vector has %d rows", v.policy, len(v.rows))
		}
		for name, want := range v.rows {
			d, ok := designatorFromString(name)
			if !ok {
				t.Fatalf("unknown designator %q", name)
			}
			if got := analyzeRow(pol.exhaustive(d)); got != want {
				t.Errorf("policy %q, %s: got %q, systemd-analyze says %q", v.policy, name, got, want)
			}
		}
	}
}

func TestParseImagePolicyMalformed(t *testing.T) {
	for _, s := range []string{
		"root",
		"verity",
		"root=banana",
		"root=ntfs",
		"root=verity+",
		"root=+verity",
		"root=verity:",
		":root=verity",
		"root=verity::usr=open",
		"root=verity=x",
		"banana=verity",
		"default=verity",
		"ROOT=verity",
		"root=Verity",
		"root=verity:root=signed",
		"=verity:=signed",
		"   ",
		" *",
		"**",
	} {
		if _, err := parseImagePolicy(s); err == nil {
			t.Errorf("parseImagePolicy(%q) succeeded, want error", s)
		}
		if err := ValidatePolicy(s); err == nil {
			t.Errorf("ValidatePolicy(%q) succeeded, want error", s)
		}
	}
}

func TestParseImagePolicyWhitespaceAndDash(t *testing.T) {
	for s, want := range map[string]string{
		" root = verity ":        "verity",
		"root= verity + signed ": "verity+signed",
		"root=-":                 "open",
		"root=":                  "open",
		"=":                      "open",
	} {
		pol, err := parseImagePolicy(s)
		if err != nil {
			t.Fatalf("parseImagePolicy(%q): %v", s, err)
		}
		if got := (pol.exhaustive(partRoot) & polUseMask).String(); got != want {
			t.Errorf("parseImagePolicy(%q) root = %s, want %s", s, got, want)
		}
	}
	if err := ValidatePolicy(""); err != nil {
		t.Errorf("empty policy (class default) rejected: %v", err)
	}
}

func TestClassDefaultPolicy(t *testing.T) {
	for _, tc := range []struct {
		class release.Class
		d     designator
		want  string
	}{
		{release.Sysext, partRoot, "verity+signed+encrypted+encryptedwithintegrity+unprotected+absent"},
		{release.Sysext, partUsr, "verity+signed+encrypted+encryptedwithintegrity+unprotected+absent"},
		{release.Sysext, partHome, "ignore"},
		{release.Sysext, partRootVerity, "unprotected+absent"},
		{release.Sysext, partUsrVeritySig, "unprotected+absent"},
		{release.Confext, partRoot, "verity+signed+encrypted+encryptedwithintegrity+unprotected+absent"},
		{release.Confext, partUsr, "ignore"},
		{release.Confext, partUsrVerity, "ignore"},
	} {
		pol, err := resolvePolicy("", tc.class)
		if err != nil {
			t.Fatal(err)
		}
		if got := (pol.exhaustive(tc.d) & polUseMask).String(); got != tc.want {
			t.Errorf("class %d %s = %s, want %s", tc.class, tc.d, got, tc.want)
		}
	}
}

func TestPolicyMayUse(t *testing.T) {
	for _, tc := range []struct {
		policy  string
		d       designator
		use     bool
		wantErr bool
	}{
		{"root=verity", partRoot, true, false},
		{"root=verity", partUsr, false, false},
		{"root=absent", partRoot, false, true},
		{"root=unused", partRoot, false, false},
		{"root=unused+absent", partRoot, false, false},
		{"root=unused+verity", partRoot, true, false},
		{"root=open:=absent", partSwap, false, true},
		{"root=verity:esp=absent", partESP, false, true},
		{"=verity", partRootVerity, true, false},
	} {
		pol, err := parseImagePolicy(tc.policy)
		if err != nil {
			t.Fatal(err)
		}
		use, err := pol.mayUse(tc.d)
		if (err != nil) != tc.wantErr || use != tc.use {
			t.Errorf("%q mayUse(%s) = %v, %v; want %v, err=%v", tc.policy, tc.d, use, err, tc.use, tc.wantErr)
		}
	}
}

func TestPolicyCheckProtection(t *testing.T) {
	const present = polEncrypted | polEncryptedWithIntegrity | polUnprotected | polUnused
	for _, tc := range []struct {
		policy string
		d      designator
		found  policyFlags
		ok     bool
	}{
		{"root=verity", partRoot, present, false},
		{"root=verity", partRoot, present | polVerity, true},
		{"root=signed", partRoot, present | polVerity, false},
		{"root=signed", partRoot, present | polVerity | polSigned, true},
		{"root=unprotected", partRoot, present | polVerity, true},
		{"root=verity", partUsr, polAbsent, true},
		{"root=verity", partUsr, polUnused, true},
		{"root=verity:usr=verity", partUsr, polAbsent, false},
		{"=verity", partRootVerity, present, false},
		{"root=encrypted", partRoot, polUnused | polUnprotected | polVerity | polSigned, false},
	} {
		pol, err := parseImagePolicy(tc.policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := pol.checkProtection(tc.d, tc.found); (err == nil) != tc.ok {
			t.Errorf("%q checkProtection(%s, %s) = %v, want ok=%v", tc.policy, tc.d, tc.found, err, tc.ok)
		}
	}
}

func TestPolicyCheckPartitionFlags(t *testing.T) {
	for _, tc := range []struct {
		policy string
		attrs  uint64
		ok     bool
	}{
		{"root=read-only-on", 0, false},
		{"root=read-only-on", gptFlagReadOnly, true},
		{"root=read-only-off", gptFlagReadOnly, false},
		{"root=read-only-on+read-only-off", 0, true},
		{"root=growfs-on", 0, false},
		{"root=growfs-off", gptFlagGrowFS, false},
		{"root=growfs-off", 0, true},
		{"root=verity", gptFlagReadOnly | gptFlagGrowFS, true},
	} {
		pol, err := parseImagePolicy(tc.policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := pol.checkPartitionFlags(partRoot, tc.attrs); (err == nil) != tc.ok {
			t.Errorf("%q flags %#x: err=%v, want ok=%v", tc.policy, tc.attrs, err, tc.ok)
		}
	}
}

func TestPolicyCheckFS(t *testing.T) {
	pol, err := parseImagePolicy("root=erofs+squashfs:usr=verity")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		d  designator
		fs fsType
		ok bool
	}{
		{partRoot, fsErofs, true},
		{partRoot, fsSquashfs, true},
		{partRoot, fsExt4, false},
		{partUsr, fsExt4, true},
	} {
		if err := pol.checkFS(tc.d, tc.fs); (err == nil) != tc.ok {
			t.Errorf("checkFS(%s, %s) = %v, want ok=%v", tc.d, tc.fs, err, tc.ok)
		}
	}
	if err := pol.checkFS(partRoot, fsExt4); err == nil || !strings.Contains(err.Error(), "erofs+squashfs") {
		t.Errorf("error must list the allowed types, got %v", err)
	}
}

func TestPolicyFlagsString(t *testing.T) {
	for f, want := range map[policyFlags]string{
		0:                                      "-",
		polOpen:                                "open",
		polIgnore:                              "ignore",
		polVerity | polSigned:                  "verity+signed",
		polVerity | polReadOnlyOn:              "verity+read-only-on",
		polVerity | polReadOnlyMask:            "verity",
		polUnprotected | polGrowFSOff | polXFS: "unprotected+growfs-off+xfs",
	} {
		if got := f.String(); got != want {
			t.Errorf("%#x.String() = %q, want %q", uint32(f), got, want)
		}
	}
}

func TestPolicyErrors(t *testing.T) {
	for policy, errno := range map[string]error{
		"garbage":                 unix.EINVAL,
		"root=verity:root=signed": unix.ENOTUNIQ,
		"=open:=ignore":           unix.ENOTUNIQ,
		"foo=verity":              unix.EBADSLT,
		"root=bogus":              unix.EBADRQC,
	} {
		if err := ValidatePolicy(policy); !errors.Is(err, errno) {
			t.Errorf("ValidatePolicy(%q) = %v, want %v", policy, err, errno)
		}
	}
}

func TestNormalizePolicy(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"-":                                 "=ignore",
		"*":                                 "=open",
		"root=verity+bogus:bogus=open:usr=": "=ignore:root=verity:usr=-",
		"=absent:usr=signed+read-only-on":   "=absent:usr=signed+read-only-on",
	} {
		got, err := NormalizePolicy(in)
		if err != nil || got != want {
			t.Errorf("NormalizePolicy(%q) = %q, %v; want %q", in, got, err, want)
			continue
		}
		if err := ValidatePolicy(got); err != nil {
			t.Errorf("normalized %q is rejected: %v", got, err)
		}
	}
	for _, bad := range []string{"root=verity:root=signed", "garbage"} {
		if _, err := NormalizePolicy(bad); err == nil {
			t.Errorf("NormalizePolicy(%q) succeeded", bad)
		}
	}
}

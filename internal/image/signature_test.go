package image

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"
)

var testRootHash = bytes.Repeat([]byte{0x2e, 0xe8, 0x2d, 0x1b}, 8)

type testIdentity struct {
	key  *rsa.PrivateKey
	cert *x509.Certificate
}

// newIdentity generates a key and certificate, self-signed when parent is
// nil, otherwise issued by parent.
func newIdentity(t *testing.T, cn string, isCA bool, notBefore, notAfter time.Time, parent *testIdentity) *testIdentity {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	signerTmpl, signerKey := tmpl, key
	if parent != nil {
		signerTmpl, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerTmpl, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testIdentity{key: key, cert: cert}
}

func validIdentity(t *testing.T, cn string, parent *testIdentity) *testIdentity {
	return newIdentity(t, cn, parent == nil, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), parent)
}

type signOpts struct {
	noAttrs  bool
	noCerts  bool
	content  string
	extra    []*x509.Certificate
	attached bool
}

// sign produces a PKCS#7 signature (DER) over the lowercase hex root hash.
func sign(t *testing.T, rootHash []byte, id *testIdentity, o signOpts) []byte {
	t.Helper()
	content := o.content
	if content == "" {
		content = hex.EncodeToString(rootHash)
	}
	sd, err := pkcs7.NewSignedData([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	sd.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	if o.noAttrs {
		err = sd.SignWithoutAttr(id.cert, id.key, pkcs7.SignerInfoConfig{})
	} else {
		err = sd.AddSigner(id.cert, id.key, pkcs7.SignerInfoConfig{})
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range o.extra {
		sd.AddCertificate(c)
	}
	if o.noCerts {
		sd.GetSignedData().Certificates.Raw = nil
	}
	if !o.attached {
		sd.Detach()
	}
	der, err := sd.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func writeCertPEM(t *testing.T, path string, certs ...*x509.Certificate) {
	t.Helper()
	var buf []byte
	for _, c := range certs {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// trustRoot creates a root with the given certificates in /etc/verity.d.
func trustRoot(t *testing.T, certs ...*x509.Certificate) []string {
	t.Helper()
	root := t.TempDir()
	for i, c := range certs {
		writeCertPEM(t, filepath.Join(root, "etc/verity.d", string(rune('a'+i))+".crt"), c)
	}
	return TrustDirs(root)
}

func mustVerify(t *testing.T, sig []byte, dirs []string, wantOK bool) {
	t.Helper()
	ok, reason, err := verifySignature(testRootHash, sig, dirs)
	if err != nil {
		t.Fatalf("verifySignature: %v", err)
	}
	if ok != wantOK {
		t.Fatalf("verifySignature = %v (%s), want %v", ok, reason, wantOK)
	}
}

func TestVerifySignatureTrustModel(t *testing.T) {
	signer := validIdentity(t, "signer", nil)
	other := validIdentity(t, "other", nil)
	ca := validIdentity(t, "ca", nil)
	leaf := validIdentity(t, "leaf", ca)
	expired := newIdentity(t, "expired", false, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour), nil)

	t.Run("signer is an anchor", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, signer, signOpts{}), trustRoot(t, signer.cert), true)
	})
	t.Run("signature without embedded certificates", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, signer, signOpts{noCerts: true, noAttrs: true}), trustRoot(t, signer.cert), true)
	})
	t.Run("untrusted signer", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, signer, signOpts{}), trustRoot(t, other.cert), false)
	})
	t.Run("only the issuing CA is trusted", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, leaf, signOpts{extra: []*x509.Certificate{ca.cert}}), trustRoot(t, ca.cert), false)
	})
	t.Run("expired signer without attributes", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, expired, signOpts{noAttrs: true}), trustRoot(t, expired.cert), true)
	})
	t.Run("expired signer with signing time", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, expired, signOpts{}), trustRoot(t, expired.cert), true)
	})
	t.Run("signed content differs", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, signer, signOpts{content: strings.Repeat("cd", 32)}), trustRoot(t, signer.cert), false)
	})
	t.Run("signature over uppercase hex", func(t *testing.T) {
		upper := strings.ToUpper(hex.EncodeToString(testRootHash))
		mustVerify(t, sign(t, testRootHash, signer, signOpts{content: upper}), trustRoot(t, signer.cert), false)
	})
	t.Run("attached content is ignored", func(t *testing.T) {
		mustVerify(t, sign(t, testRootHash, signer, signOpts{content: "other content", attached: true}), trustRoot(t, signer.cert), false)
	})
	t.Run("malformed certificate next to a valid one", func(t *testing.T) {
		dirs := trustRoot(t, signer.cert)
		if err := os.WriteFile(filepath.Join(dirs[0], "0-garbage.crt"), []byte("-----BEGIN CERTIFICATE-----\nZm9v\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustVerify(t, sign(t, testRootHash, signer, signOpts{}), dirs, true)
	})
	t.Run("only the first certificate of a file counts", func(t *testing.T) {
		root := t.TempDir()
		writeCertPEM(t, filepath.Join(root, "etc/verity.d/bundle.crt"), other.cert, signer.cert)
		mustVerify(t, sign(t, testRootHash, signer, signOpts{}), TrustDirs(root), false)
	})
}

func TestVerifySignatureMalformedDER(t *testing.T) {
	signer := validIdentity(t, "signer", nil)
	dirs := trustRoot(t, signer.cert)
	for _, der := range [][]byte{{0x1f, 0x80}, {0x1f, 0x05}, {0x30, 0x81}, {0x30, 0x84, 0x01}, []byte("garbage")} {
		if _, _, err := verifySignature(testRootHash, der, dirs); err == nil {
			t.Errorf("malformed DER %x accepted", der)
		}
	}
	// Without trust anchors the signature is never parsed.
	ok, reason, err := verifySignature(testRootHash, []byte{0x30, 0x81}, TrustDirs(t.TempDir()))
	if ok || err != nil || !strings.Contains(reason, "no trusted certificates") {
		t.Errorf("no anchors: ok=%v reason=%q err=%v", ok, reason, err)
	}
}

// nested wraps content in depth definite-length SEQUENCEs.
func nested(depth int, content []byte) []byte {
	der := content
	for range depth {
		var l []byte
		switch n := len(der); {
		case n < 0x80:
			l = []byte{byte(n)}
		case n < 0x100:
			l = []byte{0x81, byte(n)}
		default:
			l = []byte{0x82, byte(n >> 8), byte(n)}
		}
		der = append(append([]byte{0x30}, l...), der...)
	}
	return der
}

func TestCheckBERDepth(t *testing.T) {
	signer := validIdentity(t, "signer", nil)
	indefinite := func(depth int) []byte {
		b := bytes.Repeat([]byte{0x30, 0x80}, depth)
		b = append(b, 0x04, 0x00)
		return append(b, bytes.Repeat([]byte{0x00, 0x00}, depth)...)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"signature", sign(t, testRootHash, signer, signOpts{}), ""},
		{"trailing data", append(nested(2, []byte{0x04, 0x00}), 0xff, 0xff), ""},
		{"high tag number", []byte{0x3f, 0x81, 0x01, 0x02, 0x04, 0x00}, ""},
		{"empty constructed", []byte{0x30, 0x00, 0x00}, ""},
		{"32 definite levels", nested(32, []byte{0x04, 0x00}), ""},
		{"33 definite levels", nested(33, []byte{0x04, 0x00}), "deeper than 32"},
		{"32 indefinite levels", indefinite(32), ""},
		{"33 indefinite levels", indefinite(33), "deeper than 32"},
		{"unterminated nesting", bytes.Repeat([]byte{0x30, 0x80}, 1<<20), "deeper than 32"},
		// The converter reads one child of an indefinite object before it
		// looks for the end-of-contents octets, so these nest.
		{"end-of-contents first", bytes.Repeat([]byte{0x30, 0x80, 0x00, 0x00}, 40), "deeper than 32"},
		{"empty", nil, "truncated"},
		{"truncated length", []byte{0x30, 0x84, 0x01}, "too long"},
		{"truncated content", []byte{0x04, 0x05, 0x00}, "truncated"},
		{"unterminated", []byte{0x30, 0x80, 0x04, 0x00}, "unterminated"},
		{"primitive indefinite", []byte{0x04, 0x80, 0x00, 0x00}, "primitive"},
		{"length with leading zero", []byte{0x04, 0x81, 0x00}, "malformed"},
		{"negative length", []byte{0x04, 0x84, 0x80, 0x00, 0x00, 0x00, 0x00}, "malformed"},
		{"five length octets", []byte{0x04, 0x85, 0x01, 0x00, 0x00, 0x00, 0x00}, "too long"},
		{"huge length", []byte{0x04, 0x84, 0x7f, 0xff, 0xff, 0xff, 0x00}, "truncated"},
	} {
		err := checkBERDepth(tc.data, maxBERDepth)
		if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: checkBERDepth = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestParsePKCS7Limits(t *testing.T) {
	signer := validIdentity(t, "signer", nil)
	dirs := trustRoot(t, signer.cert)
	padded := nested(1, append(sign(t, testRootHash, signer, signOpts{}), make([]byte, maxSignatureSize)...))
	if _, _, err := verifySignature(testRootHash, padded, dirs); err == nil || !strings.Contains(err.Error(), "more than the 65536 allowed") {
		t.Errorf("oversized signature: %v", err)
	}
	// Deep nesting within the signature partition limit used to overflow
	// the stack of the recursive parser, which cannot be recovered from.
	deep := bytes.Repeat([]byte{0x30, 0x80}, maxSignatureSize/2)
	if _, _, err := verifySignature(testRootHash, deep, dirs); err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("deeply nested signature: %v", err)
	}
	blob := fmt.Appendf(nil, `{"rootHash":"%x","signature":"%s"}`, testRootHash,
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x30, 0x80}, 1_500_000)))
	vsig, err := parseVeritySig(blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifySignature(vsig.RootHash, vsig.Signature, dirs); err == nil {
		t.Error("deeply nested signature from a signature partition accepted")
	}
}

func TestVerifySignatureUserspaceSwitches(t *testing.T) {
	signer := validIdentity(t, "signer", nil)
	dirs := trustRoot(t, signer.cert)
	sig := sign(t, testRootHash, signer, signOpts{})

	t.Setenv("SYSTEMD_PROC_CMDLINE", "quiet")
	t.Setenv("SYSTEMD_ALLOW_USERSPACE_VERITY", "0")
	mustVerify(t, sig, dirs, false)
	t.Setenv("SYSTEMD_ALLOW_USERSPACE_VERITY", "bogus")
	mustVerify(t, sig, dirs, false)
	t.Setenv("SYSTEMD_ALLOW_USERSPACE_VERITY", "yes")
	mustVerify(t, sig, dirs, true)
	t.Setenv("SYSTEMD_PROC_CMDLINE", "systemd.allow_userspace_verity=1 systemd.allow-userspace-verity=no")
	mustVerify(t, sig, dirs, false)
	t.Setenv("SYSTEMD_PROC_CMDLINE", "systemd.allow_userspace_verity=0 systemd.allow_userspace_verity")
	mustVerify(t, sig, dirs, false)
	t.Setenv("SYSTEMD_PROC_CMDLINE", "systemd.allow_userspace_verity")
	mustVerify(t, sig, dirs, true)
}

func TestTrustAnchorFiles(t *testing.T) {
	root := t.TempDir()
	dirs := TrustDirs(root)
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("usr/lib/verity.d/a.crt", "x")
	write("usr/local/lib/verity.d/a.crt", "x")
	write("run/verity.d/b.crt", "x")
	write("usr/lib/verity.d/b.crt", "x")
	write("usr/lib/verity.d/c.crt", "x")
	write("etc/verity.d/c.crt", "")
	write("usr/lib/verity.d/d.crt", "x")
	if err := os.Symlink("/dev/null", filepath.Join(root, "run/verity.d/d.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc/verity.d/e.crt"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("usr/lib/verity.d/e.crt", "x")
	write("usr/lib/verity.d/f.pem", "x")
	write("etc/verity.d/.g.crt", "x")

	got := trustAnchorFiles(dirs)
	want := []string{
		filepath.Join(root, "usr/local/lib/verity.d/a.crt"),
		filepath.Join(root, "run/verity.d/b.crt"),
		filepath.Join(root, "usr/lib/verity.d/e.crt"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("trustAnchorFiles =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if d := TrustDirs("/"); d[0] != "/etc/verity.d" || d[3] != "/usr/lib/verity.d" || len(d) != 4 {
		t.Errorf("TrustDirs(/) = %v", d)
	}
}

func TestVerifySignatureIgnoresHiddenAnchors(t *testing.T) {
	signer := validIdentity(t, "signer", nil)
	root := t.TempDir()
	writeCertPEM(t, filepath.Join(root, "etc/verity.d/.revoked.crt"), signer.cert)
	writeCertPEM(t, filepath.Join(root, "etc/verity.d/vendor.crt~"), signer.cert)
	ok, reason, err := verifySignature(testRootHash, sign(t, testRootHash, signer, signOpts{}), TrustDirs(root))
	if ok || err != nil || !strings.Contains(reason, "no trusted certificates") {
		t.Errorf("signature by a hidden anchor: ok=%v reason=%q err=%v", ok, reason, err)
	}
}

func TestVerifySignatureAnchorPrecedence(t *testing.T) {
	signer := validIdentity(t, "signer", nil)
	other := validIdentity(t, "other", nil)
	sig := sign(t, testRootHash, signer, signOpts{})

	root := t.TempDir()
	writeCertPEM(t, filepath.Join(root, "usr/lib/verity.d/vendor.crt"), signer.cert)
	mustVerify(t, sig, TrustDirs(root), true)

	writeCertPEM(t, filepath.Join(root, "etc/verity.d/vendor.crt"), other.cert)
	mustVerify(t, sig, TrustDirs(root), false)

	if err := os.Remove(filepath.Join(root, "etc/verity.d/vendor.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(root, "etc/verity.d/vendor.crt")); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, sig, TrustDirs(root), false)
}

func TestParseVeritySig(t *testing.T) {
	sig := []byte{1, 2, 3}
	mk := func(rootHash, signature string) []byte {
		b, _ := json.Marshal(map[string]string{"rootHash": rootHash, "signature": signature})
		return b
	}
	valid := mk(hex.EncodeToString(testRootHash), base64.StdEncoding.EncodeToString(sig))
	padded := append(bytes.Clone(valid), make([]byte, 4096-len(valid))...)

	got, err := parseVeritySig(padded)
	if err != nil || !bytes.Equal(got.RootHash, testRootHash) || !bytes.Equal(got.Signature, sig) {
		t.Fatalf("padded blob: %+v, %v", got, err)
	}
	upper := mk(strings.ToUpper(hex.EncodeToString(testRootHash)), " AQID\n")
	if got, err := parseVeritySig(upper); err != nil || !bytes.Equal(got.RootHash, testRootHash) || !bytes.Equal(got.Signature, sig) {
		t.Errorf("uppercase hex / whitespace base64: %+v, %v", got, err)
	}

	for name, blob := range map[string][]byte{
		"embedded NUL":      append(append(bytes.Clone(valid), 0), 'x'),
		"missing rootHash":  []byte(`{"signature":"AQID"}`),
		"missing signature": []byte(`{"rootHash":"00"}`),
		"bad hex":           mk("zz", "AQID"),
		"bad base64":        mk("00", "!!!"),
		"not JSON":          []byte("not json"),
		"rootHash number":   []byte(`{"rootHash":1,"signature":"AQID"}`),
		"too large":         make([]byte, maxVeritySigSize+1),
	} {
		if _, err := parseVeritySig(blob); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

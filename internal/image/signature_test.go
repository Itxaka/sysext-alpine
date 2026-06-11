package image

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"
)

// testRootHash is a syntactically valid sha256 verity root hash.
const testRootHash = "2ee82d1b9b1e21ebe6943d808cee0b6dd51e0c8cd06a3c3e1f8120a08e42b7bf"

type testIdentity struct {
	key  *rsa.PrivateKey
	cert *x509.Certificate
}

// newIdentity generates an RSA key and a certificate with the given subject,
// self-signed when parent is nil, otherwise issued by parent.
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

func validIdentity(t *testing.T, cn string, isCA bool, parent *testIdentity) *testIdentity {
	return newIdentity(t, cn, isCA,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour), parent)
}

// signRootHash produces a detached PKCS#7 signature (DER) over the ASCII
// hex string, embedding the signer certificate plus any extra certs.
func signRootHash(t *testing.T, rootHash string, id *testIdentity, extra ...*x509.Certificate) []byte {
	t.Helper()
	sd, err := pkcs7.NewSignedData([]byte(rootHash))
	if err != nil {
		t.Fatal(err)
	}
	sd.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	if err := sd.AddSigner(id.cert, id.key, pkcs7.SignerInfoConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range extra {
		sd.AddCertificate(c)
	}
	sd.Detach()
	der, err := sd.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// trustDirWith writes the given certificates as PEM files into a fresh
// directory and returns its path.
func trustDirWith(t *testing.T, certs ...*x509.Certificate) string {
	t.Helper()
	dir := t.TempDir()
	for i, c := range certs {
		writeCertPEM(t, filepath.Join(dir, "anchor"+string(rune('a'+i))+".crt"), c)
	}
	return dir
}

func writeCertPEM(t *testing.T, path string, certs ...*x509.Certificate) {
	t.Helper()
	var buf []byte
	for _, c := range certs {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sigFor(t *testing.T, rootHash string, id *testIdentity, extra ...*x509.Certificate) *veritySig {
	t.Helper()
	return &veritySig{
		RootHash:  rootHash,
		Signature: base64.StdEncoding.EncodeToString(signRootHash(t, rootHash, id, extra...)),
	}
}

func TestVerifySignatureValid(t *testing.T) {
	id := validIdentity(t, "sysext-test", false, nil)
	trust := trustDirWith(t, id.cert)

	if err := verifySignature(sigFor(t, testRootHash, id), testRootHash, trust); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifySignatureRootHashMismatch(t *testing.T) {
	id := validIdentity(t, "sysext-test", false, nil)
	trust := trustDirWith(t, id.cert)

	other := strings.Repeat("ab", 32)
	err := verifySignature(sigFor(t, testRootHash, id), other, trust)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want rootHash mismatch", err)
	}
}

func TestVerifySignatureSignedWrongHash(t *testing.T) {
	// Signature is over a different string than the rootHash field claims.
	id := validIdentity(t, "sysext-test", false, nil)
	trust := trustDirWith(t, id.cert)

	sig := &veritySig{
		RootHash:  testRootHash,
		Signature: base64.StdEncoding.EncodeToString(signRootHash(t, strings.Repeat("cd", 32), id)),
	}
	err := verifySignature(sig, testRootHash, trust)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("err = %v, want signature verification failure", err)
	}
}

func TestVerifySignatureUntrustedSigner(t *testing.T) {
	signer := validIdentity(t, "sysext-evil", false, nil)
	other := validIdentity(t, "sysext-good", false, nil)
	trust := trustDirWith(t, other.cert)

	err := verifySignature(sigFor(t, testRootHash, signer), testRootHash, trust)
	if err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("err = %v, want untrusted signer", err)
	}
}

func TestVerifySignatureChainsToAnchor(t *testing.T) {
	ca := validIdentity(t, "sysext-ca", true, nil)
	leaf := validIdentity(t, "sysext-leaf", false, ca)
	trust := trustDirWith(t, ca.cert) // only the CA is trusted

	if err := verifySignature(sigFor(t, testRootHash, leaf, ca.cert), testRootHash, trust); err != nil {
		t.Fatalf("chained signer rejected: %v", err)
	}
}

func TestVerifySignatureCorruptBase64(t *testing.T) {
	id := validIdentity(t, "sysext-test", false, nil)
	trust := trustDirWith(t, id.cert)

	sig := sigFor(t, testRootHash, id)
	sig.Signature = "!!!not-base64!!!"
	err := verifySignature(sig, testRootHash, trust)
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("err = %v, want base64 decode failure", err)
	}
}

func TestVerifySignatureCorruptDER(t *testing.T) {
	id := validIdentity(t, "sysext-test", false, nil)
	trust := trustDirWith(t, id.cert)

	sig := sigFor(t, testRootHash, id)
	sig.Signature = base64.StdEncoding.EncodeToString([]byte("garbage, not PKCS#7"))
	if err := verifySignature(sig, testRootHash, trust); err == nil {
		t.Fatal("corrupt DER accepted")
	}
}

func TestVerifySignatureFingerprint(t *testing.T) {
	id := validIdentity(t, "sysext-test", false, nil)
	trust := trustDirWith(t, id.cert)

	fp := sha256.Sum256(id.cert.Raw)
	sig := sigFor(t, testRootHash, id)
	sig.CertificateFingerprint = hex.EncodeToString(fp[:])
	if err := verifySignature(sig, testRootHash, trust); err != nil {
		t.Fatalf("matching fingerprint rejected: %v", err)
	}

	sig.CertificateFingerprint = strings.Repeat("00", 32)
	err := verifySignature(sig, testRootHash, trust)
	if err == nil || !strings.Contains(err.Error(), "certificateFingerprint") {
		t.Fatalf("err = %v, want fingerprint mismatch", err)
	}
}

func TestVerifySignatureExpiredSigner(t *testing.T) {
	expired := newIdentity(t, "sysext-expired", false,
		time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour), nil)
	trust := trustDirWith(t, expired.cert)

	// Rejected either by the pkcs7 signing-time check (signed attributes
	// present) or by our explicit validity-period check (no attributes);
	// both mention the certificate validity.
	err := verifySignature(sigFor(t, testRootHash, expired), testRootHash, trust)
	if err == nil || !strings.Contains(err.Error(), "validity") {
		t.Fatalf("err = %v, want expired certificate rejection", err)
	}

	// Without authenticated attributes (the -noattr case) our explicit
	// expiry check must catch it.
	sd, err := pkcs7.NewSignedData([]byte(testRootHash))
	if err != nil {
		t.Fatal(err)
	}
	sd.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	if err := sd.SignWithoutAttr(expired.cert, expired.key, pkcs7.SignerInfoConfig{}); err != nil {
		t.Fatal(err)
	}
	sd.Detach()
	der, err := sd.Finish()
	if err != nil {
		t.Fatal(err)
	}
	sig := &veritySig{RootHash: testRootHash, Signature: base64.StdEncoding.EncodeToString(der)}
	err = verifySignature(sig, testRootHash, trust)
	if err == nil || !strings.Contains(err.Error(), "validity period") {
		t.Fatalf("err = %v, want explicit validity-period rejection", err)
	}
}

func TestVerifySignatureMultipleAnchors(t *testing.T) {
	id := validIdentity(t, "sysext-test", false, nil)
	otherA := validIdentity(t, "other-a", false, nil)
	otherB := validIdentity(t, "other-b", false, nil)
	trust := trustDirWith(t, otherA.cert, id.cert, otherB.cert)

	if err := verifySignature(sigFor(t, testRootHash, id), testRootHash, trust); err != nil {
		t.Fatalf("signer among multiple anchors rejected: %v", err)
	}

	// Multiple PEM blocks inside one .crt file must also work.
	dir := t.TempDir()
	writeCertPEM(t, filepath.Join(dir, "bundle.crt"), otherA.cert, id.cert)
	if err := verifySignature(sigFor(t, testRootHash, id), testRootHash, dir); err != nil {
		t.Fatalf("signer in multi-block bundle rejected: %v", err)
	}
}

func TestVerifySignatureNoTrustAnchors(t *testing.T) {
	id := validIdentity(t, "sysext-test", false, nil)

	for _, dir := range []string{
		filepath.Join(t.TempDir(), "does-not-exist"), // missing dir
		t.TempDir(), // empty dir
	} {
		err := verifySignature(sigFor(t, testRootHash, id), testRootHash, dir)
		if err == nil || !strings.Contains(err.Error(), "no trust anchors") {
			t.Fatalf("trust dir %s: err = %v, want no-trust-anchors", dir, err)
		}
	}
}

func TestParseVeritySigPadding(t *testing.T) {
	raw, err := json.Marshal(&veritySig{RootHash: testRootHash, Signature: "c2ln"})
	if err != nil {
		t.Fatal(err)
	}
	// NUL-pad to a multiple of 4096 like the spec mandates.
	padded := make([]byte, (len(raw)+4095)&^4095)
	copy(padded, raw)
	if len(padded)%4096 != 0 {
		t.Fatalf("test bug: padded length %d not a 4096 multiple", len(padded))
	}

	sig, err := parseVeritySig(padded)
	if err != nil {
		t.Fatalf("padded signature blob rejected: %v", err)
	}
	if sig.RootHash != testRootHash || sig.Signature != "c2ln" {
		t.Fatalf("unexpected parse result: %+v", sig)
	}
}

func TestParseVeritySigMissingFields(t *testing.T) {
	for _, blob := range []string{
		`{"signature":"c2ln"}`,
		`{"rootHash":"` + testRootHash + `"}`,
		`not json at all`,
	} {
		if _, err := parseVeritySig([]byte(blob)); err == nil {
			t.Errorf("blob %q accepted, want error", blob)
		}
	}
}

func TestReadVeritySigBlobFromPaddedFile(t *testing.T) {
	// readVeritySigBlob + parseVeritySig over a synthetic NUL-padded
	// "partition" stored in a regular file.
	raw, err := json.Marshal(&veritySig{RootHash: testRootHash, Signature: "c2ln"})
	if err != nil {
		t.Fatal(err)
	}
	padded := make([]byte, 8192)
	copy(padded, raw)

	path := filepath.Join(t.TempDir(), "sigpart")
	if err := os.WriteFile(path, padded, 0o644); err != nil {
		t.Fatal(err)
	}

	blob, err := readVeritySigBlob(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) != 8192 {
		t.Fatalf("blob length %d, want 8192", len(blob))
	}
	sig, err := parseVeritySig(blob)
	if err != nil {
		t.Fatal(err)
	}
	if sig.RootHash != testRootHash {
		t.Fatalf("rootHash = %q, want %q", sig.RootHash, testRootHash)
	}
}

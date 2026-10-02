package image

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// FuzzPartitionTable feeds untrusted image headers through sector size
// probing, partition table parsing and dissection.
func FuzzPartitionTable(f *testing.F) {
	x := archPartitionTypes[len(archPartitionTypes)-1].types
	f.Add(buildGPT(f, 512, testPart{typ: x[0]}, testPart{typ: x[1]}, testPart{typ: x[2]}))
	f.Add(buildGPT(f, 4096, testPart{typ: x[3], label: "usr_1"}, testPart{typ: gptTypeLinuxGeneric}))
	mbr := make([]byte, 1024)
	mbr[510], mbr[511] = 0x55, 0xaa
	mbr[446], mbr[450], mbr[458] = 0x80, 0x83, 8
	f.Add(mbr)
	pol := classDefaultPolicy(release.Sysext)
	f.Fuzz(func(t *testing.T, img []byte) {
		r := bytes.NewReader(img)
		ss, err := probeSectorSize(r)
		if err != nil {
			return
		}
		pt, err := readPartitionTable(r, int64(len(img)), pick(ss != 0, ss, 512))
		if err != nil {
			return
		}
		ds := &dissector{
			table:     pt,
			policy:    pol,
			verity:    &veritySettings{designator: partInvalid},
			native:    "x86-64",
			readSig:   func(p partition) (*veritySig, error) { return readVeritySig(r, p, pt.sectorSize) },
			machineID: func() ([16]byte, error) { return [16]byte{1}, nil },
		}
		d, err := ds.dissect()
		if err != nil {
			return
		}
		vs := ds.verity
		_ = d.loadSigPartitionRootHash(vs, r, pt.sectorSize)
		_ = d.guessRootHash(vs)
	})
}

func FuzzParseVeritySuperblock(f *testing.F) {
	f.Add(mkVeritySB(nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		sb, err := parseVeritySuperblock(b)
		if err != nil {
			return
		}
		_ = verityParams(sb, "7:1", "7:2", make([]byte, verityDigestSizes[sb.Algorithm]), "")
	})
}

func FuzzParseVeritySig(f *testing.F) {
	j, _ := json.Marshal(map[string]string{"rootHash": "abcd", "signature": "AQID"})
	f.Add(j)
	f.Add(append(j, make([]byte, 100)...))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parseVeritySig(b) })
}

// FuzzVerifySignature feeds untrusted DER to the PKCS#7 parser with a trust
// anchor installed, so parsing is always reached.
func FuzzVerifySignature(f *testing.F) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fuzz"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		f.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		f.Fatal(err)
	}
	root := f.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/verity.d"), 0o755); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/verity.d/fuzz.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		f.Fatal(err)
	}
	dirs := TrustDirs(root)

	rootHash := bytes.Repeat([]byte{0xab}, 32)
	sd, err := pkcs7.NewSignedData([]byte(hex.EncodeToString(rootHash)))
	if err != nil {
		f.Fatal(err)
	}
	if err := sd.AddSigner(cert, key, pkcs7.SignerInfoConfig{}); err != nil {
		f.Fatal(err)
	}
	sd.Detach()
	sig, err := sd.Finish()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sig)
	f.Add([]byte{0x30, 0x81})
	f.Add([]byte{0x30, 0x84, 0x01})
	f.Add([]byte{0x1f, 0x80})
	f.Add(bytes.Repeat([]byte{0x30, 0x80}, 40))
	f.Add(bytes.Repeat([]byte{0x30, 0x80, 0x00, 0x00}, 40))
	f.Fuzz(func(t *testing.T, der []byte) {
		_, _, _ = verifySignature(rootHash, der, dirs)
	})
}

func FuzzParseImagePolicy(f *testing.F) {
	for _, s := range []string{"root=verity+signed:usr=unprotected+absent", "=open", "*", "-", "~", "root=read-only-on+erofs:=ignore"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		pol, err := parseImagePolicy(s)
		if err != nil {
			return
		}
		for d := range numDesignators {
			_, _ = pol.mayUse(d)
			_ = pol.exhaustive(d).String()
		}
	})
}

func FuzzDetectFS(f *testing.F) {
	b := make([]byte, 4096)
	copy(b, "hsqs")
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = detectFS(bytes.NewReader(b)) })
}

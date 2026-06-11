package image

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/smallstep/pkcs7"
)

// PKCS#7 verity root-hash signature verification, per the UAPI Discoverable
// Partitions Specification: the verity-signature partition carries a JSON
// object, NUL-padded to a multiple of 4096 bytes, with fields
//
//	"rootHash"               — the verity root hash, lowercase hex (mandatory)
//	"signature"              — base64 DER PKCS#7 signature over the exact
//	                           ASCII hex string stored in rootHash (mandatory)
//	"certificateFingerprint" — SHA256 of the signer certificate in DER form,
//	                           lowercase hex without colons (optional)
//
// Trust model (mirrors systemd): the signature must validate against X.509
// certificates installed as PEM files in <trustDir>/*.crt (default
// /etc/verity.d). The signature is accepted when the PKCS#7 structure
// verifies AND the signer certificate either is byte-identical to one of
// the trusted certificates or chains to one of them (x509.Verify with the
// trusted certificates as roots, any extended key usage). Certificate
// validity periods are honored: an expired or not-yet-valid signer
// certificate is rejected, even when byte-identical to a trust anchor.

// defaultTrustDir is where trusted verity signing certificates live when
// MountOpts.TrustDir is empty.
const defaultTrustDir = "/etc/verity.d"

// maxVeritySigSize bounds how much of a verity-signature partition is read;
// real signature blobs are a few KiB.
const maxVeritySigSize = 4 << 20

// veritySig is the JSON object stored in a verity-signature partition.
type veritySig struct {
	RootHash               string `json:"rootHash"`
	Signature              string `json:"signature"`
	CertificateFingerprint string `json:"certificateFingerprint"`
}

// parseVeritySig trims the trailing NUL padding from the partition content
// and unmarshals the signature JSON, checking the mandatory fields.
func parseVeritySig(blob []byte) (*veritySig, error) {
	blob = bytes.TrimRight(blob, "\x00")
	var sig veritySig
	if err := json.Unmarshal(blob, &sig); err != nil {
		return nil, fmt.Errorf("parsing verity signature JSON: %w", err)
	}
	if sig.RootHash == "" {
		return nil, errors.New("verity signature JSON: missing mandatory rootHash field")
	}
	if sig.Signature == "" {
		return nil, errors.New("verity signature JSON: missing mandatory signature field")
	}
	return &sig, nil
}

// readVeritySigBlob reads the raw (NUL-padded) signature blob from the
// verity-signature partition device.
func readVeritySigBlob(dev string) ([]byte, error) {
	f, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	blob, err := io.ReadAll(io.LimitReader(f, maxVeritySigSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading verity signature partition %s: %w", dev, err)
	}
	if len(blob) > maxVeritySigSize {
		return nil, fmt.Errorf("verity signature partition %s larger than %d bytes", dev, maxVeritySigSize)
	}
	return blob, nil
}

// loadTrustAnchors loads all PEM certificates from <trustDir>/*.crt (a file
// may contain multiple CERTIFICATE blocks). A missing or empty directory is
// an error: signature verification without trust anchors is meaningless.
func loadTrustAnchors(trustDir string) ([]*x509.Certificate, error) {
	files, err := filepath.Glob(filepath.Join(trustDir, "*.crt"))
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading trust anchor %s: %w", path, err)
		}
		for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
			if block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parsing certificate in %s: %w", path, err)
			}
			certs = append(certs, cert)
		}
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no trust anchors: no usable certificates in %s (install PEM certificates as *.crt)", trustDir)
	}
	return certs, nil
}

// verifySignature checks a parsed verity-signature object against the
// expected (GUID-reconstructed) root hash and the trust anchors in trustDir
// (defaultTrustDir when empty). See the file comment for the trust model.
func verifySignature(sig *veritySig, expectedRootHash, trustDir string) error {
	if trustDir == "" {
		trustDir = defaultTrustDir
	}

	if !strings.EqualFold(sig.RootHash, expectedRootHash) {
		return fmt.Errorf("verity signature rootHash %q does not match root hash %q from partition GUIDs",
			sig.RootHash, expectedRootHash)
	}

	der, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return fmt.Errorf("decoding verity signature base64: %w", err)
	}
	p7, err := pkcs7.Parse(der)
	if err != nil {
		return fmt.Errorf("parsing PKCS#7 signature: %w", err)
	}

	// The signed data is the exact ASCII hex string stored in rootHash.
	// Force it as the content even for non-detached signatures so the
	// signature is always bound to the root hash, never to attacker-chosen
	// embedded content.
	p7.Content = []byte(sig.RootHash)

	trusted, err := loadTrustAnchors(trustDir)
	if err != nil {
		return err
	}

	if err := p7.Verify(); err != nil {
		return fmt.Errorf("PKCS#7 signature verification failed: %w", err)
	}

	signer := p7.GetOnlySigner()
	if signer == nil {
		return errors.New("PKCS#7 signature must contain exactly one signer")
	}

	if sig.CertificateFingerprint != "" {
		fp := sha256.Sum256(signer.Raw)
		want := strings.ToLower(strings.ReplaceAll(sig.CertificateFingerprint, ":", ""))
		if hex.EncodeToString(fp[:]) != want {
			return fmt.Errorf("certificateFingerprint %q does not match signer certificate (sha256 %s)",
				sig.CertificateFingerprint, hex.EncodeToString(fp[:]))
		}
	}

	// Honor the certificate validity period (on both trust paths).
	now := time.Now()
	if now.Before(signer.NotBefore) || now.After(signer.NotAfter) {
		return fmt.Errorf("signer certificate outside its validity period (notBefore=%s notAfter=%s)",
			signer.NotBefore.Format(time.RFC3339), signer.NotAfter.Format(time.RFC3339))
	}

	// Trust path 1: signer is byte-identical to an installed anchor.
	for _, anchor := range trusted {
		if bytes.Equal(anchor.Raw, signer.Raw) {
			return nil
		}
	}

	// Trust path 2: signer chains to an installed anchor. Other
	// certificates carried in the PKCS#7 structure may serve as
	// intermediates. No hostname or EKU restrictions beyond signing
	// validity.
	roots := x509.NewCertPool()
	for _, anchor := range trusted {
		roots.AddCert(anchor)
	}
	intermediates := x509.NewCertPool()
	for _, cert := range p7.Certificates {
		if !bytes.Equal(cert.Raw, signer.Raw) {
			intermediates.AddCert(cert)
		}
	}
	if _, err := signer.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return fmt.Errorf("signer certificate not trusted by %s: %w", trustDir, err)
	}
	return nil
}

// verifyImageSignature locates the verity-signature partition (by type
// GUID), reads and parses its JSON payload and verifies the PKCS#7
// signature against the root hash reconstructed from the data/verity
// partition unique GUIDs.
func verifyImageSignature(loopDev string, parts []gptPartition, sigType string, data, verity gptPartition, trustDir string) error {
	sigPart := findByType(parts, sigType)
	if sigPart == nil {
		return errors.New("no verity signature partition")
	}
	expected, err := rootHashFromGUIDs(data.UniqueGUID, verity.UniqueGUID)
	if err != nil {
		return err
	}
	sigDev, err := ensurePartitionNode(loopDev, sigPart.Index)
	if err != nil {
		return err
	}
	blob, err := readVeritySigBlob(sigDev)
	if err != nil {
		return err
	}
	sig, err := parseVeritySig(blob)
	if err != nil {
		return err
	}
	return verifySignature(sig, expected, trustDir)
}

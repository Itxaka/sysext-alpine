package image

import (
	"bytes"
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
	"slices"
	"strings"
	"time"

	"github.com/smallstep/pkcs7"
	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// Root hash signatures follow the UAPI Discoverable Partitions
// Specification: the verity signature partition holds a JSON object,
// NUL-padded, whose "rootHash" is the hex root hash and whose "signature"
// is a base64 DER PKCS#7 signature over the lowercase hex root hash.
//
// Signatures are verified like systemd v262 validate_signature_userspace():
// the trusted certificates are *.crt files from the verity.d directories
// (first certificate per file, unreadable or unparsable files skipped), and
// the signer must be one of them (PKCS7_NOINTERN|PKCS7_NOVERIFY: certificates
// embedded in the signature are ignored, no chain building, no validity
// period checks).

// maxVeritySigSize bounds the signature partition, like systemd, and the
// .roothash.p7s/.usrhash.p7s files.
const maxVeritySigSize = 4 << 20

// maxSignatureSize bounds the PKCS#7 signature parsed in userspace. A root
// hash signature holds the signer's certificate and one signature, a few
// KiB; the kernel's limit for the add_key() payload that hands it to
// dm-verity is 32767 bytes, so nothing larger is a signature the kernel
// could check either.
const maxSignatureSize = 64 << 10

// maxBERDepth bounds the nesting of the signature's ASN.1 structure. Root
// hash signatures nest about a dozen levels; OpenSSL, which systemd parses
// them with, refuses more than 30.
const maxBERDepth = 32

// veritySig is the decoded content of a verity signature partition.
type veritySig struct {
	RootHash  []byte
	Signature []byte
}

// parseVeritySig decodes a signature partition (systemd's
// acquire_sig_for_roothash()).
func parseVeritySig(blob []byte) (*veritySig, error) {
	if len(blob) > maxVeritySigSize {
		return nil, fmt.Errorf("verity signature partition larger than %d bytes", maxVeritySigSize)
	}
	if i := bytes.IndexByte(blob, 0); i >= 0 {
		if !isZero(blob[i:]) {
			return nil, errors.New("verity signature data contains an embedded NUL byte")
		}
		blob = blob[:i]
	}
	var raw struct {
		RootHash  *string `json:"rootHash"`
		Signature *string `json:"signature"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, fmt.Errorf("parsing verity signature JSON: %w", err)
	}
	if raw.RootHash == nil {
		return nil, errors.New("verity signature JSON lacks the rootHash field")
	}
	if raw.Signature == nil {
		return nil, errors.New("verity signature JSON lacks the signature field")
	}
	rootHash, err := hex.DecodeString(*raw.RootHash)
	if err != nil {
		return nil, fmt.Errorf("verity signature JSON: invalid rootHash: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(stripSpace(*raw.Signature))
	if err != nil {
		return nil, fmt.Errorf("verity signature JSON: invalid signature: %w", err)
	}
	return &veritySig{RootHash: rootHash, Signature: sig}, nil
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

// readVeritySig reads and decodes the signature partition p of the image.
func readVeritySig(r io.ReaderAt, p partition, ss int64) (*veritySig, error) {
	size := p.size(ss)
	if size > maxVeritySigSize {
		return nil, fmt.Errorf("verity signature partition %d larger than %d bytes", p.Index, maxVeritySigSize)
	}
	blob := make([]byte, size)
	if _, err := r.ReadAt(blob, p.offset(ss)); err != nil {
		return nil, fmt.Errorf("reading verity signature partition %d: %w", p.Index, err)
	}
	return parseVeritySig(blob)
}

// TrustDirs returns the directories searched for trusted dm-verity signing
// certificates (*.crt) below root, in priority order: systemd's
// CONF_PATHS("verity.d").
func TrustDirs(root string) []string {
	dirs := []string{"/etc/verity.d", "/run/verity.d", "/usr/local/lib/verity.d", "/usr/lib/verity.d"}
	for i, d := range dirs {
		dirs[i] = filepath.Join("/", root, d)
	}
	return dirs
}

// trustAnchorFiles lists the *.crt files of dirs with the selection rules of
// systemd's conf_files_list_nulstr(CONF_FILES_REGULAR|CONF_FILES_FILTER_MASKED):
// hidden and backup files are skipped, a file name found in an earlier
// directory overrides later ones, an empty file or a symlink to /dev/null
// masks the name, non-regular files are skipped. The result is sorted by
// file name. Unlike there, symlinks are resolved as seen from the host, not
// confined to a root.
func trustAnchorFiles(dirs []string) []string {
	chosen := map[string]string{}
	masked := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if release.HiddenOrBackupFile(name) || !strings.HasSuffix(name, ".crt") || chosen[name] != "" || masked[name] {
				continue
			}
			p := filepath.Join(dir, name)
			if isMask(p) {
				masked[name] = true
				continue
			}
			if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			chosen[name] = p
		}
	}
	names := make([]string, 0, len(chosen))
	for n := range chosen {
		names = append(names, n)
	}
	slices.Sort(names)
	files := make([]string, len(names))
	for i, n := range names {
		files[i] = chosen[n]
	}
	return files
}

// isMask reports whether p masks a configuration file: a symlink to
// /dev/null (also when /dev/null does not exist), the null device itself,
// or an empty regular file.
func isMask(p string) bool {
	if target, err := filepath.EvalSymlinks(p); err == nil && target == "/dev/null" {
		return true
	} else if err != nil {
		if t, lerr := os.Readlink(p); lerr == nil && t == "/dev/null" {
			return true
		}
	}
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		return false
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFCHR:
		return st.Rdev == unix.Mkdev(1, 3)
	case unix.S_IFREG:
		return st.Size == 0
	}
	return false
}

// loadTrustAnchors returns the first certificate of each file; files that
// cannot be read or parsed are skipped, like PEM_read_X509() failures in
// systemd.
func loadTrustAnchors(files []string) []*x509.Certificate {
	var certs []*x509.Certificate
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
			if block.Type != "CERTIFICATE" && block.Type != "X509 CERTIFICATE" {
				continue
			}
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				certs = append(certs, cert)
			}
			break
		}
	}
	return certs
}

// userspaceVerityAllowed mirrors the $SYSTEMD_ALLOW_USERSPACE_VERITY and
// systemd.allow_userspace_verity= switches.
func userspaceVerityAllowed() (bool, string) {
	if !envStrictlyEnabled("SYSTEMD_ALLOW_USERSPACE_VERITY") {
		return false, "userspace signature verification disabled via $SYSTEMD_ALLOW_USERSPACE_VERITY"
	}
	if !cmdlineBool("systemd.allow_userspace_verity") {
		return false, "userspace signature verification disabled via systemd.allow_userspace_verity="
	}
	return true, ""
}

// verifySignature checks a PKCS#7 signature over the lowercase hex root
// hash against the certificates in dirs. ok=false (with a reason) means the
// signature could not be validated; an error means the signature data is
// malformed.
func verifySignature(rootHash, sig []byte, dirs []string) (ok bool, reason string, err error) {
	if allowed, why := userspaceVerityAllowed(); !allowed {
		return false, why, nil
	}
	files := trustAnchorFiles(dirs)
	if len(files) == 0 {
		return false, "no trusted certificates found in " + strings.Join(dirs, ", "), nil
	}

	p7, err := parsePKCS7(sig)
	if err != nil {
		return false, "", err
	}
	p7.Content = []byte(hex.EncodeToString(rootHash))
	p7.Certificates = signerCandidates(loadTrustAnchors(files))
	if err := verifyPKCS7(p7); err != nil {
		return false, err.Error(), nil
	}
	return true, "", nil
}

// signerCandidates makes the trust anchors the only certificates the signer
// is looked up in, with the validity period widened: OpenSSL's NOVERIFY
// performs no time checks, while the pkcs7 package compares a signingTime
// attribute against the signer's validity.
func signerCandidates(anchors []*x509.Certificate) []*x509.Certificate {
	out := make([]*x509.Certificate, len(anchors))
	for i, a := range anchors {
		c := *a
		c.NotBefore = time.Time{}
		c.NotAfter = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		out[i] = &c
	}
	return out
}

// parsePKCS7 parses an untrusted DER (or BER) signature. The size and
// nesting depth are checked first: the parser recurses once per level.
func parsePKCS7(der []byte) (p7 *pkcs7.PKCS7, err error) {
	if len(der) > maxSignatureSize {
		return nil, fmt.Errorf("parsing PKCS#7 signature: %d bytes, more than the %d allowed", len(der), maxSignatureSize)
	}
	if err := checkBERDepth(der, maxBERDepth); err != nil {
		return nil, fmt.Errorf("parsing PKCS#7 signature: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			p7, err = nil, fmt.Errorf("parsing PKCS#7 signature: malformed data (%v)", r)
		}
	}()
	p7, err = pkcs7.Parse(der)
	if err != nil {
		return nil, fmt.Errorf("parsing PKCS#7 signature: %w", err)
	}
	return p7, nil
}

func verifyPKCS7(p7 *pkcs7.PKCS7) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PKCS#7 verification failed: malformed data (%v)", r)
		}
	}()
	if err := p7.Verify(); err != nil {
		return fmt.Errorf("PKCS#7 verification failed: %w", err)
	}
	return nil
}

// checkBERDepth walks the first BER object of data the way the pkcs7
// package's BER to DER conversion does, without recursion, and fails when
// constructed objects nest deeper than maxDepth or the encoding is
// malformed.
func checkBERDepth(data []byte, maxDepth int) error {
	type frame struct {
		end        int
		indefinite bool
	}
	var stack []frame
	offset := 0
	for {
		constructed, indefinite, contentStart, contentEnd, err := berHeader(data, offset)
		if err != nil {
			return err
		}
		if !constructed {
			offset = contentEnd
		} else {
			if len(stack) == maxDepth {
				return fmt.Errorf("ASN.1 structure nested deeper than %d levels", maxDepth)
			}
			stack = append(stack, frame{end: contentEnd, indefinite: indefinite})
			offset = contentStart
			if indefinite {
				// The first child is read before looking for the
				// end-of-contents octets.
				continue
			}
		}
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.indefinite {
				if len(data)-offset < 2 {
					return errors.New("unterminated indefinite length ASN.1 object")
				}
				if data[offset] != 0 || data[offset+1] != 0 {
					break
				}
				offset += 2
			} else {
				if offset < top.end {
					break
				}
				offset = top.end
			}
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			return nil
		}
	}
}

// berHeader decodes the identifier and length octets at offset with the
// rules of the pkcs7 package's readObject(). For indefinite lengths
// contentEnd is the content start.
func berHeader(data []byte, offset int) (constructed, indefinite bool, contentStart, contentEnd int, err error) {
	truncated := errors.New("truncated ASN.1 object")
	if offset+1 >= len(data) {
		return false, false, 0, 0, truncated
	}
	b := data[offset]
	offset++
	if b&0x1f == 0x1f {
		for data[offset] >= 0x80 {
			if offset++; offset >= len(data) {
				return false, false, 0, 0, truncated
			}
		}
		if offset++; offset >= len(data) {
			return false, false, 0, 0, truncated
		}
	}
	constructed = b&0x20 != 0
	l := data[offset]
	offset++
	length := 0
	switch {
	case l == 0x80:
		if !constructed {
			return false, false, 0, 0, errors.New("indefinite length ASN.1 object with primitive encoding")
		}
		indefinite = true
	case l > 0x80:
		n := int(l & 0x7f)
		switch {
		case n > 4 || offset+n > len(data):
			return false, false, 0, 0, errors.New("ASN.1 length too long")
		case data[offset] == 0 || (n == 4 && data[offset] > 0x7f):
			return false, false, 0, 0, errors.New("malformed ASN.1 length")
		}
		for range n {
			length = length<<8 | int(data[offset])
			offset++
		}
	default:
		length = int(l)
	}
	if length > len(data)-offset {
		return false, false, 0, 0, truncated
	}
	return constructed, indefinite, offset, offset + length, nil
}

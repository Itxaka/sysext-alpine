package image

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// veritySettings is systemd's VeritySettings: what is known about the
// verity protection of an image before and after dissection.
type veritySettings struct {
	rootHash []byte
	sig      []byte
	// dataPath is an external hash tree file (<image>.verity); its presence
	// makes the image a single unpartitioned filesystem.
	dataPath string
	// designator is the data partition the settings apply to (root or
	// usr), partInvalid while unknown.
	designator designator
}

// covers mirrors verity_settings_data_covers(): enough information to set
// up verity for d from an external hash tree file.
func (v *veritySettings) covers(d designator) bool {
	return (v.designator == d || (d == partRoot && v.designator == partInvalid)) &&
		len(v.rootHash) > 0 && v.dataPath != ""
}

// auxiliaryPath mirrors build_auxiliary_path(): a trailing ".raw" is
// replaced by the suffix.
func auxiliaryPath(image, suffix string) string {
	return strings.TrimSuffix(image, ".raw") + suffix
}

// loadVeritySidecars mirrors systemd's verity_settings_load() for an image
// file: the root hash from the user.verity.roothash/usrhash xattr or the
// .roothash/.usrhash file, the signature from .roothash.p7s/.usrhash.p7s
// and the hash tree from .verity, all next to the image. Device nodes in
// /dev or /sys have none.
func loadVeritySidecars(image string) (*veritySettings, error) {
	vs := &veritySettings{designator: partInvalid}
	if isDevicePath(image) || envDisabled("SYSTEMD_DISSECT_VERITY_SIDECAR") {
		return vs, nil
	}

	text, err := readRootHashSidecar(image, "user.verity.roothash", ".roothash")
	if err != nil {
		return nil, err
	}
	if text != "" {
		vs.designator = partRoot
	} else {
		if text, err = readRootHashSidecar(image, "user.verity.usrhash", ".usrhash"); err != nil {
			return nil, err
		}
		if text != "" {
			vs.designator = partUsr
		}
	}
	if text != "" {
		if vs.rootHash, err = hex.DecodeString(text); err != nil {
			return nil, fmt.Errorf("%s: invalid verity root hash: %w", image, err)
		}
		if len(vs.rootHash) < 16 {
			return nil, fmt.Errorf("%s: verity root hash too short", image)
		}
	}

	if len(vs.rootHash) > 0 {
		for _, c := range []struct {
			d      designator
			suffix string
		}{{partRoot, ".roothash.p7s"}, {partUsr, ".usrhash.p7s"}} {
			if vs.designator != c.d {
				continue
			}
			p := auxiliaryPath(image, c.suffix)
			sig, err := readFileMax(p, maxVeritySigSize)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if len(sig) == 0 {
				return nil, fmt.Errorf("%s: empty verity signature file", p)
			}
			vs.sig = sig
		}
	}

	data := auxiliaryPath(image, ".verity")
	if _, err := os.Stat(data); err == nil {
		vs.dataPath = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return vs, nil
}

// isDevicePath is systemd's is_device_path() for a clean path: something
// below /dev or /sys.
func isDevicePath(p string) bool {
	for _, prefix := range []string{"/dev/", "/sys/"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok && strings.Trim(rest, "/") != "" {
			return true
		}
	}
	return false
}

// readRootHashSidecar returns the root hash text from the xattr, falling
// back to the sidecar file; "" when neither exists.
func readRootHashSidecar(image, xattr, suffix string) (string, error) {
	buf := make([]byte, 256)
	for {
		n, err := unix.Getxattr(image, xattr, buf)
		if err == nil {
			return strings.TrimSpace(strings.TrimRight(string(buf[:n]), "\x00")), nil
		}
		if errors.Is(err, unix.ERANGE) && len(buf) < 1<<16 {
			buf = make([]byte, len(buf)*4)
			continue
		}
		if !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.ENOSYS) {
			return "", fmt.Errorf("reading %s of %s: %w", xattr, image, err)
		}
		break
	}
	data, err := os.ReadFile(auxiliaryPath(image, suffix))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line), nil
}

// readFileMax reads a file of at most limit bytes.
func readFileMax(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, limit)
	}
	return data, nil
}

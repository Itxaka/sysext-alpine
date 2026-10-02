package overlay

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
)

// The origin marker identifies what a merge was built from, so that refresh
// can skip work when nothing changed. Its content is the JSON object
// systemd-sysext builds in merge_subprocess():
//
//	{"mutable": {"mode": M, "mutableDirs": {H: DIR}},
//	 "mountOptions": S,
//	 "extensions": {NAME: {"path": P, "verityHash": HEX}
//	              | {"path": P, "onMountId": N, "fileHandle": {"type": T, "handle": HEX}
//	                 | "inode": N, "crtime": USEC, "mtime": USEC}}}
//
// written pretty-printed like sd_json_variant_format(SD_JSON_FORMAT_PRETTY|
// SD_JSON_FORMAT_NEWLINE).

// imageIdentity is the per-image part of the origin.
type imageIdentity struct {
	Path       string
	VerityHash string
	MountID    uint64
	HandleType int32
	Handle     []byte // nil when the filesystem provides no handle
	Inode      uint64
	CrTime     int64
	MTime      int64
}

type mutableDir struct {
	Hierarchy, Dir string
}

// origin is the origin marker content, kept ordered like systemd emits it.
type origin struct {
	Mode         string
	MutableDirs  []mutableDir
	MountOptions string
	Names        []string
	Images       map[string]imageIdentity
}

// jsonNode is a minimal ordered JSON tree for the pretty printer.
type jsonNode struct {
	str    *string
	num    *string
	fields []jsonField
}

type jsonField struct {
	key   string
	value jsonNode
}

func jstr(s string) jsonNode { return jsonNode{str: &s} }

func jnum[T int32 | int64 | uint64](v T) jsonNode {
	s := fmt.Sprint(v)
	return jsonNode{num: &s}
}

func jobj(fields ...jsonField) jsonNode {
	if fields == nil {
		fields = []jsonField{}
	}
	return jsonNode{fields: fields}
}

func (o *origin) tree() jsonNode {
	mutable := []jsonField{{"mode", jstr(o.Mode)}}
	if len(o.MutableDirs) > 0 {
		var dirs []jsonField
		for _, d := range o.MutableDirs {
			dirs = append(dirs, jsonField{d.Hierarchy, jstr(d.Dir)})
		}
		mutable = append(mutable, jsonField{"mutableDirs", jobj(dirs...)})
	}
	top := []jsonField{{"mutable", jobj(mutable...)}}
	if o.MountOptions != "" {
		top = append(top, jsonField{"mountOptions", jstr(o.MountOptions)})
	}
	if len(o.Names) > 0 {
		var exts []jsonField
		for _, name := range o.Names {
			exts = append(exts, jsonField{name, o.Images[name].tree()})
		}
		top = append(top, jsonField{"extensions", jobj(exts...)})
	}
	return jobj(top...)
}

func (id imageIdentity) tree() jsonNode {
	fields := []jsonField{{"path", jstr(id.Path)}}
	if id.VerityHash != "" {
		return jobj(append(fields, jsonField{"verityHash", jstr(id.VerityHash)})...)
	}
	fields = append(fields, jsonField{"onMountId", jnum(id.MountID)})
	if id.Handle != nil {
		fields = append(fields, jsonField{"fileHandle", jobj(
			jsonField{"type", jnum(id.HandleType)},
			jsonField{"handle", jstr(hex.EncodeToString(id.Handle))},
		)})
	} else {
		fields = append(fields, jsonField{"inode", jnum(id.Inode)})
	}
	return jobj(append(fields,
		jsonField{"crtime", jnum(id.CrTime)},
		jsonField{"mtime", jnum(id.MTime)},
	)...)
}

// String renders the origin like systemd writes it.
func (o *origin) String() string {
	var b strings.Builder
	formatJSON(&b, o.tree(), "")
	b.WriteByte('\n')
	return b.String()
}

// formatJSON mirrors sd-json's pretty printer: tab indentation, " : "
// between key and value.
func formatJSON(b *strings.Builder, n jsonNode, prefix string) {
	switch {
	case n.str != nil:
		formatJSONString(b, *n.str)
	case n.num != nil:
		b.WriteString(*n.num)
	case len(n.fields) == 0:
		b.WriteString("{}")
	default:
		inner := prefix + "\t"
		b.WriteString("{\n")
		for i, f := range n.fields {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString(inner)
			formatJSONString(b, f.key)
			b.WriteString(" : ")
			formatJSON(b, f.value, inner)
		}
		b.WriteByte('\n')
		b.WriteString(prefix)
		b.WriteByte('}')
	}
}

func formatJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < ' ' {
				fmt.Fprintf(b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

// originEqual is sd_json_variant_equal() for two origin documents: object
// member order does not matter. An unparsable old document is an error.
func originEqual(newContent, oldContent string) (bool, error) {
	parse := func(s string) (any, error) {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	}
	n, err := parse(newContent)
	if err != nil {
		return false, err
	}
	o, err := parse(oldContent)
	if err != nil {
		return false, fmt.Errorf("failed to parse existing extension origin content: %w", err)
	}
	return reflect.DeepEqual(n, o), nil
}

// pathWithoutRoot strips the root prefix from an image path for the origin,
// like systemd does with --root.
func pathWithoutRoot(path string, roots ...string) string {
	for _, root := range roots {
		if root == "" || root == "/" {
			continue
		}
		if rest, ok := strings.CutPrefix(path, root); ok && strings.HasPrefix(rest, "/") {
			return rest
		}
	}
	return path
}

// atHandleFID is AT_HANDLE_FID: request a handle usable for identification
// only, supported by more filesystems.
const atHandleFID = 0x200

// identify collects the weak image identifiers of discover-image.c's
// image_make(): file handle (or inode), mount id, birth and modification
// time.
func identify(img discover.Image, path string) (imageIdentity, error) {
	id := imageIdentity{Path: path, CrTime: img.CrTime, MTime: img.MTime}
	fd, err := unix.Open(img.Path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return id, &os.PathError{Op: "open", Path: img.Path, Err: err}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return id, &os.PathError{Op: "stat", Path: img.Path, Err: err}
	}
	id.Inode = st.Ino

	handle, legacyID, err := nameToHandle(fd)
	switch {
	case err == nil:
		id.HandleType, id.Handle = handle.Type(), bytes.Clone(handle.Bytes())
		if id.Handle == nil {
			id.Handle = []byte{}
		}
		if unique, ok := statxMountID(fd, unix.STATX_MNT_ID_UNIQUE); ok {
			id.MountID = unique
		} else {
			id.MountID = uint64(legacyID)
		}
	case handleErrorIsFatal(err):
		return id, &os.PathError{Op: "name_to_handle_at", Path: img.Path, Err: err}
	default:
		if unique, ok := statxMountID(fd, unix.STATX_MNT_ID_UNIQUE); ok {
			id.MountID = unique
		} else if legacy, ok := statxMountID(fd, unix.STATX_MNT_ID); ok {
			id.MountID = legacy
		} else {
			mid, err := fdinfoMountID(fd)
			if err != nil {
				return id, err
			}
			id.MountID = mid
		}
	}
	return id, nil
}

// nameToHandle is name_to_handle_at_try_fid(): AT_HANDLE_FID first, then a
// plain handle for kernels without it.
func nameToHandle(fd int) (unix.FileHandle, int, error) {
	h, mid, err := unix.NameToHandleAt(fd, "", unix.AT_EMPTY_PATH|atHandleFID)
	if err == nil || handleErrorIsFatal(err) {
		return h, mid, err
	}
	return unix.NameToHandleAt(fd, "", unix.AT_EMPTY_PATH)
}

// handleErrorIsFatal is is_name_to_handle_at_fatal_error().
func handleErrorIsFatal(err error) bool {
	return !errnoIsNotSupported(err) && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) &&
		!errors.Is(err, unix.EOVERFLOW) && !errors.Is(err, unix.EINVAL)
}

// errnoIsNotSupported is ERRNO_IS_NOT_SUPPORTED().
func errnoIsNotSupported(err error) bool {
	for _, e := range []unix.Errno{unix.EOPNOTSUPP, unix.ENOTTY, unix.ENOSYS, unix.EAFNOSUPPORT,
		unix.EPFNOSUPPORT, unix.EPROTONOSUPPORT, unix.ESOCKTNOSUPPORT, unix.ENOPROTOOPT} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func statxMountID(fd, mask int) (uint64, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_STATX_DONT_SYNC, mask, &stx); err != nil {
		return 0, false
	}
	return stx.Mnt_id, stx.Mask&uint32(mask) != 0
}

func fdinfoMountID(fd int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("no mnt_id in fdinfo")
}

package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func extSB(compat, incompat, roCompat uint32) func([]byte) {
	return func(b []byte) {
		binary.LittleEndian.PutUint16(b[1024+56:], 0xef53)
		binary.LittleEndian.PutUint32(b[1024+92:], compat)
		binary.LittleEndian.PutUint32(b[1024+96:], incompat)
		binary.LittleEndian.PutUint32(b[1024+100:], roCompat)
	}
}

func TestDetectFS(t *testing.T) {
	mk := func(size int, plant ...func([]byte)) []byte {
		b := make([]byte, size)
		for _, p := range plant {
			p(b)
		}
		return b
	}
	squashfs := func(b []byte) { copy(b, "hsqs"); binary.LittleEndian.PutUint16(b[28:], 4) }
	cases := []struct {
		name string
		data []byte
		want fsType
	}{
		{"squashfs", mk(4096, squashfs), fsSquashfs},
		{"squashfs v3", mk(4096, func(b []byte) { copy(b, "hsqs"); binary.LittleEndian.PutUint16(b[28:], 3) }), ""},
		{"erofs", mk(4096, func(b []byte) { binary.LittleEndian.PutUint32(b[1024:], 0xe0f5e1e2); b[1036] = 12 }), fsErofs},
		{"erofs bad block size", mk(4096, func(b []byte) { binary.LittleEndian.PutUint32(b[1024:], 0xe0f5e1e2); b[1036] = 30 }), ""},
		{"ext4 by extents", mk(4096, extSB(0x4, 0x40, 0)), fsExt4},
		{"ext4 without journal", mk(4096, extSB(0, 0x2, 0x8)), fsExt4},
		{"ext4 metadata_csum", mk(4096, extSB(0x4, 0x2, 0x400)), fsExt4},
		{"ext3", mk(4096, extSB(0x4, 0x2|0x4, 0x1)), fsExt3},
		{"ext2", mk(4096, extSB(0, 0x2, 0x1)), fsExt2},
		{"ext journal device", mk(4096, extSB(0, 0x8, 0)), ""},
		{"xfs", mk(4096, func(b []byte) { copy(b, "XFSB"); binary.BigEndian.PutUint32(b[4:], 4096) }), fsXFS},
		{"btrfs", mk(0x10048, func(b []byte) { copy(b[0x10040:], "_BHRfS_M") }), fsBtrfs},
		{"f2fs", mk(4096, func(b []byte) { binary.LittleEndian.PutUint32(b[1024:], 0xf2f52010) }), fsF2FS},
		{"vfat", mk(4096, func(b []byte) {
			binary.LittleEndian.PutUint16(b[11:], 512)
			b[13], b[16] = 4, 2
			binary.LittleEndian.PutUint16(b[14:], 1)
			copy(b[54:], "FAT16   ")
			binary.LittleEndian.PutUint16(b[510:], 0xaa55)
		}), fsVfat},
		{"luks", mk(4096, func(b []byte) { copy(b, "LUKS\xba\xbe") }), fsLUKS},
		{"zeros", mk(8192), ""},
		{"tiny", []byte{1}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		got, err := detectFS(bytes.NewReader(tc.data))
		if err != nil || got != tc.want {
			t.Errorf("%s: detectFS = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	ambiguous := mk(4096, squashfs, extSB(0x4, 0x40, 0))
	if _, err := detectFS(bytes.NewReader(ambiguous)); !errors.Is(err, errAmbiguousFS) || !errors.Is(err, unix.EUCLEAN) {
		t.Errorf("squashfs+ext4 signatures: err = %v, want ambiguity", err)
	}
}

func TestFSTypeAllowed(t *testing.T) {
	for fs, want := range map[fsType]bool{
		fsExt4: true, fsSquashfs: true, fsErofs: true, fsXFS: true, fsBtrfs: true,
		fsF2FS: true, fsVfat: true, fsExt2: false, fsExt3: false, fsLUKS: false,
	} {
		if got := fsTypeAllowed(fs); got != want {
			t.Errorf("fsTypeAllowed(%s) = %v, want %v", fs, got, want)
		}
	}
	t.Setenv("SYSTEMD_DISSECT_FILE_SYSTEMS", "ext2:squashfs")
	if !fsTypeAllowed(fsExt2) || fsTypeAllowed(fsExt4) {
		t.Error("$SYSTEMD_DISSECT_FILE_SYSTEMS not honoured")
	}
}

func TestMountOptions(t *testing.T) {
	for fs, want := range map[fsType]string{
		fsExt4:     "norecovery",
		fsExt3:     "norecovery",
		fsXFS:      "norecovery",
		fsBtrfs:    "rescue=nologreplay",
		fsF2FS:     "norecovery",
		fsSquashfs: "",
		fsErofs:    "",
		fsVfat:     "",
	} {
		if got := mountOptions(fs)[0]; got != want {
			t.Errorf("mountOptions(%s)[0] = %q, want %q", fs, got, want)
		}
	}
	if o := mountOptions(fsBtrfs); len(o) != 2 || o[1] != "norecovery" {
		t.Errorf("btrfs fallback = %v", o)
	}
}

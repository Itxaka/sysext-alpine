package release

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArchFromMachine(t *testing.T) {
	tests := []struct {
		goarch, machine, want string
	}{
		{"amd64", "x86_64", "x86-64"},
		{"amd64", "i686", "x86"},
		{"amd64", "i586", "x86"},
		{"amd64", "i386", "x86"},
		{"386", "x86_64", "x86-64"},
		{"386", "i686", "x86"},
		{"arm64", "aarch64", "arm64"},
		{"arm64", "aarch64_be", "arm64-be"},
		{"arm64", "armv8l", "arm"},
		{"arm", "aarch64", "arm64"},
		{"arm", "armv7l", "arm"},
		{"arm", "armv6l", "arm"},
		{"arm", "armv7b", "arm-be"},
		{"arm", "armv5tejl", "arm"},
		{"ppc64le", "ppc64le", "ppc64-le"},
		{"ppc64", "ppc64", "ppc64"},
		{"ppc64le", "ppc", "ppc"},
		{"s390x", "s390x", "s390x"},
		{"s390x", "s390", "s390"},
		{"riscv64", "riscv64", "riscv64"},
		{"riscv64", "riscv", "riscv64"},
		{"riscv64", "riscv32", "riscv32"},
		{"loong64", "loongarch64", "loongarch64"},
		{"mips64le", "mips64", "mips64"},
		{"mipsle", "mips", "mips"},
	}
	for _, tc := range tests {
		got, ok := archFromMachine(tc.goarch, tc.machine)
		if !ok || got != tc.want {
			t.Errorf("archFromMachine(%s, %s) = %q, %v; want %q", tc.goarch, tc.machine, got, ok, tc.want)
		}
		if !IsArchitecture(got) {
			t.Errorf("%q is not a systemd architecture", got)
		}
	}
	for _, tc := range []struct{ goarch, machine string }{
		{"amd64", "aarch64"},
		{"arm64", "x86_64"},
		{"amd64", "bogus"},
		{"wasm", "x86_64"},
	} {
		if got, ok := archFromMachine(tc.goarch, tc.machine); ok {
			t.Errorf("archFromMachine(%s, %s) = %q, want no match", tc.goarch, tc.machine, got)
		}
	}
}

func TestNativeArchitectures(t *testing.T) {
	tests := []struct {
		goarch, native, secondary string
	}{
		{"amd64", "x86-64", "x86"},
		{"386", "x86", ""},
		{"arm64", "arm64", "arm"},
		{"arm", "arm", ""},
		{"riscv64", "riscv64", ""},
		{"ppc64le", "ppc64-le", "ppc-le"},
		{"ppc64", "ppc64", "ppc"},
		{"s390x", "s390x", "s390"},
		{"loong64", "loongarch64", ""},
		{"mips", "mips", ""},
		{"mipsle", "mips-le", ""},
		{"mips64", "mips64", ""},
		{"mips64le", "mips64-le", ""},
	}
	for _, tc := range tests {
		native, secondary := nativeArchitectures(tc.goarch)
		if native != tc.native || secondary != tc.secondary {
			t.Errorf("nativeArchitectures(%s) = %q, %q; want %q, %q", tc.goarch, native, secondary, tc.native, tc.secondary)
		}
		if !IsArchitecture(native) || (secondary != "" && !IsArchitecture(secondary)) {
			t.Errorf("nativeArchitectures(%s) returned a non-systemd identifier", tc.goarch)
		}
	}
	if NativeArchitecture() == "" {
		t.Error("NativeArchitecture() is empty")
	}
}

func TestHostArchitecture(t *testing.T) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		t.Fatal(err)
	}
	want, ok := archFromMachine(runtime.GOARCH, unix.ByteSliceToString(u.Machine[:]))
	if !ok {
		want = NativeArchitecture()
	}
	if got := HostArchitecture(); got != want {
		t.Errorf("HostArchitecture() = %q, want %q", got, want)
	}
}

func TestIsArchitecture(t *testing.T) {
	for _, s := range []string{"x86-64", "arm64", "s390", "tilegx", "mips64-le"} {
		if !IsArchitecture(s) {
			t.Errorf("IsArchitecture(%q) = false", s)
		}
	}
	for _, s := range []string{"", "amd64", "x86_64", "aarch64", "_any", "X86-64"} {
		if IsArchitecture(s) {
			t.Errorf("IsArchitecture(%q) = true", s)
		}
	}
}

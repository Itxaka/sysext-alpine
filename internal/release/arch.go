package release

import (
	"runtime"
	"slices"
	"sync"

	"golang.org/x/sys/unix"
)

// architectures lists systemd's architecture identifiers
// (architecture_table in src/basic/architecture.c).
var architectures = []string{
	"arm64", "arm64-be", "arm", "arm-be", "alpha", "arc", "arc-be", "cris",
	"x86-64", "x86", "ia64", "loongarch64", "m68k", "mips64-le", "mips64",
	"mips-le", "mips", "nios2", "parisc64", "parisc", "ppc64-le", "ppc64",
	"ppc", "ppc-le", "riscv32", "riscv64", "s390x", "s390", "sh64", "sh",
	"sparc64", "sparc", "tilegx",
}

// IsArchitecture reports whether s is a systemd architecture identifier.
func IsArchitecture(s string) bool {
	return slices.Contains(architectures, s)
}

type machineArch struct{ machine, arch string }

// unameMachines is uname_architecture()'s per-CPU-family table mapping
// uname(2) machine strings to architecture identifiers. Like systemd's
// compile-time selection, only the family of the running binary is
// consulted.
var unameMachines = map[string][]machineArch{
	"arm": {
		{"aarch64", "arm64"}, {"aarch64_be", "arm64-be"},
		{"armv8l", "arm"}, {"armv8b", "arm-be"},
		{"armv7ml", "arm"}, {"armv7mb", "arm-be"},
		{"armv7l", "arm"}, {"armv7b", "arm-be"},
		{"armv6l", "arm"}, {"armv6b", "arm-be"},
		{"armv5tl", "arm"}, {"armv5tel", "arm"}, {"armv5tejl", "arm"},
		{"armv5tejb", "arm-be"}, {"armv5teb", "arm-be"}, {"armv5tb", "arm-be"},
		{"armv4tl", "arm"}, {"armv4tb", "arm-be"},
		{"armv4l", "arm"}, {"armv4b", "arm-be"},
	},
	"x86": {
		{"x86_64", "x86-64"}, {"i686", "x86"}, {"i586", "x86"}, {"i486", "x86"}, {"i386", "x86"},
	},
	"loongarch": {{"loongarch64", "loongarch64"}},
	"mips":      {{"mips64", "mips64"}, {"mips", "mips"}},
	"ppc": {
		{"ppc64le", "ppc64-le"}, {"ppc64", "ppc64"}, {"ppcle", "ppc-le"}, {"ppc", "ppc"},
	},
	"riscv": {{"riscv64", "riscv64"}, {"riscv32", "riscv32"}, {"riscv", "riscv64"}},
	"s390":  {{"s390x", "s390x"}, {"s390", "s390"}},
}

func cpuFamily(goarch string) string {
	switch goarch {
	case "amd64", "386":
		return "x86"
	case "arm64", "arm":
		return "arm"
	case "loong64":
		return "loongarch"
	case "mips", "mipsle", "mips64", "mips64le":
		return "mips"
	case "ppc64", "ppc64le":
		return "ppc"
	case "riscv64":
		return "riscv"
	case "s390x":
		return "s390"
	}
	return ""
}

// archFromMachine maps a uname(2) machine string to an architecture
// identifier using the table of goarch's CPU family.
func archFromMachine(goarch, machine string) (string, bool) {
	for _, m := range unameMachines[cpuFamily(goarch)] {
		if m.machine == machine {
			return m.arch, true
		}
	}
	return "", false
}

// nativeArchitectures returns the architecture a binary built for goarch
// runs natively and the secondary architecture it can also execute
// (native_architecture() and ARCHITECTURE_SECONDARY in architecture.h).
func nativeArchitectures(goarch string) (native, secondary string) {
	switch goarch {
	case "amd64":
		return "x86-64", "x86"
	case "386":
		return "x86", ""
	case "arm64":
		return "arm64", "arm"
	case "arm":
		return "arm", ""
	case "loong64":
		return "loongarch64", ""
	case "mips":
		return "mips", ""
	case "mipsle":
		return "mips-le", ""
	case "mips64":
		return "mips64", ""
	case "mips64le":
		return "mips64-le", ""
	case "ppc64":
		return "ppc64", "ppc"
	case "ppc64le":
		return "ppc64-le", "ppc-le"
	case "riscv64":
		return "riscv64", ""
	case "s390x":
		return "s390x", "s390"
	}
	return goarch, ""
}

// NativeArchitecture returns the architecture this binary was built for
// (systemd's native_architecture()), which selects GPT partition types and
// versioned (vpick) image entries.
func NativeArchitecture() string {
	native, _ := nativeArchitectures(runtime.GOARCH)
	return native
}

// SecondaryArchitecture returns the additional architecture the native one
// can execute (x86 on x86-64, arm on arm64, ...), or "".
func SecondaryArchitecture() string {
	_, secondary := nativeArchitectures(runtime.GOARCH)
	return secondary
}

var hostArchitecture = sync.OnceValue(func() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		if arch, ok := archFromMachine(runtime.GOARCH, unix.ByteSliceToString(u.Machine[:])); ok {
			return arch
		}
	}
	return NativeArchitecture()
})

// HostArchitecture returns the architecture of the running kernel as seen
// through uname(2), honoring personality(2) (setarch linux32), like
// systemd's uname_architecture(). It is what ARCHITECTURE= in
// extension-release files is matched against.
func HostArchitecture() string {
	return hostArchitecture()
}

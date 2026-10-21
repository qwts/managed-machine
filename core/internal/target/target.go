// Package target defines the five first-release targets of ADR-0014 and maps
// a host's operating system and native architecture onto them without
// changing their accepted spelling.
package target

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// Target is one of the five canonical identifiers of ADR-0014 as amended on
// 2026-10-21: exactly the Go GOOS/GOARCH pair. It is declared as data so the
// accepted list, not the build, decides what the matrix supports.
type Target string

const (
	DarwinARM64  Target = "darwin/arm64"
	LinuxARM64   Target = "linux/arm64"
	LinuxAMD64   Target = "linux/amd64"
	WindowsARM64 Target = "windows/arm64"
	WindowsAMD64 Target = "windows/amd64"
)

// All returns the five targets in the order the specification lists them.
func All() []Target {
	return []Target{DarwinARM64, LinuxARM64, LinuxAMD64, WindowsARM64, WindowsAMD64}
}

// canonical maps (os, architecture family) to the accepted identifier.
var canonical = map[[2]string]Target{
	{"darwin", "arm64"}:  DarwinARM64,
	{"linux", "arm64"}:   LinuxARM64,
	{"linux", "amd64"}:   LinuxAMD64,
	{"windows", "arm64"}: WindowsARM64,
	{"windows", "amd64"}: WindowsAMD64,
}

// family folds the architecture spellings produced by Go, uname, Windows and
// GitHub runner labels into the two families the matrix supports. These are
// host facts only; identifiers given as input (Parse) accept no aliases.
var family = map[string]string{
	"arm64": "arm64", "aarch64": "arm64",
	"amd64": "amd64", "x86_64": "amd64", "x64": "amd64",
}

// UnsupportedError reports an (os, architecture) pair outside the matrix.
type UnsupportedError struct {
	OS   string
	Arch string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("unsupported target %s/%s; supported: %s", e.OS, e.Arch, joinAll())
}

func joinAll() string {
	parts := make([]string, 0, 5)
	for _, t := range All() {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ", ")
}

// Normalize maps an operating system name (as Go spells GOOS) and a native
// architecture spelling (arm64, aarch64, amd64, x86_64 or x64) to the
// canonical identifier. Matching is case-insensitive on input; the output
// spelling is always the accepted one.
func Normalize(os, arch string) (Target, error) {
	o := strings.ToLower(strings.TrimSpace(os))
	a, ok := family[strings.ToLower(strings.TrimSpace(arch))]
	if !ok {
		return "", &UnsupportedError{OS: o, Arch: arch}
	}
	t, ok := canonical[[2]string{o, a}]
	if !ok {
		return "", &UnsupportedError{OS: o, Arch: arch}
	}
	return t, nil
}

// Parse accepts a canonical identifier exactly as spelled and rejects every
// other spelling, so configuration never drifts to "linux/amd64" by accident.
func Parse(s string) (Target, error) {
	for _, t := range All() {
		if string(t) == s {
			return t, nil
		}
	}
	return "", fmt.Errorf("unknown target %q; supported: %s", s, joinAll())
}

// Facts are the injectable platform facts a host adapter supplies. OS is the
// Go GOOS name; NativeArch is the architecture of the machine, not of the
// running binary, so an emulated process still reports the real host.
type Facts struct {
	OS         string
	NativeArch string
	// ProcessArch is the architecture the running binary was built for.
	ProcessArch string
}

// Host describes the machine the process runs on.
type Host struct {
	Target   Target
	Emulated bool
}

// FromFacts maps supplied facts to a Host. It performs no system calls, so
// every target can be exercised in tests on any machine.
func FromFacts(f Facts) (Host, error) {
	t, err := Normalize(f.OS, f.NativeArch)
	if err != nil {
		return Host{}, err
	}
	emulated := false
	if f.ProcessArch != "" {
		emulated = family[strings.ToLower(f.ProcessArch)] != family[strings.ToLower(f.NativeArch)]
	}
	return Host{Target: t, Emulated: emulated}, nil
}

// Detect probes the running machine. The native architecture comes from the
// operating system (IsWow64Process2 on Windows, uname on Unix), never from
// the runner label or the build architecture alone.
func Detect() (Host, error) {
	native, err := nativeArchitecture()
	if err != nil {
		return Host{}, err
	}
	return FromFacts(Facts{OS: runtime.GOOS, NativeArch: native, ProcessArch: runtime.GOARCH})
}

// Spellings lists every accepted input spelling, for help text.
func Spellings() []string {
	keys := make([]string, 0, len(family))
	for k := range family {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

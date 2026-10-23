package target

import (
	"errors"
	"testing"
)

func TestAllFiveSpellings(t *testing.T) {
	want := []string{"darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64"}
	got := All()
	if len(got) != len(want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("All()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNormalizeMapsHostFactsOntoAcceptedSpellings(t *testing.T) {
	cases := []struct {
		os, arch string
		want     Target
	}{
		{"darwin", "arm64", DarwinARM64},
		{"linux", "aarch64", LinuxARM64}, // uname -m on Linux ARM64
		{"linux", "arm64", LinuxARM64},   // runtime.GOARCH
		{"linux", "x86_64", LinuxAMD64},  // uname -m on Linux x64: spelling folds to amd64
		{"linux", "amd64", LinuxAMD64},
		{"windows", "arm64", WindowsARM64}, // IsWow64Process2 0xaa64
		{"windows", "amd64", WindowsAMD64}, // IsWow64Process2 0x8664
		{"windows", "x64", WindowsAMD64},   // runner label
		{"Windows", "AMD64", WindowsAMD64}, // case-insensitive input, canonical output
	}
	for _, c := range cases {
		got, err := Normalize(c.os, c.arch)
		if err != nil {
			t.Errorf("Normalize(%q, %q): %v", c.os, c.arch, err)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q, %q) = %q, want %q", c.os, c.arch, got, c.want)
		}
	}
}

func TestNormalizeRejectsHostsOutsideTheMatrix(t *testing.T) {
	cases := [][2]string{
		{"darwin", "x86_64"}, // Intel macOS is out of scope for v1
		{"linux", "riscv64"},
		{"freebsd", "amd64"},
		{"windows", "386"},
		{"", ""},
	}
	for _, c := range cases {
		_, err := Normalize(c[0], c[1])
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Errorf("Normalize(%q, %q) = %v, want *UnsupportedError", c[0], c[1], err)
		}
	}
}

func TestParseAcceptsOnlyCanonicalSpellings(t *testing.T) {
	for _, want := range All() {
		got, err := Parse(string(want))
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v", want, got, err)
		}
	}
	for _, s := range []string{"linux/x86_64", "linux/aarch64", "windows/x64", "Linux/amd64", "darwin/arm64 ", "darwin-arm64", ""} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted an alias; identifiers given as input accept no aliases", s)
		}
	}
}

func TestFromFactsReportsEmulation(t *testing.T) {
	host, err := FromFacts(Facts{OS: "windows", NativeArch: "arm64", ProcessArch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if host.Target != WindowsARM64 || !host.Emulated {
		t.Errorf("x64 process on an ARM64 host = %+v, want windows/arm64 emulated", host)
	}
	host, err = FromFacts(Facts{OS: "darwin", NativeArch: "arm64", ProcessArch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if host.Emulated {
		t.Errorf("native process reported as emulated: %+v", host)
	}
	if _, err := FromFacts(Facts{OS: "darwin", NativeArch: "x86_64", ProcessArch: "x86_64"}); err == nil {
		t.Error("Intel macOS host accepted")
	}
}

func TestDetectRunsOnThisHost(t *testing.T) {
	// The hosted runners are the authority on which identifier is correct;
	// locally we only require that the probe maps this machine onto one of
	// the five without error.
	host, err := Detect()
	if err != nil {
		t.Skipf("host outside the matrix or probe unavailable: %v", err)
	}
	if _, err := Parse(string(host.Target)); err != nil {
		t.Errorf("Detect() produced a non-canonical identifier %q", host.Target)
	}
}

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testTeam = "ABCDE12345"

// syntheticMacho returns an arm64 Mach-O whose only load commands are a
// __LINKEDIT segment and LC_CODE_SIGNATURE, followed by code and a signature.
func syntheticMacho(code, signature []byte) []byte {
	le := binary.LittleEndian
	const cmdsLen = 72 + 16
	dataoff := machHeader64Len + cmdsLen + len(code)
	b := make([]byte, dataoff+len(signature))
	le.PutUint32(b[0:], 0xfeedfacf)
	le.PutUint32(b[4:], 0x0100000c)
	le.PutUint32(b[12:], 2)
	le.PutUint32(b[16:], 2)
	le.PutUint32(b[20:], cmdsLen)
	seg := b[machHeader64Len:]
	le.PutUint32(seg[0:], lcSegment64)
	le.PutUint32(seg[4:], 72)
	copy(seg[8:24], "__LINKEDIT")
	le.PutUint64(seg[32:], uint64(len(signature)+4096))
	le.PutUint64(seg[40:], uint64(dataoff))
	le.PutUint64(seg[48:], uint64(len(signature)))
	sig := seg[72:]
	le.PutUint32(sig[0:], lcCodeSignature)
	le.PutUint32(sig[4:], 16)
	le.PutUint32(sig[8:], uint32(dataoff))
	le.PutUint32(sig[12:], uint32(len(signature)))
	copy(b[machHeader64Len+cmdsLen:], code)
	copy(b[dataoff:], signature)
	return b
}

func TestMachoEqualIgnoringSignatureAcceptsResignedBinary(t *testing.T) {
	code := bytes.Repeat([]byte{0xAA}, 64)
	unsigned := syntheticMacho(code, []byte("adhoc-linker-signature"))
	signed := syntheticMacho(code, []byte("developer id signature naming "+testTeam+" with a longer blob"))
	if err := machoEqualIgnoringSignature(unsigned, signed, testTeam); err != nil {
		t.Fatalf("resigned binary refused: %v", err)
	}
}

func TestMachoEqualIgnoringSignatureRefusesChangedCode(t *testing.T) {
	code := bytes.Repeat([]byte{0xAA}, 64)
	changed := append([]byte(nil), code...)
	changed[10] = 0xBB
	unsigned := syntheticMacho(code, []byte("adhoc"))
	signed := syntheticMacho(changed, []byte("sig "+testTeam))
	err := machoEqualIgnoringSignature(unsigned, signed, testTeam)
	if err == nil || !strings.Contains(err.Error(), "code differs") {
		t.Fatalf("changed code accepted or wrong error: %v", err)
	}
}

func TestMachoEqualIgnoringSignatureRefusesWrongTeam(t *testing.T) {
	code := bytes.Repeat([]byte{0xAA}, 64)
	unsigned := syntheticMacho(code, []byte("adhoc"))
	signed := syntheticMacho(code, []byte("sig ZZZZZ99999"))
	err := machoEqualIgnoringSignature(unsigned, signed, testTeam)
	if err == nil || !strings.Contains(err.Error(), "Team ID") {
		t.Fatalf("wrong team accepted or wrong error: %v", err)
	}
}

func TestMachoEqualIgnoringSignatureRefusesTrailingData(t *testing.T) {
	code := bytes.Repeat([]byte{0xAA}, 64)
	unsigned := syntheticMacho(code, []byte("adhoc"))
	signed := append(syntheticMacho(code, []byte("sig "+testTeam)), []byte("payload")...)
	if err := machoEqualIgnoringSignature(unsigned, signed, testTeam); err == nil {
		t.Fatal("data appended after the signature was accepted")
	}
}

func TestMachoEqualIgnoringSignatureWithCodesign(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("codesign re-signing needs darwin/arm64")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unsignedPath := filepath.Join(dir, "unsigned")
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", unsignedPath, src)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=darwin", "GOARCH=arm64", "GO111MODULE=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	unsigned, err := os.ReadFile(unsignedPath)
	if err != nil {
		t.Fatal(err)
	}
	signedPath := filepath.Join(dir, "signed")
	if err := os.WriteFile(signedPath, unsigned, 0o755); err != nil {
		t.Fatal(err)
	}
	sign := exec.Command("/usr/bin/codesign", "--force", "-s", "-", "--options", "runtime", "-i", bundleID, signedPath)
	if out, err := sign.CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v: %s", err, out)
	}
	signed, err := os.ReadFile(signedPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(signed, unsigned) {
		t.Fatal("codesign left the binary unchanged")
	}
	if err := machoEqualIgnoringSignature(unsigned, signed, bundleID); err != nil {
		t.Fatalf("codesign-resigned binary refused: %v", err)
	}
}

func writeArchiveDist(t *testing.T, dir, version string, darwin []archiveEntry) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, spec := range releaseTargets() {
		name := spec.archiveName(version)
		var data []byte
		var err error
		switch {
		case spec.Target == "darwin/arm64":
			data, err = buildTarGz(darwin)
		case spec.IsZip:
			data, err = buildZip([]archiveEntry{{Name: spec.BinaryName, Mode: 0o755, Data: []byte(name)}})
		default:
			data, err = buildTarGz([]archiveEntry{{Name: spec.BinaryName, Mode: 0o755, Data: []byte(name)}})
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, sha256Hex(data)+"  "+name)
	}
	sortSumLines(lines)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sortSumLines(lines []string) {
	for i := 1; i < len(lines); i++ {
		for j := i; j > 0 && lines[j][66:] < lines[j-1][66:]; j-- {
			lines[j], lines[j-1] = lines[j-1], lines[j]
		}
	}
}

func unsignedDarwin(binary []byte) []archiveEntry {
	return append([]archiveEntry{
		{Name: "LICENSE", Mode: 0o644, Data: []byte("license\n")},
		{Name: bundleExecutable, Mode: 0o755, Data: binary},
	}, darwinBundleEntries("1.2.3", binary)...)
}

func writeTree(t *testing.T, root string, entries []archiveEntry) {
	t.Helper()
	for _, e := range entries {
		p := filepath.Join(root, filepath.FromSlash(e.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, e.Data, os.FileMode(e.Mode)); err != nil {
			t.Fatal(err)
		}
	}
}

func signedTree(binary, signedBinary, profile []byte) []archiveEntry {
	return []archiveEntry{
		{Name: "LICENSE", Mode: 0o644, Data: []byte("license\n")},
		{Name: bundleExecutable, Mode: 0o755, Data: signedBinary},
		{Name: bundleInfoPlist, Mode: 0o644, Data: infoPlist("1.2.3")},
		{Name: bundleMainExe, Mode: 0o755, Data: signedBinary},
		{Name: bundleProfile, Mode: 0o644, Data: profile},
		{Name: bundleSealed, Mode: 0o644, Data: []byte("<plist/>")},
	}
}

func TestRepackAndVerifySigned(t *testing.T) {
	dir := t.TempDir()
	code := bytes.Repeat([]byte{0x42}, 128)
	unsignedBin := syntheticMacho(code, []byte("adhoc"))
	signedBin := syntheticMacho(code, []byte("developer id "+testTeam))
	unsigned := filepath.Join(dir, "unsigned")
	writeArchiveDist(t, unsigned, "1.2.3", unsignedDarwin(unsignedBin))

	cases := []struct {
		name    string
		tree    []archiveEntry
		wantErr string
	}{
		{"signed", signedTree(unsignedBin, signedBin, []byte("profile "+testTeam+"."+bundleID)), ""},
		{"profile for another app", signedTree(unsignedBin, signedBin, []byte("profile "+testTeam+".other")), "does not name"},
		{"extra file", append(signedTree(unsignedBin, signedBin, []byte(testTeam+"."+bundleID)), archiveEntry{Name: bundleName + "/Contents/MacOS/helper", Mode: 0o755, Data: []byte("x")}), "unexpected file"},
		{"changed plist", func() []archiveEntry {
			tree := signedTree(unsignedBin, signedBin, []byte(testTeam+"."+bundleID))
			tree[2].Data = []byte("<plist>changed</plist>")
			return tree
		}(), "Info.plist differs"},
		{"unsigned binary left in place", signedTree(unsignedBin, unsignedBin, []byte(testTeam+"."+bundleID)), "Team ID"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := filepath.Join(dir, "tree", string(rune('a'+i)))
			writeTree(t, tree, tc.tree)
			out := filepath.Join(dir, "signed", string(rune('a'+i)))
			var stdout bytes.Buffer
			if err := repackCmd([]string{"-dist", unsigned, "-tree", tree, "-out", out}, &stdout); err != nil {
				t.Fatalf("repack: %v", err)
			}
			repacked := strings.TrimSpace(stdout.String())
			stdout.Reset()
			err := verifySignedCmd([]string{"-unsigned", unsigned, "-signed", out, "-team", testTeam}, &stdout)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verify-signed: %v", err)
				}
				if got := strings.TrimSpace(stdout.String()); got != repacked {
					t.Fatalf("verify-signed printed %s, repack printed %s", got, repacked)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("verify-signed error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestVerifySignedRefusesChangedOtherArchive(t *testing.T) {
	dir := t.TempDir()
	code := bytes.Repeat([]byte{0x42}, 128)
	unsignedBin := syntheticMacho(code, []byte("adhoc"))
	signedBin := syntheticMacho(code, []byte("developer id "+testTeam))
	unsigned := filepath.Join(dir, "unsigned")
	writeArchiveDist(t, unsigned, "1.2.3", unsignedDarwin(unsignedBin))
	tree := filepath.Join(dir, "tree")
	writeTree(t, tree, signedTree(unsignedBin, signedBin, []byte(testTeam+"."+bundleID)))
	out := filepath.Join(dir, "signed")
	if err := repackCmd([]string{"-dist", unsigned, "-tree", tree, "-out", out}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(out, "managed-machine_1.2.3_linux_amd64.tar.gz")
	data, err := buildTarGz([]archiveEntry{{Name: "managed-machine", Mode: 0o755, Data: []byte("swapped")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		if strings.HasSuffix(line, "linux_amd64.tar.gz") {
			line = sha256Hex(data) + "  managed-machine_1.2.3_linux_amd64.tar.gz"
		}
		lines = append(lines, line)
	}
	if err := os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = verifySignedCmd([]string{"-unsigned", unsigned, "-signed", out, "-team", testTeam}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "linux_amd64.tar.gz differs") {
		t.Fatalf("verify-signed error = %v", err)
	}
}

func TestRepackRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	code := bytes.Repeat([]byte{0x42}, 128)
	unsignedBin := syntheticMacho(code, []byte("adhoc"))
	unsigned := filepath.Join(dir, "unsigned")
	writeArchiveDist(t, unsigned, "1.2.3", unsignedDarwin(unsignedBin))
	tree := filepath.Join(dir, "tree")
	writeTree(t, tree, []archiveEntry{{Name: "LICENSE", Mode: 0o644, Data: []byte("x")}})
	if err := os.Symlink("/etc/passwd", filepath.Join(tree, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	err := repackCmd([]string{"-dist", unsigned, "-tree", tree, "-out", filepath.Join(dir, "out")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "only regular files") {
		t.Fatalf("repack error = %v", err)
	}
}

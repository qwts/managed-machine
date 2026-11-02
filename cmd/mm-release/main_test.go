package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildDeterminismArchiveContentsAndVerify(t *testing.T) {
	const wantVersion = "0.9.0"

	out1 := filepath.Join(t.TempDir(), "dist1")
	out2 := filepath.Join(t.TempDir(), "dist2")

	var outBuf, errBuf bytes.Buffer
	if code := run([]string{"build", "-version", wantVersion, "-out", out1}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("first build failed (exit %d): %s", code, errBuf.String())
	}
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"build", "-version", wantVersion, "-out", out2}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("second build failed (exit %d): %s", code, errBuf.String())
	}

	sums1, err := os.ReadFile(filepath.Join(out1, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read out1 SHA256SUMS: %v", err)
	}
	sums2, err := os.ReadFile(filepath.Join(out2, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read out2 SHA256SUMS: %v", err)
	}
	if !bytes.Equal(sums1, sums2) {
		t.Fatalf("SHA256SUMS differs across two builds:\n--- build 1 ---\n%s--- build 2 ---\n%s", sums1, sums2)
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	wantLicense, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	wantLicense = bytes.ReplaceAll(wantLicense, []byte("\r\n"), []byte("\n"))

	wantArchives := []struct {
		name  string
		goos  string
		arch  string
		isZip bool
	}{
		{"managed-machine_0.9.0_darwin_arm64.tar.gz", "darwin", "arm64", false},
		{"managed-machine_0.9.0_linux_amd64.tar.gz", "linux", "amd64", false},
		{"managed-machine_0.9.0_linux_arm64.tar.gz", "linux", "arm64", false},
		{"managed-machine_0.9.0_windows_amd64.zip", "windows", "amd64", true},
		{"managed-machine_0.9.0_windows_arm64.zip", "windows", "arm64", true},
	}

	var darwinDigest string
	for _, wa := range wantArchives {
		b1, err := os.ReadFile(filepath.Join(out1, wa.name))
		if err != nil {
			t.Fatalf("read out1/%s: %v", wa.name, err)
		}
		b2, err := os.ReadFile(filepath.Join(out2, wa.name))
		if err != nil {
			t.Fatalf("read out2/%s: %v", wa.name, err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("archive %s is not byte-identical across two builds (%d vs %d bytes)", wa.name, len(b1), len(b2))
		}
		if wa.goos == "darwin" && wa.arch == "arm64" {
			darwinDigest = sha256Hex(b1)
		}

		var binBytes []byte
		if wa.isZip {
			binBytes = checkZipArchive(t, wa.name, b1, wantLicense)
		} else {
			binBytes = checkTarGzArchive(t, wa.name, b1, wantLicense)
		}

		// Assert embedded version in every binary: run the host binary and read
		// the embedded version string in the others.
		checkBinaryVersionString(t, wa.name, binBytes, wantVersion)
		if wa.goos == runtime.GOOS && wa.arch == runtime.GOARCH {
			runHostBinaryVersionChecks(t, binBytes, wantVersion, wa.goos+"/"+wa.arch)
		}
	}

	// Verify passes on an untouched dist directory.
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"verify", "-dist", out1}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("verify on untouched dist failed (exit %d): %s", code, errBuf.String())
	}

	// Verify fails (exit 1) naming the archive on a one-byte change to any archive.
	for _, wa := range wantArchives {
		p := filepath.Join(out1, wa.name)
		orig, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		mut := append([]byte(nil), orig...)
		mut[len(mut)/2] ^= 0xff
		if err := os.WriteFile(p, mut, 0o644); err != nil {
			t.Fatal(err)
		}
		outBuf.Reset()
		errBuf.Reset()
		code := run([]string{"verify", "-dist", out1}, &outBuf, &errBuf)
		if err := os.WriteFile(p, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		if code != 1 {
			t.Errorf("verify after 1-byte archive edit to %s: exit %d, want 1", wa.name, code)
		}
		if !strings.Contains(errBuf.String(), wa.name) {
			t.Errorf("verify after 1-byte archive edit to %s did not name %s: %q", wa.name, wa.name, errBuf.String())
		}
	}

	// Verify fails (exit 1) naming the file on a one-byte change to any SHA256SUMS line.
	sumsPath := filepath.Join(out1, "SHA256SUMS")
	lines := strings.Split(strings.TrimSuffix(string(sums1), "\n"), "\n")
	for i, line := range lines {
		_, fileOnLine, _ := strings.Cut(line, "  ")
		for _, pos := range []int{0, len(line) - 1} {
			mutLines := append([]string(nil), lines...)
			b := []byte(mutLines[i])
			if b[pos] == 'a' {
				b[pos] = 'b'
			} else {
				b[pos] = 'a'
			}
			mutLines[i] = string(b)
			if err := os.WriteFile(sumsPath, []byte(strings.Join(mutLines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			outBuf.Reset()
			errBuf.Reset()
			code := run([]string{"verify", "-dist", out1}, &outBuf, &errBuf)
			if err := os.WriteFile(sumsPath, sums1, 0o644); err != nil {
				t.Fatal(err)
			}
			if code != 1 {
				t.Errorf("verify after 1-byte SHA256SUMS edit (line %d pos %d): exit %d, want 1", i+1, pos, code)
			}
			errText := errBuf.String()
			if !strings.Contains(errText, "SHA256SUMS") && !strings.Contains(errText, fileOnLine) {
				t.Errorf("verify after 1-byte SHA256SUMS edit (line %d pos %d) did not name file: %q", i+1, pos, errText)
			}
		}
	}

	// Verify -formula <path> -require-pin passes when tag, version and core sha256 match,
	// and exits 1 naming the field when any of them disagrees.
	testFormulaPinVerification(t, out1, wantVersion, darwinDigest)
}

func checkTarGzArchive(t *testing.T, archiveName string, raw []byte, wantLicense []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: gzip.NewReader: %v", archiveName, err)
	}
	defer gz.Close()
	if gz.Name != "" {
		t.Errorf("%s: gzip header Name = %q, want empty", archiveName, gz.Name)
	}
	if !gz.ModTime.IsZero() {
		t.Errorf("%s: gzip header ModTime = %v, want zero", archiveName, gz.ModTime)
	}
	if len(gz.Extra) != 0 {
		t.Errorf("%s: gzip header Extra = %x, want empty", archiveName, gz.Extra)
	}

	tr := tar.NewReader(gz)
	type gotEntry struct {
		hdr  *tar.Header
		data []byte
	}
	var entries []gotEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: tar.Next: %v", archiveName, err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("%s: read tar entry %s: %v", archiveName, hdr.Name, err)
		}
		entries = append(entries, gotEntry{hdr: hdr, data: data})
	}
	if strings.Contains(archiveName, "_darwin_arm64.") {
		if len(entries) != 4 {
			t.Fatalf("%s: got %d tar entries, want 4", archiveName, len(entries))
		}
		if entries[0].hdr.Name != bundleInfoPlist || entries[1].hdr.Name != bundleMainExe {
			t.Fatalf("%s: bundle entry names = [%q, %q], want [%s, %s]",
				archiveName, entries[0].hdr.Name, entries[1].hdr.Name, bundleInfoPlist, bundleMainExe)
		}
		if entries[0].hdr.Mode != 0o644 || entries[1].hdr.Mode != 0o755 {
			t.Errorf("%s: bundle entry modes = %#o, %#o, want 0644, 0755", archiveName, entries[0].hdr.Mode, entries[1].hdr.Mode)
		}
		if !bytes.Contains(entries[0].data, []byte("<string>"+bundleID+"</string>")) {
			t.Errorf("%s: Info.plist does not name %s", archiveName, bundleID)
		}
		if !bytes.Equal(entries[1].data, entries[3].data) {
			t.Errorf("%s: bundle executable differs from managed-machine", archiveName)
		}
		entries = entries[2:]
	}
	if len(entries) != 2 {
		t.Fatalf("%s: got %d tar entries, want 2", archiveName, len(entries))
	}
	if entries[0].hdr.Name != "LICENSE" || entries[1].hdr.Name != "managed-machine" {
		t.Fatalf("%s: tar entry names = [%q, %q], want [LICENSE, managed-machine]",
			archiveName, entries[0].hdr.Name, entries[1].hdr.Name)
	}
	if entries[0].hdr.Mode != 0o644 {
		t.Errorf("%s: LICENSE mode = %#o, want 0644", archiveName, entries[0].hdr.Mode)
	}
	if entries[1].hdr.Mode != 0o755 {
		t.Errorf("%s: managed-machine mode = %#o, want 0755", archiveName, entries[1].hdr.Mode)
	}
	for _, e := range entries {
		if e.hdr.Uid != 0 || e.hdr.Gid != 0 {
			t.Errorf("%s: %s uid/gid = %d/%d, want 0/0", archiveName, e.hdr.Name, e.hdr.Uid, e.hdr.Gid)
		}
		if e.hdr.Uname != "" || e.hdr.Gname != "" {
			t.Errorf("%s: %s uname/gname = %q/%q, want empty", archiveName, e.hdr.Name, e.hdr.Uname, e.hdr.Gname)
		}
		if !e.hdr.ModTime.UTC().Equal(archiveEpoch) {
			t.Errorf("%s: %s mtime = %s, want %s", archiveName, e.hdr.Name, e.hdr.ModTime.UTC(), archiveEpoch)
		}
	}
	if !bytes.Equal(entries[0].data, wantLicense) {
		t.Errorf("%s: LICENSE content mismatch", archiveName)
	}
	return entries[1].data
}

func checkZipArchive(t *testing.T, archiveName string, raw []byte, wantLicense []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("%s: zip.NewReader: %v", archiveName, err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("%s: got %d zip entries, want 2", archiveName, len(zr.File))
	}
	if zr.File[0].Name != "LICENSE" || zr.File[1].Name != "managed-machine.exe" {
		t.Fatalf("%s: zip entry names = [%q, %q], want [LICENSE, managed-machine.exe]",
			archiveName, zr.File[0].Name, zr.File[1].Name)
	}
	var binBytes []byte
	for _, f := range zr.File {
		if !f.Modified.UTC().Equal(archiveEpoch) {
			t.Errorf("%s: %s Modified = %s, want %s", archiveName, f.Name, f.Modified.UTC(), archiveEpoch)
		}
		if len(f.Extra) != 0 {
			t.Errorf("%s: %s central directory Extra = %x, want empty", archiveName, f.Name, f.Extra)
		}
		if n := localHeaderExtraLen(t, raw, f.Name); n != 0 {
			t.Errorf("%s: %s local header extra length = %d, want 0", archiveName, f.Name, n)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("%s: open %s: %v", archiveName, f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: read %s: %v", archiveName, f.Name, err)
		}
		if f.Name == "LICENSE" {
			if !bytes.Equal(data, wantLicense) {
				t.Errorf("%s: LICENSE content mismatch", archiveName)
			}
		} else {
			binBytes = data
		}
	}
	return binBytes
}

func localHeaderExtraLen(t *testing.T, zipBytes []byte, wantName string) int {
	t.Helper()
	// Scan local file headers (signature 0x04034b50) to verify local extra length is 0.
	sig := []byte{0x50, 0x4b, 0x03, 0x04}
	off := 0
	for {
		idx := bytes.Index(zipBytes[off:], sig)
		if idx < 0 {
			break
		}
		pos := off + idx
		if pos+30 > len(zipBytes) {
			break
		}
		nameLen := int(binary.LittleEndian.Uint16(zipBytes[pos+26 : pos+28]))
		extraLen := int(binary.LittleEndian.Uint16(zipBytes[pos+28 : pos+30]))
		if pos+30+nameLen <= len(zipBytes) && string(zipBytes[pos+30:pos+30+nameLen]) == wantName {
			return extraLen
		}
		off = pos + 4
	}
	t.Fatalf("local file header for %s not found", wantName)
	return -1
}

func checkBinaryVersionString(t *testing.T, archiveName string, binBytes []byte, wantVersion string) {
	t.Helper()
	if !bytes.Contains(binBytes, []byte(wantVersion)) {
		t.Errorf("%s: binary does not contain embedded version string %q", archiveName, wantVersion)
	}
	if _, err := buildinfo.Read(bytes.NewReader(binBytes)); err != nil {
		t.Fatalf("%s: buildinfo.Read: %v", archiveName, err)
	}
}

func runHostBinaryVersionChecks(t *testing.T, binBytes []byte, wantVersion, wantTarget string) {
	t.Helper()
	tmp := t.TempDir()
	binName := "managed-machine"
	if runtime.GOOS == "windows" {
		binName = "managed-machine.exe"
	}
	binPath := filepath.Join(tmp, binName)
	if err := os.WriteFile(binPath, binBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. managed-machine --version
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binPath, "--version")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("host binary --version failed: %v (stderr %q)", err, stderr.String())
	}
	if stdout.String() != wantVersion+"\n" {
		t.Errorf("host binary --version: got %q, want %q", stdout.String(), wantVersion+"\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("host binary --version stderr: %q", stderr.String())
	}

	// 2. managed-machine version (default compact JSONL)
	stdout.Reset()
	stderr.Reset()
	cmd = exec.Command(binPath, "version")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("host binary version failed: %v (stderr %q)", err, stderr.String())
	}
	wantRecord := fmt.Sprintf("{\"version\":%q,\"target\":%q,\"toolchain\":%q}\n", wantVersion, wantTarget, runtime.Version())
	if stdout.String() != wantRecord {
		t.Errorf("host binary version: got %q, want %q", stdout.String(), wantRecord)
	}
	if stderr.Len() != 0 {
		t.Errorf("host binary version stderr: %q", stderr.String())
	}

	// 3. Unflagged build prints 0.0.0-dev.
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	unflaggedPath := filepath.Join(tmp, "unflagged-"+binName)
	buildUnflagged := exec.Command("go", "build", "-o", unflaggedPath, "./cmd/managed-machine")
	buildUnflagged.Dir = repoRoot
	if out, err := buildUnflagged.CombinedOutput(); err != nil {
		t.Fatalf("unflagged go build failed: %v: %s", err, out)
	}
	stdout.Reset()
	stderr.Reset()
	cmd = exec.Command(unflaggedPath, "--version")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("unflagged binary --version failed: %v (stderr %q)", err, stderr.String())
	}
	if stdout.String() != "0.0.0-dev\n" {
		t.Errorf("unflagged binary --version: got %q, want %q", stdout.String(), "0.0.0-dev\n")
	}
}

func testFormulaPinVerification(t *testing.T, distDir, version, darwinDigest string) {
	t.Helper()
	makeFormula := func(tag, ver, coreSHA string) string {
		p := filepath.Join(t.TempDir(), "managed-machine.rb")
		content := fmt.Sprintf(`class ManagedMachine < Formula
  url "https://github.com/qwts/managed-machine.git",
      using: :git,
      tag:   %q
  version %q

  resource "managed-machine-config" do
    url "https://github.com/qwts/managed-machine-config.git",
        using:    :git,
        tag:      %q,
        revision: "0123456789abcdef0123456789abcdef01234567"
  end

  resource "core" do
    url "https://github.com/qwts/managed-machine/releases/download/%s/managed-machine_%s_darwin_arm64.tar.gz"
    sha256 %q
  end
end
`, tag, ver, tag, tag, ver, coreSHA)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	var outBuf, errBuf bytes.Buffer
	validFormula := makeFormula("v"+version, version, darwinDigest)
	if code := run([]string{"verify", "-dist", distDir, "-formula", validFormula, "-require-pin"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("verify with matching formula pin failed (exit %d): %s", code, errBuf.String())
	}

	badCases := []struct {
		name      string
		formula   string
		wantField string
	}{
		{
			name:      "wrong tag",
			formula:   makeFormula("v0.8.12", version, darwinDigest),
			wantField: "tag",
		},
		{
			name:      "wrong version",
			formula:   makeFormula("v"+version, "0.8.12", darwinDigest),
			wantField: "version",
		},
		{
			name:      "wrong core sha256",
			formula:   makeFormula("v"+version, version, strings.Repeat("0", 64)),
			wantField: "sha256",
		},
	}
	for _, tc := range badCases {
		outBuf.Reset()
		errBuf.Reset()
		code := run([]string{"verify", "-dist", distDir, "-formula", tc.formula, "-require-pin"}, &outBuf, &errBuf)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", tc.name, code)
		}
		if !strings.Contains(errBuf.String(), tc.wantField) {
			t.Errorf("%s: stderr %q does not name %q", tc.name, errBuf.String(), tc.wantField)
		}
	}
}

func TestCLIUsageAndEdgeCases(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	if code := run(nil, &outBuf, &errBuf); code != 2 {
		t.Errorf("no args: exit %d, want 2", code)
	}
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"-h"}, &outBuf, &errBuf); code != 0 || !strings.Contains(outBuf.String(), "mm-release build") {
		t.Errorf("-h: exit %d, stdout %q", code, outBuf.String())
	}
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"unknown"}, &outBuf, &errBuf); code != 2 {
		t.Errorf("unknown subcommand: exit %d, want 2", code)
	}
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"build", "-version", "bad", "-out", t.TempDir()}, &outBuf, &errBuf); code != 1 {
		t.Errorf("bad version: exit %d, want 1", code)
	}
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"verify", "-dist", filepath.Join(t.TempDir(), "missing")}, &outBuf, &errBuf); code != 1 || !strings.Contains(errBuf.String(), "SHA256SUMS") {
		t.Errorf("missing dist: exit %d, stderr %q", code, errBuf.String())
	}
}

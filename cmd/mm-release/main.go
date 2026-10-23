// Command mm-release builds and verifies the five deterministic release
// archives and SHA256SUMS for managed-machine (S6 story R02; decisions D2, D3).
// It is release tooling, not part of the managed-machine CLI contract.
//
//	mm-release build -version <X.Y.Z> -out <dir>
//	mm-release verify -dist <dir> [-formula <path> -require-pin]
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qwts/managed-machine/internal/target"
)

const usageText = "usage: mm-release build -version <X.Y.Z> -out <dir>\n" +
	"       mm-release verify -dist <dir> [-formula <path> -require-pin]"

// archiveEpoch is the constant modification time written to every tar and zip
// entry (1980-01-01T00:00:00Z, the earliest instant MS-DOS zip represents; D3).
var archiveEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

var (
	semverRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type targetSpec struct {
	Target     target.Target
	OS         string
	Arch       string
	IsZip      bool
	BinaryName string
}

func releaseTargets() []targetSpec {
	all := target.All()
	out := make([]targetSpec, 0, len(all))
	for _, t := range all {
		parts := strings.SplitN(string(t), "/", 2)
		goos, goarch := parts[0], parts[1]
		isWin := goos == "windows"
		binName := "managed-machine"
		if isWin {
			binName = "managed-machine.exe"
		}
		out = append(out, targetSpec{
			Target:     t,
			OS:         goos,
			Arch:       goarch,
			IsZip:      isWin,
			BinaryName: binName,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OS != out[j].OS {
			return out[i].OS < out[j].OS
		}
		return out[i].Arch < out[j].Arch
	})
	return out
}

func (s targetSpec) archiveName(version string) string {
	ext := ".tar.gz"
	if s.IsZip {
		ext = ".zip"
	}
	return fmt.Sprintf("managed-machine_%s_%s_%s%s", version, s.OS, s.Arch, ext)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprintln(stdout, usageText)
		return 0
	case "build":
		if err := buildCmd(args[1:], stdout); err != nil {
			fmt.Fprintf(stderr, "mm-release build: %v\n", err)
			return 1
		}
		return 0
	case "verify":
		if err := verifyCmd(args[1:], stdout); err != nil {
			fmt.Fprintf(stderr, "mm-release verify: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "mm-release: unknown subcommand %q\n%s\n", args[0], usageText)
		return 2
	}
}

func buildCmd(args []string, stdout io.Writer) error {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprintln(stdout, usageText)
			return nil
		}
	}
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	version := fs.String("version", "", "release version X.Y.Z")
	outDir := fs.String("out", "", "output directory for archives and SHA256SUMS")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if !semverRe.MatchString(*version) {
		return fmt.Errorf("-version must be X.Y.Z (got %q)", *version)
	}
	if strings.TrimSpace(*outDir) == "" {
		return errors.New("-out is required")
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	licenseRaw, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil {
		return fmt.Errorf("read LICENSE: %w", err)
	}
	licenseBytes := bytes.ReplaceAll(licenseRaw, []byte("\r\n"), []byte("\n"))

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	workDir, err := os.MkdirTemp("", "mm-release-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	targets := releaseTargets()
	sumLines := make([]string, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, spec := range targets {
		wg.Add(1)
		go func(i int, spec targetSpec) {
			defer wg.Done()
			binDir := filepath.Join(workDir, spec.OS+"_"+spec.Arch)
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				errs[i] = err
				return
			}
			binPath := filepath.Join(binDir, spec.BinaryName)
			if err := compileTarget(repoRoot, *version, spec, binPath); err != nil {
				errs[i] = err
				return
			}
			binBytes, err := os.ReadFile(binPath)
			if err != nil {
				errs[i] = fmt.Errorf("read compiled binary for %s: %w", spec.Target, err)
				return
			}

			entries := []archiveEntry{
				{Name: "LICENSE", Mode: 0o644, Data: licenseBytes},
				{Name: spec.BinaryName, Mode: 0o755, Data: binBytes},
			}
			sort.Slice(entries, func(a, b int) bool {
				return entries[a].Name < entries[b].Name
			})

			var archiveBytes []byte
			if spec.IsZip {
				archiveBytes, err = buildZip(entries)
			} else {
				archiveBytes, err = buildTarGz(entries)
			}
			if err != nil {
				errs[i] = fmt.Errorf("archive %s: %w", spec.Target, err)
				return
			}

			archiveName := spec.archiveName(*version)
			archivePath := filepath.Join(*outDir, archiveName)
			if err := os.WriteFile(archivePath, archiveBytes, 0o644); err != nil {
				errs[i] = fmt.Errorf("write %s: %w", archiveName, err)
				return
			}
			digest := sha256Hex(archiveBytes)
			sumLines[i] = fmt.Sprintf("%s  %s", digest, archiveName)
		}(i, spec)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	sort.Slice(sumLines, func(i, j int) bool {
		return sumLines[i][66:] < sumLines[j][66:]
	})
	sumsContent := strings.Join(sumLines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(*outDir, "SHA256SUMS"), []byte(sumsContent), 0o644); err != nil {
		return fmt.Errorf("write SHA256SUMS: %w", err)
	}
	return nil
}

// compileTarget cross-builds cmd/managed-machine with the exact D3 command:
// CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-s -w -X main.version=<X.Y.Z>"
func compileTarget(repoRoot, version string, spec targetSpec, outPath string) error {
	ldflags := "-s -w -X main.version=" + version
	cmd := exec.Command("go", "build", "-buildvcs=false", "-ldflags", ldflags, "-o", outPath, "./cmd/managed-machine")
	cmd.Dir = repoRoot
	cmd.Env = cleanBuildEnv(os.Environ(), spec.OS, spec.Arch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("go build %s: %v: %s", spec.Target, err, msg)
		}
		return fmt.Errorf("go build %s: %w", spec.Target, err)
	}
	return nil
}

func cleanBuildEnv(base []string, goos, goarch string) []string {
	out := make([]string, 0, len(base)+5)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GOOS", "GOARCH", "GOARM", "GOAMD64", "GOARM64", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN":
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"GOOS="+goos,
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
		"GOFLAGS=-trimpath",
		"GOTOOLCHAIN=local",
	)
}

type archiveEntry struct {
	Name string
	Mode int64
	Data []byte
}

func buildTarGz(entries []archiveEntry) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	gz.Name = ""
	gz.Comment = ""
	gz.Extra = nil
	gz.ModTime = time.Time{}
	gz.OS = 255

	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.Name,
			Size:     int64(len(e.Data)),
			Mode:     e.Mode,
			Uid:      0,
			Gid:      0,
			Uname:    "",
			Gname:    "",
			ModTime:  archiveEpoch,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildZip(entries []archiveEntry) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestCompression)
	})
	for _, e := range entries {
		fh := &zip.FileHeader{
			Name:   e.Name,
			Method: zip.Deflate,
		}
		fh.SetModTime(archiveEpoch)
		// Leave Modified zero so archive/zip writes the MS-DOS 1980-01-01T00:00:00Z
		// timestamp in ModifiedDate/ModifiedTime without appending an extended
		// timestamp Extra field (D3 requires no extra fields).
		fh.Modified = time.Time{}
		fh.SetMode(fs.FileMode(e.Mode))
		fh.Extra = nil

		w, err := zw.CreateHeader(fh)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(e.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func verifyCmd(args []string, stdout io.Writer) error {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprintln(stdout, usageText)
			return nil
		}
	}
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	distDir := fs.String("dist", "", "directory containing SHA256SUMS and the five archives")
	formulaPath := fs.String("formula", "", "path to Formula/managed-machine.rb")
	requirePin := fs.Bool("require-pin", false, "require the formula's tag, version and core sha256 to match")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*distDir) == "" {
		return errors.New("-dist is required")
	}

	version, digests, err := verifyDist(*distDir)
	if err != nil {
		return err
	}
	if *requirePin {
		fp := *formulaPath
		if strings.TrimSpace(fp) == "" {
			fp = "Formula/managed-machine.rb"
			if _, statErr := os.Stat(fp); statErr != nil {
				if root, rootErr := findRepoRoot(); rootErr == nil {
					fp = filepath.Join(root, "Formula", "managed-machine.rb")
				}
			}
		}
		darwinArchive := fmt.Sprintf("managed-machine_%s_darwin_arm64.tar.gz", version)
		darwinSHA, ok := digests[darwinArchive]
		if !ok {
			return fmt.Errorf("missing darwin archive %s in SHA256SUMS", darwinArchive)
		}
		if err := verifyFormula(fp, version, darwinSHA); err != nil {
			return err
		}
	}
	return nil
}

func verifyDist(distDir string) (string, map[string]string, error) {
	sumsPath := filepath.Join(distDir, "SHA256SUMS")
	rawSums, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", nil, fmt.Errorf("SHA256SUMS: %w", err)
	}
	if len(rawSums) == 0 || bytes.ContainsRune(rawSums, '\r') || rawSums[len(rawSums)-1] != '\n' {
		return "", nil, errors.New("SHA256SUMS: must be non-empty LF-terminated lines")
	}
	lines := strings.Split(strings.TrimSuffix(string(rawSums), "\n"), "\n")
	targets := releaseTargets()
	if len(lines) != len(targets) {
		return "", nil, fmt.Errorf("SHA256SUMS: got %d lines, want %d", len(lines), len(targets))
	}

	type parsedLine struct {
		sum  string
		name string
	}
	parsed := make([]parsedLine, 0, len(lines))
	var version string
	for i, line := range lines {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || !sha256Re.MatchString(sum) || name == "" || strings.Contains(name, " ") {
			return "", nil, fmt.Errorf("SHA256SUMS:%d: malformed line %q", i+1, line)
		}
		v, err := extractArchiveVersion(name)
		if err != nil {
			return "", nil, fmt.Errorf("SHA256SUMS:%d: %s: %w", i+1, name, err)
		}
		if version == "" {
			version = v
		} else if v != version {
			return "", nil, fmt.Errorf("SHA256SUMS:%d: %s has version %s, want %s", i+1, name, v, version)
		}
		parsed = append(parsed, parsedLine{sum: sum, name: name})
	}

	wantNames := make([]string, 0, len(targets))
	for _, spec := range targets {
		wantNames = append(wantNames, spec.archiveName(version))
	}
	sort.Strings(wantNames)

	digests := make(map[string]string, len(parsed))
	for i, p := range parsed {
		if p.name != wantNames[i] {
			return "", nil, fmt.Errorf("SHA256SUMS:%d: got %s, want %s", i+1, p.name, wantNames[i])
		}
		archivePath := filepath.Join(distDir, p.name)
		data, err := os.ReadFile(archivePath)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", p.name, err)
		}
		gotSum := sha256Hex(data)
		if gotSum != p.sum {
			return "", nil, fmt.Errorf("SHA256SUMS: %s: digest mismatch: got %s, want %s", p.name, gotSum, p.sum)
		}
		digests[p.name] = gotSum
	}

	ents, err := os.ReadDir(distDir)
	if err != nil {
		return "", nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if name == "SHA256SUMS" {
			continue
		}
		if _, ok := digests[name]; !ok {
			return "", nil, fmt.Errorf("unexpected file %s in dist directory", name)
		}
	}
	return version, digests, nil
}

func extractArchiveVersion(name string) (string, error) {
	if !strings.HasPrefix(name, "managed-machine_") {
		return "", errors.New("unexpected archive prefix")
	}
	rest := strings.TrimPrefix(name, "managed-machine_")
	for _, spec := range releaseTargets() {
		suffix := fmt.Sprintf("_%s_%s.tar.gz", spec.OS, spec.Arch)
		if spec.IsZip {
			suffix = fmt.Sprintf("_%s_%s.zip", spec.OS, spec.Arch)
		}
		if strings.HasSuffix(rest, suffix) {
			v := strings.TrimSuffix(rest, suffix)
			if !semverRe.MatchString(v) {
				return "", fmt.Errorf("invalid version %q", v)
			}
			return v, nil
		}
	}
	return "", errors.New("unrecognized target suffix")
}

var (
	formulaTagRe      = regexp.MustCompile(`^\s*tag:\s*"([^"]*)"`)
	formulaVersionRe  = regexp.MustCompile(`^\s*version\s+"([^"]*)"`)
	formulaResourceRe = regexp.MustCompile(`^\s*resource\s+"([^"]*)"\s+do\s*$`)
	formulaEndRe      = regexp.MustCompile(`^\s*end\s*$`)
	formulaSHA256Re   = regexp.MustCompile(`^\s*sha256\s+"([^"]*)"`)
)

func verifyFormula(formulaPath, version, wantCoreSHA string) error {
	raw, err := os.ReadFile(formulaPath)
	if err != nil {
		return fmt.Errorf("formula %s: %w", formulaPath, err)
	}
	var (
		gotTag        string
		gotVersion    string
		inResource    string
		gotCoreSHA    string
		foundCoreRsrc bool
	)
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for _, line := range lines {
		if m := formulaResourceRe.FindStringSubmatch(line); m != nil {
			inResource = m[1]
			if inResource == "core" {
				foundCoreRsrc = true
			}
			continue
		}
		if inResource != "" {
			if formulaEndRe.MatchString(line) {
				inResource = ""
				continue
			}
			if inResource == "core" {
				if m := formulaSHA256Re.FindStringSubmatch(line); m != nil && gotCoreSHA == "" {
					gotCoreSHA = m[1]
				}
			}
			continue
		}
		if m := formulaTagRe.FindStringSubmatch(line); m != nil && gotTag == "" {
			gotTag = m[1]
		}
		if m := formulaVersionRe.FindStringSubmatch(line); m != nil && gotVersion == "" {
			gotVersion = m[1]
		}
	}

	wantTag := "v" + version
	if gotTag != wantTag {
		return fmt.Errorf("formula tag: got %q, want %q", gotTag, wantTag)
	}
	if gotVersion != version {
		return fmt.Errorf("formula version: got %q, want %q", gotVersion, version)
	}
	if !foundCoreRsrc || gotCoreSHA != wantCoreSHA {
		return fmt.Errorf("formula core sha256: got %q, want %q", gotCoreSHA, wantCoreSHA)
	}
	return nil
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) &&
			fileExists(filepath.Join(dir, "LICENSE")) &&
			fileExists(filepath.Join(dir, "cmd", "managed-machine", "main.go")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not locate managed-machine repository root (go.mod, LICENSE, cmd/managed-machine)")
		}
		dir = parent
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

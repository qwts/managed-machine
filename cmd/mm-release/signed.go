package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/macho"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var teamIDRe = regexp.MustCompile(`^[A-Z0-9]{10}$`)

func darwinArchiveName(version string) string {
	return fmt.Sprintf("managed-machine_%s_darwin_arm64.tar.gz", version)
}

// repackCmd replaces the darwin archive of a verified dist with one built from
// a signed tree, using the same deterministic tar writer as build, and copies
// the other four archives unchanged.
func repackCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("repack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	distDir := fs.String("dist", "", "verified unsigned dist directory")
	treeDir := fs.String("tree", "", "signed darwin archive tree")
	outDir := fs.String("out", "", "output directory for the signed dist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	for flagName, v := range map[string]string{"-dist": *distDir, "-tree": *treeDir, "-out": *outDir} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", flagName)
		}
	}
	if filepath.Clean(*distDir) == filepath.Clean(*outDir) {
		return errors.New("-out must differ from -dist")
	}
	version, digests, err := verifyDist(*distDir)
	if err != nil {
		return fmt.Errorf("unsigned dist: %w", err)
	}
	entries, err := readTree(*treeDir)
	if err != nil {
		return err
	}
	archive, err := buildTarGz(entries)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	darwinName := darwinArchiveName(version)
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		data := archive
		if name != darwinName {
			data, err = os.ReadFile(filepath.Join(*distDir, name))
			if err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(*outDir, name), data, 0o644); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s  %s", sha256Hex(data), name))
	}
	if err := os.WriteFile(filepath.Join(*outDir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, sha256Hex(archive))
	return nil
}

func readTree(root string) ([]archiveEntry, error) {
	var entries []archiveEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root || d.IsDir() {
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: only regular files may be archived", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		entries = append(entries, archiveEntry{Name: filepath.ToSlash(rel), Mode: mode, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name < entries[b].Name })
	return entries, nil
}

// verifySignedCmd proves a signed dist is the deterministic build plus
// signatures: the four other archives are byte-identical, and in the darwin
// archive every file is identical except the two Mach-O binaries, which must
// match outside their code signature and carry the expected Team ID. It
// prints the signed darwin archive digest.
func verifySignedCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify-signed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	unsignedDir := fs.String("unsigned", "", "deterministic dist directory")
	signedDir := fs.String("signed", "", "signed dist directory")
	team := fs.String("team", "", "Developer ID Team ID the signatures must name")
	formulaPath := fs.String("formula", "", "path to Formula/managed-machine.rb")
	requirePin := fs.Bool("require-pin", false, "require the formula's tag, version and core sha256 to match the signed darwin archive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*unsignedDir) == "" || strings.TrimSpace(*signedDir) == "" {
		return errors.New("-unsigned and -signed are required")
	}
	if !teamIDRe.MatchString(*team) {
		return fmt.Errorf("-team must be a 10-character Team ID (got %q)", *team)
	}
	uVersion, uDigests, err := verifyDist(*unsignedDir)
	if err != nil {
		return fmt.Errorf("unsigned dist: %w", err)
	}
	sVersion, sDigests, err := verifyDist(*signedDir)
	if err != nil {
		return fmt.Errorf("signed dist: %w", err)
	}
	if uVersion != sVersion {
		return fmt.Errorf("signed dist is version %s, unsigned is %s", sVersion, uVersion)
	}
	darwinName := darwinArchiveName(uVersion)
	for name, sum := range uDigests {
		if name == darwinName {
			continue
		}
		if sDigests[name] != sum {
			return fmt.Errorf("%s differs from the deterministic build", name)
		}
	}
	unsignedEntries, err := readTarGzFile(filepath.Join(*unsignedDir, darwinName))
	if err != nil {
		return fmt.Errorf("unsigned %s: %w", darwinName, err)
	}
	signedEntries, err := readTarGzFile(filepath.Join(*signedDir, darwinName))
	if err != nil {
		return fmt.Errorf("signed %s: %w", darwinName, err)
	}
	if err := compareSignedDarwin(unsignedEntries, signedEntries, *team); err != nil {
		return fmt.Errorf("%s: %w", darwinName, err)
	}
	signedSHA := sDigests[darwinName]
	if *requirePin {
		fp := *formulaPath
		if strings.TrimSpace(fp) == "" {
			return errors.New("-require-pin needs -formula")
		}
		if err := verifyFormula(fp, uVersion, signedSHA); err != nil {
			return err
		}
	}
	fmt.Fprintln(stdout, signedSHA)
	return nil
}

func compareSignedDarwin(unsigned, signed map[string]archiveEntry, team string) error {
	for name, u := range unsigned {
		s, ok := signed[name]
		if !ok {
			return fmt.Errorf("signed archive is missing %s", name)
		}
		if s.Mode != u.Mode {
			return fmt.Errorf("%s: mode %#o, want %#o", name, s.Mode, u.Mode)
		}
		if name == bundleMainExe || name == bundleExecutable {
			if err := machoEqualIgnoringSignature(u.Data, s.Data, team); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		if !bytes.Equal(u.Data, s.Data) {
			return fmt.Errorf("%s differs from the deterministic build", name)
		}
	}
	for name := range signed {
		if _, ok := unsigned[name]; ok {
			continue
		}
		if !signedOnlyEntries[name] {
			return fmt.Errorf("signed archive adds unexpected file %s", name)
		}
	}
	for _, required := range []string{bundleProfile, bundleSealed} {
		if _, ok := signed[required]; !ok {
			return fmt.Errorf("signed archive is missing %s", required)
		}
	}
	appID := team + "." + bundleID
	if !bytes.Contains(signed[bundleProfile].Data, []byte(appID)) {
		return fmt.Errorf("%s does not name %s", bundleProfile, appID)
	}
	return nil
}

func readTarGzFile(p string) (map[string]archiveEntry, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]archiveEntry{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s: only regular files are allowed", hdr.Name)
		}
		clean := path.Clean(hdr.Name)
		if clean != hdr.Name || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("%s: unsafe entry name", hdr.Name)
		}
		if _, dup := out[clean]; dup {
			return nil, fmt.Errorf("%s: duplicate entry", clean)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[clean] = archiveEntry{Name: clean, Mode: hdr.Mode, Data: data}
	}
	return out, nil
}

const (
	lcSegment64     = 0x19
	lcCodeSignature = 0x1d
	machHeader64Len = 32
)

// machoSignatureFields locates the code signature and the three length
// fields codesign rewrites when it replaces a signature in place: the
// LC_CODE_SIGNATURE datasize and the __LINKEDIT vmsize and filesize.
type machoSignatureFields struct {
	dataoff  int
	datasize int
	patch    [3][2]int
}

func readMachoSignatureFields(b []byte) (machoSignatureFields, error) {
	var out machoSignatureFields
	f, err := macho.NewFile(bytes.NewReader(b))
	if err != nil {
		return out, err
	}
	if f.Magic != macho.Magic64 || f.Cpu != macho.CpuArm64 {
		return out, errors.New("not an arm64 Mach-O")
	}
	le := binary.LittleEndian
	off := machHeader64Len
	var haveSig, haveLinkedit bool
	for i := uint32(0); i < f.Ncmd; i++ {
		if off+8 > len(b) {
			return out, errors.New("truncated load commands")
		}
		cmd, size := le.Uint32(b[off:]), int(le.Uint32(b[off+4:]))
		if size < 8 || off+size > len(b) {
			return out, errors.New("malformed load command")
		}
		switch cmd {
		case lcSegment64:
			if size < 72 {
				return out, errors.New("malformed segment command")
			}
			if strings.TrimRight(string(b[off+8:off+24]), "\x00") == "__LINKEDIT" {
				out.patch[1] = [2]int{off + 32, 8}
				out.patch[2] = [2]int{off + 48, 8}
				haveLinkedit = true
			}
		case lcCodeSignature:
			if size < 16 {
				return out, errors.New("malformed code signature command")
			}
			out.dataoff = int(le.Uint32(b[off+8:]))
			out.datasize = int(le.Uint32(b[off+12:]))
			out.patch[0] = [2]int{off + 12, 4}
			haveSig = true
		}
		off += size
	}
	if !haveSig || !haveLinkedit {
		return out, errors.New("no code signature")
	}
	if out.dataoff < off || out.dataoff+out.datasize != len(b) {
		return out, errors.New("code signature is not the end of the file")
	}
	return out, nil
}

func machoEqualIgnoringSignature(unsigned, signed []byte, team string) error {
	u, err := readMachoSignatureFields(unsigned)
	if err != nil {
		return fmt.Errorf("unsigned binary: %w", err)
	}
	s, err := readMachoSignatureFields(signed)
	if err != nil {
		return fmt.Errorf("signed binary: %w", err)
	}
	if u.dataoff != s.dataoff || u.patch != s.patch {
		return errors.New("signed binary layout differs from the deterministic build")
	}
	a := append([]byte(nil), unsigned[:u.dataoff]...)
	b := append([]byte(nil), signed[:s.dataoff]...)
	for _, p := range u.patch {
		clear(a[p[0] : p[0]+p[1]])
		clear(b[p[0] : p[0]+p[1]])
	}
	if !bytes.Equal(a, b) {
		return errors.New("code differs from the deterministic build")
	}
	if !bytes.Contains(signed[s.dataoff:], []byte(team)) {
		return fmt.Errorf("signature does not name Team ID %s", team)
	}
	return nil
}

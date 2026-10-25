package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/fetch"
	"github.com/qwts/managed-machine/internal/statedir"
)

const (
	providerVersion = "builtin"
	filePerm        = os.FileMode(0o755)
)

// Operation identifiers recorded in the plan and journal.
const (
	OpDownload = "download"
	OpInstall  = "install"
)

// Provider is the cross-platform archive provider (S7 N03; D4). Every field is
// an injectable seam so tests never touch the real home, real state directory
// or the real network.
type Provider struct {
	// GOOS is the operating system of the target (darwin, linux or windows).
	// When empty, it is derived from Request.Platform, the injected
	// environment, or runtime.GOOS.
	GOOS string
	// Environ returns the base environment, normally os.Environ.
	Environ func() []string
	// Client fetches the archive over HTTPS; nil means DefaultClient.
	Client *http.Client
	// StateDir is the platform state directory under which stage/<plan>,
	// plans/ and journal/ live.
	StateDir string
	// PlanID is the active plan identifier for stage/<plan> when Request.Plan
	// is empty.
	PlanID string
	// Writable reports whether a directory is writable by the current user;
	// nil uses command.IsWritableDir.
	Writable func(path string) bool
	// URLHosts supplies the allowed download hosts when Step is called
	// directly in a test without a preceding Plan call.
	URLHosts []string
	// MaxBytes caps the size of the downloaded archive and of each extracted
	// archive member; <= 0 uses fetch.MaxBytes.
	MaxBytes int64

	plannedHosts  []string
	plannedStage  string
	plannedBinDir string
}

// Name returns the provider name recipes use.
func (p *Provider) Name() string { return Name }

// Version reports the built-in provider version.
func (p *Provider) Version(context.Context) (string, error) {
	return providerVersion, nil
}

func (p *Provider) env() []string {
	if p.Environ != nil {
		return p.Environ()
	}
	return nil
}

func (p *Provider) envValue(key string) string {
	for _, kv := range p.env() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		if strings.EqualFold(kv[:i], key) {
			return kv[i+1:]
		}
	}
	return ""
}

func (p *Provider) goos(platform string) string {
	if i := strings.IndexByte(platform, '/'); i > 0 {
		return platform[:i]
	}
	if p.GOOS != "" {
		return p.GOOS
	}
	if p.envValue("USERPROFILE") != "" && p.envValue("HOME") == "" {
		return "windows"
	}
	return runtime.GOOS
}

func (p *Provider) maxBytes() int64 {
	if p.MaxBytes > 0 {
		return p.MaxBytes
	}
	return fetch.MaxBytes
}

func (p *Provider) home(platform string) (string, error) {
	if p.goos(platform) == "windows" {
		if h := p.envValue("USERPROFILE"); h != "" {
			return h, nil
		}
		if h := p.envValue("HOME"); h != "" {
			return h, nil
		}
		return "", fmt.Errorf("%w: the environment names no USERPROFILE, so ~/.local/bin is unknown", ErrUnsupported)
	}
	if h := p.envValue("HOME"); h != "" {
		return h, nil
	}
	if h := p.envValue("USERPROFILE"); h != "" {
		return h, nil
	}
	return "", fmt.Errorf("%w: the environment names no HOME, so ~/.local/bin is unknown", ErrUnsupported)
}

func (p *Provider) binDir(tgt, platform string) (string, error) {
	customRoot, err := ValidateTarget(tgt)
	if err != nil {
		return "", err
	}
	if tgt == "user" {
		home, err := p.home(platform)
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "bin"), nil
	}
	return filepath.Join(customRoot, "bin"), nil
}

func (p *Provider) checkBinDir(binDir string) error {
	info, err := os.Stat(binDir)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("%w: %s does not exist or is not writable", ErrPrivilege, binDir)
	case err != nil:
		return fmt.Errorf("%w: %s cannot be read: %v", ErrPrivilege, binDir, err)
	case !info.IsDir():
		return fmt.Errorf("%w: %s exists and is not a directory", ErrUnsupported, binDir)
	}
	writable := false
	if p.Writable != nil {
		writable = p.Writable(binDir)
	} else {
		writable = command.IsWritableDir(binDir)
	}
	if !writable {
		return fmt.Errorf("%w (%s); managed-machine never elevates", ErrPrivilege, binDir)
	}
	return nil
}

func (p *Provider) resolveStateDir(reqStateDir, platform string) string {
	if reqStateDir != "" {
		return filepath.Clean(reqStateDir)
	}
	if p.StateDir != "" {
		return filepath.Clean(p.StateDir)
	}
	if d, err := statedir.Path(p.goos(platform), p.envValue); err == nil {
		return d
	}
	return ""
}

func (p *Provider) resolveStageDir(reqStateDir, reqPlan, platform string) string {
	stateDir := p.resolveStateDir(reqStateDir, platform)
	if stateDir == "" {
		return ""
	}
	planID := reqPlan
	if planID == "" {
		planID = p.PlanID
	}
	if planID == "" {
		planID = "default"
	}
	return filepath.Join(stateDir, "stage", planID)
}

func (p *Provider) stageDir() string {
	if p.plannedStage != "" {
		return p.plannedStage
	}
	return p.resolveStageDir("", "", "")
}

func (p *Provider) hosts() []string {
	if len(p.plannedHosts) > 0 {
		return p.plannedHosts
	}
	return p.URLHosts
}

// Plan resolves an archive request without changing the machine or performing
// network access.
func (p *Provider) Plan(_ context.Context, r Request) (*Resolution, error) {
	if err := ValidatePlatform(r.Platform, r.Platforms); err != nil {
		return nil, err
	}
	opts, err := ParseOptions(r.Options)
	if err != nil {
		return nil, err
	}
	binDir, err := p.binDir(r.Target, r.Platform)
	if err != nil {
		return nil, err
	}
	if err := p.checkBinDir(binDir); err != nil {
		return nil, err
	}

	stateDir := p.resolveStateDir(r.StateDir, r.Platform)
	p.plannedHosts = opts.URLHosts
	p.plannedStage = p.resolveStageDir(r.StateDir, r.Plan, r.Platform)
	p.plannedBinDir = binDir

	preconditions := []plan.Precondition{
		{Kind: "target", Subject: "bin", Requirement: "writable at " + binDir, Observed: "writable"},
	}

	dests := make([]string, 0, len(opts.Files))
	existingCount := 0
	var firstExisting, nonRegular string
	for _, f := range opts.Files {
		base := BaseName(f)
		if !validBaseName(base) {
			continue
		}
		dest := filepath.Join(binDir, base)
		dests = append(dests, dest)
		info, statErr := os.Lstat(dest)
		switch {
		case statErr == nil:
			existingCount++
			if firstExisting == "" {
				firstExisting = dest
			}
			if !info.Mode().IsRegular() && nonRegular == "" {
				nonRegular = dest
			}
		case !os.IsNotExist(statErr):
			return nil, fmt.Errorf("inspect %s: %v", dest, statErr)
		}
	}

	pkgName := r.Software
	if pkgName == "" && len(opts.Files) > 0 {
		pkgName = BaseName(opts.Files[0])
	}

	if existingCount > 0 {
		if nonRegular != "" {
			return nil, provider.Kind(ErrOccupied, fmt.Sprintf("%s is already occupied by a non-regular entry", nonRegular))
		}
		if existingCount != len(opts.Files) {
			return nil, provider.Kind(ErrOccupied, fmt.Sprintf("%s is already occupied; remove it before installing", firstExisting))
		}
		if !p.receiptCovers(stateDir, r.Software, r.Target, opts.Version, dests) {
			return nil, provider.Kind(ErrOccupied, fmt.Sprintf("%s is already occupied; remove it before installing", firstExisting))
		}
		return &Resolution{
			Package:         pkgName,
			Version:         opts.Version,
			Installed:       true,
			ProviderVersion: providerVersion,
			Preconditions:   preconditions,
		}, nil
	}

	opName := "software:" + r.Software
	ops := []plan.Operation{
		{
			ID:        OpDownload,
			Operation: opName,
			Target:    r.Target,
			Action:    OpDownload,
			Provider:  Name,
			Params: map[string]string{
				"url":    opts.URL,
				"sha256": opts.SHA256,
				"format": opts.Format,
			},
		},
		{
			ID:        OpInstall,
			Operation: opName,
			Target:    r.Target,
			Action:    OpInstall,
			Provider:  Name,
			Params: map[string]string{
				"bin_dir":      binDir,
				"destinations": strings.Join(dests, ","),
				"files":        strings.Join(opts.Files, ","),
				"format":       opts.Format,
				"version":      opts.Version,
			},
		},
	}

	return &Resolution{
		Package:         pkgName,
		Version:         opts.Version,
		ProviderVersion: providerVersion,
		Preconditions:   preconditions,
		Operations:      ops,
	}, nil
}

// Check re-evaluates one planned precondition before apply runs.
func (p *Provider) Check(_ context.Context, c plan.Precondition) (string, error) {
	if c.Kind == "target" && c.Subject == "bin" {
		binDir := strings.TrimPrefix(c.Requirement, "writable at ")
		if err := p.checkBinDir(binDir); err != nil {
			return "not writable", err
		}
		return "writable", nil
	}
	return "", fmt.Errorf("unknown %s precondition %s %s", Name, c.Kind, c.Subject)
}

// Step returns the performer for one planned operation (download or install).
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	if op.Provider != Name {
		return nil, fmt.Errorf("%w: operation %s has provider %q", ErrUnsupported, op.ID, op.Provider)
	}
	kind := op.ID
	if kind != OpDownload && kind != OpInstall {
		kind = op.Action
	}
	switch kind {
	case OpDownload:
		rawURL := op.Params["url"]
		sha := strings.ToLower(strings.TrimSpace(op.Params["sha256"]))
		format := op.Params["format"]
		if rawURL == "" || !sha256Pattern.MatchString(sha) || (format != FormatTarGz && format != FormatZip) {
			return nil, fmt.Errorf("%w: malformed download operation %s", ErrUnsupported, op.ID)
		}
		hosts := p.hosts()
		if rawHosts := op.Params["url_hosts"]; rawHosts != "" {
			hosts = fetch.SplitHosts(rawHosts)
		}
		if len(hosts) == 0 {
			return nil, fmt.Errorf("%w: download operation %s has no url_hosts", ErrUnsupported, op.ID)
		}
		stageDir := op.Params["stage"]
		if stageDir == "" {
			stageDir = p.stageDir()
		}
		if stageDir == "" || !filepath.IsAbs(stageDir) {
			return nil, fmt.Errorf("%w: download operation %s has no absolute stage directory", ErrUnsupported, op.ID)
		}
		return &downloadStep{
			p:        p,
			url:      rawURL,
			sha256:   sha,
			format:   format,
			hosts:    hosts,
			stageDir: filepath.Clean(stageDir),
		}, nil

	case OpInstall:
		format := op.Params["format"]
		version := strings.TrimSpace(op.Params["version"])
		files := SplitFiles(op.Params["files"])
		binDir := op.Params["bin_dir"]
		if binDir == "" {
			binDir = p.plannedBinDir
		}
		if (format != FormatTarGz && format != FormatZip) || version == "" || len(files) == 0 || binDir == "" || !filepath.IsAbs(binDir) {
			return nil, fmt.Errorf("%w: malformed install operation %s", ErrUnsupported, op.ID)
		}
		stageDir := op.Params["stage"]
		if stageDir == "" {
			stageDir = p.stageDir()
		}
		if stageDir == "" || !filepath.IsAbs(stageDir) {
			return nil, fmt.Errorf("%w: install operation %s has no absolute stage directory", ErrUnsupported, op.ID)
		}
		return &installStep{
			p:        p,
			format:   format,
			version:  version,
			files:    files,
			binDir:   filepath.Clean(binDir),
			stageDir: filepath.Clean(stageDir),
		}, nil
	}
	return nil, fmt.Errorf("%w: unknown %s operation %q", ErrUnsupported, Name, op.ID)
}

type downloadStep struct {
	p        *Provider
	url      string
	sha256   string
	format   string
	hosts    []string
	stageDir string
}

func (s *downloadStep) artifactPath() string {
	return filepath.Join(s.stageDir, "artifact."+s.format)
}

func (s *downloadStep) Apply(ctx context.Context) (execute.Evidence, error) {
	dest := s.artifactPath()
	if err := fetch.Download(ctx, s.p.Client, s.url, s.hosts, s.p.maxBytes(), dest); err != nil {
		_ = os.RemoveAll(s.stageDir)
		return nil, err
	}
	return execute.Evidence{"url": s.url, "artifact": dest, "format": s.format}, nil
}

func (s *downloadStep) Verify(_ context.Context) (execute.Evidence, error) {
	dest := s.artifactPath()
	got, err := fetch.FileSHA256(dest)
	if err != nil {
		_ = os.RemoveAll(s.stageDir)
		return nil, err
	}
	ev := execute.Evidence{"sha256": got}
	if got != s.sha256 {
		_ = os.RemoveAll(s.stageDir)
		return ev, provider.Kind(ErrArtifactMismatch, fmt.Sprintf("downloaded artifact sha256 is %s, expected %s", got, s.sha256))
	}
	return ev, nil
}

type installStep struct {
	p              *Provider
	format         string
	version        string
	files          []string
	binDir         string
	stageDir       string
	appliedDigests map[string]string
}

type extractedMember struct {
	member  string
	tmpPath string
	dest    string
	sha256  string
}

func (s *installStep) Apply(_ context.Context) (execute.Evidence, error) {
	seenBase := make(map[string]string, len(s.files))
	for _, f := range s.files {
		if err := ValidateMemberPath(f); err != nil {
			_ = os.RemoveAll(s.stageDir)
			return nil, err
		}
		base := BaseName(f)
		if prev, exists := seenBase[base]; exists {
			_ = os.RemoveAll(s.stageDir)
			return nil, fmt.Errorf("%w: members %q and %q share destination base name %q", ErrUnsupported, prev, f, base)
		}
		seenBase[base] = f
	}

	artifact := filepath.Join(s.stageDir, "artifact."+s.format)
	maxBytes := s.p.maxBytes()

	var (
		extracted []extractedMember
		err       error
	)
	switch s.format {
	case FormatTarGz:
		extracted, err = s.extractTarGz(artifact, maxBytes)
	case FormatZip:
		extracted, err = s.extractZip(artifact, maxBytes)
	default:
		err = fmt.Errorf("%w: unsupported format %q", ErrUnsupported, s.format)
	}
	if err != nil {
		cleanupTmpFiles(extracted)
		_ = os.RemoveAll(s.stageDir)
		return nil, err
	}

	dests := make([]string, 0, len(extracted))
	fileDigests := make(map[string]string, len(extracted))
	appliedDigests := make(map[string]string, len(extracted))
	var renamed []string

	for _, m := range extracted {
		if err := os.Rename(m.tmpPath, m.dest); err != nil {
			cleanupTmpFiles(extracted)
			for _, d := range renamed {
				_ = os.Remove(d)
			}
			_ = os.RemoveAll(s.stageDir)
			return nil, fmt.Errorf("place %s: %v", m.dest, err)
		}
		renamed = append(renamed, m.dest)
		dests = append(dests, m.dest)
		fileDigests[m.dest] = m.sha256
		appliedDigests[m.dest] = m.sha256
	}

	_ = os.RemoveAll(s.stageDir)
	s.appliedDigests = appliedDigests

	return execute.Evidence{
		"destinations": dests,
		"files":        fileDigests,
		"file_mode":    "0755",
		"format":       s.format,
		"version":      s.version,
	}, nil
}

func (s *installStep) Verify(_ context.Context) (execute.Evidence, error) {
	if len(s.appliedDigests) != len(s.files) {
		return nil, fmt.Errorf("missing apply evidence digests for %s", s.binDir)
	}
	dests := make([]string, 0, len(s.files))
	verified := make(map[string]string, len(s.files))
	for _, f := range s.files {
		dest := filepath.Join(s.binDir, BaseName(f))
		want, ok := s.appliedDigests[dest]
		if !ok || !sha256Pattern.MatchString(want) {
			return nil, fmt.Errorf("missing apply evidence digest for %s", dest)
		}
		info, err := os.Lstat(dest)
		if err != nil {
			return nil, fmt.Errorf("stat installed file %s: %v", dest, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("installed file %s is not a regular file", dest)
		}
		if runtime.GOOS != "windows" {
			if info.Mode().Perm() != filePerm {
				return nil, fmt.Errorf("installed file %s has mode %04o, expected %04o", dest, info.Mode().Perm(), filePerm)
			}
		} else {
			if info.Mode().Perm()&0o400 == 0 {
				return nil, fmt.Errorf("installed file %s is not readable (mode %04o)", dest, info.Mode().Perm())
			}
		}
		got, err := fetch.FileSHA256(dest)
		if err != nil {
			return nil, fmt.Errorf("hash installed file %s: %v", dest, err)
		}
		if got != want {
			return execute.Evidence{"destinations": dests, "files": verified},
				fmt.Errorf("installed file %s sha256 is %s, expected %s from apply evidence", dest, got, want)
		}
		dests = append(dests, dest)
		verified[dest] = got
	}
	return execute.Evidence{
		"destinations": dests,
		"files":        verified,
		"file_mode":    "0755",
		"version":      s.version,
	}, nil
}

func cleanupTmpFiles(extracted []extractedMember) {
	for _, m := range extracted {
		if m.tmpPath != "" {
			_ = os.Remove(m.tmpPath)
		}
	}
}

// validateArchiveHeaderPath checks that an entry name inside an archive is
// never absolute, volume-prefixed, backslash-separated, or a ".." traversal.
func validateArchiveHeaderPath(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: archive contains an entry with an empty path", ErrUnsupported)
	}
	if strings.ContainsAny(raw, "\\\x00\r\n") {
		return fmt.Errorf("%w: archive entry %q contains invalid characters", ErrUnsupported, raw)
	}
	if strings.HasPrefix(raw, "/") {
		return fmt.Errorf("%w: archive entry %q must not be an absolute path", ErrUnsupported, raw)
	}
	if (len(raw) >= 2 && raw[1] == ':') || filepath.VolumeName(raw) != "" {
		return fmt.Errorf("%w: archive entry %q must not contain a volume prefix", ErrUnsupported, raw)
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return fmt.Errorf("%w: archive entry %q contains \"..\" path traversal", ErrUnsupported, raw)
		}
	}
	return nil
}

func (s *installStep) extractTarGz(artifact string, maxBytes int64) ([]extractedMember, error) {
	f, err := os.Open(artifact)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %v", artifact, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%w: archive is not a valid tar.gz: %v", ErrUnsupported, err)
	}
	defer gz.Close()

	wanted := make(map[string]bool, len(s.files))
	for _, m := range s.files {
		wanted[m] = true
	}
	found := make(map[string]extractedMember, len(s.files))
	var created []extractedMember

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive is not a valid tar.gz: %v", ErrUnsupported, err)
		}
		if err := validateArchiveHeaderPath(hdr.Name); err != nil {
			cleanupTmpFiles(created)
			return nil, err
		}

		trimmed := strings.TrimSuffix(hdr.Name, "/")
		matchName := ""
		if wanted[hdr.Name] {
			matchName = hdr.Name
		} else if wanted[trimmed] {
			matchName = trimmed
		}
		if matchName == "" {
			continue
		}

		if _, dup := found[matchName]; dup {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive contains duplicate member %q", ErrUnsupported, matchName)
		}

		switch hdr.Typeflag {
		case tar.TypeSymlink:
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q is a symlink, not a regular file", ErrUnsupported, matchName)
		case tar.TypeLink:
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q is a hard link, not a regular file", ErrUnsupported, matchName)
		case tar.TypeDir:
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q is a directory, not a regular file", ErrUnsupported, matchName)
		case tar.TypeReg, tar.TypeRegA:
			if hdr.FileInfo().IsDir() || strings.HasSuffix(hdr.Name, "/") || !hdr.FileInfo().Mode().IsRegular() {
				cleanupTmpFiles(created)
				return nil, fmt.Errorf("%w: archive member %q is not a regular file", ErrUnsupported, matchName)
			}
		default:
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q is not a regular file (type %d)", ErrUnsupported, matchName, hdr.Typeflag)
		}

		if hdr.Size < 0 || hdr.Size > maxBytes {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q size %d exceeds maximum %d bytes", ErrUnsupported, matchName, hdr.Size, maxBytes)
		}

		em, err := s.writeMemberTmp(matchName, tr, maxBytes)
		if err != nil {
			cleanupTmpFiles(created)
			return nil, err
		}
		created = append(created, em)
		found[matchName] = em
	}

	out := make([]extractedMember, 0, len(s.files))
	for _, m := range s.files {
		em, ok := found[m]
		if !ok {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: listed member %q is missing from archive", ErrUnsupported, m)
		}
		out = append(out, em)
	}
	return out, nil
}

func (s *installStep) extractZip(artifact string, maxBytes int64) ([]extractedMember, error) {
	f, err := os.Open(artifact)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %v", artifact, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat archive %s: %v", artifact, err)
	}
	var magic [4]byte
	if n, _ := io.ReadFull(f, magic[:]); n < 4 ||
		(!bytes.Equal(magic[:], []byte("PK\x03\x04")) && !bytes.Equal(magic[:], []byte("PK\x05\x06"))) {
		return nil, fmt.Errorf("%w: archive is not a valid zip", ErrUnsupported)
	}

	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return nil, fmt.Errorf("%w: archive is not a valid zip: %v", ErrUnsupported, err)
	}

	wanted := make(map[string]bool, len(s.files))
	for _, m := range s.files {
		wanted[m] = true
	}
	found := make(map[string]extractedMember, len(s.files))
	var created []extractedMember

	for _, zf := range zr.File {
		if err := validateArchiveHeaderPath(zf.Name); err != nil {
			cleanupTmpFiles(created)
			return nil, err
		}

		trimmed := strings.TrimSuffix(zf.Name, "/")
		matchName := ""
		if wanted[zf.Name] {
			matchName = zf.Name
		} else if wanted[trimmed] {
			matchName = trimmed
		}
		if matchName == "" {
			continue
		}

		if _, dup := found[matchName]; dup {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive contains duplicate member %q", ErrUnsupported, matchName)
		}

		fi := zf.FileInfo()
		mode := fi.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q is a symlink, not a regular file", ErrUnsupported, matchName)
		case fi.IsDir() || strings.HasSuffix(zf.Name, "/"):
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q is a directory, not a regular file", ErrUnsupported, matchName)
		case !mode.IsRegular():
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q is not a regular file", ErrUnsupported, matchName)
		}

		if zf.UncompressedSize64 > uint64(maxBytes) {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: archive member %q size %d exceeds maximum %d bytes", ErrUnsupported, matchName, zf.UncompressedSize64, maxBytes)
		}

		rc, err := zf.Open()
		if err != nil {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: open zip member %q: %v", ErrUnsupported, matchName, err)
		}
		em, copyErr := s.writeMemberTmp(matchName, rc, maxBytes)
		closeErr := rc.Close()
		if copyErr != nil {
			cleanupTmpFiles(created)
			return nil, copyErr
		}
		if closeErr != nil {
			cleanupTmpFiles(created)
			if em.tmpPath != "" {
				_ = os.Remove(em.tmpPath)
			}
			return nil, fmt.Errorf("%w: read zip member %q: %v", ErrUnsupported, matchName, closeErr)
		}
		created = append(created, em)
		found[matchName] = em
	}

	out := make([]extractedMember, 0, len(s.files))
	for _, m := range s.files {
		em, ok := found[m]
		if !ok {
			cleanupTmpFiles(created)
			return nil, fmt.Errorf("%w: listed member %q is missing from archive", ErrUnsupported, m)
		}
		out = append(out, em)
	}
	return out, nil
}

func (s *installStep) writeMemberTmp(member string, r io.Reader, maxBytes int64) (extractedMember, error) {
	base := BaseName(member)
	dest := filepath.Join(s.binDir, base)
	tmpPath := dest + ".mm-tmp"

	out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return extractedMember{}, fmt.Errorf("create %s: %v", tmpPath, err)
	}

	h := sha256.New()
	limited := io.LimitReader(r, maxBytes+1)
	n, copyErr := io.Copy(io.MultiWriter(out, h), limited)
	if copyErr != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return extractedMember{}, fmt.Errorf("%w: extract member %q: %v", ErrUnsupported, member, copyErr)
	}
	if n > maxBytes {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return extractedMember{}, fmt.Errorf("%w: archive member %q exceeds maximum %d bytes", ErrUnsupported, member, maxBytes)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return extractedMember{}, fmt.Errorf("sync %s: %v", tmpPath, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return extractedMember{}, fmt.Errorf("close %s: %v", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, filePerm); err != nil && runtime.GOOS != "windows" {
		_ = os.Remove(tmpPath)
		return extractedMember{}, fmt.Errorf("chmod %s: %v", tmpPath, err)
	}

	return extractedMember{
		member:  member,
		tmpPath: tmpPath,
		dest:    dest,
		sha256:  hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// receiptCovers reports whether the latest applied receipt for (software,
// target) in stateDir was for version and named every path in dests. It reads
// existing plan and journal files directly without acquiring the store lock or
// creating any directory.
func (p *Provider) receiptCovers(stateDir, software, tgt, version string, dests []string) bool {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(software) == "" || strings.TrimSpace(tgt) == "" {
		return false
	}
	plansDir := filepath.Join(stateDir, "plans")
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		return false
	}

	type candidate struct {
		doc       plan.Document
		receiptAt string
	}
	var applied []candidate

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "plan-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(plansDir, name))
		if err != nil {
			continue
		}
		var doc plan.Document
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if doc.Command != "software --install" || doc.Software != software || doc.Target != tgt || doc.State != plan.Applied {
			continue
		}
		journalPath := filepath.Join(stateDir, "journal", doc.Plan+".jsonl")
		receiptAt, ok := readAppliedJournal(journalPath)
		if !ok {
			continue
		}
		if receiptAt == "" {
			receiptAt = doc.CreatedAt
		}
		applied = append(applied, candidate{doc: doc, receiptAt: receiptAt})
	}

	if len(applied) == 0 {
		return false
	}

	sort.Slice(applied, func(i, j int) bool {
		ti, errI := time.Parse(time.RFC3339, applied[i].doc.CreatedAt)
		tj, errJ := time.Parse(time.RFC3339, applied[j].doc.CreatedAt)
		if errI == nil && errJ == nil && !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if applied[i].receiptAt != applied[j].receiptAt {
			return applied[i].receiptAt < applied[j].receiptAt
		}
		return applied[i].doc.Plan < applied[j].doc.Plan
	})

	// Walk from the latest applied plan backwards, skipping 0-operation
	// idempotent reruns to inspect the latest applied plan that installed or
	// declared operations for this (software, target).
	for i := len(applied) - 1; i >= 0; i-- {
		doc := applied[i].doc
		ver, covered := extractPlanCoverage(stateDir, doc)
		if len(doc.Operations) == 0 && ver == "" && len(covered) == 0 {
			continue
		}
		if ver != version {
			return false
		}
		for _, d := range dests {
			if !covered[filepath.Clean(d)] {
				return false
			}
		}
		return true
	}
	return false
}

func readAppliedJournal(journalPath string) (string, bool) {
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec plan.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Action == plan.ActionReceipt && rec.Outcome == plan.OutcomeApplied {
			return rec.At, true
		}
		return "", false
	}
	return "", false
}

func extractPlanCoverage(stateDir string, doc plan.Document) (string, map[string]bool) {
	covered := make(map[string]bool)
	var version string

	for _, op := range doc.Operations {
		if v := strings.TrimSpace(op.Params["version"]); v != "" {
			version = v
		}
		if rawDests := op.Params["destinations"]; rawDests != "" {
			for _, d := range SplitFiles(rawDests) {
				covered[filepath.Clean(d)] = true
			}
		}
		if d := strings.TrimSpace(op.Params["destination"]); d != "" {
			covered[filepath.Clean(d)] = true
		}
		binDir := strings.TrimSpace(op.Params["bin_dir"])
		if binDir != "" {
			for _, f := range SplitFiles(op.Params["files"]) {
				base := BaseName(f)
				if validBaseName(base) {
					covered[filepath.Clean(filepath.Join(binDir, base))] = true
				}
			}
		}
	}

	journalPath := filepath.Join(stateDir, "journal", doc.Plan+".jsonl")
	if raw, err := os.ReadFile(journalPath); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec plan.Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Evidence == nil {
				continue
			}
			if v, ok := rec.Evidence["version"].(string); ok && v != "" && version == "" {
				version = v
			}
			if ds, ok := rec.Evidence["destinations"].([]any); ok {
				for _, item := range ds {
					if s, ok := item.(string); ok && s != "" {
						covered[filepath.Clean(s)] = true
					}
				}
			}
			if fm, ok := rec.Evidence["files"].(map[string]any); ok {
				for k := range fm {
					if k != "" {
						covered[filepath.Clean(k)] = true
					}
				}
			}
		}
	}

	return version, covered
}

var _ provider.Provider = (*Provider)(nil)

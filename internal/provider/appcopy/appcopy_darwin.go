//go:build darwin

package appcopy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/codesign"
	"github.com/qwts/managed-machine/internal/provider/fetch"
	"github.com/qwts/managed-machine/internal/statedir"
)

const (
	// HdiutilPath is the absolute path to the macOS disk image utility.
	HdiutilPath = "/usr/bin/hdiutil"
	// DittoPath is the absolute path to the macOS archive/bundle copier.
	DittoPath = "/usr/bin/ditto"
	// PlutilPath is the absolute path to the macOS property list utility.
	PlutilPath = "/usr/bin/plutil"

	providerVersion = "builtin"
)

// Operation identifiers recorded in the plan and journal.
const (
	OpDownload = "download"
	OpStage    = "stage"
	OpPlace    = "place"
)

// Provider is the darwin/arm64 app-copy provider. Every field is a seam for
// tests so no test ever invokes host tools, reaches the network or touches
// /Applications.
type Provider struct {
	// Run starts hdiutil, ditto, plutil, codesign and spctl by absolute path.
	Run Runner
	// Environ returns the base environment, normally os.Environ.
	Environ func() []string
	// Client fetches the artifact over HTTPS; nil means DefaultClient.
	Client *http.Client
	// StateDir is the platform state directory under which stage/<plan> lives.
	StateDir string
	// PlanID is the active plan identifier for stage/<plan> when Request.Plan
	// is empty.
	PlanID string
	// AppDir overrides the global /Applications directory in tests.
	AppDir string
	// Writable reports whether a directory is writable by the current user;
	// nil uses command.IsWritableDir.
	Writable func(path string) bool
	// URLHosts supplies the allowed download hosts when Step is called
	// directly in a test without a preceding Plan call.
	URLHosts []string

	plannedHosts []string
	plannedStage string
	plannedDest  string
}

// Name returns the provider name recipes use.
func (p *Provider) Name() string { return Name }

// Version reports the built-in provider version.
func (p *Provider) Version(context.Context) (string, error) {
	return providerVersion, nil
}

func (p *Provider) runner() Runner {
	if p.Run != nil {
		return p.Run
	}
	return Exec
}

func (p *Provider) env() []string {
	if p.Environ != nil {
		return p.Environ()
	}
	return nil
}

func (p *Provider) envValue(key string) string {
	prefix := key + "="
	for _, kv := range p.env() {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func (p *Provider) run(ctx context.Context, tool string, args ...string) (Result, error) {
	return p.runner()(ctx, tool, args, p.env())
}

func (p *Provider) appDir(target string) (string, error) {
	if err := ValidateTarget(target); err != nil {
		return "", err
	}
	if target == "global" {
		if p.AppDir != "" {
			return filepath.Clean(p.AppDir), nil
		}
		return "/Applications", nil
	}
	home := p.envValue("HOME")
	if home == "" {
		return "", fmt.Errorf("%w: the environment names no HOME, so the user Applications directory is unknown", ErrUnsupported)
	}
	return filepath.Join(home, "Applications"), nil
}

func (p *Provider) checkAppDir(appDir string) error {
	if info, err := os.Stat(appDir); err == nil && !info.IsDir() {
		return fmt.Errorf("%w: %s exists and is not a directory", ErrUnsupported, appDir)
	}
	writable := false
	if p.Writable != nil {
		writable = p.Writable(appDir)
	} else {
		writable = command.IsWritableDir(appDir)
	}
	if !writable {
		return fmt.Errorf("%w (%s); managed-machine never elevates", ErrPrivilege, appDir)
	}
	return nil
}

func (p *Provider) resolveStageDir(reqStateDir, reqPlan string) string {
	stateDir := reqStateDir
	if stateDir == "" {
		stateDir = p.StateDir
	}
	if stateDir == "" {
		if d, err := statedir.Path("darwin", p.envValue); err == nil {
			stateDir = d
		}
	}
	planID := reqPlan
	if planID == "" {
		planID = p.PlanID
	}
	if planID == "" {
		planID = "default"
	}
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "stage", planID)
}

func (p *Provider) stageDir() string {
	if p.plannedStage != "" {
		return p.plannedStage
	}
	return p.resolveStageDir("", "")
}

func (p *Provider) hosts() []string {
	if len(p.plannedHosts) > 0 {
		return p.plannedHosts
	}
	return p.URLHosts
}

// Plan resolves an app-copy request without changing the machine or
// performing network access.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	if err := ValidatePlatform(r.Platform, r.Platforms); err != nil {
		return nil, err
	}
	opts, err := ParseOptions(r.Options)
	if err != nil {
		return nil, err
	}
	appDir, err := p.appDir(r.Target)
	if err != nil {
		return nil, err
	}
	if err := p.checkAppDir(appDir); err != nil {
		return nil, err
	}

	dest := filepath.Join(appDir, opts.AppName)
	p.plannedHosts = opts.URLHosts
	p.plannedStage = p.resolveStageDir(r.StateDir, r.Plan)
	p.plannedDest = dest

	preconditions := []plan.Precondition{
		{Kind: "target", Subject: "applications", Requirement: "writable at " + appDir, Observed: "writable"},
	}

	info, statErr := os.Lstat(dest)
	switch {
	case statErr == nil:
		if !info.IsDir() {
			return nil, provider.Kind(ErrOccupied, fmt.Sprintf("%s is already occupied by a non-bundle entry", dest))
		}
		if err := p.verifyBundle(ctx, dest, opts.TeamID, opts.Version); err != nil {
			return nil, provider.Kind(ErrOccupied, fmt.Sprintf("%s is already occupied (%v); remove or adopt it before installing", dest, err))
		}
		return &Resolution{
			Package:         opts.AppName,
			Version:         opts.Version,
			Installed:       true,
			ProviderVersion: providerVersion,
			Preconditions:   preconditions,
		}, nil
	case !os.IsNotExist(statErr):
		return nil, fmt.Errorf("inspect %s: %v", dest, statErr)
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
			ID:        OpStage,
			Operation: opName,
			Target:    r.Target,
			Action:    OpStage,
			Provider:  Name,
			Params: map[string]string{
				"app_name": opts.AppName,
				"format":   opts.Format,
				"team_id":  opts.TeamID,
				"version":  opts.Version,
			},
		},
		{
			ID:        OpPlace,
			Operation: opName,
			Target:    r.Target,
			Action:    OpPlace,
			Provider:  Name,
			Params: map[string]string{
				"app_name":    opts.AppName,
				"destination": dest,
				"format":      opts.Format,
				"team_id":     opts.TeamID,
				"version":     opts.Version,
			},
		},
	}

	return &Resolution{
		Package:         opts.AppName,
		Version:         opts.Version,
		ProviderVersion: providerVersion,
		Preconditions:   preconditions,
		Operations:      ops,
	}, nil
}

// Check re-evaluates one planned precondition before apply runs.
func (p *Provider) Check(_ context.Context, c plan.Precondition) (string, error) {
	if c.Kind == "target" && c.Subject == "applications" {
		appDir := strings.TrimPrefix(c.Requirement, "writable at ")
		if err := p.checkAppDir(appDir); err != nil {
			return "not writable", err
		}
		return "writable", nil
	}
	return "", fmt.Errorf("unknown %s precondition %s %s", Name, c.Kind, c.Subject)
}

// Step returns the performer for one planned operation (download, stage or
// place).
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	if op.Provider != Name {
		return nil, fmt.Errorf("%w: operation %s has provider %q", ErrUnsupported, op.ID, op.Provider)
	}
	kind := op.ID
	if kind != OpDownload && kind != OpStage && kind != OpPlace {
		kind = op.Action
	}
	switch kind {
	case OpDownload:
		rawURL := op.Params["url"]
		sha := strings.ToLower(strings.TrimSpace(op.Params["sha256"]))
		format := op.Params["format"]
		if rawURL == "" || !sha256Pattern.MatchString(sha) || (format != FormatDMG && format != FormatZip) {
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
			stageDir: stageDir,
		}, nil

	case OpStage:
		appName := op.Params["app_name"]
		format := op.Params["format"]
		teamID := op.Params["team_id"]
		version := op.Params["version"]
		if !ValidAppName(appName) || (format != FormatDMG && format != FormatZip) || !teamIDPattern.MatchString(teamID) || version == "" {
			return nil, fmt.Errorf("%w: malformed stage operation %s", ErrUnsupported, op.ID)
		}
		stageDir := op.Params["stage"]
		if stageDir == "" {
			stageDir = p.stageDir()
		}
		if stageDir == "" || !filepath.IsAbs(stageDir) {
			return nil, fmt.Errorf("%w: stage operation %s has no absolute stage directory", ErrUnsupported, op.ID)
		}
		return &stageStep{
			p:        p,
			appName:  appName,
			format:   format,
			teamID:   teamID,
			version:  version,
			stageDir: stageDir,
		}, nil

	case OpPlace:
		appName := op.Params["app_name"]
		dest := op.Params["destination"]
		if dest == "" {
			dest = p.plannedDest
		}
		format := op.Params["format"]
		teamID := op.Params["team_id"]
		version := op.Params["version"]
		if !ValidAppName(appName) || dest == "" || !filepath.IsAbs(dest) ||
			(format != FormatDMG && format != FormatZip) || !teamIDPattern.MatchString(teamID) || version == "" {
			return nil, fmt.Errorf("%w: malformed place operation %s", ErrUnsupported, op.ID)
		}
		stageDir := op.Params["stage"]
		if stageDir == "" {
			stageDir = p.stageDir()
		}
		if stageDir == "" || !filepath.IsAbs(stageDir) {
			return nil, fmt.Errorf("%w: place operation %s has no absolute stage directory", ErrUnsupported, op.ID)
		}
		return &placeStep{
			p:        p,
			appName:  appName,
			dest:     dest,
			format:   format,
			teamID:   teamID,
			version:  version,
			stageDir: stageDir,
		}, nil
	}
	return nil, fmt.Errorf("%w: unknown %s operation %q", ErrUnsupported, Name, op.ID)
}

func (p *Provider) verifyBundle(ctx context.Context, bundle, teamID, wantVersion string) error {
	if err := codesign.VerifyWithEnv(ctx, p.runner(), p.env(), bundle, teamID, false); err != nil {
		return err
	}
	gotVersion, err := p.bundleVersion(ctx, bundle)
	if err != nil {
		return err
	}
	if gotVersion != wantVersion {
		return fmt.Errorf("%s CFBundleShortVersionString is %q, expected %q", bundle, gotVersion, wantVersion)
	}
	return nil
}

func (p *Provider) bundleVersion(ctx context.Context, bundle string) (string, error) {
	plist := filepath.Join(bundle, "Contents", "Info.plist")
	r, err := p.run(ctx, PlutilPath, "-extract", "CFBundleShortVersionString", "raw", plist)
	if err != nil {
		return "", err
	}
	if r.Exit != 0 {
		return "", fmt.Errorf("plutil exited %d on %s: %s", r.Exit, plist, firstLine(r.Stderr, r.Stdout))
	}
	v := strings.TrimSpace(string(r.Stdout))
	if v == "" {
		return "", fmt.Errorf("plutil returned empty CFBundleShortVersionString for %s", plist)
	}
	return v, nil
}

func (p *Provider) cleanupStage(ctx context.Context, stageDir, format string) {
	if format == FormatDMG {
		mnt := filepath.Join(stageDir, "mnt")
		if _, err := os.Stat(mnt); err == nil {
			_, _ = p.run(ctx, HdiutilPath, "detach", mnt)
		}
	}
	if stageDir != "" {
		_ = os.RemoveAll(stageDir)
	}
}

func stagedBundlePath(stageDir, format, appName string) string {
	if format == FormatDMG {
		return filepath.Join(stageDir, "mnt", appName)
	}
	return filepath.Join(stageDir, "extract", appName)
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
	if err := fetch.Download(ctx, s.p.Client, s.url, s.hosts, fetch.MaxBytes, dest); err != nil {
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

type stageStep struct {
	p        *Provider
	appName  string
	format   string
	teamID   string
	version  string
	stageDir string
}

func (s *stageStep) Apply(ctx context.Context) (execute.Evidence, error) {
	artifact := filepath.Join(s.stageDir, "artifact."+s.format)
	staged := stagedBundlePath(s.stageDir, s.format, s.appName)

	switch s.format {
	case FormatDMG:
		mnt := filepath.Join(s.stageDir, "mnt")
		args := []string{"attach", "-nobrowse", "-readonly", "-mountpoint", mnt, artifact}
		r, err := s.p.run(ctx, HdiutilPath, args...)
		if err != nil {
			_ = os.RemoveAll(s.stageDir)
			return nil, err
		}
		ev := execute.Evidence{"command": HdiutilPath + " " + strings.Join(args, " "), "exit": r.Exit}
		if r.Exit != 0 {
			_ = os.RemoveAll(s.stageDir)
			return ev, fmt.Errorf("hdiutil attach exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
		}
		info, statErr := os.Stat(staged)
		if statErr != nil || !info.IsDir() {
			s.p.cleanupStage(ctx, s.stageDir, s.format)
			return ev, fmt.Errorf("staged bundle %s missing at %s", s.appName, staged)
		}
		ev["staged"] = staged
		return ev, nil

	case FormatZip:
		extract := filepath.Join(s.stageDir, "extract")
		args := []string{"-x", "-k", artifact, extract}
		r, err := s.p.run(ctx, DittoPath, args...)
		if err != nil {
			_ = os.RemoveAll(s.stageDir)
			return nil, err
		}
		ev := execute.Evidence{"command": DittoPath + " " + strings.Join(args, " "), "exit": r.Exit}
		if r.Exit != 0 {
			_ = os.RemoveAll(s.stageDir)
			return ev, fmt.Errorf("ditto -x -k exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
		}
		info, statErr := os.Stat(staged)
		if statErr != nil || !info.IsDir() {
			_ = os.RemoveAll(s.stageDir)
			return ev, fmt.Errorf("staged bundle %s missing at %s", s.appName, staged)
		}
		ev["staged"] = staged
		return ev, nil
	}
	_ = os.RemoveAll(s.stageDir)
	return nil, fmt.Errorf("%w: unsupported format %q", ErrUnsupported, s.format)
}

func (s *stageStep) Verify(ctx context.Context) (execute.Evidence, error) {
	staged := stagedBundlePath(s.stageDir, s.format, s.appName)
	if err := codesign.VerifyWithEnv(ctx, s.p.runner(), s.p.env(), staged, s.teamID, false); err != nil {
		s.p.cleanupStage(ctx, s.stageDir, s.format)
		return nil, err
	}
	gotVer, err := s.p.bundleVersion(ctx, staged)
	if err != nil {
		s.p.cleanupStage(ctx, s.stageDir, s.format)
		return nil, err
	}
	ev := execute.Evidence{"team_id": s.teamID, "version": gotVer}
	if gotVer != s.version {
		s.p.cleanupStage(ctx, s.stageDir, s.format)
		return ev, fmt.Errorf("staged bundle %s CFBundleShortVersionString is %q, expected %q", s.appName, gotVer, s.version)
	}
	return ev, nil
}

type placeStep struct {
	p        *Provider
	appName  string
	dest     string
	format   string
	teamID   string
	version  string
	stageDir string
}

func (s *placeStep) Apply(ctx context.Context) (execute.Evidence, error) {
	staged := stagedBundlePath(s.stageDir, s.format, s.appName)
	if err := os.MkdirAll(filepath.Dir(s.dest), 0o755); err != nil {
		s.p.cleanupStage(ctx, s.stageDir, s.format)
		return nil, fmt.Errorf("create Applications directory %s: %v", filepath.Dir(s.dest), err)
	}
	args := []string{staged, s.dest}
	r, err := s.p.run(ctx, DittoPath, args...)
	var detachErr error
	if s.format == FormatDMG {
		mnt := filepath.Join(s.stageDir, "mnt")
		dr, dErr := s.p.run(ctx, HdiutilPath, "detach", mnt)
		if dErr != nil {
			detachErr = dErr
		} else if dr.Exit != 0 {
			detachErr = fmt.Errorf("hdiutil detach exited %d: %s", dr.Exit, firstLine(dr.Stderr, dr.Stdout))
		}
	}
	_ = os.RemoveAll(s.stageDir)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": DittoPath + " " + strings.Join(args, " "), "exit": r.Exit, "destination": s.dest}
	if r.Exit != 0 {
		return ev, fmt.Errorf("ditto exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	if detachErr != nil {
		return ev, detachErr
	}
	return ev, nil
}

func (s *placeStep) Verify(ctx context.Context) (execute.Evidence, error) {
	if err := codesign.VerifyWithEnv(ctx, s.p.runner(), s.p.env(), s.dest, s.teamID, false); err != nil {
		return nil, err
	}
	gotVer, err := s.p.bundleVersion(ctx, s.dest)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"destination": s.dest, "team_id": s.teamID, "version": gotVer}
	if gotVer != s.version {
		return ev, fmt.Errorf("placed bundle %s CFBundleShortVersionString is %q, expected %q", s.appName, gotVer, s.version)
	}
	return ev, nil
}

func firstLine(bs ...[]byte) string {
	for _, b := range bs {
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.SplitN(s, "\n", 2)[0]
		}
	}
	return ""
}

var _ provider.Provider = (*Provider)(nil)

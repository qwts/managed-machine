// Package appcopy is the built-in macOS application bundle provider (S7 N02;
// D1, D3, D5, D6). It installs .app bundles from a pinned HTTPS DMG or zip
// into /Applications (global) or $HOME/Applications (user) on darwin/arm64 in
// three saved operations (download, stage, place), verifying the pinned
// SHA-256 digest and the Developer ID Team ID before touching the destination.
// Option validation builds on every target; execution carries the darwin build
// tag with an ErrUnsupported stub elsewhere.
package appcopy

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/fetch"
)

// Name is the provider name in software recipes.
const Name = "app-copy"

const (
	// FormatDMG is a disk image mounted read-only with /usr/bin/hdiutil.
	FormatDMG = "dmg"
	// FormatZip is a zip archive extracted with /usr/bin/ditto -x -k.
	FormatZip = "zip"
	// SupportedPlatform is the sole platform app-copy supports (ADR-0014).
	SupportedPlatform = "darwin/arm64"
)

// Result, Runner, Exec, Request and Resolution are the shared provider types.
type (
	Result     = provider.Result
	Runner     = provider.Runner
	Request    = provider.Request
	Resolution = provider.Resolution
)

// Exec is the real Runner.
var Exec = provider.Exec

// DefaultClient returns the default HTTPS client for artifact downloads.
func DefaultClient() *http.Client { return fetch.DefaultClient() }

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported       = provider.Kind(provider.ErrUnsupported, "unsupported by the app-copy provider")
	ErrPrivilege         = provider.Kind(provider.ErrPrivilege, "the Applications directory is not writable by the current user")
	ErrArtifactMismatch  = provider.ErrArtifactMismatch
	ErrSignatureRejected = provider.ErrSignatureRejected
	ErrHostUnlisted      = provider.ErrHostUnlisted
	ErrOccupied          = provider.ErrOccupied
)

var (
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	teamIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{2,32}$`)
)

// Options holds the validated options of an app-copy recipe.
type Options struct {
	URL      string
	SHA256   string
	URLHosts []string
	AppName  string
	TeamID   string
	Version  string
	Format   string
}

var knownOptions = map[string]bool{
	"url":       true,
	"sha256":    true,
	"url_hosts": true,
	"app_name":  true,
	"team_id":   true,
	"version":   true,
	"format":    true,
}

// ParseOptions validates the recipe options of an app-copy request.
func ParseOptions(raw map[string]string) (Options, error) {
	for k := range raw {
		if !knownOptions[k] {
			return Options{}, fmt.Errorf("%w: unknown option %q for %s", ErrUnsupported, k, Name)
		}
	}
	for _, req := range []string{"url", "sha256", "url_hosts", "app_name", "team_id", "version", "format"} {
		if strings.TrimSpace(raw[req]) == "" {
			return Options{}, fmt.Errorf("%w: missing required option %q", ErrUnsupported, req)
		}
	}

	hosts := fetch.SplitHosts(raw["url_hosts"])
	if len(hosts) == 0 {
		return Options{}, fmt.Errorf("%w: option \"url_hosts\" must name at least one host", ErrUnsupported)
	}
	for _, h := range hosts {
		if strings.ContainsAny(h, "/:\\") {
			return Options{}, fmt.Errorf("%w: invalid url_hosts entry %q", ErrUnsupported, h)
		}
	}

	rawURL := strings.TrimSpace(raw["url"])
	if _, host, err := fetch.ValidateScheme(rawURL); err != nil {
		return Options{}, fmt.Errorf("%w: option \"url\": %v", ErrUnsupported, err)
	} else if !fetch.HostAllowed(host, hosts) {
		return Options{}, fmt.Errorf("%w: option \"url\" host %q is not in url_hosts", ErrUnsupported, host)
	}

	digest := strings.ToLower(strings.TrimSpace(raw["sha256"]))
	if !sha256Pattern.MatchString(digest) {
		return Options{}, fmt.Errorf("%w: option \"sha256\" must be a 64-character hex digest", ErrUnsupported)
	}

	format := strings.TrimSpace(raw["format"])
	if format != FormatDMG && format != FormatZip {
		return Options{}, fmt.Errorf("%w: option \"format\" must be %q or %q, got %q", ErrUnsupported, FormatDMG, FormatZip, format)
	}

	appName := strings.TrimSpace(raw["app_name"])
	if !ValidAppName(appName) {
		return Options{}, fmt.Errorf("%w: option \"app_name\" %q must be a single .app bundle name", ErrUnsupported, appName)
	}

	teamID := strings.TrimSpace(raw["team_id"])
	if !teamIDPattern.MatchString(teamID) {
		return Options{}, fmt.Errorf("%w: option \"team_id\" %q is invalid", ErrUnsupported, teamID)
	}

	version := strings.TrimSpace(raw["version"])
	if version == "" || strings.ContainsAny(version, "\r\n\x00") {
		return Options{}, fmt.Errorf("%w: option \"version\" is invalid", ErrUnsupported)
	}

	return Options{
		URL:      rawURL,
		SHA256:   digest,
		URLHosts: hosts,
		AppName:  appName,
		TeamID:   teamID,
		Version:  version,
		Format:   format,
	}, nil
}

// ValidAppName reports whether s is a single .app bundle directory name with
// no path separators or traversal segments.
func ValidAppName(s string) bool {
	if len(s) <= len(".app") || !strings.HasSuffix(s, ".app") {
		return false
	}
	if strings.ContainsAny(s, "/\\\x00\r\n") || s == ".app" || strings.HasPrefix(s, "..") {
		return false
	}
	return true
}

// ValidatePlatform checks that the host platform and every platform declared
// by the recipe is darwin/arm64.
func ValidatePlatform(platform string, platforms []string) error {
	if platform != "" && platform != SupportedPlatform {
		return fmt.Errorf("%w: platform %s; %s supports only %s", ErrUnsupported, platform, Name, SupportedPlatform)
	}
	for _, p := range platforms {
		if p != SupportedPlatform {
			return fmt.Errorf("%w: recipe declares platform %s; %s supports only %s", ErrUnsupported, p, Name, SupportedPlatform)
		}
	}
	return nil
}

// ValidateTarget checks that target is global or user; custom roots are
// refused for app-copy.
func ValidateTarget(target string) error {
	switch target {
	case "global", "user":
		return nil
	default:
		return fmt.Errorf("%w: target %s; %s supports only global and user targets", ErrUnsupported, target, Name)
	}
}

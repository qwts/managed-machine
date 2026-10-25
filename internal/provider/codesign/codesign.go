// Package codesign verifies macOS application bundle signatures and
// Developer ID Team IDs for the brew cask and app-copy providers (S7 D5).
// It ports verify_app_signature of lib/cask-app.sh one to one and invokes
// /usr/bin/codesign and /usr/bin/spctl exclusively through an injected
// provider.Runner seam so tests never execute host binaries.
package codesign

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/qwts/managed-machine/internal/provider"
)

const (
	// CodesignPath is the absolute path to the macOS codesign tool.
	CodesignPath = "/usr/bin/codesign"
	// SpctlPath is the absolute path to the macOS Gatekeeper assessment tool.
	SpctlPath = "/usr/bin/spctl"
)

// Verify checks that bundle is signed with expected Developer ID Team ID
// teamID and passes strict deep cryptographic verification, or, when bundle
// detritus trips --strict, passes a Gatekeeper assessment naming teamID.
// Any other outcome returns an error matching provider.ErrSignatureRejected.
func Verify(ctx context.Context, run provider.Runner, bundle, teamID string) error {
	return VerifyWithEnv(ctx, run, nil, bundle, teamID, false)
}

// VerifyRolling checks bundle like Verify, and when allowRollingURL is true
// requires the Gatekeeper notarization assessment even if codesign --verify
// passes, because a rolling URL has no pinned SHA-256 digest.
func VerifyRolling(ctx context.Context, run provider.Runner, bundle, teamID string, allowRollingURL bool) error {
	return VerifyWithEnv(ctx, run, nil, bundle, teamID, allowRollingURL)
}

// VerifyNotarized checks bundle and unconditionally requires Gatekeeper
// notarization assessment naming teamID.
func VerifyNotarized(ctx context.Context, run provider.Runner, bundle, teamID string) error {
	return VerifyWithEnv(ctx, run, nil, bundle, teamID, true)
}

// VerifyWithEnv performs the D5 bundle signature check with an explicit
// process environment.
func VerifyWithEnv(ctx context.Context, run provider.Runner, env []string, bundle, teamID string, allowRollingURL bool) error {
	if run == nil {
		var err error
		run, err = defaultRunner()
		if err != nil {
			return err
		}
	}
	bundle = strings.TrimSpace(bundle)
	teamID = strings.TrimSpace(teamID)
	if bundle == "" || teamID == "" {
		return provider.Kind(provider.ErrSignatureRejected, "bundle path and team_id are required")
	}
	info, err := os.Stat(bundle)
	if err != nil || !info.IsDir() {
		return provider.Kind(provider.ErrSignatureRejected, fmt.Sprintf("expected app bundle missing at %s", bundle))
	}

	res, err := run(ctx, CodesignPath, []string{"-dv", "--verbose=2", bundle}, env)
	if err != nil || res.Exit != 0 {
		return provider.Kind(provider.ErrSignatureRejected, fmt.Sprintf("codesign inspect failed for %s", bundle))
	}
	combined := string(res.Stdout) + "\n" + string(res.Stderr)
	hasAuthorityLine := false
	hasDevIDAuthority := false
	actualTeam := ""
	for _, raw := range strings.Split(combined, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if strings.HasPrefix(line, "Authority=") {
			hasAuthorityLine = true
			if strings.Contains(line, "Authority=Developer ID Application") {
				hasDevIDAuthority = true
			}
		}
		if actualTeam == "" && strings.HasPrefix(line, "TeamIdentifier=") {
			actualTeam = strings.TrimSpace(strings.TrimPrefix(line, "TeamIdentifier="))
		}
	}
	if hasAuthorityLine && !hasDevIDAuthority {
		return provider.Kind(provider.ErrSignatureRejected, fmt.Sprintf("%s is not signed with a Developer ID Application certificate", bundle))
	}
	if actualTeam != teamID {
		if actualTeam == "" {
			actualTeam = "none"
		}
		return provider.Kind(provider.ErrSignatureRejected, fmt.Sprintf("%s TeamIdentifier is %q, expected %q", bundle, actualTeam, teamID))
	}

	if !allowRollingURL {
		vRes, vErr := run(ctx, CodesignPath, []string{"--verify", "--deep", "--strict", bundle}, env)
		if vErr == nil && vRes.Exit == 0 {
			return nil
		}
	}

	sRes, sErr := run(ctx, SpctlPath, []string{"-a", "-t", "exec", "-vv", bundle}, env)
	if sErr == nil && sRes.Exit == 0 {
		sOut := string(sRes.Stdout) + "\n" + string(sRes.Stderr)
		sourceOK := !strings.Contains(sOut, "source=") || strings.Contains(sOut, "source=Notarized Developer ID")
		if sourceOK && strings.Contains(sOut, "("+teamID+")") {
			return nil
		}
	}
	return provider.Kind(provider.ErrSignatureRejected, fmt.Sprintf("codesign --verify and Gatekeeper notarization check failed for %s", bundle))
}

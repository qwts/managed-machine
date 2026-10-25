package codesign

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/provider"
)

func makeBundle(t *testing.T) string {
	t.Helper()
	b := filepath.Join(t.TempDir(), "Fixture.app")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerifyPassesStrictCodesign(t *testing.T) {
	bundle := makeBundle(t)
	var calls []string
	run := func(_ context.Context, path string, args, _ []string) (provider.Result, error) {
		calls = append(calls, path+" "+strings.Join(args, " "))
		switch {
		case path == CodesignPath && args[0] == "-dv":
			return provider.Result{
				Stderr: []byte("Authority=Developer ID Application: Example Corp (TEAM123456)\nTeamIdentifier=TEAM123456\n"),
			}, nil
		case path == CodesignPath && args[0] == "--verify":
			return provider.Result{}, nil
		}
		t.Fatalf("unexpected call %s %v", path, args)
		return provider.Result{}, nil
	}
	if err := Verify(context.Background(), run, bundle, "TEAM123456"); err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (inspect + verify), got %v", calls)
	}
}

func TestVerifyFallsBackToSpctlOnDetritus(t *testing.T) {
	bundle := makeBundle(t)
	run := func(_ context.Context, path string, args, _ []string) (provider.Result, error) {
		switch {
		case path == CodesignPath && args[0] == "-dv":
			return provider.Result{
				Stderr: []byte("Authority=Developer ID Application: Example Corp (TEAM123456)\nTeamIdentifier=TEAM123456\n"),
			}, nil
		case path == CodesignPath && args[0] == "--verify":
			return provider.Result{Exit: 1, Stderr: []byte("resource envelope is obsolete\n")}, nil
		case path == SpctlPath:
			return provider.Result{
				Stderr: []byte(bundle + ": accepted\nsource=Notarized Developer ID\norigin=Developer ID Application: Example Corp (TEAM123456)\n"),
			}, nil
		}
		t.Fatalf("unexpected call %s %v", path, args)
		return provider.Result{}, nil
	}
	if err := Verify(context.Background(), run, bundle, "TEAM123456"); err != nil {
		t.Fatalf("Verify fallback failed: %v", err)
	}
}

func TestVerifyRollingRequiresSpctl(t *testing.T) {
	bundle := makeBundle(t)
	spctlCalled := false
	run := func(_ context.Context, path string, args, _ []string) (provider.Result, error) {
		switch {
		case path == CodesignPath && args[0] == "-dv":
			return provider.Result{
				Stderr: []byte("Authority=Developer ID Application: Example Corp (TEAM123456)\nTeamIdentifier=TEAM123456\n"),
			}, nil
		case path == SpctlPath:
			spctlCalled = true
			return provider.Result{
				Stderr: []byte(bundle + ": accepted\nsource=Notarized Developer ID\norigin=Developer ID Application: Example Corp (TEAM123456)\n"),
			}, nil
		}
		t.Fatalf("unexpected call %s %v", path, args)
		return provider.Result{}, nil
	}
	if err := VerifyRolling(context.Background(), run, bundle, "TEAM123456", true); err != nil {
		t.Fatalf("VerifyRolling failed: %v", err)
	}
	if !spctlCalled {
		t.Fatal("VerifyRolling did not invoke spctl")
	}
}

func TestVerifyRejections(t *testing.T) {
	bundle := makeBundle(t)

	cases := []struct {
		name string
		run  provider.Runner
	}{
		{
			name: "wrong team id",
			run: func(_ context.Context, path string, args, _ []string) (provider.Result, error) {
				return provider.Result{
					Stderr: []byte("Authority=Developer ID Application: Other (OTHERTEAM1)\nTeamIdentifier=OTHERTEAM1\n"),
				}, nil
			},
		},
		{
			name: "non-developer-id authority",
			run: func(_ context.Context, path string, args, _ []string) (provider.Result, error) {
				return provider.Result{
					Stderr: []byte("Authority=Apple Development: Someone (TEAM123456)\nTeamIdentifier=TEAM123456\n"),
				}, nil
			},
		},
		{
			name: "verify and spctl both fail",
			run: func(_ context.Context, path string, args, _ []string) (provider.Result, error) {
				switch {
				case path == CodesignPath && args[0] == "-dv":
					return provider.Result{
						Stderr: []byte("Authority=Developer ID Application: Example (TEAM123456)\nTeamIdentifier=TEAM123456\n"),
					}, nil
				case path == CodesignPath && args[0] == "--verify":
					return provider.Result{Exit: 1}, nil
				case path == SpctlPath:
					return provider.Result{Exit: 3, Stderr: []byte("rejected\nsource=Unnotarized Developer ID\n")}, nil
				}
				return provider.Result{Exit: 1}, nil
			},
		},
		{
			name: "spctl team mismatch",
			run: func(_ context.Context, path string, args, _ []string) (provider.Result, error) {
				switch {
				case path == CodesignPath && args[0] == "-dv":
					return provider.Result{
						Stderr: []byte("Authority=Developer ID Application: Example (TEAM123456)\nTeamIdentifier=TEAM123456\n"),
					}, nil
				case path == CodesignPath && args[0] == "--verify":
					return provider.Result{Exit: 1}, nil
				case path == SpctlPath:
					return provider.Result{
						Stderr: []byte("accepted\nsource=Notarized Developer ID\norigin=Developer ID Application: Other (WRONGTEAM9)\n"),
					}, nil
				}
				return provider.Result{Exit: 1}, nil
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Verify(context.Background(), c.run, bundle, "TEAM123456")
			if !errors.Is(err, provider.ErrSignatureRejected) {
				t.Fatalf("got %v, want ErrSignatureRejected", err)
			}
		})
	}
}

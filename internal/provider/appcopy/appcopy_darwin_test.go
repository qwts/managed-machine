//go:build darwin

package appcopy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/codesign"
	"github.com/qwts/managed-machine/internal/source"
)

type harness struct {
	t         *testing.T
	stateDir  string
	homeDir   string
	appDir    string
	srv       *httptest.Server
	client    *http.Client
	payload   []byte
	digest    string
	teamID    string
	bundleVer string
	calls     []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	payload := []byte("synthetic-bundle-archive-payload")
	sum := sha256.Sum256(payload)
	h := &harness{
		t:         t,
		stateDir:  filepath.Join(root, "state"),
		homeDir:   filepath.Join(root, "home"),
		appDir:    filepath.Join(root, "Applications"),
		payload:   payload,
		digest:    hex.EncodeToString(sum[:]),
		teamID:    "TEAM123456",
		bundleVer: "1.2.3",
	}
	if err := os.MkdirAll(h.appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.homeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Fixture.dmg", "/Fixture.zip":
			_, _ = w.Write(h.payload)
		case "/corrupt.dmg":
			_, _ = w.Write([]byte("tampered-bytes"))
		case "/redirect-unlisted":
			http.Redirect(w, r, "https://off-allowlist.example.invalid/Fixture.dmg", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	h.srv = srv
	h.client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	return h
}

func (h *harness) run(_ context.Context, path string, args, _ []string) (Result, error) {
	h.calls = append(h.calls, path+" "+strings.Join(args, " "))
	switch path {
	case HdiutilPath:
		if len(args) >= 5 && args[0] == "attach" && args[1] == "-nobrowse" && args[2] == "-readonly" && args[3] == "-mountpoint" {
			mnt := args[4]
			bundle := filepath.Join(mnt, "Fixture.app", "Contents")
			if err := os.MkdirAll(bundle, 0o755); err != nil {
				return Result{Exit: 1}, err
			}
			if err := os.WriteFile(filepath.Join(bundle, "Info.plist"), []byte(h.bundleVer), 0o644); err != nil {
				return Result{Exit: 1}, err
			}
			return Result{}, nil
		}
		if len(args) == 2 && args[0] == "detach" {
			_ = os.RemoveAll(args[1])
			return Result{}, nil
		}
	case DittoPath:
		if len(args) == 4 && args[0] == "-x" && args[1] == "-k" {
			extract := args[3]
			bundle := filepath.Join(extract, "Fixture.app", "Contents")
			if err := os.MkdirAll(bundle, 0o755); err != nil {
				return Result{Exit: 1}, err
			}
			if err := os.WriteFile(filepath.Join(bundle, "Info.plist"), []byte(h.bundleVer), 0o644); err != nil {
				return Result{Exit: 1}, err
			}
			return Result{}, nil
		}
		if len(args) == 2 {
			src, dst := args[0], args[1]
			info, err := os.Stat(filepath.Join(src, "Contents", "Info.plist"))
			if err != nil || info.IsDir() {
				return Result{Exit: 1, Stderr: []byte("source bundle missing\n")}, nil
			}
			if err := os.MkdirAll(filepath.Join(dst, "Contents"), 0o755); err != nil {
				return Result{Exit: 1}, err
			}
			data, _ := os.ReadFile(filepath.Join(src, "Contents", "Info.plist"))
			if err := os.WriteFile(filepath.Join(dst, "Contents", "Info.plist"), data, 0o644); err != nil {
				return Result{Exit: 1}, err
			}
			return Result{}, nil
		}
	case PlutilPath:
		if len(args) == 4 && args[0] == "-extract" && args[1] == "CFBundleShortVersionString" && args[2] == "raw" {
			data, err := os.ReadFile(args[3])
			if err != nil {
				return Result{Exit: 1, Stderr: []byte(err.Error())}, nil
			}
			return Result{Stdout: append(data, '\n')}, nil
		}
	case codesign.CodesignPath:
		if len(args) == 3 && args[0] == "-dv" {
			return Result{
				Stderr: []byte("Authority=Developer ID Application: Fixture Corp (" + h.teamID + ")\nTeamIdentifier=" + h.teamID + "\n"),
			}, nil
		}
		if len(args) == 4 && args[0] == "--verify" {
			return Result{}, nil
		}
	case codesign.SpctlPath:
		return Result{
			Stderr: []byte("accepted\nsource=Notarized Developer ID\norigin=Developer ID Application: Fixture Corp (" + h.teamID + ")\n"),
		}, nil
	}
	h.t.Fatalf("unexpected command %s %v", path, args)
	return Result{Exit: 1}, nil
}

func (h *harness) provider() *Provider {
	return &Provider{
		Run:      h.run,
		Environ:  func() []string { return []string{"HOME=" + h.homeDir} },
		Client:   h.client,
		StateDir: h.stateDir,
		AppDir:   h.appDir,
	}
}

func (h *harness) installEnv() install.Env {
	p := h.provider()
	return install.Env{
		StateDir: func() (string, error) { return h.stateDir, nil },
		Account:  func() (string, error) { return "tester", nil },
		Machine:  func(context.Context) (string, error) { return "machine-1", nil },
		Now:      func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) },
		Provider: func(name, _ string) provider.Provider {
			if name == Name {
				return p
			}
			return nil
		},
	}
}

func (h *harness) installReq(urlPath, format, target string) install.Request {
	sw := definition.Software{SchemaVersion: 1, ID: "fixture-app"}
	return install.Request{
		Entry: source.Entry{
			Software: sw,
			Source:   source.Identity{Kind: "local", Location: "test"},
			Revision: "sha256:11",
			File:     "fixture-app.yaml",
			Digest:   "sha256:22",
		},
		Recipe: definition.Recipe{
			ID:        Name,
			Provider:  Name,
			Package:   "Fixture.app",
			Platforms: []string{"darwin/arm64"},
			Targets:   []string{"global", "user"},
			Options: map[string]any{
				"url":       h.srv.URL + urlPath,
				"sha256":    h.digest,
				"url_hosts": []string{"127.0.0.1"},
				"app_name":  "Fixture.app",
				"team_id":   "TEAM123456",
				"version":   "1.2.3",
				"format":    format,
			},
		},
		Target:   target,
		Platform: "darwin/arm64",
	}
}

func TestDMGAndZipInstallAndIdempotentRerun(t *testing.T) {
	for _, format := range []string{FormatDMG, FormatZip} {
		t.Run(format, func(t *testing.T) {
			h := newHarness(t)
			req := h.installReq("/Fixture."+format, format, "global")
			res, err := install.Run(context.Background(), h.installEnv(), req)
			if err != nil {
				t.Fatalf("install (%s): %v", format, err)
			}
			if res.Outcome != "ok" || res.ResolvedVersion != "1.2.3" {
				t.Fatalf("unexpected result: %+v", res)
			}
			placed := filepath.Join(h.appDir, "Fixture.app", "Contents", "Info.plist")
			if _, err := os.Stat(placed); err != nil {
				t.Fatalf("placed bundle missing at %s: %v", placed, err)
			}
			stagePath := filepath.Join(h.stateDir, "stage", res.Plan)
			if _, err := os.Stat(stagePath); !os.IsNotExist(err) {
				t.Errorf("stage directory %s was not removed after success", stagePath)
			}

			st, err := plan.Open(h.stateDir, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := st.Load(res.Plan)
			_ = st.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Operations) != 3 || doc.Operations[0].ID != OpDownload || doc.Operations[1].ID != OpStage || doc.Operations[2].ID != OpPlace {
				t.Fatalf("expected [download, stage, place] operations, got %+v", doc.Operations)
			}

			// Re-running when already present with matching Team ID and version is unchanged with 0 operations.
			beforeCalls := len(h.calls)
			res2, err := install.Run(context.Background(), h.installEnv(), req)
			if err != nil {
				t.Fatalf("re-run (%s): %v", format, err)
			}
			if res2.Outcome != "unchanged" || res2.ResolvedVersion != "1.2.3" {
				t.Fatalf("expected unchanged outcome, got %+v", res2)
			}
			st2, err := plan.Open(h.stateDir, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			doc2, err := st2.Load(res2.Plan)
			_ = st2.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(doc2.Operations) != 0 {
				t.Errorf("unchanged plan had %d operations", len(doc2.Operations))
			}
			for _, c := range h.calls[beforeCalls:] {
				if strings.HasPrefix(c, HdiutilPath) || strings.HasPrefix(c, DittoPath) {
					t.Errorf("unchanged run invoked staging/placing tool: %s", c)
				}
			}
		})
	}
}

func TestUserTargetInstallsUnderHomeApplications(t *testing.T) {
	h := newHarness(t)
	req := h.installReq("/Fixture.zip", FormatZip, "user")
	res, err := install.Run(context.Background(), h.installEnv(), req)
	if err != nil || res.Outcome != "ok" {
		t.Fatalf("user install: %+v %v", res, err)
	}
	userBundle := filepath.Join(h.homeDir, "Applications", "Fixture.app", "Contents", "Info.plist")
	if _, err := os.Stat(userBundle); err != nil {
		t.Fatalf("expected bundle in $HOME/Applications, got %v", err)
	}
}

func TestOccupiedDestinationRefusedAtPlanTime(t *testing.T) {
	h := newHarness(t)
	// Pre-populate /Applications/Fixture.app with a different version.
	dest := filepath.Join(h.appDir, "Fixture.app", "Contents")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "Info.plist"), []byte("0.9.0"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := install.Run(context.Background(), h.installEnv(), h.installReq("/Fixture.dmg", FormatDMG, "global"))
	if !errors.Is(err, ErrOccupied) {
		t.Fatalf("expected ErrOccupied, got %v", err)
	}
	// Plan-time refusal writes no state directory.
	if _, statErr := os.Stat(h.stateDir); !os.IsNotExist(statErr) {
		t.Errorf("state directory was written on occupied refusal")
	}
	// Existing bundle was not modified.
	got, _ := os.ReadFile(filepath.Join(dest, "Info.plist"))
	if string(got) != "0.9.0" {
		t.Errorf("occupied bundle was modified: %q", string(got))
	}
}

func TestThreeRefusalsLeaveDestinationUntouchedAndRecordFailedOperation(t *testing.T) {
	cases := []struct {
		name       string
		urlPath    string
		mutHarness func(*harness)
		wantErr    error
		wantFailed string
		wantNotRun []string
	}{
		{
			name:       "unlisted redirect host",
			urlPath:    "/redirect-unlisted",
			mutHarness: func(*harness) {},
			wantErr:    ErrHostUnlisted,
			wantFailed: OpDownload,
			wantNotRun: []string{OpStage, OpPlace},
		},
		{
			name:       "artifact digest mismatch",
			urlPath:    "/corrupt.dmg",
			mutHarness: func(*harness) {},
			wantErr:    ErrArtifactMismatch,
			wantFailed: OpDownload,
			wantNotRun: []string{OpStage, OpPlace},
		},
		{
			name:       "signature team id mismatch",
			urlPath:    "/Fixture.dmg",
			mutHarness: func(h *harness) { h.teamID = "WRONGTEAM9" },
			wantErr:    ErrSignatureRejected,
			wantFailed: OpStage,
			wantNotRun: []string{OpPlace},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.mutHarness(h)
			res, err := install.Run(context.Background(), h.installEnv(), h.installReq(c.urlPath, FormatDMG, "global"))
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("got err %v, want %v", err, c.wantErr)
			}
			if res.Plan == "" {
				t.Fatal("expected saved plan id on apply-time failure")
			}

			// 1. No-destination-write rule: <appdir> has no entries.
			entries, err := os.ReadDir(h.appDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("destination %s was touched on failure: %v", h.appDir, entries)
			}

			// 2. Stage directory was removed on failure.
			stagePath := filepath.Join(h.stateDir, "stage", res.Plan)
			if _, statErr := os.Stat(stagePath); !os.IsNotExist(statErr) {
				t.Errorf("stage directory %s still exists after failure", stagePath)
			}

			// 3. Receipt names the failed operation and the operations not run.
			st, err := plan.Open(h.stateDir, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			doc, err := st.Load(res.Plan)
			if err != nil || doc.State != plan.Failed {
				t.Fatalf("plan state = %+v, err = %v", doc, err)
			}
			rc, err := st.Receipt(res.Plan)
			if err != nil || rc == nil {
				t.Fatalf("receipt missing: %v", err)
			}
			if rc.Outcome != plan.OutcomeFailed || rc.Operation != c.wantFailed {
				t.Errorf("receipt operation = %q outcome = %q, want %q failed", rc.Operation, rc.Outcome, c.wantFailed)
			}
			if gotFailed, _ := rc.Evidence["failed_operation"].(string); gotFailed != c.wantFailed {
				t.Errorf("receipt failed_operation = %q, want %q", gotFailed, c.wantFailed)
			}
		})
	}
}

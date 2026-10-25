package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	"runtime"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/source"
)

type archiveEntry struct {
	name     string
	body     []byte
	typeflag byte // 0 means regular file (tar.TypeReg)
	linkname string
	mode     os.FileMode
}

func buildTarGz(t *testing.T, entries []archiveEntry) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := int64(0o755)
		if e.mode != 0 {
			mode = int64(e.mode.Perm())
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Linkname: e.linkname,
			Mode:     mode,
			Size:     int64(len(e.body)),
		}
		if tf == tar.TypeDir || tf == tar.TypeSymlink || tf == tar.TypeLink {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func buildZip(t *testing.T, entries []archiveEntry) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{
			Name:   e.name,
			Method: zip.Deflate,
		}
		mode := os.FileMode(0o755)
		if e.mode != 0 {
			mode = e.mode
		}
		switch e.typeflag {
		case tar.TypeDir:
			mode |= os.ModeDir
			hdr.Method = zip.Store
		case tar.TypeSymlink:
			mode |= os.ModeSymlink
			hdr.Method = zip.Store
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.body) > 0 {
			if _, err := w.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

type harness struct {
	t          *testing.T
	rootDir    string
	stateDir   string
	homeDir    string
	userBinDir string
	customRoot string
	customBin  string
	srv        *httptest.Server
	client     *http.Client
	artifacts  map[string][]byte
	digests    map[string]string
	maxBytes   int64
	writable   func(string) bool
	requests   int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	userBin := filepath.Join(home, ".local", "bin")
	customRoot := filepath.Join(root, "custom")
	customBin := filepath.Join(customRoot, "bin")
	for _, dir := range []string{userBin, customBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	h := &harness{
		t:          t,
		rootDir:    root,
		stateDir:   filepath.Join(root, "state"),
		homeDir:    home,
		userBinDir: userBin,
		customRoot: customRoot,
		customBin:  customBin,
		artifacts:  map[string][]byte{},
		digests:    map[string]string{},
	}

	defaultEntries := []archiveEntry{
		{name: "tool-1.2.3/", typeflag: tar.TypeDir},
		{name: "tool-1.2.3/README.md", body: []byte("unlisted documentation\n")},
		{name: "tool-1.2.3/bin/tool", body: []byte("binary-tool-v1.2.3")},
		{name: "tool-1.2.3/bin/tool-helper.exe", body: []byte("binary-helper-v1.2.3")},
	}
	tgzBytes, tgzSum := buildTarGz(t, defaultEntries)
	zipBytes, zipSum := buildZip(t, defaultEntries)
	h.artifacts["/tool.tar.gz"] = tgzBytes
	h.digests["/tool.tar.gz"] = tgzSum
	h.artifacts["/tool.zip"] = zipBytes
	h.digests["/tool.zip"] = zipSum

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
		h.requests++
		if r.URL.Path == "/redirect-unlisted" {
			http.Redirect(w, r, "https://off-allowlist.example.invalid/tool.tar.gz", http.StatusFound)
			return
		}
		if r.URL.Path == "/corrupt.tar.gz" {
			_, _ = w.Write([]byte("tampered-bytes"))
			return
		}
		if b, ok := h.artifacts[r.URL.Path]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
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

func (h *harness) provider() *Provider {
	return &Provider{
		Environ: func() []string {
			return []string{
				"HOME=" + h.homeDir,
				"USERPROFILE=" + h.homeDir,
			}
		},
		Client:   h.client,
		StateDir: h.stateDir,
		MaxBytes: h.maxBytes,
		Writable: h.writable,
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

func (h *harness) installReq(urlPath, digest, format, target string, files []string) install.Request {
	sw := definition.Software{SchemaVersion: 1, ID: "fixture-tool"}
	return install.Request{
		Entry: source.Entry{
			Software: sw,
			Source:   source.Identity{Kind: "local", Location: "test"},
			Revision: "sha256:11",
			File:     "fixture-tool.yaml",
			Digest:   "sha256:22",
		},
		Recipe: definition.Recipe{
			ID:        Name,
			Provider:  Name,
			Platforms: []string{"darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64"},
			Targets:   []string{"user", "custom"},
			Options: map[string]any{
				"url":       h.srv.URL + urlPath,
				"sha256":    digest,
				"url_hosts": []string{"127.0.0.1"},
				"version":   "1.2.3",
				"format":    format,
				"files":     files,
			},
		},
		Target:   target,
		Platform: "darwin/arm64",
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestTarGzAndZipInstallUserAndCustomAndIdempotentRerun(t *testing.T) {
	for _, format := range []string{FormatTarGz, FormatZip} {
		t.Run(format, func(t *testing.T) {
			h := newHarness(t)
			urlPath := "/tool." + format
			files := []string{"tool-1.2.3/bin/tool", "tool-1.2.3/bin/tool-helper.exe"}
			req := h.installReq(urlPath, h.digests[urlPath], format, "user", files)

			res, err := install.Run(context.Background(), h.installEnv(), req)
			if err != nil {
				t.Fatalf("install (%s): %v", format, err)
			}
			if res.Outcome != "ok" || res.ResolvedVersion != "1.2.3" {
				t.Fatalf("unexpected result: %+v", res)
			}

			for name, wantBody := range map[string]string{
				"tool":            "binary-tool-v1.2.3",
				"tool-helper.exe": "binary-helper-v1.2.3",
			} {
				p := filepath.Join(h.userBinDir, name)
				got, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("installed binary %s missing: %v", p, err)
				}
				if string(got) != wantBody {
					t.Errorf("%s body = %q, want %q", name, string(got), wantBody)
				}
				info, err := os.Lstat(p)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
					t.Errorf("%s mode = %04o, want 0755", name, info.Mode().Perm())
				}
				if _, err := os.Stat(p + ".mm-tmp"); !os.IsNotExist(err) {
					t.Errorf("temporary file %s.mm-tmp still exists", p)
				}
			}

			// Unlisted archive entry was not extracted.
			if _, err := os.Stat(filepath.Join(h.userBinDir, "README.md")); !os.IsNotExist(err) {
				t.Errorf("unlisted archive entry README.md was extracted into %s", h.userBinDir)
			}

			// Stage directory was removed after success.
			stagePath := filepath.Join(h.stateDir, "stage", res.Plan)
			if _, err := os.Stat(stagePath); !os.IsNotExist(err) {
				t.Errorf("stage directory %s was not removed after success", stagePath)
			}

			// Saved plan has exactly two operations: download and install.
			st, err := plan.Open(h.stateDir, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := st.Load(res.Plan)
			_ = st.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Operations) != 2 || doc.Operations[0].ID != OpDownload || doc.Operations[1].ID != OpInstall {
				t.Fatalf("expected [download, install] operations, got %+v", doc.Operations)
			}

			// Second and third runs are idempotent (Installed: true, Outcome: "unchanged", 0 operations, 0 HTTP requests).
			for runNum := 2; runNum <= 3; runNum++ {
				beforeReqs := h.requests
				resN, err := install.Run(context.Background(), h.installEnv(), req)
				if err != nil {
					t.Fatalf("rerun %d (%s): %v", runNum, format, err)
				}
				if resN.Outcome != "unchanged" || resN.ResolvedVersion != "1.2.3" {
					t.Fatalf("rerun %d: expected unchanged outcome, got %+v", runNum, resN)
				}
				if h.requests != beforeReqs {
					t.Errorf("rerun %d performed %d network requests, want 0", runNum, h.requests-beforeReqs)
				}
				stN, err := plan.Open(h.stateDir, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				docN, err := stN.Load(resN.Plan)
				_ = stN.Close()
				if err != nil {
					t.Fatal(err)
				}
				if len(docN.Operations) != 0 {
					t.Errorf("rerun %d plan had %d operations, want 0", runNum, len(docN.Operations))
				}
			}

			// Custom target installs to <root>/bin.
			customReq := h.installReq(urlPath, h.digests[urlPath], format, "custom:"+h.customRoot, []string{"tool-1.2.3/bin/tool"})
			customRes, err := install.Run(context.Background(), h.installEnv(), customReq)
			if err != nil || customRes.Outcome != "ok" {
				t.Fatalf("custom install (%s): %+v %v", format, customRes, err)
			}
			if _, err := os.Stat(filepath.Join(h.customBin, "tool")); err != nil {
				t.Fatalf("expected tool in %s: %v", h.customBin, err)
			}
		})
	}
}

func TestPlanRefusalsAreReadOnlyAndNetworkFree(t *testing.T) {
	t.Run("global target refused", func(t *testing.T) {
		h := newHarness(t)
		req := h.installReq("/tool.tar.gz", h.digests["/tool.tar.gz"], FormatTarGz, "global", []string{"tool-1.2.3/bin/tool"})
		_, err := install.Run(context.Background(), h.installEnv(), req)
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("expected ErrUnsupported for global target, got %v", err)
		}
		if h.requests != 0 {
			t.Errorf("Plan made %d network requests", h.requests)
		}
		if _, statErr := os.Stat(h.stateDir); !os.IsNotExist(statErr) {
			t.Errorf("state directory was created on plan refusal")
		}
	})

	t.Run("missing bin directory refused with ErrPrivilege", func(t *testing.T) {
		h := newHarness(t)
		if err := os.RemoveAll(h.userBinDir); err != nil {
			t.Fatal(err)
		}
		req := h.installReq("/tool.tar.gz", h.digests["/tool.tar.gz"], FormatTarGz, "user", []string{"tool-1.2.3/bin/tool"})
		_, err := install.Run(context.Background(), h.installEnv(), req)
		if !errors.Is(err, ErrPrivilege) {
			t.Fatalf("expected ErrPrivilege when bin directory does not exist, got %v", err)
		}
		if h.requests != 0 {
			t.Errorf("Plan made %d network requests", h.requests)
		}
		if _, statErr := os.Stat(h.userBinDir); !os.IsNotExist(statErr) {
			t.Errorf("Plan created missing bin directory %s", h.userBinDir)
		}
	})

	t.Run("unwritable bin directory refused with ErrPrivilege", func(t *testing.T) {
		h := newHarness(t)
		h.writable = func(string) bool { return false }
		req := h.installReq("/tool.tar.gz", h.digests["/tool.tar.gz"], FormatTarGz, "user", []string{"tool-1.2.3/bin/tool"})
		_, err := install.Run(context.Background(), h.installEnv(), req)
		if !errors.Is(err, ErrPrivilege) {
			t.Fatalf("expected ErrPrivilege when bin directory is not writable, got %v", err)
		}
		if h.requests != 0 {
			t.Errorf("Plan made %d network requests", h.requests)
		}
	})

	t.Run("occupied destination without receipt refused with ErrOccupied", func(t *testing.T) {
		h := newHarness(t)
		occupied := filepath.Join(h.userBinDir, "tool")
		if err := os.WriteFile(occupied, []byte("pre-existing"), 0o755); err != nil {
			t.Fatal(err)
		}
		req := h.installReq("/tool.tar.gz", h.digests["/tool.tar.gz"], FormatTarGz, "user", []string{"tool-1.2.3/bin/tool"})
		_, err := install.Run(context.Background(), h.installEnv(), req)
		if !errors.Is(err, ErrOccupied) {
			t.Fatalf("expected ErrOccupied, got %v", err)
		}
		if h.requests != 0 {
			t.Errorf("Plan made %d network requests", h.requests)
		}
		if _, statErr := os.Stat(h.stateDir); !os.IsNotExist(statErr) {
			t.Errorf("state directory was created on occupied refusal")
		}
		got, _ := os.ReadFile(occupied)
		if string(got) != "pre-existing" {
			t.Errorf("occupied file was modified: %q", string(got))
		}
	})

	t.Run("occupied destination from older receipt version refused with ErrOccupied", func(t *testing.T) {
		h := newHarness(t)
		req1 := h.installReq("/tool.tar.gz", h.digests["/tool.tar.gz"], FormatTarGz, "user", []string{"tool-1.2.3/bin/tool"})
		if _, err := install.Run(context.Background(), h.installEnv(), req1); err != nil {
			t.Fatalf("initial install: %v", err)
		}
		req2 := h.installReq("/tool.tar.gz", h.digests["/tool.tar.gz"], FormatTarGz, "user", []string{"tool-1.2.3/bin/tool"})
		req2.Recipe.Options["version"] = "2.0.0"
		beforeReqs := h.requests
		_, err := install.Run(context.Background(), h.installEnv(), req2)
		if !errors.Is(err, ErrOccupied) {
			t.Fatalf("expected ErrOccupied when installed receipt version differs, got %v", err)
		}
		if h.requests != beforeReqs {
			t.Errorf("Plan made %d network requests on version-mismatch occupied refusal", h.requests-beforeReqs)
		}
	})
}

func TestDownloadRefusalsLeaveBinUntouched(t *testing.T) {
	cases := []struct {
		name    string
		urlPath string
		wantErr error
	}{
		{
			name:    "unlisted redirect host",
			urlPath: "/redirect-unlisted",
			wantErr: ErrHostUnlisted,
		},
		{
			name:    "artifact sha256 mismatch",
			urlPath: "/corrupt.tar.gz",
			wantErr: ErrArtifactMismatch,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			req := h.installReq(c.urlPath, h.digests["/tool.tar.gz"], FormatTarGz, "user", []string{"tool-1.2.3/bin/tool"})
			res, err := install.Run(context.Background(), h.installEnv(), req)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("got err %v, want %v", err, c.wantErr)
			}
			entries, err := os.ReadDir(h.userBinDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("user bin dir was touched on download failure: %v", entries)
			}
			stagePath := filepath.Join(h.stateDir, "stage", res.Plan)
			if _, statErr := os.Stat(stagePath); !os.IsNotExist(statErr) {
				t.Errorf("stage directory %s still exists after failure", stagePath)
			}
			st, err := plan.Open(h.stateDir, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			rc, err := st.Receipt(res.Plan)
			if err != nil || rc == nil {
				t.Fatalf("receipt missing: %v", err)
			}
			if gotFailed, _ := rc.Evidence["failed_operation"].(string); gotFailed != OpDownload {
				t.Errorf("receipt failed_operation = %q, want %q", gotFailed, OpDownload)
			}
		})
	}
}

func TestInstallRefusalsReturnErrUnsupportedAndLeaveNothingBehind(t *testing.T) {
	h := newHarness(t)

	tgzSymlink, sumTgzSymlink := buildTarGz(t, []archiveEntry{
		{name: "bin/first", body: []byte("first-ok")},
		{name: "bin/tool", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	zipSymlink, sumZipSymlink := buildZip(t, []archiveEntry{
		{name: "bin/first", body: []byte("first-ok")},
		{name: "bin/tool", typeflag: tar.TypeSymlink, body: []byte("/etc/passwd")},
	})
	tgzHardlink, sumTgzHardlink := buildTarGz(t, []archiveEntry{
		{name: "bin/first", body: []byte("first-ok")},
		{name: "bin/tool", typeflag: tar.TypeLink, linkname: "bin/first"},
	})
	tgzDir, sumTgzDir := buildTarGz(t, []archiveEntry{
		{name: "bin/tool/", typeflag: tar.TypeDir},
	})
	zipDir, sumZipDir := buildZip(t, []archiveEntry{
		{name: "bin/tool/", typeflag: tar.TypeDir},
	})
	tgzAbs, sumTgzAbs := buildTarGz(t, []archiveEntry{
		{name: "/abs/tool", body: []byte("bad")},
	})
	tgzDotDot, sumTgzDotDot := buildTarGz(t, []archiveEntry{
		{name: "../tool", body: []byte("bad")},
	})
	zipDotDot, sumZipDotDot := buildZip(t, []archiveEntry{
		{name: "a/../tool", body: []byte("bad")},
	})
	tgzOversize, sumTgzOversize := buildTarGz(t, []archiveEntry{
		{name: "bin/first", body: []byte("small")},
		{name: "bin/tool", body: bytes.Repeat([]byte("x"), 2048)},
	})
	zipOversize, sumZipOversize := buildZip(t, []archiveEntry{
		{name: "bin/first", body: []byte("small")},
		{name: "bin/tool", body: bytes.Repeat([]byte("x"), 2048)},
	})
	notAnArchive := []byte("plain text not an archive")
	sumNotAnArchive := sha256Hex(notAnArchive)

	h.artifacts["/symlink.tar.gz"] = tgzSymlink
	h.artifacts["/symlink.zip"] = zipSymlink
	h.artifacts["/hardlink.tar.gz"] = tgzHardlink
	h.artifacts["/dir.tar.gz"] = tgzDir
	h.artifacts["/dir.zip"] = zipDir
	h.artifacts["/abs.tar.gz"] = tgzAbs
	h.artifacts["/dotdot.tar.gz"] = tgzDotDot
	h.artifacts["/dotdot.zip"] = zipDotDot
	h.artifacts["/oversize.tar.gz"] = tgzOversize
	h.artifacts["/oversize.zip"] = zipOversize
	h.artifacts["/plain.bin"] = notAnArchive

	cases := []struct {
		name     string
		urlPath  string
		digest   string
		format   string
		files    []string
		maxBytes int64
	}{
		{
			name:    "missing listed member (tar.gz)",
			urlPath: "/tool.tar.gz",
			digest:  h.digests["/tool.tar.gz"],
			format:  FormatTarGz,
			files:   []string{"tool-1.2.3/bin/tool", "tool-1.2.3/bin/does-not-exist"},
		},
		{
			name:    "missing listed member (zip)",
			urlPath: "/tool.zip",
			digest:  h.digests["/tool.zip"],
			format:  FormatZip,
			files:   []string{"tool-1.2.3/bin/tool", "tool-1.2.3/bin/does-not-exist"},
		},
		{
			name:    "symlink member (tar.gz)",
			urlPath: "/symlink.tar.gz",
			digest:  sumTgzSymlink,
			format:  FormatTarGz,
			files:   []string{"bin/first", "bin/tool"},
		},
		{
			name:    "symlink member (zip)",
			urlPath: "/symlink.zip",
			digest:  sumZipSymlink,
			format:  FormatZip,
			files:   []string{"bin/first", "bin/tool"},
		},
		{
			name:    "hard link member (tar.gz)",
			urlPath: "/hardlink.tar.gz",
			digest:  sumTgzHardlink,
			format:  FormatTarGz,
			files:   []string{"bin/first", "bin/tool"},
		},
		{
			name:    "directory member (tar.gz)",
			urlPath: "/dir.tar.gz",
			digest:  sumTgzDir,
			format:  FormatTarGz,
			files:   []string{"bin/tool"},
		},
		{
			name:    "directory member (zip)",
			urlPath: "/dir.zip",
			digest:  sumZipDir,
			format:  FormatZip,
			files:   []string{"bin/tool"},
		},
		{
			name:    "absolute member path",
			urlPath: "/abs.tar.gz",
			digest:  sumTgzAbs,
			format:  FormatTarGz,
			files:   []string{"/abs/tool"},
		},
		{
			name:    "dotdot member path (tar.gz)",
			urlPath: "/dotdot.tar.gz",
			digest:  sumTgzDotDot,
			format:  FormatTarGz,
			files:   []string{"../tool"},
		},
		{
			name:    "dotdot member path (zip)",
			urlPath: "/dotdot.zip",
			digest:  sumZipDotDot,
			format:  FormatZip,
			files:   []string{"a/../tool"},
		},
		{
			name:     "member larger than fetch bound (tar.gz)",
			urlPath:  "/oversize.tar.gz",
			digest:   sumTgzOversize,
			format:   FormatTarGz,
			files:    []string{"bin/first", "bin/tool"},
			maxBytes: 1024,
		},
		{
			name:     "member larger than fetch bound (zip)",
			urlPath:  "/oversize.zip",
			digest:   sumZipOversize,
			format:   FormatZip,
			files:    []string{"bin/first", "bin/tool"},
			maxBytes: 1024,
		},
		{
			name:    "format mismatch zip bytes with format tar.gz",
			urlPath: "/tool.zip",
			digest:  h.digests["/tool.zip"],
			format:  FormatTarGz,
			files:   []string{"tool-1.2.3/bin/tool"},
		},
		{
			name:    "format mismatch tar.gz bytes with format zip",
			urlPath: "/tool.tar.gz",
			digest:  h.digests["/tool.tar.gz"],
			format:  FormatZip,
			files:   []string{"tool-1.2.3/bin/tool"},
		},
		{
			name:    "corrupt non-archive bytes",
			urlPath: "/plain.bin",
			digest:  sumNotAnArchive,
			format:  FormatTarGz,
			files:   []string{"bin/tool"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.maxBytes = c.maxBytes
			defer func() { h.maxBytes = 0 }()

			req := h.installReq(c.urlPath, c.digest, c.format, "user", c.files)
			res, err := install.Run(context.Background(), h.installEnv(), req)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("got err %v, want ErrUnsupported", err)
			}
			if res.Plan == "" {
				t.Fatal("expected saved plan id on apply-time install failure")
			}

			// Nothing under <bin> is left behind (neither final binaries nor .mm-tmp files).
			entries, err := os.ReadDir(h.userBinDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("bin directory %s was not clean after failed install: %v", h.userBinDir, entries)
			}

			// Stage directory was removed on failure.
			stagePath := filepath.Join(h.stateDir, "stage", res.Plan)
			if _, statErr := os.Stat(stagePath); !os.IsNotExist(statErr) {
				t.Errorf("stage directory %s still exists after failure", stagePath)
			}

			// Receipt names failed_operation == "install".
			st, err := plan.Open(h.stateDir, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			rc, err := st.Receipt(res.Plan)
			if err != nil || rc == nil {
				t.Fatalf("receipt missing: %v", err)
			}
			if gotFailed, _ := rc.Evidence["failed_operation"].(string); gotFailed != OpInstall {
				t.Errorf("receipt failed_operation = %q, want %q", gotFailed, OpInstall)
			}
		})
	}
}

func TestInstallStepVerifyDetectsTamperedDigestAndMode(t *testing.T) {
	h := newHarness(t)
	stageDir := filepath.Join(h.stateDir, "stage", "plan-verify")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "artifact.tar.gz"), h.artifacts["/tool.tar.gz"], 0o644); err != nil {
		t.Fatal(err)
	}

	p := h.provider()
	step, err := p.Step(plan.Operation{
		ID:       OpInstall,
		Action:   OpInstall,
		Provider: Name,
		Params: map[string]string{
			"bin_dir": h.userBinDir,
			"files":   "tool-1.2.3/bin/tool",
			"format":  FormatTarGz,
			"stage":   stageDir,
			"version": "1.2.3",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := step.Verify(context.Background()); err != nil {
		t.Fatalf("Verify on clean install: %v", err)
	}

	dest := filepath.Join(h.userBinDir, "tool")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dest, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := step.Verify(context.Background()); err == nil {
			t.Errorf("Verify accepted mode 0644")
		}
		if err := os.Chmod(dest, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(dest, []byte("tampered-after-apply"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := step.Verify(context.Background()); err == nil {
		t.Errorf("Verify accepted tampered destination digest")
	}
}

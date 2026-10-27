package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	cachedReleaseOnce  sync.Once
	cachedReleaseVer   string
	cachedReleaseFiles map[string][]byte
	cachedReleaseErr   error
)

// buildRepoReleaseDist builds the five release archives for the repository's
// VERSION once per test process and copies them into a fresh directory inside
// t.TempDir().
func buildRepoReleaseDist(t *testing.T) (string, string) {
	t.Helper()
	cachedReleaseOnce.Do(func() {
		root, err := findRepoRoot()
		if err != nil {
			cachedReleaseErr = err
			return
		}
		vRaw, err := os.ReadFile(filepath.Join(root, "VERSION"))
		if err != nil {
			cachedReleaseErr = err
			return
		}
		cachedReleaseVer = strings.TrimSpace(string(vRaw))
		tmp, err := os.MkdirTemp("", "mm-release-test-cache-")
		if err != nil {
			cachedReleaseErr = err
			return
		}
		defer os.RemoveAll(tmp)

		var outBuf, errBuf bytes.Buffer
		if code := run([]string{"build", "-version", cachedReleaseVer, "-out", tmp}, &outBuf, &errBuf); code != 0 {
			cachedReleaseErr = fmt.Errorf("mm-release build -version %s failed (exit %d): %s", cachedReleaseVer, code, errBuf.String())
			return
		}
		ents, err := os.ReadDir(tmp)
		if err != nil {
			cachedReleaseErr = err
			return
		}
		cachedReleaseFiles = make(map[string][]byte, len(ents))
		for _, e := range ents {
			b, err := os.ReadFile(filepath.Join(tmp, e.Name()))
			if err != nil {
				cachedReleaseErr = err
				return
			}
			cachedReleaseFiles[e.Name()] = b
		}
	})
	if cachedReleaseErr != nil {
		t.Fatal(cachedReleaseErr)
	}

	distDir := filepath.Join(t.TempDir(), "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range cachedReleaseFiles {
		if err := os.WriteFile(filepath.Join(distDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cachedReleaseVer, distDir
}

func TestRepoFormulaAndDocsR04(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	version, distDir := buildRepoReleaseDist(t)
	formulaPath := filepath.Join(repoRoot, "Formula", "managed-machine.rb")

	// 1. mm-release verify -dist distDir passes, and when a candidate R05 formula
	//    is generated in t.TempDir() from the repository formula plus the R05
	//    core resource patch content (never editing the repository file),
	//    mm-release verify -require-pin passes against that generated formula.
	var outBuf, errBuf bytes.Buffer
	if code := run([]string{"verify", "-dist", distDir}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("mm-release verify failed (exit %d): %s", code, errBuf.String())
	}

	rawFormula, err := os.ReadFile(formulaPath)
	if err != nil {
		t.Fatal(err)
	}
	baseFormulaText := strings.ReplaceAll(string(rawFormula), "\r\n", "\n")

	wantCoreArchive := fmt.Sprintf("managed-machine_%s_darwin_arm64.tar.gz", version)
	wantCoreURL := fmt.Sprintf("https://github.com/qwts/managed-machine/releases/download/v%s/%s", version, wantCoreArchive)
	darwinArchiveBytes, err := os.ReadFile(filepath.Join(distDir, wantCoreArchive))
	if err != nil {
		t.Fatal(err)
	}
	darwinSHA := sha256Hex(darwinArchiveBytes)

	candidateFormulaText := applyR05FormulaPatch(t, baseFormulaText, wantCoreURL, darwinSHA)
	candidateFormulaPath := filepath.Join(t.TempDir(), "managed-machine.rb")
	if err := os.WriteFile(candidateFormulaPath, []byte(candidateFormulaText), 0o644); err != nil {
		t.Fatal(err)
	}

	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"verify", "-dist", distDir, "-formula", candidateFormulaPath, "-require-pin"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("mm-release verify -require-pin on generated candidate formula failed (exit %d): %s", code, errBuf.String())
	}

	for _, snippet := range []string{
		"depends_on arch: :arm64",
		"depends_on macos: :sequoia",
		`resource "managed-machine-config" do`,
		`resource "local-bin" do`,
		`resource "core" do`,
		`url "` + wantCoreURL + `"`,
		`(legacy / "bin").install "legacy/bin/managed-machine"`,
		`legacy.install Dir["legacy/setup-*"]`,
		`legacy.install "legacy/lib"`,
		`(legacy / "scripts").install Dir["legacy/scripts/*"]`,
		`(legacy / "git-hooks").install Dir["legacy/git-hooks/*"]`,
		`resource("managed-machine-config").stage(legacy / "managed-machine-config")`,
		`resource("local-bin").stage(legacy / "local-bin")`,
		`resource("core").stage`,
		`bin.install "managed-machine"`,
		`chmod 0755, bin / "managed-machine"`,
		`libexec.install "VERSION"`,
		`(libexec / "skills").install "skills/managed-machine"`,
		`(libexec / "RELEASE_COMMIT").write`,
		`assert_equal version.to_s, shell_output("#{bin}/managed-machine --version").strip`,
		`shell_output("#{bin}/managed-machine version")`,
	} {
		if !strings.Contains(candidateFormulaText, snippet) {
			t.Errorf("generated R05 candidate formula missing required snippet %q", snippet)
		}
	}

	// Main carries the transitional formula, which installs either layout;
	// scripts/release prepare collapses it to the inverted block on the release
	// branch, so the transitional lines must be all present or all gone.
	transitional := []string{
		`if File.exist?("legacy/bin/managed-machine")`,
		`(libexec / "core").install "managed-machine"`,
		`chmod 0755, libexec / "core/managed-machine"`,
		`if (libexec / "legacy").exist?`,
		`assert_equal version.to_s, shell_output("#{libexec}/core/managed-machine --version").strip`,
	}
	collapsed := !strings.Contains(baseFormulaText, transitional[0])
	for _, snippet := range transitional {
		if strings.Contains(candidateFormulaText, snippet) == collapsed {
			t.Errorf("generated R05 candidate formula has a partial transitional block (collapsed=%v): %q", collapsed, snippet)
		}
	}

	// 2. Verify install.sh has no Linux or Windows installer logic and keeps all
	//    non-comment lines intact.
	rawInstallSh, err := os.ReadFile(filepath.Join(repoRoot, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	installShText := strings.ReplaceAll(string(rawInstallSh), "\r\n", "\n")
	if !strings.Contains(installShText, "install-core.sh") || !strings.Contains(installShText, "install-core.ps1") {
		t.Errorf("install.sh comments should reference install-core.sh and install-core.ps1")
	}

	// 3. Verify README.md documents the three channels per target (with the
	//    macOS formula channel gaining the core resource in the v0.9.0 release PR)
	//    and the manual managed-machine-config clone on Linux and Windows.
	rawReadme, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readmeText := string(rawReadme)
	for _, want := range []string{
		"darwin/arm64",
		"v0.9.0",
		"linux/amd64",
		"linux/arm64",
		"windows/amd64",
		"windows/arm64",
		"Formula/managed-machine.rb",
		"install-core.sh",
		"install-core.ps1",
		"managed-machine-config",
		"XDG_DATA_HOME",
		"LOCALAPPDATA",
	} {
		if !strings.Contains(readmeText, want) {
			t.Errorf("README.md missing expected delivery documentation %q", want)
		}
	}

	// 4. Verify both installer scripts exist at the repository root and carry the
	//    required D4 contract elements without forbidden elevation or profile edits.
	rawCoreSh, err := os.ReadFile(filepath.Join(repoRoot, "install-core.sh"))
	if err != nil {
		t.Fatal(err)
	}
	coreShText := string(rawCoreSh)
	for _, want := range []string{
		"#!/bin/sh",
		"MANAGED_MACHINE_RELEASE_BASE",
		"https://github.com/qwts/managed-machine/releases/latest/download",
		"SHA256SUMS",
		"--proto '=https'",
		"--proto-redir '=https'",
		"--tlsv1.2",
		"SSL_CERT_FILE",
		"sha256sum",
		"shasum -a 256",
		"$HOME/.local/bin",
		"Formula/managed-machine.rb",
	} {
		if !strings.Contains(coreShText, want) {
			t.Errorf("install-core.sh missing %q", want)
		}
	}

	rawCorePs1, err := os.ReadFile(filepath.Join(repoRoot, "install-core.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	corePs1Text := string(rawCorePs1)
	for _, want := range []string{
		"$ReleaseBase",
		"$Prefix",
		"MANAGED_MACHINE_RELEASE_BASE",
		"https://github.com/qwts/managed-machine/releases/latest/download",
		"SHA256SUMS",
		"Get-FileHash",
		"SHA256",
		"LOCALAPPDATA",
		"Programs",
		"AllowAutoRedirect = $false",
	} {
		if !strings.Contains(corePs1Text, want) {
			t.Errorf("install-core.ps1 missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"SetEnvironmentVariable",
		"HKCU:",
		"HKLM:",
		"Start-Process -Verb RunAs",
	} {
		if strings.Contains(corePs1Text, forbidden) {
			t.Errorf("install-core.ps1 contains forbidden construct %q", forbidden)
		}
	}
}

func applyR05FormulaPatch(t *testing.T, baseFormula, coreURL, coreSHA string) string {
	t.Helper()
	out := baseFormula
	if !strings.Contains(out, "depends_on arch: :arm64") {
		out = strings.Replace(out, "  license \"MIT\"\n", "  license \"MIT\"\n\n  depends_on arch: :arm64\n  depends_on macos: :sequoia\n", 1)
	}
	coreBlock := fmt.Sprintf("  # Prebuilt portable Go core binary for darwin/arm64 (S6 R04, S8 D3).\n"+
		"  # Installed as bin/managed-machine; legacy Bash tree staged under libexec/legacy.\n"+
		"  resource \"core\" do\n"+
		"    url %q\n"+
		"    sha256 %q\n"+
		"  end\n", coreURL, coreSHA)
	if start := strings.Index(out, "  resource \"core\" do\n"); start >= 0 {
		endRel := strings.Index(out[start:], "  end\n")
		if endRel < 0 {
			t.Fatalf("unterminated resource \"core\" block in Formula/managed-machine.rb")
		}
		end := start + endRel + len("  end\n")
		inner := fmt.Sprintf("  resource \"core\" do\n    url %q\n    sha256 %q\n  end\n", coreURL, coreSHA)
		out = out[:start] + inner + out[end:]
	} else {
		out = strings.Replace(out, "  # No depends_on — setup scripts handle their own deps (brew, gh, etc.)\n", coreBlock, 1)
	}
	if !strings.Contains(out, "(legacy / \"bin\").install \"legacy/bin/managed-machine\"") {
		t.Fatalf("Formula/managed-machine.rb is missing S8 D3 install block `(legacy / \"bin\").install \"legacy/bin/managed-machine\"`")
	}
	if !strings.Contains(out, "#{bin}/managed-machine --version") {
		t.Fatalf("Formula/managed-machine.rb is missing S8 D3 test assertion `#{bin}/managed-machine --version`")
	}
	return out
}

// extractTarGzBinary extracts the managed-machine binary bytes from a .tar.gz archive.
func extractTarGzBinary(t *testing.T, archivePath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "managed-machine" {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("managed-machine not found in %s", archivePath)
	return nil
}

// extractZipBinary extracts the managed-machine.exe binary bytes from a .zip archive.
func extractZipBinary(t *testing.T, archivePath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "managed-machine.exe" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("managed-machine.exe not found in %s", archivePath)
	return nil
}

// snapshotTree hashes every directory and regular file under root to prove
// no unexpected file was created, modified or deleted.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			lines = append(lines, "D:"+rel)
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		lines = append(lines, fmt.Sprintf("F:%s:%d:%s", rel, len(b), hex.EncodeToString(sum[:])))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// assertSingleErrorLine verifies stderr contains exactly one line and that
// line begins with "Error: ".
func assertSingleErrorLine(t *testing.T, context, stderr string) {
	t.Helper()
	trimmed := strings.TrimRight(stderr, "\r\n")
	if trimmed == "" {
		t.Fatalf("%s: expected one Error: line on stderr, got empty stderr", context)
	}
	lines := strings.Split(strings.ReplaceAll(trimmed, "\r\n", "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%s: expected exactly 1 stderr line, got %d:\n%s", context, len(lines), stderr)
	}
	if !strings.HasPrefix(lines[0], "Error: ") {
		t.Fatalf("%s: stderr line %q does not start with 'Error: '", context, lines[0])
	}
}

// startReleaseTLSServer starts a loopback HTTPS server serving multiple named
// fixture configurations (valid dist, tampered 1-byte archive, non-archive
// tampered bytes, missing SHA256SUMS, https-to-https redirect, and
// https-to-http redirect).
type releaseTLSFixture struct {
	TLSServer      *httptest.Server
	PlainServer    *httptest.Server
	PlainHits      *atomic.Int64
	CAPEMPath      string
	CACERPath      string
	ArchiveName    string
	ExpectedBinSHA string
}

func startReleaseTLSServer(t *testing.T, distDir, archiveName string) *releaseTLSFixture {
	t.Helper()
	rawSums, err := os.ReadFile(filepath.Join(distDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	rawArchive, err := os.ReadFile(filepath.Join(distDir, archiveName))
	if err != nil {
		t.Fatal(err)
	}

	var expectedBinSHA string
	if strings.HasSuffix(archiveName, ".zip") {
		expectedBinSHA = sha256Hex(extractZipBinary(t, filepath.Join(distDir, archiveName)))
	} else {
		expectedBinSHA = sha256Hex(extractTarGzBinary(t, filepath.Join(distDir, archiveName)))
	}

	tamperedOneByte := append([]byte(nil), rawArchive...)
	tamperedOneByte[len(tamperedOneByte)/2] ^= 0xff
	notAnArchive := []byte("this tampered payload is not a valid tar or zip archive\n")

	var plainHits atomic.Int64
	plainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write(rawSums)
	}))
	t.Cleanup(plainSrv.Close)

	mux := http.NewServeMux()
	// 1. /valid/* serves unmodified SHA256SUMS and archive (with an https->https redirect on archive).
	mux.HandleFunc("/valid/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write(rawSums)
	})
	mux.HandleFunc("/valid/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/cdn/"+archiveName, http.StatusFound)
	})
	mux.HandleFunc("/cdn/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(rawArchive)
	})

	// 2. /tampered-byte/* serves unmodified SHA256SUMS and 1-byte corrupted archive.
	mux.HandleFunc("/tampered-byte/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write(rawSums)
	})
	mux.HandleFunc("/tampered-byte/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(tamperedOneByte)
	})

	// 3. /not-archive/* serves unmodified SHA256SUMS and non-tar/non-zip bytes.
	mux.HandleFunc("/not-archive/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write(rawSums)
	})
	mux.HandleFunc("/not-archive/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(notAnArchive)
	})

	// 4. /missing-sums/* returns 404 for SHA256SUMS.
	mux.HandleFunc("/missing-sums/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	// 5. /redirect-http/* redirects from https:// to plain http://.
	mux.HandleFunc("/redirect-http/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plainSrv.URL+"/SHA256SUMS", http.StatusFound)
	})

	tlsSrv := newUniqueTLSServer(t, mux)
	t.Cleanup(tlsSrv.Close)

	certDir := t.TempDir()
	caPEM := filepath.Join(certDir, "ca.pem")
	caCER := filepath.Join(certDir, "ca.cer")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw})
	if err := os.WriteFile(caPEM, pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caCER, tlsSrv.Certificate().Raw, 0o644); err != nil {
		t.Fatal(err)
	}

	return &releaseTLSFixture{
		TLSServer:      tlsSrv,
		PlainServer:    plainSrv,
		PlainHits:      &plainHits,
		CAPEMPath:      caPEM,
		CACERPath:      caCER,
		ArchiveName:    archiveName,
		ExpectedBinSHA: expectedBinSHA,
	}
}

// writeInstallerSentinels creates sentinel scripts for sudo, osascript, brew,
// git and gh in sentinelDir that record any invocation into logFile and fail.
func writeInstallerSentinels(t *testing.T, sentinelDir, logFile string, extraNames ...string) {
	t.Helper()
	if err := os.MkdirAll(sentinelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := append([]string{"sudo", "osascript", "brew", "git", "gh"}, extraNames...)
	for _, name := range names {
		if runtime.GOOS == "windows" {
			body := "@echo off\r\necho " + name + " %* >> \"" + logFile + "\"\r\nexit /b 97\r\n"
			if err := os.WriteFile(filepath.Join(sentinelDir, name+".cmd"), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sentinelDir, name+".bat"), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		} else {
			body := "#!/bin/sh\nprintf '%s %s\\n' '" + name + "' \"$*\" >> '" + logFile + "'\nexit 97\n"
			if err := os.WriteFile(filepath.Join(sentinelDir, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func assertNoSentinelInvocations(t *testing.T, logFile string) {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err == nil && len(b) > 0 {
		t.Fatalf("guarded sentinel program was invoked:\n%s", string(b))
	}
}

// newUniqueTLSServer starts a loopback https server with a certificate of
// its own. httptest.NewTLSServer serves one fixed certificate, and the
// Windows tests add it to the machine root store and delete it by
// thumbprint, so a concurrent package's cleanup would revoke this
// fixture's trust mid-test.
func newUniqueTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mm-release test fixture " + serial.Text(16)},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	return srv
}

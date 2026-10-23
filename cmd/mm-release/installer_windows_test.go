//go:build windows

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallCorePs1(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	version, distDir := buildRepoReleaseDist(t)
	scriptPath := filepath.Join(repoRoot, "install-core.ps1")
	archiveName := fmt.Sprintf("managed-machine_%s_windows_%s.zip", version, runtime.GOARCH)
	wantBinBytes := extractZipBinary(t, filepath.Join(distDir, archiveName))
	wantBinSHA := sha256Hex(wantBinBytes)

	psExe := findPowerShellExe(t)

	// 1. Directory base install with explicit -Prefix, second-run idempotence,
	//    and default %LOCALAPPDATA%\Programs\managed-machine prefix.
	t.Run("directory-base", func(t *testing.T) {
		harness := t.TempDir()
		homeDir, localAppData, appData := setupWindowsHarnessHome(t, harness)
		tmpDir := filepath.Join(harness, "tmp")
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinelDir := filepath.Join(harness, "sentinels")
		sentinelLog := filepath.Join(harness, "sentinel.log")
		writeInstallerSentinels(t, sentinelDir, sentinelLog)

		prefixDir := filepath.Join(harness, "custom-prefix", "bin")
		beforeHome := snapshotTree(t, homeDir)
		beforeUserEnv := readWindowsUserEnvironment(t, psExe)
		pathEnv := sentinelDir + string(os.PathListSeparator) + os.Getenv("PATH")

		env := map[string]string{
			"USERPROFILE":                  homeDir,
			"HOME":                         homeDir,
			"LOCALAPPDATA":                 localAppData,
			"APPDATA":                      appData,
			"TEMP":                         tmpDir,
			"TMP":                          tmpDir,
			"MANAGED_MACHINE_RELEASE_BASE": distDir,
			"PATH":                         pathEnv,
		}
		code, stdout, stderr := runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", prefixDir}, env)
		if code != 0 {
			t.Fatalf("directory-base first install failed (exit %d): stderr=%q stdout=%q", code, stderr, stdout)
		}
		if stderr != "" {
			t.Errorf("unexpected stderr on first install: %q", stderr)
		}
		if !strings.Contains(stdout, wantBinSHA) {
			t.Errorf("first install stdout %q does not report binary digest %s", stdout, wantBinSHA)
		}
		if !strings.Contains(stdout, prefixDir) {
			t.Errorf("first install stdout %q does not name prefix %s", stdout, prefixDir)
		}

		installedBin := filepath.Join(prefixDir, "managed-machine.exe")
		gotBinBytes, err := os.ReadFile(installedBin)
		if err != nil {
			t.Fatalf("read installed binary: %v", err)
		}
		if gotSHA := sha256Hex(gotBinBytes); gotSHA != wantBinSHA {
			t.Fatalf("installed binary sha256 = %s, want %s", gotSHA, wantBinSHA)
		}

		// Execute installed binary with --version.
		verOut, err := exec.Command(installedBin, "--version").CombinedOutput()
		if err != nil {
			t.Fatalf("installed managed-machine.exe --version failed: %v (%s)", err, verOut)
		}
		if strings.TrimRight(string(verOut), "\r\n") != version {
			t.Errorf("installed managed-machine.exe --version = %q, want %q", string(verOut), version+"\n")
		}

		// Verify no file outside prefix and tmpDir was created: harness home is byte-identical.
		if afterHome := snapshotTree(t, homeDir); afterHome != beforeHome {
			t.Fatalf("harness home changed when installing to explicit -Prefix:\nbefore:\n%s\nafter:\n%s", beforeHome, afterHome)
		}
		if afterUserEnv := readWindowsUserEnvironment(t, psExe); afterUserEnv != beforeUserEnv {
			t.Fatalf("Windows user environment changed:\nbefore:\n%s\nafter:\n%s", beforeUserEnv, afterUserEnv)
		}

		// Second run: set a sentinel mtime in the past and verify the binary is not rewritten.
		pastTime := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
		if err := os.Chtimes(installedBin, pastTime, pastTime); err != nil {
			t.Fatal(err)
		}
		code2, stdout2, stderr2 := runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", prefixDir}, env)
		if code2 != 0 {
			t.Fatalf("second run failed (exit %d): stderr=%q stdout=%q", code2, stderr2, stdout2)
		}
		if stderr2 != "" {
			t.Errorf("unexpected stderr on second run: %q", stderr2)
		}
		if !strings.Contains(stdout2, wantBinSHA) || !strings.Contains(stdout2, prefixDir) {
			t.Errorf("second run stdout %q missing digest %s or prefix %s", stdout2, wantBinSHA, prefixDir)
		}
		fi, err := os.Stat(installedBin)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().UTC().Equal(pastTime) {
			t.Errorf("second run rewrote binary: mtime = %s, want %s", fi.ModTime().UTC(), pastTime)
		}

		// Default prefix (%LOCALAPPDATA%\Programs\managed-machine) when -Prefix is omitted.
		code3, stdout3, stderr3 := runPowerShellScript(t, psExe, scriptPath, nil, env)
		if code3 != 0 {
			t.Fatalf("default-prefix install failed (exit %d): stderr=%q", code3, stderr3)
		}
		defaultPrefix := filepath.Join(localAppData, "Programs", "managed-machine")
		if !strings.Contains(stdout3, defaultPrefix) || !strings.Contains(stdout3, wantBinSHA) {
			t.Errorf("default-prefix stdout %q missing prefix %s or digest %s", stdout3, defaultPrefix, wantBinSHA)
		}
		defaultBin := filepath.Join(defaultPrefix, "managed-machine.exe")
		if b, err := os.ReadFile(defaultBin); err != nil || sha256Hex(b) != wantBinSHA {
			t.Errorf("default-prefix binary missing or digest mismatch: %v", err)
		}
		assertNoSentinelInvocations(t, sentinelLog)
	})

	// 2. Loopback HTTPS base (trusted in Windows store on hosted runners),
	//    tampered archive, non-zip archive, missing SHA256SUMS, plain-http
	//    base, non-https redirect, and unsupported host.
	t.Run("loopback-https-and-negatives", func(t *testing.T) {
		fx := startReleaseTLSServer(t, distDir, archiveName)

		harness := t.TempDir()
		homeDir, localAppData, appData := setupWindowsHarnessHome(t, harness)
		tmpDir := filepath.Join(harness, "tmp")
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinelDir := filepath.Join(harness, "sentinels")
		sentinelLog := filepath.Join(harness, "sentinel.log")
		writeInstallerSentinels(t, sentinelDir, sentinelLog)
		pathEnv := sentinelDir + string(os.PathListSeparator) + os.Getenv("PATH")

		env := map[string]string{
			"USERPROFILE":  homeDir,
			"HOME":         homeDir,
			"LOCALAPPDATA": localAppData,
			"APPDATA":      appData,
			"TEMP":         tmpDir,
			"TMP":          tmpDir,
			"PATH":         pathEnv,
		}

		// 2a. Plain-http base: exit 2, one Error: line, 0 hits on plain server.
		fx.PlainHits.Store(0)
		httpPrefix := filepath.Join(harness, "http-prefix")
		env["MANAGED_MACHINE_RELEASE_BASE"] = fx.PlainServer.URL
		code, stdout, stderr := runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", httpPrefix}, env)
		if code != 2 {
			t.Errorf("plain-http base: exit %d, want 2 (stderr=%q)", code, stderr)
		}
		if stdout != "" {
			t.Errorf("plain-http base stdout = %q", stdout)
		}
		assertSingleErrorLine(t, "plain-http base", stderr)
		if hits := fx.PlainHits.Load(); hits != 0 {
			t.Errorf("plain-http server received %d requests, want 0", hits)
		}

		// 2b. Unsupported host: exit 2, one Error: line.
		hostPrefix := filepath.Join(harness, "unsupported-host")
		env["MANAGED_MACHINE_RELEASE_BASE"] = distDir
		env["PROCESSOR_ARCHITECTURE"] = "x86"
		env["PROCESSOR_ARCHITEW6432"] = ""
		code, stdout, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", hostPrefix}, env)
		delete(env, "PROCESSOR_ARCHITECTURE")
		delete(env, "PROCESSOR_ARCHITEW6432")
		if code != 2 {
			t.Errorf("unsupported host: exit %d, want 2 (stderr=%q)", code, stderr)
		}
		assertSingleErrorLine(t, "unsupported host", stderr)

		// 2b-dir. Tampered 1-byte archive, non-zip archive, and missing SHA256SUMS against directory base.
		emptyDir := filepath.Join(harness, "empty-dist")
		if err := os.MkdirAll(emptyDir, 0o755); err != nil {
			t.Fatal(err)
		}
		env["MANAGED_MACHINE_RELEASE_BASE"] = emptyDir
		code, _, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", filepath.Join(harness, "dir-missing-sums")}, env)
		if code != 1 {
			t.Errorf("dir missing SHA256SUMS: exit %d, want 1 (stderr=%q)", code, stderr)
		}
		assertSingleErrorLine(t, "dir missing SHA256SUMS", stderr)

		notZipDir := filepath.Join(harness, "not-zip-dist")
		if err := os.MkdirAll(notZipDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sumsRaw, err := os.ReadFile(filepath.Join(distDir, "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(notZipDir, "SHA256SUMS"), sumsRaw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(notZipDir, archiveName), []byte("not a valid zip file\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dirNotZipPrefix := filepath.Join(harness, "dir-not-zip-prefix")
		env["MANAGED_MACHINE_RELEASE_BASE"] = notZipDir
		code, _, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", dirNotZipPrefix}, env)
		if code != 1 {
			t.Errorf("dir non-zip tampered archive: exit %d, want 1 (stderr=%q)", code, stderr)
		}
		assertSingleErrorLine(t, "dir non-zip tampered archive", stderr)
		if !strings.Contains(stderr, "SHA-256 digest mismatch") {
			t.Errorf("dir non-zip tampered archive did not fail at digest check: %q", stderr)
		}
		if _, err := os.Stat(dirNotZipPrefix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dir non-zip tampered archive created prefix %s", dirNotZipPrefix)
		}

		// Trust loopback certificate in the Windows Root store when running on
		// a GitHub-hosted runner (matching the S2 gate in cmd/mm-walkthrough).
		if os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted" {
			cleanupCert, err := trustWindowsRootCert(fx.CACERPath)
			if err != nil {
				t.Fatalf("trust fixture certificate: %v", err)
			}
			defer cleanupCert()

			// 2c. Valid loopback HTTPS install + second-run idempotence.
			httpsPrefix := filepath.Join(harness, "https-prefix")
			env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/valid"
			code, stdout, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", httpsPrefix}, env)
			if code != 0 {
				t.Fatalf("HTTPS install failed (exit %d): stderr=%q stdout=%q", code, stderr, stdout)
			}
			if stderr != "" {
				t.Errorf("HTTPS install stderr = %q", stderr)
			}
			if !strings.Contains(stdout, wantBinSHA) || !strings.Contains(stdout, httpsPrefix) {
				t.Errorf("HTTPS install stdout %q missing digest %s or prefix %s", stdout, wantBinSHA, httpsPrefix)
			}
			installedBin := filepath.Join(httpsPrefix, "managed-machine.exe")
			verOut, err := exec.Command(installedBin, "--version").CombinedOutput()
			if err != nil || strings.TrimRight(string(verOut), "\r\n") != version {
				t.Fatalf("HTTPS installed binary --version = %q, err = %v", string(verOut), err)
			}
			pastTime := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
			if err := os.Chtimes(installedBin, pastTime, pastTime); err != nil {
				t.Fatal(err)
			}
			code2, stdout2, stderr2 := runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", httpsPrefix}, env)
			if code2 != 0 || stderr2 != "" || !strings.Contains(stdout2, wantBinSHA) {
				t.Fatalf("HTTPS second run: exit=%d stderr=%q stdout=%q", code2, stderr2, stdout2)
			}
			if fi, err := os.Stat(installedBin); err != nil || !fi.ModTime().UTC().Equal(pastTime) {
				t.Errorf("HTTPS second run rewrote binary")
			}

			// 2d. Single-byte tampered archive: exit 1, one Error: line, nothing written to prefix.
			tamperedPrefix := filepath.Join(harness, "tampered-prefix")
			env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/tampered-byte"
			code, stdout, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", tamperedPrefix}, env)
			if code != 1 {
				t.Errorf("tampered archive: exit %d, want 1 (stderr=%q)", code, stderr)
			}
			assertSingleErrorLine(t, "tampered archive", stderr)
			if _, err := os.Stat(tamperedPrefix); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("tampered archive created prefix %s", tamperedPrefix)
			}

			// 2e. Verify before extracting: tampered archive that is not a valid zip.
			notZipPrefix := filepath.Join(harness, "not-zip-prefix")
			env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/not-archive"
			code, stdout, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", notZipPrefix}, env)
			if code != 1 {
				t.Errorf("non-zip tampered archive: exit %d, want 1 (stderr=%q)", code, stderr)
			}
			assertSingleErrorLine(t, "non-zip tampered archive", stderr)
			if !strings.Contains(stderr, "SHA-256 digest mismatch") {
				t.Errorf("non-zip tampered archive did not fail at digest check: %q", stderr)
			}
			if _, err := os.Stat(notZipPrefix); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("non-zip tampered archive created prefix %s", notZipPrefix)
			}

			// 2f. Missing SHA256SUMS: exit 1, one Error: line.
			missingSumsPrefix := filepath.Join(harness, "missing-sums-prefix")
			env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/missing-sums"
			code, stdout, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", missingSumsPrefix}, env)
			if code != 1 {
				t.Errorf("missing SHA256SUMS: exit %d, want 1 (stderr=%q)", code, stderr)
			}
			assertSingleErrorLine(t, "missing SHA256SUMS", stderr)

			// 2g. Non-https redirect from https server: exit 1, one Error: line, 0 hits on plain server.
			fx.PlainHits.Store(0)
			redirPrefix := filepath.Join(harness, "redir-prefix")
			env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/redirect-http"
			code, stdout, stderr = runPowerShellScript(t, psExe, scriptPath, []string{"-Prefix", redirPrefix}, env)
			if code != 1 {
				t.Errorf("non-https redirect: exit %d, want 1 (stderr=%q)", code, stderr)
			}
			assertSingleErrorLine(t, "non-https redirect", stderr)
			if hits := fx.PlainHits.Load(); hits != 0 {
				t.Errorf("plain-http redirect target received %d requests, want 0", hits)
			}
		}

		assertNoSentinelInvocations(t, sentinelLog)
	})
}

func findPowerShellExe(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Fatal("neither pwsh.exe nor powershell.exe found on PATH")
	return ""
}

func setupWindowsHarnessHome(t *testing.T, harnessRoot string) (string, string, string) {
	t.Helper()
	homeDir := filepath.Join(harnessRoot, "home")
	localAppData := filepath.Join(homeDir, "AppData", "Local")
	appData := filepath.Join(homeDir, "AppData", "Roaming")
	psProfileDir := filepath.Join(homeDir, "Documents", "PowerShell")
	winPsProfileDir := filepath.Join(homeDir, "Documents", "WindowsPowerShell")
	for _, d := range []string{homeDir, localAppData, appData, psProfileDir, winPsProfileDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(psProfileDir, "Microsoft.PowerShell_profile.ps1"), []byte("# test pwsh profile\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(winPsProfileDir, "Microsoft.PowerShell_profile.ps1"), []byte("# test windows powershell profile\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return homeDir, localAppData, appData
}

func readWindowsUserEnvironment(t *testing.T, psExe string) string {
	t.Helper()
	cmd := exec.Command(psExe, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
		"[Environment]::GetEnvironmentVariables('User').GetEnumerator() | Sort-Object Name | ForEach-Object { \"$($_.Name)=$($_.Value)\" }")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read Windows User environment: %v (%s)", err, out)
	}
	return string(out)
}

func runPowerShellScript(t *testing.T, psExe, scriptPath string, args []string, envOverrides map[string]string) (int, string, string) {
	t.Helper()
	psArgs := append([]string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath}, args...)
	cmd := exec.Command(psExe, psArgs...)
	envMap := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			envMap[strings.ToUpper(k)] = k + "=" + v
		}
	}
	delete(envMap, "MANAGED_MACHINE_RELEASE_BASE")
	delete(envMap, "SSL_CERT_FILE")
	for k, v := range envOverrides {
		envMap[strings.ToUpper(k)] = k + "=" + v
	}
	env := make([]string, 0, len(envMap))
	for _, kv := range envMap {
		env = append(env, kv)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %s: %v", scriptPath, err)
		}
	}
	return code, stdout.String(), stderr.String()
}

func trustWindowsRootCert(cerPath string) (func(), error) {
	raw, err := os.ReadFile(cerPath)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(raw)
	thumbprint := strings.ToUpper(hex.EncodeToString(sum[:]))
	out, err := exec.Command("certutil", "-addstore", "Root", cerPath).CombinedOutput()
	if err != nil {
		return nil, errors.New("certutil -addstore: " + strings.TrimSpace(string(out)))
	}
	return func() {
		exec.Command("certutil", "-delstore", "Root", thumbprint).Run()
	}, nil
}

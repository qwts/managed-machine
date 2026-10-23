//go:build !windows

package main

import (
	"bytes"
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

func TestInstallCoreSh(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	version, distDir := buildRepoReleaseDist(t)
	scriptPath := filepath.Join(repoRoot, "install-core.sh")

	if runtime.GOOS == "darwin" {
		// On macos-15 (darwin/arm64), running install-core.sh with the real
		// host uname must exit 2 with one Error: line naming the formula,
		// without touching the home or prefix or starting any sentinel.
		t.Run("darwin-native-refusal", func(t *testing.T) {
			harness := t.TempDir()
			homeDir := setupUnixHarnessHome(t, harness)
			tmpDir := filepath.Join(harness, "tmp")
			if err := os.MkdirAll(tmpDir, 0o755); err != nil {
				t.Fatal(err)
			}
			sentinelDir := filepath.Join(harness, "sentinels")
			sentinelLog := filepath.Join(harness, "sentinel.log")
			writeInstallerSentinels(t, sentinelDir, sentinelLog)

			prefixDir := filepath.Join(harness, "prefix")
			beforeHome := snapshotTree(t, homeDir)

			code, stdout, stderr := runUnixScript(t, scriptPath, []string{"--prefix", prefixDir}, map[string]string{
				"HOME":                         homeDir,
				"TMPDIR":                       tmpDir,
				"MANAGED_MACHINE_RELEASE_BASE": distDir,
				"PATH":                         sentinelDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			})
			if code != 2 {
				t.Fatalf("install-core.sh on darwin: exit %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("install-core.sh on darwin: unexpected stdout %q", stdout)
			}
			assertSingleErrorLine(t, "darwin refusal", stderr)
			if !strings.Contains(stderr, "Formula/managed-machine.rb") && !strings.Contains(stderr, "qwts/managed-machine") {
				t.Errorf("darwin refusal stderr %q does not name the formula", stderr)
			}
			if _, err := os.Stat(prefixDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("darwin refusal created prefix directory %s", prefixDir)
			}
			if afterHome := snapshotTree(t, homeDir); afterHome != beforeHome {
				t.Errorf("darwin refusal modified harness home:\nbefore:\n%s\nafter:\n%s", beforeHome, afterHome)
			}
			assertNoSentinelInvocations(t, sentinelLog)
		})

		// Also exercise the full Linux install and negative suite on darwin by
		// wrapping the native darwin binary inside a linux_<arch>.tar.gz fixture
		// and providing a uname shim in PATH.
		t.Run("linux-suite-via-uname-shim", func(t *testing.T) {
			shimDist := makeHostRunnableLinuxDist(t, repoRoot, version, distDir, runtime.GOARCH)
			shimDir := writeUnameShim(t, "Linux", unameArchForGOARCH(runtime.GOARCH))
			runInstallCoreShSuite(t, scriptPath, version, shimDist, runtime.GOARCH, shimDir)
		})
		return
	}

	if runtime.GOOS == "linux" {
		runInstallCoreShSuite(t, scriptPath, version, distDir, runtime.GOARCH, "")
	}
}

func unameArchForGOARCH(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

func writeUnameShim(t *testing.T, sysname, machine string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uname-shim")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n  -s) printf '%%s\\n' %q ;;\n  -m) printf '%%s\\n' %q ;;\n  *) printf '%%s %%s\\n' %q %q ;;\nesac\n",
		sysname, machine, sysname, machine)
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// makeHostRunnableLinuxDist replaces the linux_<arch>.tar.gz binary in a copy
// of distDir with the host's native binary so the installed executable can be
// invoked with --version when the test runs on darwin.
func makeHostRunnableLinuxDist(t *testing.T, repoRoot, version, srcDist, arch string) string {
	t.Helper()
	dstDist := filepath.Join(t.TempDir(), "shim-dist")
	if err := os.MkdirAll(dstDist, 0o755); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(srcDist)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(srcDist, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dstDist, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	hostArchive := fmt.Sprintf("managed-machine_%s_%s_%s.tar.gz", version, runtime.GOOS, arch)
	hostBin := extractTarGzBinary(t, filepath.Join(srcDist, hostArchive))
	licenseRaw, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	licenseBytes := bytes.ReplaceAll(licenseRaw, []byte("\r\n"), []byte("\n"))

	linuxTarGz, err := buildTarGz([]archiveEntry{
		{Name: "LICENSE", Mode: 0o644, Data: licenseBytes},
		{Name: "managed-machine", Mode: 0o755, Data: hostBin},
	})
	if err != nil {
		t.Fatal(err)
	}
	linuxArchive := fmt.Sprintf("managed-machine_%s_linux_%s.tar.gz", version, arch)
	if err := os.WriteFile(filepath.Join(dstDist, linuxArchive), linuxTarGz, 0o644); err != nil {
		t.Fatal(err)
	}
	newSHA := sha256Hex(linuxTarGz)

	sumsRaw, err := os.ReadFile(filepath.Join(dstDist, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(sumsRaw), "\n"), "\n")
	for i, line := range lines {
		if strings.HasSuffix(line, "  "+linuxArchive) {
			lines[i] = newSHA + "  " + linuxArchive
		}
	}
	if err := os.WriteFile(filepath.Join(dstDist, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dstDist
}

func setupUnixHarnessHome(t *testing.T, harnessRoot string) string {
	t.Helper()
	homeDir := filepath.Join(harnessRoot, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]string{
		".profile":      "# test .profile\nexport PATH=\"/usr/bin:/bin\"\n",
		".bashrc":       "# test .bashrc\n",
		".bash_profile": "# test .bash_profile\n",
		".zshenv":       "# test .zshenv\n",
		".zprofile":     "# test .zprofile\n",
		".zshrc":        "# test .zshrc\n",
	}
	for name, content := range profiles {
		if err := os.WriteFile(filepath.Join(homeDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return homeDir
}

func runUnixScript(t *testing.T, scriptPath string, args []string, envOverrides map[string]string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{scriptPath}, args...)...)
	envMap := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			envMap[k] = v
		}
	}
	delete(envMap, "MANAGED_MACHINE_RELEASE_BASE")
	delete(envMap, "SSL_CERT_FILE")
	for k, v := range envOverrides {
		if v == "" && k == "UNSET_HOME" {
			delete(envMap, "HOME")
			continue
		}
		envMap[k] = v
	}
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
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

func runInstallCoreShSuite(t *testing.T, scriptPath, version, distDir, arch, unameShimDir string) {
	t.Helper()
	archiveName := fmt.Sprintf("managed-machine_%s_linux_%s.tar.gz", version, arch)
	wantBinBytes := extractTarGzBinary(t, filepath.Join(distDir, archiveName))
	wantBinSHA := sha256Hex(wantBinBytes)

	buildPathEnv := func(sentinelDir string) string {
		parts := []string{sentinelDir}
		if unameShimDir != "" {
			parts = append(parts, unameShimDir)
		}
		parts = append(parts, os.Getenv("PATH"))
		return strings.Join(parts, string(os.PathListSeparator))
	}

	// 1. Directory base install with explicit --prefix, second-run idempotence,
	//    and default $HOME/.local/bin prefix.
	t.Run("directory-base", func(t *testing.T) {
		harness := t.TempDir()
		homeDir := setupUnixHarnessHome(t, harness)
		tmpDir := filepath.Join(harness, "tmp")
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinelDir := filepath.Join(harness, "sentinels")
		sentinelLog := filepath.Join(harness, "sentinel.log")
		writeInstallerSentinels(t, sentinelDir, sentinelLog)

		prefixDir := filepath.Join(harness, "custom-prefix", "bin")
		beforeHome := snapshotTree(t, homeDir)
		pathEnv := buildPathEnv(sentinelDir)

		env := map[string]string{
			"HOME":                         homeDir,
			"TMPDIR":                       tmpDir,
			"MANAGED_MACHINE_RELEASE_BASE": distDir,
			"PATH":                         pathEnv,
		}
		code, stdout, stderr := runUnixScript(t, scriptPath, []string{"--prefix", prefixDir}, env)
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

		installedBin := filepath.Join(prefixDir, "managed-machine")
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
			t.Fatalf("installed managed-machine --version failed: %v (%s)", err, verOut)
		}
		if string(verOut) != version+"\n" {
			t.Errorf("installed managed-machine --version = %q, want %q", string(verOut), version+"\n")
		}

		// Verify no file outside prefix and tmpDir was created: harness home is byte-identical.
		if afterHome := snapshotTree(t, homeDir); afterHome != beforeHome {
			t.Fatalf("harness home changed when installing to explicit --prefix:\nbefore:\n%s\nafter:\n%s", beforeHome, afterHome)
		}
		// Verify tmpDir has no leftover files.
		if ents, err := os.ReadDir(tmpDir); err != nil || len(ents) != 0 {
			t.Errorf("temporary directory not clean after install: %v (err=%v)", ents, err)
		}

		// Second run: set a sentinel mtime in the past and verify the binary is not rewritten.
		pastTime := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
		if err := os.Chtimes(installedBin, pastTime, pastTime); err != nil {
			t.Fatal(err)
		}
		code2, stdout2, stderr2 := runUnixScript(t, scriptPath, []string{"--prefix", prefixDir}, env)
		if code2 != 0 {
			t.Fatalf("second run failed (exit %d): stderr=%q stdout=%q", code2, stderr2, stdout2)
		}
		if stderr2 != "" {
			t.Errorf("unexpected stderr on second run: %q", stderr2)
		}
		if !strings.Contains(stdout2, wantBinSHA) {
			t.Errorf("second run stdout %q does not report binary digest %s", stdout2, wantBinSHA)
		}
		if !strings.Contains(stdout2, prefixDir) {
			t.Errorf("second run stdout %q does not name prefix %s", stdout2, prefixDir)
		}
		fi, err := os.Stat(installedBin)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().UTC().Equal(pastTime) {
			t.Errorf("second run rewrote binary: mtime = %s, want %s", fi.ModTime().UTC(), pastTime)
		}
		if afterHome := snapshotTree(t, homeDir); afterHome != beforeHome {
			t.Errorf("harness home changed on second run")
		}

		// Default prefix ($HOME/.local/bin) when --prefix is omitted: shell profiles stay byte-identical.
		code3, stdout3, stderr3 := runUnixScript(t, scriptPath, nil, env)
		if code3 != 0 {
			t.Fatalf("default-prefix install failed (exit %d): stderr=%q", code3, stderr3)
		}
		defaultPrefix := filepath.Join(homeDir, ".local", "bin")
		if !strings.Contains(stdout3, defaultPrefix) || !strings.Contains(stdout3, wantBinSHA) {
			t.Errorf("default-prefix stdout %q missing prefix %s or digest %s", stdout3, defaultPrefix, wantBinSHA)
		}
		defaultBin := filepath.Join(defaultPrefix, "managed-machine")
		if b, err := os.ReadFile(defaultBin); err != nil || sha256Hex(b) != wantBinSHA {
			t.Errorf("default-prefix binary missing or digest mismatch: %v", err)
		}
		assertNoSentinelInvocations(t, sentinelLog)
	})

	// 2. Loopback HTTPS base (trusted via SSL_CERT_FILE), tampered archive,
	//    non-tar archive (verify before extracting), missing SHA256SUMS,
	//    plain-http base, non-https redirect, and unsupported hosts.
	t.Run("loopback-https-and-negatives", func(t *testing.T) {
		fx := startReleaseTLSServer(t, distDir, archiveName)

		harness := t.TempDir()
		homeDir := setupUnixHarnessHome(t, harness)
		tmpDir := filepath.Join(harness, "tmp")
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinelDir := filepath.Join(harness, "sentinels")
		sentinelLog := filepath.Join(harness, "sentinel.log")
		writeInstallerSentinels(t, sentinelDir, sentinelLog)
		pathEnv := buildPathEnv(sentinelDir)
		beforeHome := snapshotTree(t, homeDir)

		// 2a. Valid loopback HTTPS install + second-run idempotence.
		httpsPrefix := filepath.Join(harness, "https-prefix")
		env := map[string]string{
			"HOME":                         homeDir,
			"TMPDIR":                       tmpDir,
			"MANAGED_MACHINE_RELEASE_BASE": fx.TLSServer.URL + "/valid",
			"SSL_CERT_FILE":                fx.CAPEMPath,
			"PATH":                         pathEnv,
		}
		code, stdout, stderr := runUnixScript(t, scriptPath, []string{"--prefix", httpsPrefix}, env)
		if code != 0 {
			t.Fatalf("HTTPS install failed (exit %d): stderr=%q stdout=%q", code, stderr, stdout)
		}
		if stderr != "" {
			t.Errorf("HTTPS install stderr = %q", stderr)
		}
		if !strings.Contains(stdout, wantBinSHA) || !strings.Contains(stdout, httpsPrefix) {
			t.Errorf("HTTPS install stdout %q missing digest %s or prefix %s", stdout, wantBinSHA, httpsPrefix)
		}
		installedBin := filepath.Join(httpsPrefix, "managed-machine")
		verOut, err := exec.Command(installedBin, "--version").CombinedOutput()
		if err != nil || string(verOut) != version+"\n" {
			t.Fatalf("HTTPS installed binary --version = %q, err = %v", string(verOut), err)
		}
		pastTime := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
		if err := os.Chtimes(installedBin, pastTime, pastTime); err != nil {
			t.Fatal(err)
		}
		code2, stdout2, stderr2 := runUnixScript(t, scriptPath, []string{"--prefix", httpsPrefix}, env)
		if code2 != 0 || stderr2 != "" || !strings.Contains(stdout2, wantBinSHA) {
			t.Fatalf("HTTPS second run: exit=%d stderr=%q stdout=%q", code2, stderr2, stdout2)
		}
		if fi, err := os.Stat(installedBin); err != nil || !fi.ModTime().UTC().Equal(pastTime) {
			t.Errorf("HTTPS second run rewrote binary")
		}

		// 2b. Single-byte tampered archive: exit 1, one Error: line, nothing written to prefix.
		tamperedPrefix := filepath.Join(harness, "tampered-prefix")
		env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/tampered-byte"
		code, stdout, stderr = runUnixScript(t, scriptPath, []string{"--prefix", tamperedPrefix}, env)
		if code != 1 {
			t.Errorf("tampered archive: exit %d, want 1 (stderr=%q)", code, stderr)
		}
		if stdout != "" {
			t.Errorf("tampered archive stdout = %q, want empty", stdout)
		}
		assertSingleErrorLine(t, "tampered archive", stderr)
		if _, err := os.Stat(tamperedPrefix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("tampered archive created prefix %s", tamperedPrefix)
		}

		// 2c. Verify before extracting: tampered archive that is not a valid tar,
		//     plus a tar sentinel on PATH to prove tar is never invoked.
		notTarSentinelDir := filepath.Join(harness, "sentinels-no-tar")
		notTarLog := filepath.Join(harness, "sentinel-no-tar.log")
		writeInstallerSentinels(t, notTarSentinelDir, notTarLog, "tar")
		notTarPrefix := filepath.Join(harness, "not-tar-prefix")
		env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/not-archive"
		env["PATH"] = buildPathEnv(notTarSentinelDir)
		code, stdout, stderr = runUnixScript(t, scriptPath, []string{"--prefix", notTarPrefix}, env)
		if code != 1 {
			t.Errorf("non-tar tampered archive: exit %d, want 1 (stderr=%q)", code, stderr)
		}
		assertSingleErrorLine(t, "non-tar tampered archive", stderr)
		if _, err := os.Stat(notTarPrefix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("non-tar tampered archive created prefix %s", notTarPrefix)
		}
		assertNoSentinelInvocations(t, notTarLog)
		env["PATH"] = pathEnv

		// 2d. Missing SHA256SUMS: exit 1, one Error: line, nothing written.
		missingSumsPrefix := filepath.Join(harness, "missing-sums-prefix")
		env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/missing-sums"
		code, stdout, stderr = runUnixScript(t, scriptPath, []string{"--prefix", missingSumsPrefix}, env)
		if code != 1 {
			t.Errorf("missing SHA256SUMS: exit %d, want 1 (stderr=%q)", code, stderr)
		}
		assertSingleErrorLine(t, "missing SHA256SUMS", stderr)
		if _, err := os.Stat(missingSumsPrefix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing SHA256SUMS created prefix %s", missingSumsPrefix)
		}

		// 2e. Plain-http base: exit 2, one Error: line, 0 hits on plain server.
		fx.PlainHits.Store(0)
		httpPrefix := filepath.Join(harness, "http-prefix")
		env["MANAGED_MACHINE_RELEASE_BASE"] = fx.PlainServer.URL
		code, stdout, stderr = runUnixScript(t, scriptPath, []string{"--prefix", httpPrefix}, env)
		if code != 2 {
			t.Errorf("plain-http base: exit %d, want 2 (stderr=%q)", code, stderr)
		}
		assertSingleErrorLine(t, "plain-http base", stderr)
		if hits := fx.PlainHits.Load(); hits != 0 {
			t.Errorf("plain-http server received %d requests, want 0", hits)
		}
		if _, err := os.Stat(httpPrefix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("plain-http base created prefix %s", httpPrefix)
		}

		// 2f. Non-https redirect from https server: exit 1, one Error: line, 0 hits on plain server.
		fx.PlainHits.Store(0)
		redirPrefix := filepath.Join(harness, "redir-prefix")
		env["MANAGED_MACHINE_RELEASE_BASE"] = fx.TLSServer.URL + "/redirect-http"
		code, stdout, stderr = runUnixScript(t, scriptPath, []string{"--prefix", redirPrefix}, env)
		if code != 1 {
			t.Errorf("non-https redirect: exit %d, want 1 (stderr=%q)", code, stderr)
		}
		assertSingleErrorLine(t, "non-https redirect", stderr)
		if hits := fx.PlainHits.Load(); hits != 0 {
			t.Errorf("plain-http redirect target received %d requests, want 0", hits)
		}
		if _, err := os.Stat(redirPrefix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("non-https redirect created prefix %s", redirPrefix)
		}

		// 2g. Unsupported hosts (Darwin -> exit 2 naming formula; FreeBSD -> exit 2; Linux/riscv64 -> exit 2).
		for _, hc := range []struct {
			sysname     string
			machine     string
			wantFormula bool
		}{
			{"Darwin", "arm64", true},
			{"FreeBSD", "amd64", false},
			{"Linux", "riscv64", false},
		} {
			uDir := writeUnameShim(t, hc.sysname, hc.machine)
			hostPrefix := filepath.Join(harness, "host-"+hc.sysname+"-"+hc.machine)
			env["MANAGED_MACHINE_RELEASE_BASE"] = distDir
			env["PATH"] = sentinelDir + string(os.PathListSeparator) + uDir + string(os.PathListSeparator) + os.Getenv("PATH")
			code, stdout, stderr = runUnixScript(t, scriptPath, []string{"--prefix", hostPrefix}, env)
			if code != 2 {
				t.Errorf("host %s/%s: exit %d, want 2 (stderr=%q)", hc.sysname, hc.machine, code, stderr)
			}
			assertSingleErrorLine(t, hc.sysname+"/"+hc.machine, stderr)
			if hc.wantFormula && !strings.Contains(stderr, "Formula/managed-machine.rb") {
				t.Errorf("darwin host stderr %q does not name Formula/managed-machine.rb", stderr)
			}
		}

		if afterHome := snapshotTree(t, homeDir); afterHome != beforeHome {
			t.Errorf("harness home modified during HTTPS/negative tests:\nbefore:\n%s\nafter:\n%s", beforeHome, afterHome)
		}
		assertNoSentinelInvocations(t, sentinelLog)
	})
}

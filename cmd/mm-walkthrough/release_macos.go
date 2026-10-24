package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/internal/diag"
)

// releaseBrewPrefixOverride is a test seam used only by unit tests in
// release_test.go to point `upgrade-from-bash` at a fake brew installation
// inside t.TempDir(). It is always empty in production and CI runs.
var releaseBrewPrefixOverride string

func (h *releaseHarness) canRunBrew() bool {
	return hostedRunner() || releaseBrewPrefixOverride != ""
}

func (h *releaseHarness) brewBin() string {
	if releaseBrewPrefixOverride != "" {
		return filepath.Join(releaseBrewPrefixOverride, "bin", "brew")
	}
	for _, p := range []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "brew"
}

func (h *releaseHarness) resolveBrewPrefix() (string, error) {
	if releaseBrewPrefixOverride != "" {
		return releaseBrewPrefixOverride, nil
	}
	code, out, errOut := h.runBrew("--prefix")
	if code != 0 {
		return "", fmt.Errorf("brew --prefix exited %d: %s", code, firstLineOf(errOut, out))
	}
	pfx := strings.TrimSpace(string(out))
	if pfx == "" {
		return "", errors.New("brew --prefix returned empty string")
	}
	return pfx, nil
}

func (h *releaseHarness) cleanupBrew() {
	if !h.brewInstalled {
		return
	}
	_, _, _ = h.runBrew("uninstall", "--force", "managed-machine")
	h.brewInstalled = false
}

func (h *releaseHarness) runBrew(args ...string) (int, []byte, []byte) {
	brewExe := h.brewBin()
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "MANAGED_MACHINE_"):
			continue
		case k == "GIT_CONFIG_GLOBAL", k == "GIT_CONFIG_NOSYSTEM":
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_INSTALL_CLEANUP=1",
		"HOMEBREW_NO_ANALYTICS=1",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	if releaseBrewPrefixOverride != "" {
		env = append(env, "HOMEBREW_PREFIX="+releaseBrewPrefixOverride)
	}
	c := exec.Command(brewExe, args...)
	c.Env, c.Dir = env, h.root
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if x, ok := err.(*exec.ExitError); ok {
		code = x.ExitCode()
	} else if err != nil {
		code = -1
		e.WriteString(err.Error())
	}
	if o.Len() > 0 {
		h.captures = append(h.captures, o.Bytes())
	}
	if e.Len() > 0 {
		h.captures = append(h.captures, e.Bytes())
	}
	return code, o.Bytes(), e.Bytes()
}

// runGitIsolated runs git in dir with an isolated environment that never reads
// the host's ~/.gitconfig or system gitconfig and disables commit/tag signing.
func (h *releaseHarness) runGitIsolated(dir string, args ...string) (string, error) {
	fullArgs := append([]string{
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgSign=false",
		"-c", "protocol.file.allow=always",
		"-c", "init.defaultBranch=main",
	}, args...)
	c := exec.Command("git", fullArgs...)
	c.Dir = dir
	gitHome := filepath.Join(h.root, "git-isolated-home")
	_ = os.MkdirAll(gitHome, 0o755)
	c.Env = []string{
		"HOME=" + gitHome,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_AUTHOR_NAME=fixture",
		"GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=fixture",
		"GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"PATH=/usr/bin:/bin:/opt/homebrew/bin:/usr/local/bin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(e.String()))
	}
	return strings.TrimSpace(o.String()), nil
}

// ensureCandidateFormulaBase returns the candidate Formula/managed-machine.rb
// content for targetVer. If rawFormula does not yet carry the S6 R05 `core`
// resource block (because scripts/release prepare runs in the release PR), it
// adds depends_on arch: :arm64, depends_on macos: :sequoia, resource "core",
// libexec/core staging and the core test assertions, and updates version/tag
// to targetVer.
func ensureCandidateFormulaBase(rawFormula, targetVer string) (string, error) {
	out := strings.ReplaceAll(rawFormula, "\r\n", "\n")
	if targetVer == "" {
		return "", errors.New("targetVer is empty")
	}

	// Ensure main url tag and version match targetVer in the base formula.
	mainTagRe := regexp.MustCompile(`(?m)^(\s*url "https://github\.com/qwts/managed-machine\.git",\n\s*using:\s*:git,\n\s*tag:\s*)"v[^"]+"`)
	out = mainTagRe.ReplaceAllString(out, `${1}"v`+targetVer+`"`)
	verRe := regexp.MustCompile(`(?m)^(\s*version )"[^"]+"`)
	out = verRe.ReplaceAllString(out, `${1}"`+targetVer+`"`)

	if !strings.Contains(out, `resource "core" do`) {
		wantCoreURL := fmt.Sprintf("https://github.com/qwts/managed-machine/releases/download/v%s/managed-machine_%s_darwin_arm64.tar.gz", targetVer, targetVer)
		placeholderSHA := strings.Repeat("0", 64)
		out = strings.Replace(out,
			"  license \"MIT\"\n",
			"  license \"MIT\"\n\n  depends_on arch: :arm64\n  depends_on macos: :sequoia\n",
			1)
		coreBlock := fmt.Sprintf("  # Prebuilt portable Go core binary for darwin/arm64 (S6 R04, decisions D3/D4).\n"+
			"  # Staged to libexec/core/managed-machine beside the Bash dispatcher.\n"+
			"  resource \"core\" do\n"+
			"    url %q\n"+
			"    sha256 %q\n"+
			"  end\n", wantCoreURL, placeholderSHA)
		if strings.Contains(out, "  # No depends_on — setup scripts handle their own deps (brew, gh, etc.)\n") {
			out = strings.Replace(out, "  # No depends_on — setup scripts handle their own deps (brew, gh, etc.)\n", coreBlock, 1)
		} else {
			out = strings.Replace(out, "  def install\n", coreBlock+"\n  def install\n", 1)
		}
		stageBlock := "    resource(\"local-bin\").stage(libexec / \"local-bin\")\n\n" +
			"    # Stage the compiled Go core binary into libexec/core/managed-machine.\n" +
			"    (libexec / \"core\").mkpath\n" +
			"    resource(\"core\").stage do\n" +
			"      (libexec / \"core\").install \"managed-machine\"\n" +
			"      chmod 0755, libexec / \"core/managed-machine\"\n" +
			"    end\n"
		out = strings.Replace(out, "    resource(\"local-bin\").stage(libexec / \"local-bin\")\n", stageBlock, 1)
		testBlock := "    assert_match \"unknown setup name\", invalid\n\n" +
			"    assert_equal version.to_s, shell_output(\"#{libexec}/core/managed-machine --version\").strip\n" +
			"    shell_output(\"#{bin}/managed-machine version\")\n"
		out = strings.Replace(out, "    assert_match \"unknown setup name\", invalid\n", testBlock, 1)
	}

	for _, req := range []string{
		"depends_on arch: :arm64",
		"depends_on macos: :sequoia",
		`resource "core" do`,
		`resource("core").stage`,
	} {
		if !strings.Contains(out, req) {
			return "", fmt.Errorf("candidate formula base missing %q", req)
		}
	}
	return out, nil
}

// deriveOldFormula rewrites only the repository URLs (and synthetic seed
// revision) in the v0.8.12 Formula/managed-machine.rb content.
func deriveOldFormula(baseFormula, checkoutDir, configRepoDir, configRev, localBinDir string) (string, error) {
	out := strings.ReplaceAll(baseFormula, "\r\n", "\n")

	// 1. Main formula URL (stays tag: "v0.8.12").
	mainRe := regexp.MustCompile(`(?m)^(\s*url )"https://github\.com/qwts/managed-machine\.git"(,\n\s*using:\s*:git,\n\s*tag:\s*"v0\.8\.12")`)
	if !mainRe.MatchString(out) {
		return "", errors.New("old formula missing expected main url/tag block")
	}
	out = mainRe.ReplaceAllString(out, `${1}"file://`+filepath.ToSlash(checkoutDir)+`"${2}`)

	// 2. managed-machine-config resource URL and revision.
	cfgRe := regexp.MustCompile(`(?m)(resource "managed-machine-config" do\n\s*url )"https://github\.com/qwts/managed-machine-config\.git"(,\n\s*using:\s*:git,\n\s*tag:\s*"v0\.8\.12",\n\s*revision:\s*)"[0-9a-f]{40}"`)
	if !cfgRe.MatchString(out) {
		return "", errors.New("old formula missing expected managed-machine-config resource block")
	}
	out = cfgRe.ReplaceAllString(out, `${1}"file://`+filepath.ToSlash(configRepoDir)+`"${2}"`+configRev+`"`)

	// 3. local-bin resource URL.
	lbRe := regexp.MustCompile(`(?m)(resource "local-bin" do\n\s*url )"https://github\.com/qwts/local-bin\.git"`)
	if !lbRe.MatchString(out) {
		return "", errors.New("old formula missing expected local-bin resource block")
	}
	out = lbRe.ReplaceAllString(out, `${1}"file://`+filepath.ToSlash(localBinDir)+`"`)

	if err := assertFormulaDiffersOnlyInURLs(baseFormula, out); err != nil {
		return "", fmt.Errorf("old formula derivation violated URL-only rule: %w", err)
	}
	return out, nil
}

// deriveCandidateFormula rewrites only the URLs and pins in the candidate
// Formula/managed-machine.rb content per D6.
func deriveCandidateFormula(baseFormula, checkoutDir, candidateSHA, configRepoDir, configTag, configRev, localBinDir, coreArchivePath, coreSHA256 string) (string, error) {
	out := strings.ReplaceAll(baseFormula, "\r\n", "\n")

	// 1. Main formula URL -> file://<checkout>, revision: "<candidate sha>".
	mainRe := regexp.MustCompile(`(?m)^(\s*url )"https://github\.com/qwts/managed-machine\.git",\n(\s*)using:\s*:git,\n(\s*)(?:tag|revision):\s*"[^"]+"`)
	if !mainRe.MatchString(out) {
		return "", errors.New("candidate formula missing expected main url block")
	}
	out = mainRe.ReplaceAllString(out, `${1}"file://`+filepath.ToSlash(checkoutDir)+`",`+"\n"+`${2}using:    :git,`+"\n"+`${3}revision: "`+candidateSHA+`"`)

	// 2. managed-machine-config resource URL, tag and revision.
	cfgRe := regexp.MustCompile(`(?m)(resource "managed-machine-config" do\n\s*url )"https://github\.com/qwts/managed-machine-config\.git"(,\n\s*using:\s*:git,\n\s*tag:\s*)"[^"]+"(,\n\s*revision:\s*)"[0-9a-f]{40}"`)
	if !cfgRe.MatchString(out) {
		return "", errors.New("candidate formula missing expected managed-machine-config resource block")
	}
	out = cfgRe.ReplaceAllString(out, `${1}"file://`+filepath.ToSlash(configRepoDir)+`"${2}"v`+configTag+`"${3}"`+configRev+`"`)

	// 3. local-bin resource URL.
	lbRe := regexp.MustCompile(`(?m)(resource "local-bin" do\n\s*url )"https://github\.com/qwts/local-bin\.git"`)
	if !lbRe.MatchString(out) {
		return "", errors.New("candidate formula missing expected local-bin resource block")
	}
	out = lbRe.ReplaceAllString(out, `${1}"file://`+filepath.ToSlash(localBinDir)+`"`)

	// 4. core resource URL and sha256.
	coreRe := regexp.MustCompile(`(?m)(resource "core" do\n\s*url )"[^"]+"(\n\s*sha256 )"[0-9a-f]{64}"`)
	if !coreRe.MatchString(out) {
		return "", errors.New("candidate formula missing expected core resource block")
	}
	out = coreRe.ReplaceAllString(out, `${1}"file://`+filepath.ToSlash(coreArchivePath)+`"${2}"`+coreSHA256+`"`)

	if err := assertFormulaDiffersOnlyInURLs(baseFormula, out); err != nil {
		return "", fmt.Errorf("candidate formula derivation violated URL-only rule: %w", err)
	}
	return out, nil
}

var (
	formulaURLRe   = regexp.MustCompile(`(?m)^(\s*url )"[^"]+"(,?)$`)
	formulaUsingRe = regexp.MustCompile(`(?m)^(\s*)using:\s+:git,$`)
	formulaPinRe   = regexp.MustCompile(`(?m)^(\s*)(?:tag|revision):\s+"[^"]+"(,?)$`)
	formulaSHARe   = regexp.MustCompile(`(?m)^(\s*sha256 )"[0-9a-f]{64}"$`)
)

// formulaSkeleton normalizes only the URL and pin lines of a formula so two
// formulae can be compared byte-for-byte everywhere else.
func formulaSkeleton(formula string) string {
	s := strings.ReplaceAll(formula, "\r\n", "\n")
	s = formulaURLRe.ReplaceAllString(s, `${1}"<URL>"${2}`)
	s = formulaUsingRe.ReplaceAllString(s, `${1}using: :git,`)
	s = formulaPinRe.ReplaceAllString(s, `${1}pin: "<PIN>"${2}`)
	s = formulaSHARe.ReplaceAllString(s, `${1}"<SHA256>"`)
	return s
}

func assertFormulaDiffersOnlyInURLs(baseFormula, derivedFormula string) error {
	if strings.TrimSpace(baseFormula) == strings.TrimSpace(derivedFormula) {
		return errors.New("derived formula did not rewrite URLs")
	}
	if strings.Contains(derivedFormula, `url "https://github.com/qwts/`) {
		return errors.New("derived formula still contains a remote GitHub URL")
	}
	skBase := formulaSkeleton(baseFormula)
	skDerived := formulaSkeleton(derivedFormula)
	if skBase != skDerived {
		return errors.New("derived formula differs from base formula outside URL/pin lines")
	}
	return nil
}

// setupSyntheticRepos builds the local bare repositories for local-bin and
// managed-machine-config (with commits A, B, C and tags v0.8.12 and
// v<upgradeVer>) and clones the repository under test with its tags.
func (h *releaseHarness) setupSyntheticRepos() error {
	if h.fakeOriginBare != "" {
		return nil
	}
	reposDir := filepath.Join(h.root, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return err
	}

	// 1. Synthetic local-bin bare repo + working builder.
	localBinBare := filepath.Join(reposDir, "local-bin.git")
	localBinWork := filepath.Join(reposDir, "local-bin-work")
	if err := os.MkdirAll(localBinBare, 0o755); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinBare, "init", "--bare", "--initial-branch=main"); err != nil {
		return err
	}
	if err := os.MkdirAll(localBinWork, 0o755); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "init", "-b", "main"); err != nil {
		return err
	}
	if err := write(filepath.Join(localBinWork, "hello-fixture"), "#!/usr/bin/env bash\necho hello-fixture\n"); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Join(localBinWork, "hello-fixture"), 0o755)
	if _, err := h.runGitIsolated(localBinWork, "add", "."); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "commit", "-m", "synthetic local-bin v1.2.3"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "tag", "v1.2.3"); err != nil {
		return err
	}
	if err := write(filepath.Join(localBinWork, "hello-fixture"), "#!/usr/bin/env bash\necho hello-fixture-v1.2.4\n"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "add", "."); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "commit", "-m", "synthetic local-bin v1.2.4"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "tag", "v1.2.4"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "remote", "add", "origin", "file://"+filepath.ToSlash(localBinBare)); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(localBinWork, "push", "--quiet", "origin", "main", "--tags"); err != nil {
		return err
	}

	// 2. Synthetic managed-machine-config bare repo (fake origin) with commits A, B, C.
	fakeOriginBare := filepath.Join(reposDir, "managed-machine-config.git")
	fakeOriginWork := filepath.Join(reposDir, "managed-machine-config-work")
	if err := os.MkdirAll(fakeOriginBare, 0o755); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginBare, "init", "--bare", "--initial-branch=main"); err != nil {
		return err
	}
	if err := os.MkdirAll(fakeOriginWork, 0o755); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "init", "-b", "main"); err != nil {
		return err
	}

	appsJSONA := `{
  "schema_version": 1,
  "apps": [
    {"name": "vscode", "kind": "signed-cask", "token": "visual-studio-code", "app_name": "Visual Studio Code.app", "command": "code", "team_id": "FIXTURETEAM", "aliases": ["code"]},
    {"name": "grok-build", "kind": "official-cli", "command": "grok", "aliases": ["grok"], "url": "https://example.invalid/install.sh"},
    {"name": "optional-app", "kind": "brew-formula", "formula": "minikube", "auto": false}
  ]
}
`
	zshrcA := fmt.Sprintf("# synthetic zshrc (%s) commit-A\nexport PATH=\"$HOME/.local/bin:$PATH\"\n", releaseSeedMarkerZSH)
	authKeys := fmt.Sprintf("%s %s\n", h.seedFixtureKey.pubLine, releaseSeedMarkerAuthKeys)
	gateGroupYAML := fmt.Sprintf(`schema_version: 1
kind: access-group
id: gate
description: Synthetic release gate access group.
maintained_by:
  - alice
accounts:
  - %s
approved_fingerprints:
  - %s
`, h.account, h.aliceKey.fingerprint)
	keyDirYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory-source
path: %q
`, filepath.ToSlash(h.keyDir))

	seedFilesA := map[string]string{
		"apps.json":                     appsJSONA,
		"local-bin.ref":                 "v1.2.3\n",
		"zsh-functions.ref":             "main\n",
		"dotfiles/zsh/.zshrc":           zshrcA,
		"ssh/authorized_keys":           authKeys,
		"fleet/machines/.gitkeep":       "",
		"fleet/access-groups/gate.yaml": gateGroupYAML,
		"fleet/key-directory.yaml":      keyDirYAML,
	}
	for rel, content := range seedFilesA {
		if err := write(filepath.Join(fakeOriginWork, filepath.FromSlash(rel)), content); err != nil {
			return err
		}
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "add", "."); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "commit", "-m", "synthetic seed commit A"); err != nil {
		return err
	}
	commitA, err := h.runGitIsolated(fakeOriginWork, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "tag", "v0.8.12"); err != nil {
		return err
	}

	// Commit B: update local-bin.ref.
	if err := write(filepath.Join(fakeOriginWork, "local-bin.ref"), "v1.2.4\n"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "add", "local-bin.ref"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "commit", "-m", "synthetic seed commit B"); err != nil {
		return err
	}
	commitB, err := h.runGitIsolated(fakeOriginWork, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	// Commit C: update dotfiles/zsh/.zshrc and tag v<upgradeVer>.
	zshrcC := fmt.Sprintf("# synthetic zshrc (%s) commit-C\nexport PATH=\"$HOME/.local/bin:$PATH\"\n", releaseSeedMarkerZSH)
	if err := write(filepath.Join(fakeOriginWork, "dotfiles", "zsh", ".zshrc"), zshrcC); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "add", "dotfiles/zsh/.zshrc"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "commit", "-m", "synthetic seed commit C"); err != nil {
		return err
	}
	commitC, err := h.runGitIsolated(fakeOriginWork, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "tag", "-f", "v"+h.upgradeVer); err != nil {
		return err
	}

	if _, err := h.runGitIsolated(fakeOriginWork, "remote", "add", "origin", "file://"+filepath.ToSlash(fakeOriginBare)); err != nil {
		return err
	}
	// Push tags (v0.8.12 -> A, v<upgradeVer> -> C) and initially set origin/main to A.
	if _, err := h.runGitIsolated(fakeOriginWork, "push", "--quiet", "origin", "--tags"); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(fakeOriginWork, "push", "--quiet", "-f", "origin", commitA+":refs/heads/main"); err != nil {
		return err
	}

	// 3. Clone the repository under test with its tags into checkoutClone.
	checkoutClone := filepath.Join(reposDir, "managed-machine")
	if _, err := h.runGitIsolated(reposDir, "clone", "--quiet", "--no-hardlinks", h.repoRoot, checkoutClone); err != nil {
		return err
	}
	// Fetch tags from repoRoot in case any tag was not copied by clone.
	_, _ = h.runGitIsolated(checkoutClone, "fetch", "--quiet", "--tags", "origin")
	if h.upgradeVer != "" {
		vDisk, _ := os.ReadFile(filepath.Join(checkoutClone, "VERSION"))
		if strings.TrimSpace(string(vDisk)) != h.upgradeVer {
			if err := write(filepath.Join(checkoutClone, "VERSION"), h.upgradeVer+"\n"); err != nil {
				return err
			}
			if _, err := h.runGitIsolated(checkoutClone, "add", "VERSION"); err != nil {
				return err
			}
			if _, err := h.runGitIsolated(checkoutClone, "commit", "-m", "set candidate VERSION "+h.upgradeVer); err != nil {
				return err
			}
		}
	}

	// 4. Populate bashStubBin with deterministic hostname, python3 symlink, git symlink.
	if err := h.setupBashStubs(); err != nil {
		return err
	}

	h.localBinBare = localBinBare
	h.fakeOriginBare = fakeOriginBare
	h.fakeOriginWork = fakeOriginWork
	h.checkoutClone = checkoutClone
	h.commitA = commitA
	h.commitB = commitB
	h.commitC = commitC
	return nil
}

func (h *releaseHarness) setFakeOriginMain(commitSHA string) error {
	_, err := h.runGitIsolated(h.fakeOriginWork, "push", "--quiet", "-f", "origin", commitSHA+":refs/heads/main")
	return err
}

func (h *releaseHarness) setupBashStubs() error {
	if err := os.MkdirAll(h.bashStubBin, 0o755); err != nil {
		return err
	}
	for _, cmd := range []string{"rustup", "rustc", "cargo", "gh", "npm", "node", "gitleaks"} {
		p := filepath.Join(h.bashStubBin, cmd)
		if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\nexit 1\n"), 0o755); err != nil {
			return err
		}
	}
	hostScript := filepath.Join(h.bashStubBin, "hostname")
	if err := os.WriteFile(hostScript, []byte("#!/usr/bin/env bash\nprintf 'fixture-host\\n'\n"), 0o755); err != nil {
		return err
	}
	scutilScript := filepath.Join(h.bashStubBin, "scutil")
	if err := os.WriteFile(scutilScript, []byte("#!/usr/bin/env bash\nprintf 'fixture-host\\n'\n"), 0o755); err != nil {
		return err
	}
	for _, realProg := range []string{"python3", "git"} {
		if target, err := exec.LookPath(realProg); err == nil && target != "" {
			link := filepath.Join(h.bashStubBin, realProg)
			_ = os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedBashFixtureHome populates homeDir with the host-state fixture
// (tests/fixtures/host-state-characterization: manifests, machine.toml,
// enrolled key and gitconfig).
func (h *releaseHarness) seedBashFixtureHome(homeDir string) error {
	stateDir := filepath.Join(homeDir, ".config", "managed-machine")
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(sshDir, 0o700)

	machineTOML := `schema_version = 1
machine_id = "sha256-fixture_machine_aaaa"
hostname = "fixture-mac"
`
	bootstrapManifest := "schema_version=1\nmode=noninteractive\ncomplete\thomebrew\t\ncomplete\tgh\t\ndeferred\tvscode\tauthorization declined\nskipped\toptional-app\tauto false\nfailed\tgrok-build\texit 1\nfinished_at=2026-10-15T01:02:03Z\n"
	updateManifest := "schema_version=1\nversion=0.0.0-fixture\ncomplete\thomebrew\t\nfailed\tgrok-build\texit 1\nfinished_at=2026-10-16T01:02:03Z\n"
	localBinManifest := fmt.Sprintf("schema_version=1\nref=v1.2.3\ncommit=0123456789abcdef0123456789abcdef01234567\ncheckout=%s/.local/share/managed-machine/local-bin/v1.2.3\nrecorded_at=2026-10-15T01:02:03Z\n",
		filepath.ToSlash(homeDir))
	zshFunctionsManifest := fmt.Sprintf("schema_version=1\nref=main\ncommit=89abcdef0123456789abcdef0123456789abcdef\ncheckout=%s/.local/share/managed-machine/zsh-functions/main\nrecorded_at=2026-10-15T01:02:03Z\n",
		filepath.ToSlash(homeDir))
	orgProfileJSON := `{"identities": [
  {"slug": "fixture-agent", "status": "active", "harness": "grok"},
  {"slug": "second-agent", "status": "active", "harness": "grok"},
  {"slug": "retired-agent", "status": "retired", "harness": "grok"}
]}
`
	files := map[string]string{
		filepath.Join(stateDir, "machine.toml"):              machineTOML,
		filepath.Join(stateDir, "bootstrap.manifest"):        bootstrapManifest,
		filepath.Join(stateDir, "update.manifest"):           updateManifest,
		filepath.Join(stateDir, "local-bin.manifest"):        localBinManifest,
		filepath.Join(stateDir, "zsh-functions.manifest"):    zshFunctionsManifest,
		filepath.Join(stateDir, "organization-profile.json"): orgProfileJSON,
		filepath.Join(sshDir, "id_rsa_github"):               "fixture-private-key-placeholder\n",
		filepath.Join(sshDir, "id_rsa_github.pub"):           h.hostStateKey.pubLine + "\n",
		filepath.Join(homeDir, ".gitconfig"):                 fmt.Sprintf("[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = %s/.ssh/id_rsa_github.pub\n", filepath.ToSlash(homeDir)),
	}
	for p, body := range files {
		if err := write(p, body); err != nil {
			return err
		}
	}
	_ = os.Chmod(filepath.Join(sshDir, "id_rsa_github"), 0o600)
	return nil
}

// snapshotLegacyManifests hashes machine.toml and every *.manifest in
// homeDir/.config/managed-machine.
func snapshotLegacyManifests(homeDir string) (map[string]string, error) {
	cfgDir := filepath.Join(homeDir, ".config", "managed-machine")
	ents, err := os.ReadDir(cfgDir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "machine.toml" || strings.HasSuffix(name, ".manifest") || name == "organization-profile.json" {
			b, err := os.ReadFile(filepath.Join(cfgDir, name))
			if err != nil {
				return nil, err
			}
			out[name] = sum(b)
		}
	}
	return out, nil
}

func (h *releaseHarness) snapshotGitCheckout(dir string) (string, error) {
	head, err := h.runGitIsolated(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	branch, err := h.runGitIsolated(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		branch = "detached"
	}
	origin, err := h.runGitIsolated(dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	statusOut, err := h.runGitIsolated(dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	tree := map[string]string{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		tree[filepath.ToSlash(rel)] = sum(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	fmt.Fprintf(&sb, "head=%s\nbranch=%s\norigin=%s\nstatus=%s\n", head, branch, origin, statusOut)
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%s\n", k, tree[k])
	}
	return sb.String(), nil
}

// execManagedMachineClean invokes binPath under a clean environment (`env -i`
// semantics) with the given homeDir, brewPrefix, and extraEnv. When
// useCoreSentinels is true, h.bins (with the git/brew/ssh sentinels) is placed
// first on PATH and sentinelLogEnv is exported so any external program started
// by the dispatcher or core is caught.
func (h *releaseHarness) execManagedMachineClean(binPath, homeDir, brewPrefix string, useCoreSentinels bool, extraEnv []string, args ...string) (int, []byte, []byte) {
	xdgData := filepath.Join(homeDir, ".local", "share")
	tmpDir := filepath.Join(homeDir, "tmp")
	_ = os.MkdirAll(xdgData, 0o755)
	_ = os.MkdirAll(tmpDir, 0o755)

	var pathParts []string
	if useCoreSentinels {
		pathParts = append(pathParts, filepath.Join(h.root, "sentinels"))
	}
	if brewPrefix != "" {
		brewBinDir := filepath.Join(brewPrefix, "bin")
		if !useCoreSentinels {
			pathParts = append(pathParts, brewBinDir)
		}
	}
	if h.bashStubBin != "" && !useCoreSentinels {
		pathParts = append(pathParts, h.bashStubBin)
	}
	pathParts = append(pathParts, "/usr/bin", "/bin", "/usr/sbin", "/sbin")

	env := []string{
		"HOME=" + homeDir,
		"XDG_DATA_HOME=" + xdgData,
		"TMPDIR=" + tmpDir,
		"TMP=" + tmpDir,
		"TEMP=" + tmpDir,
		"PATH=" + strings.Join(pathParts, string(os.PathListSeparator)),
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if brewPrefix != "" {
		env = append(env, "HOMEBREW_PREFIX="+brewPrefix)
	}
	if h.fakeOriginBare != "" {
		env = append(env, "MANAGED_MACHINE_CONFIG_REPO_URL=file://"+filepath.ToSlash(h.fakeOriginBare))
	}
	if useCoreSentinels {
		env = append(env, sentinelLogEnv+"="+h.log)
	}
	env = append(env, extraEnv...)

	c := exec.Command(binPath, args...)
	c.Env, c.Dir = env, homeDir
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if x, ok := err.(*exec.ExitError); ok {
		code = x.ExitCode()
	} else if err != nil {
		code = -1
		e.WriteString(err.Error())
	}
	if o.Len() > 0 {
		h.captures = append(h.captures, o.Bytes())
	}
	if e.Len() > 0 {
		h.captures = append(h.captures, e.Bytes())
	}
	return code, o.Bytes(), e.Bytes()
}

func normalizeStatusJSONVersion(raw []byte) (string, string, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", "", err
	}
	secs, ok := doc["sections"].(map[string]any)
	if !ok {
		return "", "", errors.New("status --json missing sections object")
	}
	mmSec, ok := secs["managed-machine"].(map[string]any)
	if !ok {
		return "", "", errors.New("status --json missing sections.managed-machine")
	}
	ver, _ := mmSec["summary"].(string)
	mmSec["summary"] = "<version>"
	if mmSec["checkout"] != nil {
		mmSec["checkout"] = "<version>"
	}
	norm, err := json.Marshal(doc)
	if err != nil {
		return "", "", err
	}
	return ver, string(norm), nil
}

// copyTree copies srcDir (dereferencing srcDir itself if it is a symlink) into
// dstDir, preserving regular file permissions and inner symlinks.
func copyTree(srcDir, dstDir string) error {
	if resolved, err := filepath.EvalSymlinks(srcDir); err == nil {
		srcDir = resolved
	}
	_ = os.RemoveAll(dstDir)
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkDest, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(linkDest, target)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

// ensureStagedTrees guarantees that h.stagedOld/opt/managed-machine (v0.8.12
// layout with seed at commitA) and h.stagedNew/opt/managed-machine (candidate
// layout with seed at commitC and libexec/core/managed-machine) exist. When
// upgrade-from-bash ran first, these are already copies of the Homebrew-staged
// kegs; when private-seed or dispatcher runs standalone in unit tests, this
// materializes the exact formula layout from the local repositories.
func (h *releaseHarness) ensureStagedTrees() error {
	if err := h.setupSyntheticRepos(); err != nil {
		return err
	}
	oldOpt := filepath.Join(h.stagedOld, "opt", "managed-machine")
	if _, err := os.Stat(filepath.Join(oldOpt, "bin", "managed-machine")); err != nil {
		if err := h.stageFormulaTreeFromGit(oldOpt, "v0.8.12", h.commitA, false); err != nil {
			return fmt.Errorf("stage v0.8.12 tree: %w", err)
		}
	}
	newOpt := filepath.Join(h.stagedNew, "opt", "managed-machine")
	if _, err := os.Stat(filepath.Join(newOpt, "bin", "managed-machine")); err != nil {
		if err := h.stageFormulaTreeFromGit(newOpt, "HEAD", h.commitC, true); err != nil {
			return fmt.Errorf("stage candidate tree: %w", err)
		}
	}
	return nil
}

func (h *releaseHarness) stageFormulaTreeFromGit(optDir, checkoutRef, seedCommit string, withCore bool) error {
	tmpCheckout := filepath.Join(h.root, "stage-work-"+filepath.Base(filepath.Dir(optDir)))
	_ = os.RemoveAll(tmpCheckout)
	if _, err := h.runGitIsolated(h.root, "clone", "--quiet", "--no-hardlinks", h.checkoutClone, tmpCheckout); err != nil {
		return err
	}
	defer os.RemoveAll(tmpCheckout)
	if _, err := h.runGitIsolated(tmpCheckout, "checkout", "--quiet", checkoutRef); err != nil {
		return err
	}
	commitSHA, err := h.runGitIsolated(tmpCheckout, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	libexecDir := filepath.Join(optDir, "libexec")
	binDir := filepath.Join(optDir, "bin")
	if err := os.MkdirAll(libexecDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	ents, err := os.ReadDir(tmpCheckout)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "setup-") && !e.IsDir() {
			b, err := os.ReadFile(filepath.Join(tmpCheckout, e.Name()))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(libexecDir, e.Name()), b, 0o755); err != nil {
				return err
			}
		}
	}
	for _, sub := range []string{"lib", "scripts", "git-hooks"} {
		src := filepath.Join(tmpCheckout, sub)
		if _, err := os.Stat(src); err == nil {
			if err := copyTree(src, filepath.Join(libexecDir, sub)); err != nil {
				return err
			}
		}
	}

	seedDest := filepath.Join(libexecDir, "managed-machine-config")
	if _, err := h.runGitIsolated(h.root, "clone", "--quiet", "--no-hardlinks", h.fakeOriginBare, seedDest); err != nil {
		return err
	}
	if _, err := h.runGitIsolated(seedDest, "checkout", "--quiet", "--detach", seedCommit); err != nil {
		return err
	}

	lbDest := filepath.Join(libexecDir, "local-bin")
	if _, err := h.runGitIsolated(h.root, "clone", "--quiet", "--no-hardlinks", h.localBinBare, lbDest); err != nil {
		return err
	}

	if vBytes, err := os.ReadFile(filepath.Join(tmpCheckout, "VERSION")); err == nil {
		if err := os.WriteFile(filepath.Join(libexecDir, "VERSION"), vBytes, 0o644); err != nil {
			return err
		}
		if err := copyTree(filepath.Join(tmpCheckout, "skills", "managed-machine"), filepath.Join(libexecDir, "skills", "managed-machine")); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(libexecDir, "RELEASE_COMMIT"), []byte(commitSHA+"\n"), 0o644); err != nil {
			return err
		}
	}

	if withCore {
		coreDir := filepath.Join(libexecDir, "core")
		if err := os.MkdirAll(coreDir, 0o755); err != nil {
			return err
		}
		archivePath := filepath.Join(h.upgradeDistDir, targetArchiveName(h.upgradeVer, "darwin/arm64"))
		coreBytes, err := extractArchiveBinaryBytes(archivePath)
		if err != nil {
			return err
		}
		if err := writeExecutableAtomically(filepath.Join(coreDir, "managed-machine"), coreBytes); err != nil {
			return err
		}
	}

	dispBytes, err := os.ReadFile(filepath.Join(tmpCheckout, "bin", "managed-machine"))
	if err != nil {
		return err
	}
	return writeExecutableAtomically(filepath.Join(binDir, "managed-machine"), dispBytes)
}

// runUpgradeFromBash implements the D6 macOS Homebrew upgrade-from-bash case.
func (h *releaseHarness) runUpgradeFromBash() {
	const name = caseReleaseUpgradeFromBash
	if !strings.HasPrefix(h.target, "darwin/") {
		h.recordCase(name, "n-a", "n-a", true, releaseFirstReleaseReason)
		return
	}
	if !h.canRunBrew() {
		h.recordCase(name, "ok", observedFailed, false, releaseHostedBrewRefusal)
		return
	}
	if err := h.setupSyntheticRepos(); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "setup synthetic repos: "+err.Error())
		return
	}

	// Derive old (v0.8.12) and new (candidate) formulae and verify they differ
	// from the base formulae only in URLs/pins.
	oldBaseRaw, err := h.runGitIsolated(h.repoRoot, "show", "v0.8.12:Formula/managed-machine.rb")
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "read v0.8.12 formula: "+err.Error())
		return
	}
	oldDerived, err := deriveOldFormula(oldBaseRaw, h.checkoutClone, h.fakeOriginBare, h.commitA, h.localBinBare)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	candRawBytes, err := os.ReadFile(filepath.Join(h.repoRoot, "Formula", "managed-machine.rb"))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "read candidate formula: "+err.Error())
		return
	}
	candBase, err := ensureCandidateFormulaBase(string(candRawBytes), h.upgradeVer)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "prepare candidate formula base: "+err.Error())
		return
	}
	candidateSHA, err := h.runGitIsolated(h.checkoutClone, "rev-parse", "HEAD")
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "rev-parse candidate HEAD: "+err.Error())
		return
	}
	coreArchive := filepath.Join(h.upgradeDistDir, targetArchiveName(h.upgradeVer, "darwin/arm64"))
	coreArchiveBytes, err := os.ReadFile(coreArchive)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "read darwin/arm64 core archive: "+err.Error())
		return
	}
	coreSHA := sum(coreArchiveBytes)
	newDerived, err := deriveCandidateFormula(candBase, h.checkoutClone, candidateSHA, h.fakeOriginBare, h.upgradeVer, h.commitC, h.localBinBare, coreArchive, coreSHA)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	oldFormulaPath := filepath.Join(h.root, "formulae", "old", "managed-machine.rb")
	newFormulaPath := filepath.Join(h.root, "formulae", "new", "managed-machine.rb")
	activeFormulaPath := filepath.Join(h.root, "formulae", "managed-machine.rb")
	if err := write(oldFormulaPath, oldDerived); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	if err := write(newFormulaPath, newDerived); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	h.step("upgrade-from-bash formula derivation", "v0.8.12 and candidate formulae derived with URL-only rewrites", true)

	brewPrefix, err := h.resolveBrewPrefix()
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	// Ensure managed-machine is not pre-installed before snapshotting brew list --versions.
	_, _, _ = h.runBrew("uninstall", "--force", "managed-machine")
	code, brewVersBefore, brewErr := h.runBrew("list", "--versions")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("brew list --versions before exited %d: %s", code, firstLineOf(brewErr, brewVersBefore)))
		return
	}
	beforeBrewHash := sum(brewVersBefore)

	if err := h.seedBashFixtureHome(h.bashHome); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "seed host-state fixture home: "+err.Error())
		return
	}
	beforeManifests, err := snapshotLegacyManifests(h.bashHome)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "snapshot legacy manifests: "+err.Error())
		return
	}

	// Ensure fake origin main starts at commitA for step (1).
	if err := h.setFakeOriginMain(h.commitA); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "set fake origin main: "+err.Error())
		return
	}

	// (1) brew install --formula <old> installs v0.8.12 into the runner's Homebrew.
	if err := write(activeFormulaPath, oldDerived); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut := h.runBrew("install", "--formula", activeFormulaPath)
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 1 (brew install v0.8.12) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.brewInstalled = true

	installedBin := filepath.Join(brewPrefix, "opt", "managed-machine", "bin", "managed-machine")
	code, vOut, vErr := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "--version")
	if code != 0 || strings.TrimSpace(string(vOut)) != "0.8.12" {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 1 (--version) exited %d: got %q (stderr %q), want \"0.8.12\"",
			code, strings.TrimSpace(string(vOut)), firstLineOf(vErr)))
		return
	}

	// setup --list materializes the writable checkout from the bundled seed.
	code, out, errOut = h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "setup", "--list")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 1 (setup --list) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	checkoutDir := filepath.Join(h.bashHome, ".local", "share", "managed-machine", "managed-machine-config")
	beforeCheckout, err := h.snapshotGitCheckout(checkoutDir)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "step 1 checkout snapshot: "+err.Error())
		return
	}
	if !strings.Contains(beforeCheckout, "head="+h.commitA+"\n") || !strings.Contains(beforeCheckout, "branch=main\n") {
		h.recordCase(name, "ok", observedFailed, false, "step 1 materialized checkout not on main at commit A: "+firstLineOf([]byte(beforeCheckout)))
		return
	}

	code, status1Bytes, errOut := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "status", "--json")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 1 (status --json) exited %d: %s", code, firstLineOf(errOut, status1Bytes)))
		return
	}
	ver1, normStatus1, err := normalizeStatusJSONVersion(status1Bytes)
	if err != nil || ver1 != "0.8.12" {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 1 status --json version = %q (%v), want \"0.8.12\"", ver1, err))
		return
	}
	_ = copyTree(filepath.Join(brewPrefix, "opt", "managed-machine"), filepath.Join(h.stagedOld, "opt", "managed-machine"))
	h.step("upgrade-from-bash step 1 (v0.8.12)", "installed v0.8.12, materialized checkout from seed, recorded status --json", true)

	// (2) brew upgrade --formula <new> takes Homebrew's upgrade path (opt link repointed, old keg retained).
	if err := write(activeFormulaPath, newDerived); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runBrew("upgrade", "--formula", activeFormulaPath)
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 (brew upgrade candidate) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	optLink := filepath.Join(brewPrefix, "opt", "managed-machine")
	linkTarget, err := os.Readlink(optLink)
	if err != nil || !strings.Contains(filepath.ToSlash(linkTarget), "Cellar/managed-machine/"+h.upgradeVer) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 opt link target = %q (%v), want Cellar/managed-machine/%s", linkTarget, err, h.upgradeVer))
		return
	}
	oldKegDir := filepath.Join(brewPrefix, "Cellar", "managed-machine", "0.8.12")
	if _, err := os.Stat(oldKegDir); err != nil {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 old 0.8.12 keg was not retained at %s: %v", oldKegDir, err))
		return
	}
	_ = copyTree(filepath.Join(brewPrefix, "opt", "managed-machine"), filepath.Join(h.stagedNew, "opt", "managed-machine"))

	// --version through the dispatcher and through libexec/core/managed-machine.
	code, vOut, vErr = h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "--version")
	if code != 0 || strings.TrimSpace(string(vOut)) != h.upgradeVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 dispatcher --version exited %d: got %q (stderr %q), want %q",
			code, strings.TrimSpace(string(vOut)), firstLineOf(vErr), h.upgradeVer))
		return
	}
	installedCore := filepath.Join(brewPrefix, "opt", "managed-machine", "libexec", "core", "managed-machine")
	code, vOut, vErr = h.execManagedMachineClean(installedCore, h.bashHome, brewPrefix, true, nil, "--version")
	if code != 0 || strings.TrimSpace(string(vOut)) != h.upgradeVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 core --version exited %d: got %q (stderr %q), want %q",
			code, strings.TrimSpace(string(vOut)), firstLineOf(vErr), h.upgradeVer))
		return
	}

	// status --json equals the record of (1) with the version field normalized.
	code, status2Bytes, errOut := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "status", "--json")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 (status --json) exited %d: %s", code, firstLineOf(errOut, status2Bytes)))
		return
	}
	ver2, normStatus2, err := normalizeStatusJSONVersion(status2Bytes)
	if err != nil || ver2 != h.upgradeVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 status --json version = %q (%v), want %q", ver2, err, h.upgradeVer))
		return
	}
	if normStatus1 != normStatus2 {
		h.recordCase(name, "ok", observedFailed, false, "step 2 status --json differs from step 1 after normalizing version")
		return
	}

	// Every legacy manifest and machine.toml byte-identical; checkout untouched.
	afterUpgradeManifests, err := snapshotLegacyManifests(h.bashHome)
	if err != nil || len(profileDiff(beforeManifests, afterUpgradeManifests)) > 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 legacy manifests changed across upgrade: %v (%v)",
			profileDiff(beforeManifests, afterUpgradeManifests), err))
		return
	}
	afterUpgradeCheckout, err := h.snapshotGitCheckout(checkoutDir)
	if err != nil || afterUpgradeCheckout != beforeCheckout {
		h.recordCase(name, "ok", observedFailed, false, "step 2 writable checkout changed during brew upgrade")
		return
	}

	// New-core routes through the dispatcher: status --section software, fleet --keys, software --available, version.
	coreRouteEnv := []string{
		"MANAGED_MACHINE_SOURCE_DIR=" + h.defs,
	}
	for _, route := range [][]string{
		{"status", "--section", "software"},
		{"fleet", "--keys"},
		{"software", "--available"},
		{"version"},
	} {
		cCode, cOut, cErr := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, true, coreRouteEnv, route...)
		if cCode != 0 || len(cErr) != 0 {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 dispatcher %v exited %d: %s", route, cCode, firstLineOf(cErr, cOut)))
			return
		}
		recs, err := parseJSONL(cOut)
		if err != nil || (route[0] != "status" && len(recs) == 0) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 dispatcher %v invalid JSONL: %v", route, err))
			return
		}
		if route[0] == "fleet" && (len(recs) < 2 || str(recs[1], "owner") != "alice" || str(recs[1], "fingerprint") != h.aliceKey.fingerprint) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 2 fleet --keys unexpected records: %v", recs))
			return
		}
	}

	// AC-T-04 cycle of a fixture config definition through the dispatcher, creating
	// state under ~/Library/Application Support/managed-machine and desired.yaml
	// beside the legacy manifests in ~/.config/managed-machine.
	bashDesired := filepath.Join(h.bashHome, ".config", "managed-machine", "desired.yaml")
	bashDest := filepath.Join(h.bashHome, "configured", "upgrade.txt")
	desiredContent := fmt.Sprintf("target: user\nconfig:\n  mm-gate-upgrade-cfg:\n    destinations:\n      upgrade-file: %q\n",
		filepath.ToSlash(bashDest))
	if err := write(bashDesired, desiredContent); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	bashCoreStateDir := filepath.Join(h.bashHome, "Library", "Application Support", "managed-machine")
	if problem := h.runBashDispatcherConfigACT04(installedBin, h.bashHome, brewPrefix, coreRouteEnv, bashCoreStateDir, "mm-gate-upgrade-cfg", bashDest); problem != "" {
		h.recordCase(name, "ok", observedFailed, false, "step 2 dispatcher AC-T-04: "+problem)
		return
	}
	if _, err := os.Stat(filepath.Join(bashCoreStateDir, "ownership.jsonl")); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "step 2 core state not created under ~/Library/Application Support/managed-machine: "+err.Error())
		return
	}
	afterACT04Manifests, err := snapshotLegacyManifests(h.bashHome)
	if err != nil || len(profileDiff(beforeManifests, afterACT04Manifests)) > 0 {
		h.recordCase(name, "ok", observedFailed, false, "step 2 legacy manifests changed after AC-T-04 cycle")
		return
	}
	h.step("upgrade-from-bash step 2 (candidate upgrade)", "opt link repointed, old keg retained, dispatcher and core routes and AC-T-04 succeeded, manifests untouched", true)

	// (3) Downgrade: brew uninstall managed-machine then brew install --formula <old>.
	code, out, errOut = h.runBrew("uninstall", "--force", "managed-machine")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 3 (brew uninstall) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.brewInstalled = false
	if err := write(activeFormulaPath, oldDerived); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runBrew("install", "--formula", activeFormulaPath)
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 3 (brew install v0.8.12 downgrade) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.brewInstalled = true

	code, vOut, vErr = h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "--version")
	if code != 0 || strings.TrimSpace(string(vOut)) != "0.8.12" {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 3 (--version after downgrade) exited %d: got %q, want \"0.8.12\"",
			code, strings.TrimSpace(string(vOut))))
		return
	}
	code, status3Bytes, errOut := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "status", "--json")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 3 (status --json) exited %d: %s", code, firstLineOf(errOut, status3Bytes)))
		return
	}
	ver3, normStatus3, err := normalizeStatusJSONVersion(status3Bytes)
	if err != nil || ver3 != "0.8.12" || normStatus3 != normStatus1 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 3 status --json mismatch (ver=%q, err=%v)", ver3, err))
		return
	}
	afterDowngradeManifests, err := snapshotLegacyManifests(h.bashHome)
	if err != nil || len(profileDiff(beforeManifests, afterDowngradeManifests)) > 0 {
		h.recordCase(name, "ok", observedFailed, false, "step 3 legacy manifests changed across downgrade")
		return
	}
	afterDowngradeCheckout, err := h.snapshotGitCheckout(checkoutDir)
	if err != nil || afterDowngradeCheckout != beforeCheckout {
		h.recordCase(name, "ok", observedFailed, false, "step 3 checkout changed across downgrade")
		return
	}
	if _, err := os.Stat(filepath.Join(bashCoreStateDir, "ownership.jsonl")); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "step 3 core state directory missing after downgrade")
		return
	}
	// On v0.8.12, `status --section software --json` yields the legacy `status-invalid-section` envelope.
	code, invOut, _ := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, false, nil, "status", "--section", "software", "--json")
	if code == 0 || !strings.Contains(string(invOut), `"status-invalid-section"`) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 3 status --section software on v0.8.12 exited %d (stdout %q), want non-zero with status-invalid-section",
			code, firstLineOf(invOut)))
		return
	}
	h.step("upgrade-from-bash step 3 (downgrade)", "downgraded to v0.8.12, status --json and manifests intact, status --section software returned legacy status-invalid-section", true)

	// (4) Re-upgrade: brew upgrade --formula <new>, verify MM_CONFIG_NO_PLAN, status present/observed, --check 0.
	if err := write(activeFormulaPath, newDerived); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runBrew("upgrade", "--formula", activeFormulaPath)
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 4 (brew upgrade re-upgrade) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	code, applyOut, applyErr := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, true, coreRouteEnv, "config", "mm-gate-upgrade-cfg", "--apply", "--user")
	if code != 2 || len(applyOut) != 0 || !strings.Contains(string(applyErr), diag.ConfigNoPlan) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 4 config --apply after re-upgrade exited %d (stderr %q), want 2 with %s",
			code, firstLineOf(applyErr), diag.ConfigNoPlan))
		return
	}
	code, stOut, stErr := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, true, coreRouteEnv, "status", "--section", "config", "mm-gate-upgrade-cfg")
	if code != 0 || len(stErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 4 status --section config exited %d: %s", code, firstLineOf(stErr, stOut)))
		return
	}
	stRecs, err := parseJSONL(stOut)
	if err != nil || len(stRecs) != 1 || str(stRecs[0], "state") != "present" || str(stRecs[0], "evidence") != "observed" {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 4 status --section config unexpected records: %v (%v)", stRecs, err))
		return
	}
	code, chkOut, chkErr := h.execManagedMachineClean(installedBin, h.bashHome, brewPrefix, true, coreRouteEnv, "config", "mm-gate-upgrade-cfg", "--check", "--user")
	if code != 0 || len(chkErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("step 4 config --check exited %d: %s", code, firstLineOf(chkErr, chkOut)))
		return
	}
	h.downgradeNoPlanVerified = true

	// Remove the keg at the end and verify the runner's Homebrew is unchanged by hash of `brew list --versions`.
	code, out, errOut = h.runBrew("uninstall", "--force", "managed-machine")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("final brew uninstall exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.brewInstalled = false
	code, brewVersAfter, brewErr := h.runBrew("list", "--versions")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("brew list --versions after exited %d: %s", code, firstLineOf(brewErr, brewVersAfter)))
		return
	}
	afterBrewHash := sum(brewVersAfter)
	if afterBrewHash != beforeBrewHash {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("brew list --versions hash changed (%s != %s)", afterBrewHash[:12], beforeBrewHash[:12]))
		return
	}

	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("v0.8.12 -> %s -> v0.8.12 -> %s verified via derived formulae; status --json, manifests, machine.toml and checkout preserved; AC-T-04 and MM_CONFIG_NO_PLAN verified; brew list --versions hash %s unchanged",
			h.upgradeVer, h.upgradeVer, afterBrewHash[:12]))
}

func (h *releaseHarness) runBashDispatcherConfigACT04(binPath, homeDir, brewPrefix string, extraEnv []string, stateDir, defID, destFile string) string {
	code, out, errOut := h.execManagedMachineClean(binPath, homeDir, brewPrefix, true, extraEnv, "config", defID, "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 1 (--plan) exited %d: %s", code, firstLineOf(errOut, out))
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) != 2 {
		return fmt.Sprintf("step 1 (--plan) want 2 records, got %d (%v)", len(recs), err)
	}
	plan1 := str(recs[0], "plan")
	if !planIDPattern.MatchString(plan1) || str(recs[1], "action") != "create" {
		return fmt.Sprintf("step 1 unexpected records: %v", recs)
	}

	code, out, errOut = h.execManagedMachineClean(binPath, homeDir, brewPrefix, true, extraEnv, "config", defID, "--apply", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 2 (--apply) exited %d: %s", code, firstLineOf(errOut, out))
	}
	if _, err := os.Stat(destFile); err != nil {
		return fmt.Sprintf("step 2 destination not written: %v", err)
	}
	h.recordPlanFromDir(stateDir, plan1)

	code, out, errOut = h.execManagedMachineClean(binPath, homeDir, brewPrefix, true, extraEnv, "status", "--section", "config", defID)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 3 (status) exited %d: %s", code, firstLineOf(errOut, out))
	}
	stRecs, err := parseJSONL(out)
	if err != nil || len(stRecs) != 1 || str(stRecs[0], "state") != "present" || str(stRecs[0], "evidence") != "observed" {
		return fmt.Sprintf("step 3 (status) unexpected records: %v (%v)", stRecs, err)
	}
	code, out, errOut = h.execManagedMachineClean(binPath, homeDir, brewPrefix, true, extraEnv, "config", defID, "--check", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 3 (--check) exited %d: %s", code, firstLineOf(errOut, out))
	}

	code, out, errOut = h.execManagedMachineClean(binPath, homeDir, brewPrefix, true, extraEnv, "config", defID, "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 4 (second --plan) exited %d: %s", code, firstLineOf(errOut, out))
	}
	recs2, err := parseJSONL(out)
	if err != nil || len(recs2) != 1 {
		return fmt.Sprintf("step 4 (second --plan) want 1 record, got %d (%v)", len(recs2), err)
	}
	plan2 := str(recs2[0], "plan")
	code, out, errOut = h.execManagedMachineClean(binPath, homeDir, brewPrefix, true, extraEnv, "config", defID, "--apply", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 4 (second --apply) exited %d: %s", code, firstLineOf(errOut, out))
	}
	h.recordPlanFromDir(stateDir, plan2)

	code, out, errOut = h.execManagedMachineClean(binPath, homeDir, brewPrefix, true, extraEnv, "config", defID, "--apply", "--user")
	if code != 2 || len(out) != 0 || !strings.Contains(string(errOut), diag.ConfigNoPlan) {
		return fmt.Sprintf("step 5 (repeat --apply) exited %d (stderr %q), want 2 with %s", code, firstLineOf(errOut), diag.ConfigNoPlan)
	}
	return ""
}

// runPrivateSeed implements the D7 macOS private-seed case: scenarios (a)-(f)
// on the synthetic seed and fake origin (commits A, B, C).
func (h *releaseHarness) runPrivateSeed() {
	const name = caseReleasePrivateSeed
	if !strings.HasPrefix(h.target, "darwin/") {
		h.recordCase(name, "n-a", "n-a", true, releaseFirstReleaseReason)
		return
	}
	if err := h.ensureStagedTrees(); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	seedHome := filepath.Join(h.root, "private-seed-home")
	if err := h.seedBashFixtureHome(seedHome); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	oldBin := filepath.Join(h.stagedOld, "opt", "managed-machine", "bin", "managed-machine")
	newBin := filepath.Join(h.stagedNew, "opt", "managed-machine", "bin", "managed-machine")
	oldSeedDir := filepath.Join(h.stagedOld, "opt", "managed-machine", "libexec", "managed-machine-config")
	newSeedDir := filepath.Join(h.stagedNew, "opt", "managed-machine", "libexec", "managed-machine-config")
	checkoutDir := filepath.Join(seedHome, ".local", "share", "managed-machine", "managed-machine-config")
	wantOrigin := "file://" + filepath.ToSlash(h.fakeOriginBare)

	// (a) Fresh install materializes from the seed: HEAD equals commit A, branch
	// main, origin equals configured URL, and seed under libexec is unchanged and clean.
	if err := h.setFakeOriginMain(h.commitA); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	seedABefore, err := h.snapshotGitCheckout(oldSeedDir)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "snapshot old seed: "+err.Error())
		return
	}
	code, out, errOut := h.execManagedMachineClean(oldBin, seedHome, h.stagedOld, false, nil, "setup", "--list")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (a) setup --list exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	headA, err := h.runGitIsolated(checkoutDir, "rev-parse", "HEAD")
	if err != nil || headA != h.commitA {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (a) HEAD = %q (%v), want %s", headA, err, h.commitA))
		return
	}
	branchA, err := h.runGitIsolated(checkoutDir, "symbolic-ref", "--short", "HEAD")
	if err != nil || branchA != "main" {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (a) branch = %q (%v), want \"main\"", branchA, err))
		return
	}
	originA, err := h.runGitIsolated(checkoutDir, "remote", "get-url", "origin")
	if err != nil || originA != wantOrigin {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (a) origin = %q (%v), want %q", originA, err, wantOrigin))
		return
	}
	seedAAfter, err := h.snapshotGitCheckout(oldSeedDir)
	if err != nil || seedAAfter != seedABefore {
		h.recordCase(name, "ok", observedFailed, false, "scenario (a) bundled seed under libexec was mutated")
		return
	}
	seedAPorcelain, err := h.runGitIsolated(oldSeedDir, "status", "--porcelain")
	if err != nil || seedAPorcelain != "" {
		h.recordCase(name, "ok", observedFailed, false, "scenario (a) bundled seed under libexec is dirty")
		return
	}

	// (b) Clean checkout behind origin across the upgrade: the upgrade itself
	// changes nothing in the checkout, and the next refresh (setup --list)
	// fast-forwards to origin's main (commit B, then C), not to the seed pin (commit C).
	if err := h.setFakeOriginMain(h.commitB); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	newSeedHead, err := h.runGitIsolated(newSeedDir, "rev-parse", "HEAD")
	if err != nil || newSeedHead != h.commitC {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (b) candidate seed HEAD = %q (%v), want commit C %s", newSeedHead, err, h.commitC))
		return
	}
	// Before refresh on the upgraded formula, read-only command (--version) leaves checkout at A.
	_, _, _ = h.execManagedMachineClean(newBin, seedHome, h.stagedNew, false, nil, "--version")
	headBeforeRefresh, err := h.runGitIsolated(checkoutDir, "rev-parse", "HEAD")
	if err != nil || headBeforeRefresh != h.commitA {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (b) checkout changed before refresh: %q, want %s", headBeforeRefresh, h.commitA))
		return
	}
	// Next refresh (setup --list) fast-forwards to origin's main (commit B), NOT the seed pin (commit C).
	code, out, errOut = h.execManagedMachineClean(newBin, seedHome, h.stagedNew, false, nil, "setup", "--list")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (b) setup --list (origin at B) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	headAfterB, err := h.runGitIsolated(checkoutDir, "rev-parse", "HEAD")
	if err != nil || headAfterB != h.commitB {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (b) refresh landed on %q (%v), want origin/main commit B (%s) rather than seed pin C (%s)",
			headAfterB, err, h.commitB, h.commitC))
		return
	}
	// Advance origin to C and refresh again -> fast-forwards to C.
	if err := h.setFakeOriginMain(h.commitC); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.execManagedMachineClean(newBin, seedHome, h.stagedNew, false, nil, "setup", "--list")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (b) setup --list (origin at C) exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	headAfterC, err := h.runGitIsolated(checkoutDir, "rev-parse", "HEAD")
	if err != nil || headAfterC != h.commitC {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (b) second refresh landed on %q (%v), want commit C %s", headAfterC, err, h.commitC))
		return
	}

	// (c) Dirty checkout (a modified tracked file) survives the upgrade and the
	// refresh byte-identical and is reported, never reset.
	zshrcPath := filepath.Join(checkoutDir, "dotfiles", "zsh", ".zshrc")
	origZshrc, err := os.ReadFile(zshrcPath)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	dirtyZshrc := append(append([]byte{}, origZshrc...), []byte("# dirty local tracked edit\n")...)
	if err := os.WriteFile(zshrcPath, dirtyZshrc, 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, outText, errText := h.execManagedMachineClean(newBin, seedHome, h.stagedNew, false, nil, "setup", "--list")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (c) setup --list exited %d: %s", code, firstLineOf(errText, outText)))
		return
	}
	if !strings.Contains(string(errText), "has local changes; leaving") {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (c) stderr did not report dirty checkout: %q", firstLineOf(errText)))
		return
	}
	code, outJSON, errJSON := h.execManagedMachineClean(newBin, seedHome, h.stagedNew, false, nil, "setup", "--list", "--json")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (c) setup --list --json exited %d: %s", code, firstLineOf(errJSON, outJSON)))
		return
	}
	if !strings.Contains(string(outJSON), `"dirty": true`) || !strings.Contains(string(outJSON), `"refresh_status": "stale_dirty"`) {
		h.recordCase(name, "ok", observedFailed, false, "scenario (c) setup --list --json did not report dirty=true and refresh_status=stale_dirty")
		return
	}
	gotZshrc, err := os.ReadFile(zshrcPath)
	if err != nil || !bytes.Equal(gotZshrc, dirtyZshrc) {
		h.recordCase(name, "ok", observedFailed, false, "scenario (c) dirty tracked file was modified or reset")
		return
	}
	if err := os.WriteFile(zshrcPath, origZshrc, 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	// (d) Divergent checkout (a local commit not on origin) is preserved and reported.
	if _, err := h.runGitIsolated(checkoutDir, "reset", "--hard", h.commitB); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	divergentZshrc := fmt.Sprintf("# synthetic zshrc (%s) divergent-local-commit\nexport PATH=\"$HOME/.local/bin:$PATH\"\n", releaseSeedMarkerZSH)
	if err := os.WriteFile(zshrcPath, []byte(divergentZshrc), 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	if _, err := h.runGitIsolated(checkoutDir, "add", "dotfiles/zsh/.zshrc"); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	if _, err := h.runGitIsolated(checkoutDir, "commit", "-m", "local divergent commit"); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	localDivSHA, err := h.runGitIsolated(checkoutDir, "rev-parse", "HEAD")
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, outText, errText = h.execManagedMachineClean(newBin, seedHome, h.stagedNew, false, nil, "setup", "--list")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (d) setup --list exited %d: %s", code, firstLineOf(errText, outText)))
		return
	}
	if !strings.Contains(string(errText), "could not rebase onto origin/main") {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (d) stderr did not report divergent rebase failure: %q", firstLineOf(errText)))
		return
	}
	code, outJSON, errJSON = h.execManagedMachineClean(newBin, seedHome, h.stagedNew, false, nil, "setup", "--list", "--json")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (d) setup --list --json exited %d: %s", code, firstLineOf(errJSON, outJSON)))
		return
	}
	if !strings.Contains(string(outJSON), `"refresh_status": "rebase_failed"`) || !strings.Contains(string(outJSON), `"stale": true`) {
		h.recordCase(name, "ok", observedFailed, false, "scenario (d) setup --list --json did not report refresh_status=rebase_failed and stale=true")
		return
	}
	headAfterDiv, err := h.runGitIsolated(checkoutDir, "rev-parse", "HEAD")
	if err != nil || headAfterDiv != localDivSHA {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (d) divergent commit was not preserved: got %q, want %s", headAfterDiv, localDivSHA))
		return
	}

	// Reset checkout back to commit C cleanly for scenario (e) and (f).
	if _, err := h.runGitIsolated(checkoutDir, "reset", "--hard", h.commitC); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	// (e) Pin mismatch: a seed pinned at an older commit (commit A in stagedOld)
	// than the live checkout (commit C) never rewinds it.
	code, out, errOut = h.execManagedMachineClean(oldBin, seedHome, h.stagedOld, false, nil, "setup", "--list")
	if code != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (e) setup --list with older seed exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	headAfterPinMismatch, err := h.runGitIsolated(checkoutDir, "rev-parse", "HEAD")
	if err != nil || headAfterPinMismatch != h.commitC {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (e) pin mismatch rewound checkout to %q, want %s", headAfterPinMismatch, h.commitC))
		return
	}

	// (f) The new core reads the materialized checkout as its policy directory
	// through the dispatcher (fleet --keys, status --section fleet with policy
	// present) while a git sentinel first on the core's PATH is never started.
	code, fkOut, fkErr := h.execManagedMachineClean(newBin, seedHome, h.stagedNew, true, nil, "fleet", "--keys")
	if code != 0 || len(fkErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (f) fleet --keys exited %d: %s", code, firstLineOf(fkErr, fkOut)))
		return
	}
	fkRecs, err := parseJSONL(fkOut)
	if err != nil || len(fkRecs) < 2 || str(fkRecs[1], "owner") != "alice" || str(fkRecs[1], "fingerprint") != h.aliceKey.fingerprint {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (f) fleet --keys unexpected records: %v (%v)", fkRecs, err))
		return
	}
	code, sfOut, sfErr := h.execManagedMachineClean(newBin, seedHome, h.stagedNew, true, nil, "status", "--section", "fleet")
	if code != 0 || len(sfErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (f) status --section fleet exited %d: %s", code, firstLineOf(sfErr, sfOut)))
		return
	}
	sfRecs, err := parseJSONL(sfOut)
	if err != nil || len(sfRecs) == 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (f) status --section fleet records: %v", err))
		return
	}
	policyPresent := false
	for _, r := range sfRecs {
		if str(r, "resource") == "policy" && str(r, "state") == "present" {
			policyPresent = true
			break
		}
	}
	if !policyPresent {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("scenario (f) status --section fleet did not report policy present: %v", sfRecs))
		return
	}
	if b, err := os.ReadFile(h.log); err == nil && len(bytes.TrimSpace(b)) > 0 {
		h.recordCase(name, "ok", observedFailed, false, "scenario (f) git or another guarded sentinel was started by the core: "+firstLineOf(b))
		return
	}

	h.recordCase(name, "ok", "ok", true,
		"synthetic seed scenarios (a)-(f) verified: fresh materialization at commit A, upgrade fast-forward to B then C, dirty and divergent checkouts preserved and reported, older seed pin never rewinds live checkout, and core read policy with zero git sentinel invocations")
}

// runDispatcher implements the D5 macOS dispatcher case: every D5 routing row
// through the installed bin/managed-machine, direct core legacy-route
// refusals, and the core-missing failure without fallback.
func (h *releaseHarness) runDispatcher() {
	const name = caseReleaseDispatcher
	if !strings.HasPrefix(h.target, "darwin/") {
		h.recordCase(name, "n-a", "n-a", true, releaseFirstReleaseReason)
		return
	}
	if err := h.ensureStagedTrees(); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	dispHome := filepath.Join(h.root, "dispatcher-home")
	if err := h.seedBashFixtureHome(dispHome); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	newBin := filepath.Join(h.stagedNew, "opt", "managed-machine", "bin", "managed-machine")
	newCore := filepath.Join(h.stagedNew, "opt", "managed-machine", "libexec", "core", "managed-machine")

	// Materialize the checkout in dispHome so both legacy and new-core routes have
	// the persistent checkout under XDG_DATA_HOME.
	if err := h.setFakeOriginMain(h.commitC); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut := h.execManagedMachineClean(newBin, dispHome, h.stagedNew, false, nil, "setup", "--list", "--json")
	if code != 0 || !strings.Contains(string(out), `"command": "setup-list"`) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("dispatcher setup --list --json exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}

	coreEnv := []string{
		"MANAGED_MACHINE_SOURCE_DIR=" + h.defs,
	}

	// 1. Assert every new-core route family through the installed bin/managed-machine.
	newCoreRoutes := [][]string{
		{"software", "--available"},
		{"config", "--available"},
		{"status", "--section", "software"},
		{"status", "--section", "config"},
		{"status", "--section", "flavor"},
		{"status", "--section=fleet"},
		{"fleet", "--keys"},
		{"fleet", "alice", "--keys"},
		{"skill"},
		{"skill", "--search", "install"},
		{"platform"},
		{"version"},
	}
	for _, args := range newCoreRoutes {
		cCode, cOut, cErr := h.execManagedMachineClean(newBin, dispHome, h.stagedNew, true, coreEnv, args...)
		if cCode != 0 || len(cErr) != 0 {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("new-core route %v exited %d: %s", args, cCode, firstLineOf(cErr, cOut)))
			return
		}
		recs, err := parseJSONL(cOut)
		if err != nil || (args[0] != "status" && len(recs) == 0) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("new-core route %v did not return JSONL records: %v", args, err))
			return
		}
	}

	// 2. Assert legacy routes through the installed bin/managed-machine.
	code, vOut, vErr := h.execManagedMachineClean(newBin, dispHome, h.stagedNew, false, nil, "--version")
	if code != 0 || strings.TrimSpace(string(vOut)) != h.upgradeVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("legacy --version exited %d: got %q (stderr %q), want %q",
			code, strings.TrimSpace(string(vOut)), firstLineOf(vErr), h.upgradeVer))
		return
	}
	code, hOut, hErr := h.execManagedMachineClean(newBin, dispHome, h.stagedNew, false, nil, "--help")
	if code != 0 || !strings.Contains(string(hOut), "Portable core commands") || !strings.Contains(string(hOut), "Compatibility (ADR-0016, ADR-0017)") {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("legacy --help exited %d: missing portable core or compatibility block (%s)", code, firstLineOf(hErr)))
		return
	}
	legacyJSONRoutes := []struct {
		args []string
		want string
	}{
		{[]string{"skill", "path", "--json"}, `"command": "skill-path"`},
		{[]string{"status", "--json"}, `"command": "status"`},
		{[]string{"status", "--section", "machine", "--json"}, `"machine_id": "sha256-fixture_machine_aaaa"`},
		{[]string{"fleet", "list", "--json"}, `"command": "fleet-list"`},
		{[]string{"catalog", "list", "--json", "--no-refresh"}, `"command": "catalog-list"`},
		{[]string{"ssh", "status", "--json"}, `"command": "ssh-status"`},
	}
	for _, tc := range legacyJSONRoutes {
		cCode, cOut, cErr := h.execManagedMachineClean(newBin, dispHome, h.stagedNew, false, nil, tc.args...)
		if cCode != 0 || !strings.Contains(string(cOut), tc.want) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("legacy route %v exited %d (stderr %q): missing %s",
				tc.args, cCode, firstLineOf(cErr), tc.want))
			return
		}
		// Non-terminal invocation must never emit deprecation notice on stderr.
		if strings.Contains(string(cErr), "is a legacy route kept for compatibility") {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("legacy route %v emitted terminal-only deprecation notice on non-TTY stderr", tc.args))
			return
		}
	}

	// 3. Direct core invocation of legacy routes refuses with MM_LEGACY_ROUTE_UNSUPPORTED (exit 2).
	legacyCoreRefusals := [][]string{
		{"setup", "--list"},
		{"catalog", "list"},
		{"adopt"},
		{"add-agent", "x"},
		{"account", "doctor", "x"},
		{"ssh", "status"},
		{"--bootstrap"},
		{"--update"},
		{"fleet", "list"},
		{"fleet", "remove", "x"},
		{"skill", "path"},
		{"status", "--section", "machine"},
	}
	for _, args := range legacyCoreRefusals {
		cCode, cOut, cErr := h.execManagedMachineClean(newCore, dispHome, h.stagedNew, true, coreEnv, args...)
		if cCode != 2 || len(cOut) != 0 || !strings.Contains(string(cErr), diag.LegacyRouteUnsupported) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("direct core %v exited %d (stdout %q, stderr %q), want exit 2 with %s",
				args, cCode, firstLineOf(cOut), firstLineOf(cErr), diag.LegacyRouteUnsupported))
			return
		}
	}

	// 4. Missing core binary in libexec/core/managed-machine fails without fallback
	// (core-missing under --json, Error: ... run brew reinstall managed-machine otherwise, exit 1).
	missingPrefix := filepath.Join(h.root, "staged", "missing-core")
	if err := copyTree(h.stagedNew, missingPrefix); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	missingCorePath := filepath.Join(missingPrefix, "opt", "managed-machine", "libexec", "core", "managed-machine")
	if err := os.Remove(missingCorePath); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	missingDispBin := filepath.Join(missingPrefix, "opt", "managed-machine", "bin", "managed-machine")

	code, mOut, mErr := h.execManagedMachineClean(missingDispBin, dispHome, missingPrefix, true, nil, "software", "--available")
	wantMissingMsg := fmt.Sprintf("Error: the managed-machine core is missing at %s; run brew reinstall managed-machine", missingCorePath)
	if code != 1 || len(mOut) != 0 || strings.TrimSpace(string(mErr)) != wantMissingMsg {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("core-missing text exited %d (stdout %q, stderr %q), want exit 1 with %q",
			code, firstLineOf(mOut), strings.TrimSpace(string(mErr)), wantMissingMsg))
		return
	}
	code, mjOut, mjErr := h.execManagedMachineClean(missingDispBin, dispHome, missingPrefix, true, nil, "status", "--section", "software", "--json")
	if code != 1 || len(mjErr) != 0 || !strings.Contains(string(mjOut), `"code": "core-missing"`) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("core-missing --json exited %d (stderr %q, stdout %q), want exit 1 with core-missing JSON envelope",
			code, firstLineOf(mjErr), firstLineOf(mjOut)))
		return
	}

	h.recordCase(name, "ok", "ok", true,
		"all D5 new-core and legacy routes verified through installed bin/managed-machine, direct core legacy routes refused with MM_LEGACY_ROUTE_UNSUPPORTED (exit 2), and core-missing failed with exit 1 without fallback")
}

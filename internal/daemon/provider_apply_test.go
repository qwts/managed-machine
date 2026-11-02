package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/npm"
	"github.com/qwts/managed-machine/internal/provider/uv"
)

func TestMinimalEnvDropsPackageManagerVars(t *testing.T) {
	env := minimalEnv("/Users/admin", []string{
		"HOME=/tmp/agent",
		"PATH=/tmp/evil:/usr/bin:/bin",
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"npm_config_prefix=/tmp",
		"UV_CACHE_DIR=/tmp",
	})
	text := strings.Join(env, "\n")
	if !strings.Contains(text, "HOME=/Users/admin") || !strings.Contains(text, "PATH="+fixedPathFor(runtime.GOOS+"/"+runtime.GOARCH, "/Users/admin")) {
		t.Fatalf("env %v", env)
	}
	winPath := fixedPathFor("windows/amd64", "/Users/admin")
	if !strings.Contains(winPath, filepath.Join("/Users/admin", "AppData", "Local", "Microsoft", "WindowsApps")) || !strings.Contains(winPath, `C:\Windows\System32`) {
		t.Fatalf("windows path %s", winPath)
	}
	for _, banned := range []string{"HOMEBREW_", "npm_config_", "UV_", "/tmp/evil", "HOME=/tmp/agent"} {
		if strings.Contains(text, banned) {
			t.Fatalf("kept %s in %v", banned, env)
		}
	}
}

func TestApplyWithProviderUsesCatalogFields(t *testing.T) {
	rec := &recordingProvider{}
	item := Item{Name: "ripgrep", Provider: "other"}
	err := applyWithProvider(context.Background(), item, "14.1.1", "user", t.TempDir(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.req.Software != "ripgrep" || rec.req.Options["version"] != "14.1.1" || rec.req.Target != "user" {
		t.Fatalf("request %#v", rec.req)
	}
	if _, ok := rec.req.Options["url"]; ok {
		t.Fatal("url reached the provider")
	}
	if !rec.applied {
		t.Fatal("apply was not called")
	}
}

func TestApplyWithProviderPassesTheFormulaToken(t *testing.T) {
	rec := &recordingProvider{}
	item := Item{Name: "ripgrep", Provider: "brew", Formula: "ripgrep"}
	err := applyWithProvider(context.Background(), item, "14.1.1", "user", t.TempDir(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.req.Options["formula"] != "ripgrep" {
		t.Fatalf("request %#v", rec.req)
	}
	if _, ok := rec.req.Options["version"]; ok {
		t.Fatal("the request option reached brew")
	}
	if !rec.applied {
		t.Fatal("apply was not called")
	}
}

func TestApplyWithProviderRefusesAFormulaOnAnotherProvider(t *testing.T) {
	rec := &recordingProvider{}
	item := Item{Name: "ripgrep", Provider: "npm", Formula: "ripgrep"}
	err := applyWithProvider(context.Background(), item, "", "user", t.TempDir(), rec)
	if !errors.Is(err, errFormulaProvider) {
		t.Fatalf("err %v", err)
	}
	if rec.applied || rec.req.Software != "" {
		t.Fatal("the provider was called")
	}
	err = applyWithProvider(context.Background(), item, "", "user", t.TempDir(), nil)
	if !errors.Is(err, errFormulaProvider) {
		t.Fatalf("nil provider: %v", err)
	}
}

func TestApplyWithProviderPassesThePackageAndToolTokens(t *testing.T) {
	rec := &recordingProvider{}
	npmItem := Item{Name: "cline", Provider: "npm", Package: "cline", Version: "3.0.74"}
	if err := applyWithProvider(context.Background(), npmItem, "1.2.3", "user", t.TempDir(), rec); err != nil {
		t.Fatal(err)
	}
	if rec.req.Options["package"] != "cline" || rec.req.Options["version"] != "3.0.74" || len(rec.req.Options) != 2 {
		t.Fatalf("npm %#v", rec.req)
	}
	rec2 := &recordingProvider{}
	uvItem := Item{Name: "mistral-vibe", Provider: "uv", Tool: "mistral-vibe", Version: "2.25.8", Python: "3.12"}
	if err := applyWithProvider(context.Background(), uvItem, "0.1.0", "user", t.TempDir(), rec2); err != nil {
		t.Fatal(err)
	}
	if rec2.req.Options["tool"] != "mistral-vibe" || rec2.req.Options["version"] != "2.25.8" || rec2.req.Options["python"] != "3.12" || len(rec2.req.Options) != 3 {
		t.Fatalf("uv %#v", rec2.req)
	}
	rec3 := &recordingProvider{}
	uvItem.Python = ""
	if err := applyWithProvider(context.Background(), uvItem, "", "user", t.TempDir(), rec3); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec3.req.Options["python"]; ok || len(rec3.req.Options) != 2 {
		t.Fatalf("uv without python %#v", rec3.req)
	}
}

func TestApplyWithProviderRefusesARecipeTokenOnTheWrongProvider(t *testing.T) {
	cases := []struct {
		item Item
		want error
	}{
		{Item{Name: "cline", Provider: "brew", Package: "cline"}, errPackageProvider},
		{Item{Name: "cline", Provider: "npm"}, errNpmPackage},
		{Item{Name: "vibe", Provider: "npm", Tool: "mistral-vibe"}, errToolProvider},
		{Item{Name: "vibe", Provider: "uv"}, errUvTool},
		{Item{Name: "jq", Provider: "brew", Formula: "jq", Package: "jq"}, errOneToken},
		{Item{Name: "cline", Provider: "npm", Package: "cline"}, errRegistryVersion},
		{Item{Name: "vibe", Provider: "uv", Tool: "mistral-vibe", Python: "3.12"}, errRegistryVersion},
		{Item{Name: "cline", Provider: "npm", Package: "cline", Version: "3.0.74", Python: "3.12"}, errPythonProvider},
		{Item{Name: "jq", Provider: "brew", Formula: "jq", Python: "3.12"}, errPythonProvider},
		{Item{Name: "cline", Provider: "npm", Package: "cline", Version: "3.0.74", URL: "https://example.com/x"}, errOneToken},
	}
	for _, tc := range cases {
		rec := &recordingProvider{}
		err := applyWithProvider(context.Background(), tc.item, "", "user", t.TempDir(), rec)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.item.Name, err)
		}
		if rec.applied || rec.req.Software != "" {
			t.Fatal("the provider was called")
		}
		if err := applyWithProvider(context.Background(), tc.item, "", "user", t.TempDir(), nil); !errors.Is(err, tc.want) {
			t.Fatalf("nil provider %s: %v", tc.item.Name, err)
		}
	}
}

func TestRunProviderConstructsNpmAndUvAndRefusesWhenMissing(t *testing.T) {
	home := t.TempDir()
	emptyEnv := func() []string {
		return []string{
			"HOME=" + home,
			"USERPROFILE=" + home,
			"APPDATA=" + filepath.Join(home, "AppData", "Roaming"),
			"PATH=" + t.TempDir(),
		}
	}
	npmItem := Item{Name: "cline", Provider: "npm", Package: "cline", Version: "3.0.74"}
	impl, err := runProvider(t.TempDir(), npmItem)
	if err != nil || impl == nil || impl.Name() != "npm" {
		t.Fatalf("npm %v %v", impl, err)
	}
	npmProv := impl.(*npm.Provider)
	npmProv.Environ = emptyEnv
	if err := applyWithProvider(context.Background(), npmItem, "", "user", t.TempDir(), npmProv); !errors.Is(err, provider.ErrMissing) {
		t.Fatalf("missing npm: %v", err)
	}
	uvItem := Item{Name: "vibe", Provider: "uv", Tool: "mistral-vibe", Version: "2.25.8"}
	impl, err = runProvider(t.TempDir(), uvItem)
	if err != nil || impl == nil || impl.Name() != "uv" {
		t.Fatalf("uv %v %v", impl, err)
	}
	uvProv := impl.(*uv.Provider)
	uvProv.Environ = emptyEnv
	if err := applyWithProvider(context.Background(), uvItem, "", "user", t.TempDir(), uvProv); !errors.Is(err, provider.ErrMissing) {
		t.Fatalf("missing uv: %v", err)
	}
}

func TestApplyWithProviderPassesACaskRecipe(t *testing.T) {
	rec := &recordingProvider{
		ops: []plan.Operation{{ID: "download"}, {ID: "stage"}, {ID: "place"}},
	}
	item := Item{
		Name: "cursor", Provider: "brew", Cask: "cursor", AppName: "Cursor.app",
		TeamID: "V7V4A7M677", URLHosts: "downloads.cursor.com", HomepageHosts: "cursor.com",
	}
	if err := applyWithProvider(context.Background(), item, "stable", "global", t.TempDir(), rec); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.steps, ",") != "download,stage,place" || strings.Join(rec.verified, ",") != "download,stage,place" {
		t.Fatalf("steps %v verified %v", rec.steps, rec.verified)
	}
	badVerify := &recordingProvider{verifyErr: errors.New("signature rejected")}
	if err := applyWithProvider(context.Background(), item, "stable", "global", t.TempDir(), badVerify); err == nil {
		t.Fatal("verify failure was ignored")
	}
	if rec.req.Options["cask"] != "cursor" || rec.req.Options["app_name"] != "Cursor.app" || rec.req.Options["team_id"] != "V7V4A7M677" {
		t.Fatalf("cask %#v", rec.req.Options)
	}
	if rec.req.Options["url_hosts"] != "downloads.cursor.com" || rec.req.Options["homepage_hosts"] != "cursor.com" {
		t.Fatalf("hosts %#v", rec.req.Options)
	}
	if _, ok := rec.req.Options["version"]; ok || rec.req.Options["allow_rolling_url"] != "" {
		t.Fatal("the request option or a rolling flag reached brew")
	}
	rolling := item
	rolling.AllowRollingURL = true
	rec2 := &recordingProvider{}
	if err := applyWithProvider(context.Background(), rolling, "", "global", t.TempDir(), rec2); err != nil {
		t.Fatal(err)
	}
	if rec2.req.Options["allow_rolling_url"] != "true" {
		t.Fatalf("rolling %#v", rec2.req.Options)
	}
}

func TestApplyWithProviderRefusesAnIncompleteCask(t *testing.T) {
	item := Item{Name: "cursor", Provider: "brew", Cask: "cursor", AppName: "Cursor.app"}
	err := applyWithProvider(context.Background(), item, "", "global", t.TempDir(), nil)
	if !errors.Is(err, errCaskFields) {
		t.Fatal(err)
	}
	other := Item{Name: "cursor", Provider: "npm", Package: "cursor", Cask: "cursor", AppName: "Cursor.app", TeamID: "V7V4A7M677", URLHosts: "example.com", HomepageHosts: "example.com"}
	if err := applyWithProvider(context.Background(), other, "", "user", t.TempDir(), nil); !errors.Is(err, errOneToken) {
		t.Fatal(err)
	}
	moved := Item{Name: "cursor", Provider: "npm", Cask: "cursor", AppName: "Cursor.app", TeamID: "V7V4A7M677", URLHosts: "example.com", HomepageHosts: "example.com"}
	if err := applyWithProvider(context.Background(), moved, "", "user", t.TempDir(), nil); !errors.Is(err, errCaskProvider) {
		t.Fatal(err)
	}
}

func TestApplyWithProviderPassesAnAppCopyRecipe(t *testing.T) {
	rec := &recordingProvider{
		ops: []plan.Operation{{ID: "download"}, {ID: "stage"}, {ID: "place"}},
	}
	sum := strings.Repeat("a", 64)
	item := Item{
		Name: "muse-app", Provider: "app-copy",
		URL: "https://example.com/muse.dmg", SHA256: sum,
		URLHosts: "example.com", AppName: "Muse.app",
		TeamID: "V7V4A7M677", Version: "1.0.0", Format: "dmg",
	}
	if err := applyWithProvider(context.Background(), item, "stable", "global", t.TempDir(), rec); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.steps, ",") != "download,stage,place" || strings.Join(rec.verified, ",") != "download,stage,place" {
		t.Fatalf("steps %v verified %v", rec.steps, rec.verified)
	}
	appImpl, err := runProvider(t.TempDir(), item)
	if err != nil || appImpl == nil || appImpl.Name() != "app-copy" {
		t.Fatalf("app-copy %v %v", appImpl, err)
	}
	if rec.req.Options["url"] != item.URL || rec.req.Options["sha256"] != sum || rec.req.Options["format"] != "dmg" {
		t.Fatalf("app-copy %#v", rec.req.Options)
	}
	if rec.req.Options["version"] != "1.0.0" {
		t.Fatalf("version %#v", rec.req.Options)
	}
	incomplete := Item{Name: "muse-app", Provider: "app-copy", URL: "https://example.com/muse.dmg"}
	if err := applyWithProvider(context.Background(), incomplete, "", "global", t.TempDir(), nil); !errors.Is(err, errAppCopyFields) {
		t.Fatal(err)
	}
	empty := Item{Name: "muse-app", Provider: "app-copy"}
	if err := applyWithProvider(context.Background(), empty, "", "global", t.TempDir(), nil); !errors.Is(err, errAppCopyFields) {
		t.Fatal(err)
	}
	wrong := item
	wrong.Provider = "brew"
	if err := applyWithProvider(context.Background(), wrong, "", "global", t.TempDir(), nil); !errors.Is(err, errOneToken) {
		t.Fatal(err)
	}
	wingetRec := &recordingProvider{}
	wingetItem := Item{Name: "ripgrep", Provider: "winget", PackageID: "BurntSushi.ripgrep.MSVC"}
	if err := applyWithProvider(context.Background(), wingetItem, "14.1.1", "user", t.TempDir(), wingetRec); err != nil {
		t.Fatal(err)
	}
	if wingetRec.req.Options["package_id"] != "BurntSushi.ripgrep.MSVC" {
		t.Fatalf("winget %#v", wingetRec.req.Options)
	}
	if _, ok := wingetRec.req.Options["version"]; ok {
		t.Fatal("the request option reached winget")
	}
	if err := applyWithProvider(context.Background(), Item{Name: "ripgrep", Provider: "winget"}, "", "user", t.TempDir(), nil); !errors.Is(err, errWingetPackageID) {
		t.Fatal(err)
	}
	if err := applyWithProvider(context.Background(), Item{Name: "ripgrep", Provider: "npm", PackageID: "BurntSushi.ripgrep.MSVC"}, "", "user", t.TempDir(), nil); !errors.Is(err, errPackageIDProvider) {
		t.Fatal(err)
	}
	wingetImpl, err := runProvider(t.TempDir(), wingetItem)
	if err != nil || wingetImpl == nil || wingetImpl.Name() != "winget" {
		t.Fatalf("winget %v %v", wingetImpl, err)
	}
	if err := applyWithProvider(context.Background(), wingetItem, "", "global", t.TempDir(), wingetImpl); !errors.Is(err, provider.ErrPrivilege) {
		t.Fatalf("winget global %v", err)
	}
	goRec := &recordingProvider{}
	goItem := Item{Name: "actionlint", Provider: "go", Module: "github.com/rhysd/actionlint", Package: "github.com/rhysd/actionlint/cmd/actionlint"}
	if err := applyWithProvider(context.Background(), goItem, "", "user", t.TempDir(), goRec); err != nil {
		t.Fatal(err)
	}
	if goRec.req.Options["module"] != goItem.Module || goRec.req.Options["package"] != goItem.Package {
		t.Fatalf("go %#v", goRec.req.Options)
	}
	if err := applyWithProvider(context.Background(), Item{Name: "actionlint", Provider: "go", Module: goItem.Module}, "", "user", t.TempDir(), nil); !errors.Is(err, errGoFields) {
		t.Fatal(err)
	}
	if err := applyWithProvider(context.Background(), Item{Name: "actionlint", Provider: "npm", Module: goItem.Module, Package: goItem.Package}, "", "user", t.TempDir(), nil); !errors.Is(err, errGoProvider) {
		t.Fatal(err)
	}
	cargoRec := &recordingProvider{}
	cargoItem := Item{Name: "taplo", Provider: "cargo", Crate: "taplo-cli"}
	if err := applyWithProvider(context.Background(), cargoItem, "", "user", t.TempDir(), cargoRec); err != nil {
		t.Fatal(err)
	}
	if cargoRec.req.Options["crate"] != "taplo-cli" {
		t.Fatalf("cargo %#v", cargoRec.req.Options)
	}
	if err := applyWithProvider(context.Background(), Item{Name: "taplo", Provider: "cargo"}, "", "user", t.TempDir(), nil); !errors.Is(err, errCargoCrate) {
		t.Fatal(err)
	}
	if err := applyWithProvider(context.Background(), Item{Name: "taplo", Provider: "npm", Crate: "taplo-cli"}, "", "user", t.TempDir(), nil); !errors.Is(err, errCrateProvider) {
		t.Fatal(err)
	}
	archRec := &recordingProvider{}
	archItem := Item{
		Name: "gitleaks", Provider: "archive",
		URL: "https://example.com/gitleaks.tar.gz", SHA256: sum,
		URLHosts: "example.com", Version: "8.18.0", Format: "tar.gz", Files: "gitleaks",
	}
	if err := applyWithProvider(context.Background(), archItem, "", "user", t.TempDir(), archRec); err != nil {
		t.Fatal(err)
	}
	if archRec.req.Options["files"] != "gitleaks" || archRec.req.Options["format"] != "tar.gz" {
		t.Fatalf("archive %#v", archRec.req.Options)
	}
	if err := applyWithProvider(context.Background(), Item{Name: "gitleaks", Provider: "archive", Files: "gitleaks"}, "", "user", t.TempDir(), nil); !errors.Is(err, errArchiveFields) {
		t.Fatal(err)
	}
	for _, it := range []Item{goItem, cargoItem, archItem} {
		impl, err := runProvider(t.TempDir(), it)
		if err != nil || impl == nil || impl.Name() != it.Provider {
			t.Fatalf("%s %v %v", it.Provider, impl, err)
		}
	}
}

func TestApplyWithProviderRefusesABrewRowWithoutAFormula(t *testing.T) {
	rec := &recordingProvider{}
	item := Item{Name: "ripgrep", Provider: "brew"}
	err := applyWithProvider(context.Background(), item, "14.1.1", "user", t.TempDir(), rec)
	if !errors.Is(err, errBrewFormula) {
		t.Fatalf("err %v", err)
	}
	if rec.applied || rec.req.Software != "" {
		t.Fatal("the provider was called")
	}
}

func TestApplyWithProviderStopsOnPrivilege(t *testing.T) {
	rec := &recordingProvider{planErr: provider.ErrPrivilege}
	err := applyWithProvider(context.Background(), Item{Name: "docker"}, "desktop", "user", t.TempDir(), rec)
	if err != provider.ErrPrivilege {
		t.Fatalf("err %v", err)
	}
	if rec.applied {
		t.Fatal("apply ran after a privilege refusal")
	}
}

func TestLoadPinnedCatalog(t *testing.T) {
	dir := t.TempDir()
	catalog, err := loadPinnedCatalog(dir)
	if err != nil || len(catalog.Items) != 0 {
		t.Fatalf("missing catalog %#v %v", catalog, err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"items":[{"name":"ripgrep","options":["14.1.1"],"targets":["user"],"provider":"brew"}]}`)
	sum := sha256.Sum256(body)
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.sha256"), []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPinnedCatalog(dir); err == nil {
		t.Fatal("unsigned catalog was accepted")
	}
	body = []byte(`{"items":[{"name":"ripgrep","options":["14.1.1"],"targets":["user"],"provider":"brew"}],"version":1}`)
	if err := writeSignedCatalog(dir, body, pub, priv); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPinnedCatalog(dir); err == nil {
		t.Fatal("a disk catalog was accepted before the signing key is anchored")
	}
	if _, err := os.Stat(filepath.Join(dir, "catalog.pub.pin")); !os.IsNotExist(err) {
		t.Fatal("a catalog pin was written from disk")
	}
	if _, err := os.Stat(filepath.Join(dir, "catalog.version")); !os.IsNotExist(err) {
		t.Fatal("a catalog version record was written from disk")
	}
}

func writeSignedCatalog(dir string, body, pub, priv []byte) error {
	sum := sha256.Sum256(body)
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), body, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.sha256"), []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.pub"), pub, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "catalog.sig"), ed25519.Sign(ed25519.PrivateKey(priv), body), 0o600)
}

func TestCatalogOnDiskIsRefused(t *testing.T) {
	dir := t.TempDir()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	zero := []byte(`{"items":[{"name":"ripgrep","options":["14.1.1"],"targets":["user"]}]}`)
	if err := writeSignedCatalog(dir, zero, pub, priv); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPinnedCatalog(dir); err == nil {
		t.Fatal("version 0 was accepted")
	}
	anchored := []byte(`{"items":[{"name":"ripgrep","options":["14.1.1"],"targets":["user"]}],"version":1}`)
	if err := writeSignedCatalog(dir, anchored, pub, priv); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.version"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "catalog.pub.pin")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := loadPinnedCatalog(dir); err == nil {
		t.Fatal("a catalog with a version record and no pin was accepted")
	}
	if err := os.Remove(filepath.Join(dir, "catalog.version")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPinnedCatalog(dir); err == nil {
		t.Fatal("deleting the version record made the catalog acceptable")
	}
}

func TestArchiveReceiptCoversSubsequentApply(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := "gitleaks"
	if runtime.GOOS == "windows" {
		bin = "gitleaks.exe"
	}
	stateDir := t.TempDir()
	sum := strings.Repeat("a", 64)
	item := Item{
		Name: "gitleaks", Targets: []string{"user"}, Provider: "archive",
		URL: "https://example.com/gitleaks.tar.gz", SHA256: sum,
		URLHosts: "example.com", Version: "8.18.0", Format: "tar.gz", Files: bin,
	}
	planner := Planner{
		Catalog:  Catalog{Items: []Item{item}},
		StateDir: stateDir,
		Account:  "admin",
		Exec: func(it Item, option, target string) error {
			return os.WriteFile(filepath.Join(binDir, bin), []byte("ok"), 0o755)
		},
	}
	id, diagID := planner.Plan("gitleaks", "", "user")
	if diagID != "" || id == "" {
		t.Fatalf("plan %s %s", id, diagID)
	}
	if got := planner.Apply(id, "", "", "", "iron", "cursor", ""); got != "" {
		t.Fatal(got)
	}
	impl, err := runProvider(home, item)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyWithProvider(context.Background(), item, "", "user", stateDir, impl); err != nil {
		t.Fatalf("reapply after daemon receipt: %v", err)
	}
}

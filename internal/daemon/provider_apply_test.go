package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
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
	if !strings.Contains(text, "HOME=/Users/admin") || !strings.Contains(text, "PATH="+fixedPath()) {
		t.Fatalf("env %v", env)
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
	npmItem := Item{Name: "cline", Provider: "npm", Package: "cline"}
	if err := applyWithProvider(context.Background(), npmItem, "1.2.3", "user", t.TempDir(), rec); err != nil {
		t.Fatal(err)
	}
	if rec.req.Options["package"] != "cline" {
		t.Fatalf("npm %#v", rec.req)
	}
	if _, ok := rec.req.Options["version"]; ok {
		t.Fatal("the request option reached npm")
	}
	rec2 := &recordingProvider{}
	uvItem := Item{Name: "mistral-vibe", Provider: "uv", Tool: "mistral-vibe"}
	if err := applyWithProvider(context.Background(), uvItem, "0.1.0", "user", t.TempDir(), rec2); err != nil {
		t.Fatal(err)
	}
	if rec2.req.Options["tool"] != "mistral-vibe" {
		t.Fatalf("uv %#v", rec2.req)
	}
	if _, ok := rec2.req.Options["version"]; ok {
		t.Fatal("the request option reached uv")
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

func TestCommandOnFixedPathRefusesAMissingTool(t *testing.T) {
	err := commandOnFixedPath("mm-not-a-tool")
	if !errors.Is(err, provider.ErrMissing) {
		t.Fatal(err)
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

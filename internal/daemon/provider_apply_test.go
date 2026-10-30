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
	item := Item{Name: "ripgrep", Provider: "brew"}
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

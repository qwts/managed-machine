//go:build darwin

package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinKeychainNeverImportsExec(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "keychain_darwin.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "os/exec" {
			t.Fatal("keychain_darwin.go must call Security.framework in-process and never import os/exec")
		}
	}
}

func TestDarwinKeychainStoreInProcessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	acct := fmt.Sprintf("daemon.key.test.%d", os.Getpid())
	store := darwinKeychainStore{
		service:      keychainService + ".test",
		account:      acct,
		accessGroup:  keychainAccessGroup,
		keychainPath: filepath.Join(dir, "daemon.keychain-db"),
	}
	t.Cleanup(func() { _ = store.Delete() })
	_ = store.Delete()

	got, ok, err := store.Load()
	if err != nil || ok || got != nil {
		t.Fatalf("expected absent item before save, got ok=%v err=%v", ok, err)
	}
	seed1 := make([]byte, 32)
	if _, err := rand.Read(seed1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(seed1); err != nil {
		t.Fatalf("Save(seed1): %v", err)
	}
	got, ok, err = store.Load()
	if err != nil || !ok || !bytes.Equal(got, seed1) {
		t.Fatalf("Load after Save(seed1): ok=%v err=%v match=%v", ok, err, bytes.Equal(got, seed1))
	}

	// Second Save updates in-place via SecItemUpdate without deleting first.
	seed2 := make([]byte, 32)
	if _, err := rand.Read(seed2); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(seed2); err != nil {
		t.Fatalf("Save(seed2) in-place update: %v", err)
	}
	got, ok, err = store.Load()
	if err != nil || !ok || !bytes.Equal(got, seed2) {
		t.Fatalf("Load after Save(seed2): ok=%v err=%v match=%v", ok, err, bytes.Equal(got, seed2))
	}

	if err := store.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, ok, err = store.Load()
	if err != nil || ok || got != nil {
		t.Fatalf("expected absent item after Delete, got ok=%v err=%v", ok, err)
	}
}

func TestDarwinLoadOrCreateKeyNeverUsesPlaintextFileOrSecurityCLI(t *testing.T) {
	dir := t.TempDir()
	stubBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stubBin, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "security.invoked")
	stubScript := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %q\nexit 99\n", sentinel)
	if err := os.WriteFile(filepath.Join(stubBin, "security"), []byte(stubScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubBin+":/usr/bin:/bin")

	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(state, "daemon.key")
	priv1, err := loadOrCreateKey(keyPath)
	if err != nil {
		t.Fatalf("loadOrCreateKey: %v", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("security CLI was invoked")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("expected no plaintext daemon.key file on darwin, got err=%v", err)
	}
	priv2, err := loadOrCreateKey(keyPath)
	if err != nil {
		t.Fatalf("second loadOrCreateKey: %v", err)
	}
	if !bytes.Equal(priv1.Public().(ed25519.PublicKey), priv2.Public().(ed25519.PublicKey)) {
		t.Fatal("reloaded key did not match")
	}
}

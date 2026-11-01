//go:build darwin

package daemon

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"go/parser"
	"go/token"
	"os"
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
	acct := fmt.Sprintf("daemon.key.test.%d", os.Getpid())
	store := darwinKeychainStore{
		service: keychainService + ".test",
		account: acct,
	}
	t.Cleanup(func() { _ = store.Delete() })
	_ = store.Delete()

	got, ok, err := store.Load()
	if err != nil || ok || got != nil {
		t.Fatalf("expected absent item before save, got ok=%v err=%v", ok, err)
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(seed); err != nil {
		t.Skipf("Keychain unavailable in this test session (%v)", err)
	}
	got, ok, err = store.Load()
	if err != nil || !ok || !bytes.Equal(got, seed) {
		t.Fatalf("Load after Save: ok=%v err=%v match=%v", ok, err, bytes.Equal(got, seed))
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, ok, err = store.Load()
	if err != nil || ok || got != nil {
		t.Fatalf("expected absent item after Delete, got ok=%v err=%v", ok, err)
	}
}

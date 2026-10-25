package fetch

import (
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
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/provider"
)

func TestHostAllowed(t *testing.T) {
	cases := []struct {
		host  string
		hosts []string
		want  bool
	}{
		{"dl.example.com", []string{"dl.example.com"}, true},
		{"www.example.com", []string{"example.com"}, true},
		{"example.com", []string{"www.example.com"}, true},
		{"DL.EXAMPLE.COM:443", []string{"dl.example.com"}, true},
		{"sub.example.com", []string{"example.com"}, false},
		{"sub.example.com", []string{".example.com"}, true},
		{"example.com", []string{".example.com"}, true},
		{"badexample.com", []string{".example.com"}, false},
		{"", []string{"example.com"}, false},
		{"example.com", nil, false},
	}
	for _, c := range cases {
		if got := HostAllowed(c.host, c.hosts); got != c.want {
			t.Errorf("HostAllowed(%q, %v) = %v, want %v", c.host, c.hosts, got, c.want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	if err := ValidateURL("https://dl.example.com/app.dmg", []string{"dl.example.com"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateURL("http://dl.example.com/app.dmg", []string{"dl.example.com"}); err == nil {
		t.Fatal("accepted non-https URL")
	}
	if err := ValidateURL("https://user:pass@dl.example.com/app.dmg", []string{"dl.example.com"}); err == nil {
		t.Fatal("accepted URL with userinfo")
	}
	err := ValidateURL("https://evil.example.com/app.dmg", []string{"dl.example.com"})
	if !errors.Is(err, provider.ErrHostUnlisted) {
		t.Fatalf("expected ErrHostUnlisted, got %v", err)
	}
}

func TestSplitHosts(t *testing.T) {
	for _, in := range []string{
		"a.example.com,b.example.com",
		"[a.example.com b.example.com]",
		" a.example.com , b.example.com ",
	} {
		got := SplitHosts(in)
		if strings.Join(got, "|") != "a.example.com|b.example.com" {
			t.Errorf("SplitHosts(%q) = %v", in, got)
		}
	}
}

func newLoopbackTLS(t *testing.T, handler http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
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

	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	return srv, client
}

func TestDownloadAndDigest(t *testing.T) {
	payload := []byte("synthetic-dmg-payload-bytes")
	sum := sha256.Sum256(payload)
	wantDigest := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	srv, client := newLoopbackTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, srv.URL+"/artifact.dmg", http.StatusFound)
		case "/artifact.dmg":
			_, _ = w.Write(payload)
		case "/unlisted":
			http.Redirect(w, r, "https://unlisted.example.invalid/artifact.dmg", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))

	dir := t.TempDir()
	dest := filepath.Join(dir, "stage", "artifact.dmg")
	if err := Download(context.Background(), client, srv.URL+"/redirect", []string{"127.0.0.1"}, 0, dest); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	gotDigest, err := FileSHA256(dest)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	if gotDigest != wantDigest {
		t.Errorf("digest = %s, want %s", gotDigest, wantDigest)
	}

	// Redirect to an unlisted host aborts with ErrHostUnlisted and writes nothing.
	badDest := filepath.Join(dir, "unlisted", "artifact.dmg")
	err = Download(context.Background(), client, srv.URL+"/unlisted", []string{"127.0.0.1"}, 0, badDest)
	if !errors.Is(err, provider.ErrHostUnlisted) {
		t.Fatalf("expected ErrHostUnlisted on unlisted redirect, got %v", err)
	}
	if _, statErr := os.Stat(badDest); !os.IsNotExist(statErr) {
		t.Errorf("unlisted redirect left file at %s", badDest)
	}

	// Size bound exceeded removes partial file.
	boundDest := filepath.Join(dir, "bound", "artifact.dmg")
	err = Download(context.Background(), client, srv.URL+"/artifact.dmg", []string{"127.0.0.1"}, 5, boundDest)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size bound error, got %v", err)
	}
	if _, statErr := os.Stat(boundDest); !os.IsNotExist(statErr) {
		t.Errorf("oversized download left file at %s", boundDest)
	}

	// More than 5 redirects fails cleanly.
	loopDest := filepath.Join(dir, "loop", "artifact.dmg")
	err = Download(context.Background(), client, srv.URL+"/loop", []string{"127.0.0.1"}, 0, loopDest)
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("expected redirect limit error, got %v", err)
	}
}

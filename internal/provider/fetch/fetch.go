// Package fetch downloads artifacts over HTTPS for the native download
// providers (S7 D5):app-copy and archive. Every request requires https,
// bounds redirects to five hops, requires the initial host and every
// redirect hop host to match url_hosts, and streams the response to disk
// with a running SHA-256 and a 2 GiB size bound. An unlisted host aborts
// the transfer with provider.ErrHostUnlisted before any bytes are kept.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/provider"
)

const (
	// MaxBytes is the default 2 GiB upper bound on a downloaded artifact.
	MaxBytes int64 = 2 << 30
	// MaxRedirects is the maximum number of HTTP redirect hops allowed.
	MaxRedirects = 5
	// DefaultTimeout bounds one artifact transfer when the caller supplies
	// no HTTP client.
	DefaultTimeout = 10 * time.Minute
)

// DefaultClient returns an HTTP client with TLS verification enabled and a
// bounded timeout. Go's default transport honors SSL_CERT_FILE on every
// target platform.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: DefaultTimeout}
}

// HostAllowed reports whether host matches at least one entry in hosts.
// Matching follows the legacy url_matches_hosts rule:
//   - a bare entry (e.g. "example.com" or "www.example.com") matches that
//     host and its "www." prefix;
//   - a leading-dot entry (e.g. ".example.com") matches the suffix itself
//     ("example.com") and any subdomain ("cdn.example.com") as written.
func HostAllowed(host string, hosts []string) bool {
	raw := normalizeHost(host)
	if raw == "" || len(hosts) == 0 {
		return false
	}
	bare := strings.TrimPrefix(raw, "www.")
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, ".") {
			suffix := strings.TrimPrefix(h, ".")
			if suffix != "" && (raw == suffix || strings.HasSuffix(raw, "."+suffix)) {
				return true
			}
			continue
		}
		if bare == strings.TrimPrefix(h, "www.") {
			return true
		}
	}
	return false
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return strings.ToLower(host)
}

// ValidateScheme checks that rawURL is a valid https URL with a host and no
// userinfo, returning the parsed URL and its lowercase hostname.
func ValidateScheme(rawURL string) (*url.URL, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("invalid URL %q: %v", rawURL, err)
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return nil, "", fmt.Errorf("URL %q must use https with a host", u.Redacted())
	}
	if u.User != nil {
		return nil, "", fmt.Errorf("URL %q must not contain credentials", u.Redacted())
	}
	return u, strings.ToLower(u.Hostname()), nil
}

// ValidateURL checks that rawURL is a valid https URL with no credentials
// and that its host is allowed by hosts. When the URL is valid https but its
// host is not in hosts, the returned error matches provider.ErrHostUnlisted.
func ValidateURL(rawURL string, hosts []string) error {
	if len(hosts) == 0 {
		return fmt.Errorf("url_hosts must not be empty")
	}
	u, host, err := ValidateScheme(rawURL)
	if err != nil {
		return err
	}
	if !HostAllowed(host, hosts) {
		return provider.Kind(provider.ErrHostUnlisted, fmt.Sprintf("host %q of %s is not in url_hosts", host, u.Redacted()))
	}
	return nil
}

// SplitHosts parses a serialized url_hosts option value into individual host
// patterns. It accepts comma-separated lists ("a.example.com,b.example.com")
// as well as Go slice strings ("[a.example.com b.example.com]").
func SplitHosts(raw string) []string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Download fetches rawURL over HTTPS into dest, enforcing hosts on the
// initial URL and every redirect hop, at most MaxRedirects redirects, and a
// maximum body size of bound bytes (defaulting to MaxBytes when bound <= 0).
// On any error dest is removed and does not exist when Download returns.
func Download(ctx context.Context, client *http.Client, rawURL string, hosts []string, bound int64, dest string) error {
	_ = os.Remove(dest)
	if err := ValidateURL(rawURL, hosts); err != nil {
		return err
	}
	if bound <= 0 {
		bound = MaxBytes
	}
	if client == nil {
		client = DefaultClient()
	}
	hc := *client
	hc.CheckRedirect = redirectPolicy(hosts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("fetch %s: %v", rawURL, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, provider.ErrHostUnlisted) {
			return provider.Kind(provider.ErrHostUnlisted, err.Error())
		}
		return fmt.Errorf("fetch %s: %v", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.Request != nil && resp.Request.URL != nil {
		if err := ValidateURL(resp.Request.URL.String(), hosts); err != nil {
			return err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("prepare stage directory for %s: %v", dest, err)
	}
	tmp := dest + ".part"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %v", dest, err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
		_ = os.Remove(dest)
	}

	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, bound+1))
	if copyErr != nil {
		cleanup()
		return fmt.Errorf("read %s: %v", rawURL, copyErr)
	}
	if n > bound {
		cleanup()
		return fmt.Errorf("fetch %s: response exceeds %d bytes", rawURL, bound)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %v", dest, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		cleanup()
		return fmt.Errorf("place %s: %v", dest, err)
	}
	_ = h.Sum(nil)
	return nil
}

func redirectPolicy(hosts []string) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", MaxRedirects)
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to non-https scheme %q", req.URL.Scheme)
		}
		if req.URL.User != nil {
			return fmt.Errorf("refusing redirect with credentials")
		}
		host := strings.ToLower(req.URL.Hostname())
		if !HostAllowed(host, hosts) {
			return provider.Kind(provider.ErrHostUnlisted, fmt.Sprintf("redirect host %q of %s is not in url_hosts", host, req.URL.Redacted()))
		}
		return nil
	}
}

// FileSHA256 computes the lowercase hex SHA-256 digest of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

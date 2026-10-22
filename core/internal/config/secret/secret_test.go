package secret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestEnvVarMappingAndResolve(t *testing.T) {
	cases := []struct {
		name    string
		refName string
		wantEnv string
		val     string
	}{
		{"plain lowercase", "token", "MM_SECRET_TOKEN", "sec-plain-1"},
		{"hyphen", "api-key", "MM_SECRET_API_KEY", "sec-hyphen-2"},
		{"slash", "work/github/token", "MM_SECRET_WORK_GITHUB_TOKEN", "sec-slash-3"},
		{"dot", "registry.npmjs.org", "MM_SECRET_REGISTRY_NPMJS_ORG", "sec-dot-4"},
		{"combined separators and digits", "org/service-v2.api_key", "MM_SECRET_ORG_SERVICE_V2_API_KEY", "sec-combo-5"},
		{"already uppercase", "PATH", "MM_SECRET_PATH", "sec-path-6"},
		{"other punctuation", "db:prod@us-east+1", "MM_SECRET_DB_PROD_US_EAST_1", "sec-punct-7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EnvVar(c.refName); got != c.wantEnv {
				t.Fatalf("EnvVar(%q) = %q, want %q", c.refName, got, c.wantEnv)
			}
			// Even when PATH and the raw refName are set in the environment,
			// only MM_SECRET_<NAME> is consulted.
			env := map[string]string{
				"PATH":    "/usr/bin:/bin",
				c.refName: "wrong-unprefixed-value",
				c.wantEnv: c.val,
			}
			var lookedUp []string
			r := NewResolver(func(k string) string {
				lookedUp = append(lookedUp, k)
				return env[k]
			})
			got, err := r.Resolve(context.Background(), c.refName)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.refName, err)
			}
			if got != c.val {
				t.Errorf("Resolve(%q) = %q, want %q", c.refName, got, c.val)
			}
			if len(lookedUp) != 1 || lookedUp[0] != c.wantEnv {
				t.Errorf("looked up %v, want [%q]", lookedUp, c.wantEnv)
			}
		})
	}
}

func TestSecretPathDoesNotReadProcessVar(t *testing.T) {
	env := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin",
		"HOME": "/Users/alice",
	}
	r := NewResolver(func(k string) string { return env[k] })
	for _, name := range []string{"PATH", "HOME", " editor "} {
		_, err := r.Resolve(context.Background(), name)
		if err == nil {
			t.Fatalf("Resolve(%q) succeeded without MM_SECRET_*", name)
		}
		var ue *UnresolvedError
		if !errors.As(err, &ue) {
			t.Fatalf("Resolve(%q) error = %T (%v), want *UnresolvedError", name, err, err)
		}
		if strings.Contains(err.Error(), "/usr") || strings.Contains(err.Error(), "alice") {
			t.Errorf("error leaked environment value: %q", err.Error())
		}
	}
}

func TestResolverOrderingAndFallbackSeam(t *testing.T) {
	const envSecret = "from-env-sentinel"
	const fbSecret = "from-fallback-sentinel"

	fallbackCalls := 0
	r := NewResolver(
		func(k string) string {
			if k == "MM_SECRET_FIRST" {
				return envSecret
			}
			return ""
		},
		func(_ context.Context, name string) (string, bool, error) {
			fallbackCalls++
			if name == "first" || name == "second" {
				return fbSecret, true, nil
			}
			return "", false, nil
		},
	)

	// 1. MM_SECRET_<NAME> wins without invoking the fallback source.
	got, err := r.Resolve(context.Background(), "first")
	if err != nil || got != envSecret {
		t.Fatalf("first: got %q (%v), want %q", got, err, envSecret)
	}
	if fallbackCalls != 0 {
		t.Errorf("fallback called %d times when MM_SECRET_FIRST was set", fallbackCalls)
	}

	// 2. When MM_SECRET_<NAME> is unset, the fallback seam resolves the secret.
	got, err = r.Resolve(context.Background(), "second")
	if err != nil || got != fbSecret {
		t.Fatalf("second: got %q (%v), want %q", got, err, fbSecret)
	}
	if fallbackCalls != 1 {
		t.Errorf("fallback called %d times, want 1", fallbackCalls)
	}

	// 3. When neither resolves, UnresolvedError names only the reference.
	_, err = r.Resolve(context.Background(), "third")
	if err == nil {
		t.Fatal("third: expected error")
	}
	if err.Error() != "secret:third: could not be resolved" {
		t.Errorf("third error = %q", err.Error())
	}

	// 4. Empty reference name is rejected without consulting sources.
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Error("empty name: expected error")
	}
}

func TestDigestAndAsset(t *testing.T) {
	sum := sha256.Sum256([]byte("work/token\x00s3cr3t-val"))
	wantHex := hex.EncodeToString(sum[:])

	if got := Digest("work/token", "s3cr3t-val"); got != wantHex {
		t.Errorf("Digest = %q, want %q", got, wantHex)
	}
	asset := Asset("work/token", "s3cr3t-val")
	if asset.Name != "secret:work/token" || asset.SHA256 != wantHex {
		t.Errorf("Asset = %+v", asset)
	}
	if strings.Contains(asset.Name, "s3cr3t-val") || strings.Contains(asset.SHA256, "s3cr3t-val") {
		t.Errorf("Asset leaked secret value: %+v", asset)
	}
	// Null separator prevents boundary collisions between (name, value) pairs.
	if Digest("ab", "c") == Digest("a", "bc") {
		t.Error("Digest collided across name/value boundary")
	}
	// Different reference names that map to the same MM_SECRET_* variable still have distinct asset digests.
	if Digest("a-b", "v") == Digest("a/b", "v") {
		t.Error("Digest collided across distinct reference names")
	}
}

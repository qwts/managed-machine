package appcopy

import (
	"errors"
	"strings"
	"testing"
)

func validOptionMap() map[string]string {
	return map[string]string{
		"url":       "https://dl.example.com/Fixture-1.2.3.dmg",
		"sha256":    strings.Repeat("a", 64),
		"url_hosts": "dl.example.com,cdn.example.com",
		"app_name":  "Fixture App.app",
		"team_id":   "TEAM123456",
		"version":   "1.2.3",
		"format":    "dmg",
	}
}

func TestParseOptionsValid(t *testing.T) {
	opts, err := ParseOptions(validOptionMap())
	if err != nil {
		t.Fatalf("ParseOptions failed: %v", err)
	}
	if opts.Format != FormatDMG || opts.AppName != "Fixture App.app" || opts.TeamID != "TEAM123456" || opts.Version != "1.2.3" || len(opts.URLHosts) != 2 {
		t.Fatalf("unexpected options: %+v", opts)
	}

	m := validOptionMap()
	m["format"] = "zip"
	m["url_hosts"] = "[dl.example.com]"
	if _, err := ParseOptions(m); err != nil {
		t.Fatalf("zip format rejected: %v", err)
	}
}

func TestParseOptionsRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"unknown option", func(m map[string]string) { m["allow_rolling_url"] = "true" }, "unknown option"},
		{"missing url", func(m map[string]string) { delete(m, "url") }, `missing required option "url"`},
		{"http url", func(m map[string]string) { m["url"] = "http://dl.example.com/a.dmg" }, "https"},
		{"unlisted url host", func(m map[string]string) { m["url"] = "https://other.example.com/a.dmg" }, "not in url_hosts"},
		{"no_check sha256", func(m map[string]string) { m["sha256"] = "no_check" }, "64-character hex"},
		{"short sha256", func(m map[string]string) { m["sha256"] = "abcd" }, "64-character hex"},
		{"bad format", func(m map[string]string) { m["format"] = "pkg" }, `option "format"`},
		{"app_name without suffix", func(m map[string]string) { m["app_name"] = "Fixture" }, ".app"},
		{"app_name with slash", func(m map[string]string) { m["app_name"] = "../Fixture.app" }, ".app"},
		{"bad team_id", func(m map[string]string) { m["team_id"] = "bad team!" }, "team_id"},
		{"empty version", func(m map[string]string) { m["version"] = " " }, `missing required option "version"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validOptionMap()
			c.mut(m)
			_, err := ParseOptions(m)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("expected ErrUnsupported, got %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestValidatePlatformAndTarget(t *testing.T) {
	if err := ValidatePlatform("darwin/arm64", []string{"darwin/arm64"}); err != nil {
		t.Fatalf("darwin/arm64 rejected: %v", err)
	}
	if err := ValidatePlatform("linux/amd64", []string{"darwin/arm64"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("linux host accepted: %v", err)
	}
	if err := ValidatePlatform("darwin/arm64", []string{"darwin/arm64", "linux/arm64"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("multi-platform recipe accepted: %v", err)
	}
	if err := ValidateTarget("global"); err != nil {
		t.Errorf("global rejected: %v", err)
	}
	if err := ValidateTarget("user"); err != nil {
		t.Errorf("user rejected: %v", err)
	}
	if err := ValidateTarget("custom:/tmp/root"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("custom target accepted: %v", err)
	}
}

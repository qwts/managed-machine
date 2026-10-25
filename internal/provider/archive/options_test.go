package archive

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func validOptionMap() map[string]string {
	return map[string]string{
		"url":       "https://dl.example.com/tool-1.2.3.tar.gz",
		"sha256":    strings.Repeat("a", 64),
		"url_hosts": "dl.example.com,cdn.example.com",
		"version":   "1.2.3",
		"format":    "tar.gz",
		"files":     "tool-1.2.3/bin/tool,tool-1.2.3/bin/tool-helper.exe",
	}
}

func TestParseOptionsValid(t *testing.T) {
	opts, err := ParseOptions(validOptionMap())
	if err != nil {
		t.Fatalf("ParseOptions failed: %v", err)
	}
	if opts.Format != FormatTarGz || opts.Version != "1.2.3" || len(opts.URLHosts) != 2 || len(opts.Files) != 2 {
		t.Fatalf("unexpected options: %+v", opts)
	}
	if BaseName(opts.Files[0]) != "tool" || BaseName(opts.Files[1]) != "tool-helper.exe" {
		t.Fatalf("unexpected base names: %q, %q", BaseName(opts.Files[0]), BaseName(opts.Files[1]))
	}

	m := validOptionMap()
	m["format"] = "zip"
	m["url_hosts"] = "[dl.example.com]"
	m["files"] = "[bin/tool.exe]"
	optsZip, err := ParseOptions(m)
	if err != nil {
		t.Fatalf("zip format rejected: %v", err)
	}
	if optsZip.Format != FormatZip || len(optsZip.Files) != 1 || optsZip.Files[0] != "bin/tool.exe" {
		t.Fatalf("unexpected zip options: %+v", optsZip)
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
		{"missing sha256", func(m map[string]string) { delete(m, "sha256") }, `missing required option "sha256"`},
		{"missing url_hosts", func(m map[string]string) { delete(m, "url_hosts") }, `missing required option "url_hosts"`},
		{"missing version", func(m map[string]string) { delete(m, "version") }, `missing required option "version"`},
		{"missing format", func(m map[string]string) { delete(m, "format") }, `missing required option "format"`},
		{"missing files", func(m map[string]string) { delete(m, "files") }, `missing required option "files"`},
		{"http url", func(m map[string]string) { m["url"] = "http://dl.example.com/a.tar.gz" }, "https"},
		{"unlisted url host", func(m map[string]string) { m["url"] = "https://other.example.com/a.tar.gz" }, "not in url_hosts"},
		{"no_check sha256", func(m map[string]string) { m["sha256"] = "no_check" }, "64-character hex"},
		{"short sha256", func(m map[string]string) { m["sha256"] = "abcd" }, "64-character hex"},
		{"bad format", func(m map[string]string) { m["format"] = "dmg" }, `option "format"`},
		{"empty files list", func(m map[string]string) { m["files"] = "[]" }, `option "files"`},
		{"duplicate destination base name", func(m map[string]string) { m["files"] = "a/tool,b/tool" }, "share destination base name"},
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

func TestValidateMemberPath(t *testing.T) {
	for _, good := range []string{"tool", "bin/tool", "rg-14.1.0/bin/rg.exe"} {
		if err := ValidateMemberPath(good); err != nil {
			t.Errorf("ValidateMemberPath(%q) failed: %v", good, err)
		}
	}
	for _, bad := range []string{
		"",
		"/etc/passwd",
		"../evil",
		"a/../evil",
		"./tool",
		"dir/",
		`bin\tool`,
		"C:/bin/tool",
		"bin/tool.mm-tmp",
	} {
		if err := ValidateMemberPath(bad); !errors.Is(err, ErrUnsupported) {
			t.Errorf("ValidateMemberPath(%q) = %v, want ErrUnsupported", bad, err)
		}
	}
}

func TestValidatePlatformAndTarget(t *testing.T) {
	allFive := []string{
		"darwin/arm64",
		"linux/arm64",
		"linux/amd64",
		"windows/arm64",
		"windows/amd64",
	}
	for _, p := range allFive {
		if err := ValidatePlatform(p, allFive); err != nil {
			t.Fatalf("%s rejected: %v", p, err)
		}
	}
	if err := ValidatePlatform("windows/amd64", []string{"darwin/arm64"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("undeclared platform accepted: %v", err)
	}
	if err := ValidatePlatform("freebsd/amd64", nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("unknown platform accepted: %v", err)
	}
	if _, err := ValidateTarget("user"); err != nil {
		t.Errorf("user target rejected: %v", err)
	}
	customRoot := filepath.Join(t.TempDir(), "custom-root")
	gotRoot, err := ValidateTarget("custom:" + customRoot)
	if err != nil || gotRoot != filepath.Clean(customRoot) {
		t.Errorf("custom target = (%q, %v), want (%q, nil)", gotRoot, err, filepath.Clean(customRoot))
	}
	if _, err := ValidateTarget("global"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("global target accepted: %v", err)
	}
	if _, err := ValidateTarget("custom:relative/root"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("relative custom root accepted: %v", err)
	}
}

// Package archive is the built-in archive provider (S7 N03; D1, D4, D5, D6).
// It installs listed regular files from a pinned HTTPS tar.gz or zip archive
// into ~/.local/bin (user) or <root>/bin (custom) on all five targets in two
// saved operations (download, install), extracting in-process with archive/tar
// plus compress/gzip or archive/zip to <bin>/<name>.mm-tmp with mode 0755 and
// renaming into place.
package archive

import (
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/fetch"
	"github.com/qwts/managed-machine/internal/target"
)

// Name is the provider name in software recipes.
const Name = "archive"

const (
	// FormatTarGz is a gzip-compressed tar archive read in-process with
	// compress/gzip and archive/tar.
	FormatTarGz = "tar.gz"
	// FormatZip is a zip archive read in-process with archive/zip.
	FormatZip = "zip"
)

// Request and Resolution are the shared provider types.
type (
	Request    = provider.Request
	Resolution = provider.Resolution
)

// DefaultClient returns the default HTTPS client for artifact downloads.
func DefaultClient() *http.Client { return fetch.DefaultClient() }

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported      = provider.Kind(provider.ErrUnsupported, "unsupported by the archive provider")
	ErrPrivilege        = provider.Kind(provider.ErrPrivilege, "the bin directory is not writable by the current user")
	ErrArtifactMismatch = provider.ErrArtifactMismatch
	ErrHostUnlisted     = provider.ErrHostUnlisted
	ErrOccupied         = provider.ErrOccupied
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Options holds the validated options of an archive recipe.
type Options struct {
	URL      string
	SHA256   string
	URLHosts []string
	Version  string
	Format   string
	Files    []string
}

var knownOptions = map[string]bool{
	"url":       true,
	"sha256":    true,
	"url_hosts": true,
	"version":   true,
	"format":    true,
	"files":     true,
}

// ParseOptions validates the recipe options of an archive request.
func ParseOptions(raw map[string]string) (Options, error) {
	for k := range raw {
		if !knownOptions[k] {
			return Options{}, fmt.Errorf("%w: unknown option %q for %s", ErrUnsupported, k, Name)
		}
	}
	for _, req := range []string{"url", "sha256", "url_hosts", "version", "format", "files"} {
		if strings.TrimSpace(raw[req]) == "" {
			return Options{}, fmt.Errorf("%w: missing required option %q", ErrUnsupported, req)
		}
	}

	hosts := fetch.SplitHosts(raw["url_hosts"])
	if len(hosts) == 0 {
		return Options{}, fmt.Errorf("%w: option \"url_hosts\" must name at least one host", ErrUnsupported)
	}
	for _, h := range hosts {
		if strings.ContainsAny(h, "/:\\") {
			return Options{}, fmt.Errorf("%w: invalid url_hosts entry %q", ErrUnsupported, h)
		}
	}

	rawURL := strings.TrimSpace(raw["url"])
	if _, host, err := fetch.ValidateScheme(rawURL); err != nil {
		return Options{}, fmt.Errorf("%w: option \"url\": %v", ErrUnsupported, err)
	} else if !fetch.HostAllowed(host, hosts) {
		return Options{}, fmt.Errorf("%w: option \"url\" host %q is not in url_hosts", ErrUnsupported, host)
	}

	digest := strings.ToLower(strings.TrimSpace(raw["sha256"]))
	if !sha256Pattern.MatchString(digest) {
		return Options{}, fmt.Errorf("%w: option \"sha256\" must be a 64-character hex digest", ErrUnsupported)
	}

	format := strings.TrimSpace(raw["format"])
	if format != FormatTarGz && format != FormatZip {
		return Options{}, fmt.Errorf("%w: option \"format\" must be %q or %q, got %q", ErrUnsupported, FormatTarGz, FormatZip, format)
	}

	version := strings.TrimSpace(raw["version"])
	if version == "" || strings.ContainsAny(version, "\r\n\x00") {
		return Options{}, fmt.Errorf("%w: option \"version\" is invalid", ErrUnsupported)
	}

	files, err := parseFilesOption(raw["files"])
	if err != nil {
		return Options{}, err
	}

	return Options{
		URL:      rawURL,
		SHA256:   digest,
		URLHosts: hosts,
		Version:  version,
		Format:   format,
		Files:    files,
	}, nil
}

// SplitFiles parses a serialized files option value into individual archive
// member paths. It accepts comma-separated lists ("bin/a,bin/b") as well as
// Go slice strings ("[bin/a bin/b]").
func SplitFiles(raw string) []string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = strings.TrimSpace(s[1 : len(s)-1])
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
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseFilesOption(raw string) ([]string, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		for _, part := range strings.Split(s, ",") {
			if strings.TrimSpace(part) == "" {
				return nil, fmt.Errorf("%w: option \"files\" contains an empty member entry", ErrUnsupported)
			}
		}
	}
	files := SplitFiles(raw)
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: option \"files\" must name at least one archive member", ErrUnsupported)
	}
	seenBase := make(map[string]string, len(files))
	for _, f := range files {
		if strings.ContainsAny(f, "\r\n\x00") {
			return nil, fmt.Errorf("%w: option \"files\" entry %q is invalid", ErrUnsupported, f)
		}
		b := BaseName(f)
		if validBaseName(b) {
			if prev, exists := seenBase[b]; exists {
				return nil, fmt.Errorf("%w: option \"files\" members %q and %q share destination base name %q", ErrUnsupported, prev, f, b)
			}
			seenBase[b] = f
		}
	}
	return files, nil
}

// BaseName returns the final path segment of an archive member path. The .exe
// suffix on Windows binaries is preserved as written.
func BaseName(member string) string {
	if i := strings.LastIndexAny(member, `/\`); i >= 0 {
		return member[i+1:]
	}
	return member
}

func validBaseName(base string) bool {
	if base == "" || base == "." || base == ".." {
		return false
	}
	if strings.ContainsAny(base, "/\\:\x00\r\n") {
		return false
	}
	if strings.HasSuffix(base, ".mm-tmp") {
		return false
	}
	return true
}

// ValidateMemberPath checks that p is a clean relative archive member path
// with no leading slash, volume prefix, backslash, empty segment, "." or ".."
// traversal segment.
func ValidateMemberPath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty archive member path", ErrUnsupported)
	}
	if strings.ContainsAny(p, "\\\x00\r\n") {
		return fmt.Errorf("%w: archive member path %q contains invalid characters", ErrUnsupported, p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: archive member path %q must not be absolute", ErrUnsupported, p)
	}
	if (len(p) >= 2 && p[1] == ':') || filepath.VolumeName(p) != "" {
		return fmt.Errorf("%w: archive member path %q must not contain a volume prefix", ErrUnsupported, p)
	}
	parts := strings.Split(p, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: archive member path %q contains invalid segment %q", ErrUnsupported, p, seg)
		}
	}
	if !validBaseName(parts[len(parts)-1]) {
		return fmt.Errorf("%w: archive member path %q has invalid base name %q", ErrUnsupported, p, parts[len(parts)-1])
	}
	return nil
}

// ValidatePlatform checks that the host platform and every platform declared
// by the recipe are canonical first-release targets, and that platform is in
// platforms when both are provided.
func ValidatePlatform(platform string, platforms []string) error {
	if platform != "" {
		if _, err := target.Parse(platform); err != nil {
			return fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
	}
	found := false
	for _, p := range platforms {
		if _, err := target.Parse(p); err != nil {
			return fmt.Errorf("%w: recipe declares unsupported platform %s", ErrUnsupported, p)
		}
		if p == platform {
			found = true
		}
	}
	if platform != "" && len(platforms) > 0 && !found {
		return fmt.Errorf("%w: platform %s is not declared by recipe (%s)", ErrUnsupported, platform, strings.Join(platforms, ", "))
	}
	return nil
}

// ValidateTarget checks that target is user or custom:<absolute path>; global
// and relative custom roots are refused for archive.
func ValidateTarget(tgt string) (string, error) {
	switch {
	case tgt == "user":
		return "", nil
	case strings.HasPrefix(tgt, "custom:"):
		root := strings.TrimPrefix(tgt, "custom:")
		if root == "" || !filepath.IsAbs(root) {
			return "", fmt.Errorf("%w: custom root %q is not an absolute path", ErrUnsupported, root)
		}
		return filepath.Clean(root), nil
	default:
		return "", fmt.Errorf("%w: target %s; %s supports only user and custom targets", ErrUnsupported, tgt, Name)
	}
}

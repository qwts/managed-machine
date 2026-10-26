package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/target"
)

// BundleRootEnv overrides the bundle root directory for `skill path` (S8 D2).
const BundleRootEnv = "MANAGED_MACHINE_BUNDLE_ROOT"

// Command is `managed-machine skill`. open loads the served bundles for
// the host target; tests inject fixtures.
func Command(open func(target.Target) (Store, error)) cli.Command {
	return cli.Command{
		Name:    "skill",
		Summary: "Serve bounded, versioned skill guidance (read-only)",
		Usage:   usage,
		Help:    func(ctx cli.Context, args []string) string { return help(open, ctx, args) },
		Run:     func(ctx cli.Context, args []string) cli.Result { return run(open, ctx, args) },
	}
}

const usage = `Usage: managed-machine skill [name] [options]

Serve bounded, structured, versioned skill guidance.
Default output is compact and agent-friendly.
`

const options = `
Options
-h, --help           Show usage and a readable catalog
--kind <kind>        Serve or filter by content kind
--bundle <bundle>    Filter by bundle
--search <term>      Pointer-only search
--version <version>  Pin a bundle version
--next <handle>      Continue from a continuation handle
--json               Return results as one JSON document
--human              Render results in a readable form

Reading guidance never authorizes or performs any action.
`

type flags struct {
	name, kind, bundle, search, version, next string
	hasSearch                                 bool
}

func parse(args []string) (flags, *diag.Diagnostic) {
	var f flags
	if d := command.Parse(args, command.Spec{
		Positional:     &f.name,
		PositionalName: "skill name",
		Values: map[string]command.Flag{
			"--kind":    {Value: &f.kind},
			"--bundle":  {Value: &f.bundle},
			"--search":  {Value: &f.search, Set: &f.hasSearch},
			"--version": {Value: &f.version},
			"--next":    {Value: &f.next},
		},
	}); d != nil {
		return f, d
	}
	switch {
	case f.hasSearch && f.name != "":
		return f, command.UsageErr("--search and a skill name are mutually exclusive")
	case f.hasSearch && f.search == "":
		return f, command.UsageErr("--search needs a non-empty term")
	case f.kind != "" && !has(Kinds, f.kind):
		return f, failure(diag.SkillUnknownKind, "unknown kind %q; kinds: %s", f.kind, strings.Join(Kinds, ", "))
	}
	return f, nil
}

func load(open func(target.Target) (Store, error), ctx cli.Context) (Store, *diag.Diagnostic) {
	host, dg := command.DetectHost(ctx)
	if dg != nil {
		return Store{}, dg
	}
	st, err := open(host.Target)
	if err != nil {
		return Store{}, failure(diag.SkillBundleUnreadable, "%v", err)
	}
	return st, nil
}

// served applies --bundle and --version.
func served(st Store, f flags) ([]Bundle, *diag.Diagnostic) {
	var bs []Bundle
	for _, b := range st.Bundles {
		if f.bundle == "" || b.Name == f.bundle {
			bs = append(bs, b)
		}
	}
	if len(bs) == 0 {
		return nil, failure(diag.SkillUnknownBundle, "bundle %q is not installed", f.bundle)
	}
	if f.version != "" {
		var keep []Bundle
		for _, b := range bs {
			if b.Version == f.version {
				keep = append(keep, b)
			}
		}
		if len(keep) == 0 {
			return nil, failure(diag.SkillUnknownVersion, "version %s is not installed%s", f.version, forBundle(f.bundle))
		}
		bs = keep
	}
	return bs, nil
}

func forBundle(b string) string {
	if b == "" {
		return ""
	}
	return " for bundle " + b
}

func run(open func(target.Target) (Store, error), ctx cli.Context, args []string) cli.Result {
	if len(args) > 0 && args[0] == "path" {
		if len(args) > 1 {
			return cli.Result{Diagnostic: command.UsageErr("skill path takes no other arguments or options")}
		}
		return runPath(ctx)
	}
	if r, ok := legacy.Match(append([]string{"skill"}, args...)); ok {
		dg := legacy.Diagnostic(r)
		return cli.Result{Diagnostic: &dg}
	}
	f, bad := parse(args)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	st, bad := load(open, ctx)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	bs, bad := served(st, f)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	if f.next != "" {
		h, ok := ParseHandle(f.next)
		if !ok {
			return cli.Result{Diagnostic: failure(diag.SkillUnknownHandle, "%q is not a continuation handle", f.next)}
		}
		if !h.Cursor {
			return serveHandle(bs, f, h)
		}
		if h.Op == "search" != f.hasSearch || h.Op == "list" && f.name != "" {
			return cli.Result{Diagnostic: failure(diag.SkillCursorMismatch, "%s cursor presented with a different request", h.Op)}
		}
	}
	if f.hasSearch {
		return search(bs, f)
	}
	if f.name != "" {
		return serveName(bs, f)
	}
	return list(bs, f)
}

func runPath(ctx cli.Context) cli.Result {
	var bundleRoot string
	if ctx.Getenv != nil {
		bundleRoot = strings.TrimSpace(ctx.Getenv(BundleRootEnv))
	}
	if bundleRoot != "" {
		bundleRoot = filepath.Clean(bundleRoot)
	} else {
		exeFn := ctx.Executable
		if exeFn == nil {
			exeFn = os.Executable
		}
		exePath, err := exeFn()
		if err != nil || strings.TrimSpace(exePath) == "" {
			msg := "cannot determine executable path for skill bundle"
			if err != nil {
				msg += ": " + err.Error()
			}
			return cli.Result{Diagnostic: failure(diag.SkillBundleMissing, "%s", msg)}
		}
		if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = resolved
		}
		bundleRoot = filepath.Clean(filepath.Join(filepath.Dir(exePath), "..", "libexec"))
	}
	bundleDir := filepath.Join(bundleRoot, "skills", "managed-machine")
	skillFile := filepath.Join(bundleDir, "SKILL.md")
	info, err := os.Stat(skillFile)
	if err != nil || !info.Mode().IsRegular() {
		return cli.Result{Diagnostic: failure(diag.SkillBundleMissing, "skill bundle is missing at %s", skillFile)}
	}
	commit := readReleaseCommit(filepath.Join(bundleRoot, "RELEASE_COMMIT"))
	rec := output.New().
		Set("command", "skill-path").
		Set("name", "managed-machine").
		Set("path", bundleDir).
		Set("skill_file", skillFile).
		Set("commit", commit)
	return cli.Result{
		Records: []*output.Record{rec},
		Human:   fmt.Sprintf("%s\ncommit %s\n", bundleDir, commit),
	}
}

func readReleaseCommit(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	line, _, _ := strings.Cut(string(data), "\n")
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if !is40Hex(line) {
		return "unknown"
	}
	return line
}

func is40Hex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func list(bs []Bundle, f flags) cli.Result {
	var es []entry
	for _, b := range bs {
		for _, s := range b.Skills {
			if f.kind != "" && !has(s.Kinds, f.kind) {
				continue
			}
			first := s.Kinds[0]
			if has(s.Kinds, "summary") {
				first = "summary"
			}
			tok := TokensEst(len(s.Content[first]))
			es = append(es, entry{bundle: b, tokens: tok, rec: output.New().Set("name", s.Name).Set("bundle", b.Name).
				Set("kinds", s.Kinds).Set("version", b.Version).Set("revision", b.Revision).Set("tokens_est", tok)})
		}
	}
	return paged("list", cursorDigest("list", "", f.bundle, f.kind, f.version, bs), es, f, len(es)+1)
}

func search(bs []Bundle, f flags) cli.Result {
	term := strings.ToLower(f.search)
	var es []entry
	for _, b := range bs {
		for _, s := range b.Skills {
			if !strings.Contains(strings.ToLower(s.Name), term) && !strings.Contains(strings.ToLower(s.Content["summary"]), term) {
				continue
			}
			for _, k := range s.Kinds {
				if f.kind != "" && k != f.kind {
					continue
				}
				tok := TokensEst(len(caveatBlock(s)) + len(s.Content[k]))
				es = append(es, entry{bundle: b, tokens: tok, rec: output.New().Set("name", s.Name).Set("kind", k).
					Set("bundle", b.Name).Set("version", b.Version).Set("revision", b.Revision).
					Set("pointer", pointer(b, s, k)).Set("tokens_est", tok)})
			}
		}
	}
	return paged("search", cursorDigest("search", f.search, f.bundle, f.kind, f.version, bs), es, f, MaxSearchRecords)
}

func paged(op, digest string, es []entry, f flags, max int) cli.Result {
	pos := 1
	if f.next != "" {
		h, _ := ParseHandle(f.next)
		if h.Digest != digest {
			return cli.Result{Diagnostic: failure(diag.SkillCursorMismatch, "the %s cursor was issued for a different query, filters or bundle snapshot", op)}
		}
		if h.Position > len(es) {
			return cli.Result{Diagnostic: failure(diag.SkillUnknownHandle, "cursor position %d is past the %d results", h.Position, len(es))}
		}
		pos = h.Position
	}
	if len(es) == 0 {
		return cli.Result{}
	}
	return cli.Result{Records: page(op, digest, es, pos, max)}
}

func findSkill(bs []Bundle, name string) (Bundle, Skill, *diag.Diagnostic) {
	var hits []int
	for i := range bs {
		if bs[i].find(name) != nil {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return Bundle{}, Skill{}, failure(diag.SkillUnknownName, "%q is not in any served bundle", name)
	case 1:
		return bs[hits[0]], *bs[hits[0]].find(name), nil
	}
	return Bundle{}, Skill{}, command.UsageErr("%q is served by several bundles; use --bundle", name)
}

func serveName(bs []Bundle, f flags) cli.Result {
	b, s, bad := findSkill(bs, f.name)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	kind := f.kind
	if kind == "" {
		kind = "summary"
		if !has(s.Kinds, kind) {
			kind = s.Kinds[0]
		}
	}
	if !has(s.Kinds, kind) {
		return cli.Result{Diagnostic: failure(diag.SkillUnsupportedKind, "%s does not provide %s; kinds: %s", s.Name, kind, strings.Join(s.Kinds, ", "))}
	}
	return chunkResult(b, s, kind, 1)
}

func serveHandle(bs []Bundle, f flags, h ParsedHandle) cli.Result {
	if f.name != "" && f.name != h.Name || f.kind != "" && f.kind != h.Kind {
		return cli.Result{Diagnostic: command.UsageErr("--next %s continues %s %s; drop the conflicting name or --kind", f.next, h.Name, h.Kind)}
	}
	for _, b := range bs {
		if b.Name != h.Bundle {
			continue
		}
		if b.Version != h.Version || b.Revision != h.Revision {
			return cli.Result{Diagnostic: failure(diag.SkillUnknownHandle, "handle is for %s@%s:%s; served is %s@%s:%s", h.Bundle, h.Version, h.Revision, b.Name, b.Version, b.Revision)}
		}
		s := b.find(h.Name)
		if s == nil || !has(s.Kinds, h.Kind) || h.Position < 2 {
			return cli.Result{Diagnostic: failure(diag.SkillUnknownHandle, "%q was not returned by a previous response", f.next)}
		}
		return chunkResult(b, *s, h.Kind, h.Position)
	}
	return cli.Result{Diagnostic: failure(diag.SkillUnknownHandle, "bundle %s of the handle is not served", h.Bundle)}
}

func chunkResult(b Bundle, s Skill, kind string, n int) cli.Result {
	rec, bad := Content(b, s, kind, n)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	content, _ := rec.Get("content")
	tok, _ := rec.Get("tokens_est")
	human := fmt.Sprintf("%s (%s) from %s %s, revision %s, about %d tokens\n\n%s\n", s.Name, kind, b.Name, b.Version, b.Revision, tok, strings.TrimRight(content.(string), "\n"))
	if next, ok := rec.Get("next"); ok {
		human += fmt.Sprintf("\nMore follows: --next %s\n", next)
	}
	return cli.Result{Records: []*output.Record{rec}, Human: human}
}

func help(open func(target.Target) (Store, error), ctx cli.Context, args []string) string {
	var b strings.Builder
	b.WriteString(usage)
	f, bad := parse(args)
	if bad == nil {
		var st Store
		st, bad = load(open, ctx)
		var bs []Bundle
		if bad == nil {
			bs, bad = served(st, f)
		}
		if bad == nil {
			rows := [][]string{{"SKILL", "BUNDLE", "KINDS", "VERSION"}}
			for _, bu := range bs {
				for _, s := range bu.Skills {
					if f.kind == "" || has(s.Kinds, f.kind) {
						rows = append(rows, []string{s.Name, bu.Name, strings.Join(s.Kinds, ","), bu.Version})
					}
				}
			}
			b.WriteString("\nSkills\n" + command.Table(rows))
			rows = [][]string{{"BUNDLE", "SOURCE", "VERSION"}}
			for _, bu := range bs {
				rows = append(rows, []string{bu.Name, bu.Source, bu.Version})
			}
			b.WriteString("\nBundles\n" + command.Table(rows))
		}
	}
	if bad != nil {
		b.WriteString("\n" + bad.Human())
	}
	b.WriteString(options)
	return b.String()
}

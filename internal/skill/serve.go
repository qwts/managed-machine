package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/output"
)

// TokensEst is the estimated token cost of n bytes of content: about four
// bytes per token. It is an estimate, labeled as one in the contract.
func TokensEst(n int) int { return (n + 3) / 4 }

// fits reports whether records serialize within the response bound in
// both the JSON Lines and the --json document forms, so a page boundary
// never depends on the presentation mode.
func fits(recs []*output.Record) bool {
	for _, m := range []output.Mode{output.Compact, output.JSON} {
		var buf bytes.Buffer
		if err := output.Write(&buf, m, recs); err != nil || buf.Len() > MaxResponseBytes {
			return false
		}
	}
	return true
}

func failure(id, format string, args ...any) *diag.Diagnostic {
	d := diag.New(id, fmt.Sprintf(format, args...))
	return &d
}

// --- content ----------------------------------------------------------------

// contentHandle is "<bundle>:<name>:<kind>@<version>:<revision>#<chunk>".
func contentHandle(b Bundle, s Skill, kind string, chunk int) string {
	return fmt.Sprintf("%s:%s:%s@%s:%s#%d", b.Name, s.Name, kind, b.Version, b.Revision, chunk)
}

func pointer(b Bundle, s Skill, kind string) string {
	return fmt.Sprintf("%s:%s:%s@%s", b.Name, s.Name, kind, b.Version)
}

func chunkRecord(b Bundle, s Skill, kind, content, next string) *output.Record {
	r := output.New().Set("name", s.Name).Set("kind", kind).Set("bundle", b.Name).
		Set("version", b.Version).Set("revision", b.Revision).
		Set("tokens_est", TokensEst(len(content))).Set("content", content)
	if next != "" {
		r.Set("next", next)
	}
	return r
}

// units splits text into pieces whose concatenation is exactly text,
// preferring paragraph boundaries, then lines, then runes, so a chunk
// boundary falls on the largest unit that fits.
func units(text string, sep string) []string {
	var out []string
	for text != "" {
		i := strings.Index(text, sep)
		if i < 0 {
			out = append(out, text)
			break
		}
		out = append(out, text[:i+len(sep)])
		text = text[i+len(sep):]
	}
	return out
}

// chunks splits an item into records, each within the response bound.
// The first chunk always starts with the caveat block, which is never
// split. The concatenated content of all chunks is exactly the item.
func chunks(b Bundle, s Skill, kind string) ([]string, error) {
	body := s.Content[kind]
	if len(body) > MaxItemBytes {
		return nil, fmt.Errorf("%s is %d bytes; the maximum item size is %d (fetch it out of band from the bundle file %s)",
			pointer(b, s, kind), len(body), MaxItemBytes, pathOf(s.Name, kind))
	}
	caveats := caveatBlock(s)
	var out []string
	cur := caveats
	pending := []string{}
	for _, p := range units(body, "\n\n") {
		pending = append(pending, p)
	}
	try := func(content string, idx int) bool {
		return fits([]*output.Record{chunkRecord(b, s, kind, content, contentHandle(b, s, kind, idx+1))})
	}
	if !try(cur, len(out)+1) {
		return nil, fmt.Errorf("mandatory caveats of %s do not fit the first response (%d bytes)", pointer(b, s, kind), MaxResponseBytes)
	}
	for len(pending) > 0 {
		u := pending[0]
		if try(cur+u, len(out)+1) {
			cur += u
			pending = pending[1:]
			continue
		}
		if cur != "" && cur != caveats {
			out = append(out, cur)
			cur = ""
			continue
		}
		// u does not fit even in a fresh chunk: split it finer.
		var finer []string
		switch {
		case strings.Contains(strings.TrimSuffix(u, "\n"), "\n"):
			finer = units(u, "\n")
		default:
			r := []rune(u)
			half := len(r) / 2
			if half == 0 {
				return nil, fmt.Errorf("%s cannot be chunked within %d bytes", pointer(b, s, kind), MaxResponseBytes)
			}
			finer = []string{string(r[:half]), string(r[half:])}
		}
		pending = append(finer, pending[1:]...)
	}
	if cur != "" || len(out) == 0 {
		out = append(out, cur)
	}
	return out, nil
}

// Content serves chunk n (1-based) of an item.
func Content(b Bundle, s Skill, kind string, n int) (*output.Record, *diag.Diagnostic) {
	cs, err := chunks(b, s, kind)
	if err != nil {
		return nil, failure(diag.SkillContentOversized, "%v", err)
	}
	if n < 1 || n > len(cs) {
		return nil, failure(diag.SkillUnknownHandle, "%s has %d chunks", pointer(b, s, kind), len(cs))
	}
	next := ""
	if n < len(cs) {
		next = contentHandle(b, s, kind, n+1)
	}
	return chunkRecord(b, s, kind, cs[n-1], next), nil
}

// ParsedHandle is a decoded continuation handle.
type ParsedHandle struct {
	Cursor        bool   // list or search cursor
	Op            string // "list" or "search" for cursors
	Digest        string
	Bundle, Name  string
	Kind, Version string
	Revision      string
	Position      int
}

// ParseHandle decodes a content handle or a list/search cursor.
func ParseHandle(h string) (ParsedHandle, bool) {
	hash := strings.LastIndex(h, "#")
	if hash < 0 {
		return ParsedHandle{}, false
	}
	pos, err := strconv.Atoi(h[hash+1:])
	if err != nil || pos < 1 || strconv.Itoa(pos) != h[hash+1:] {
		return ParsedHandle{}, false
	}
	head := h[:hash]
	for _, op := range []string{"list", "search"} {
		if d, ok := strings.CutPrefix(head, op+"@"); ok {
			if len(d) != 16 || strings.Trim(d, "0123456789abcdef") != "" {
				return ParsedHandle{}, false
			}
			return ParsedHandle{Cursor: true, Op: op, Digest: d, Position: pos}, true
		}
	}
	at := strings.LastIndex(head, "@")
	if at < 0 {
		return ParsedHandle{}, false
	}
	ident, ver := head[:at], head[at+1:]
	vr := strings.SplitN(ver, ":", 2)
	parts := strings.Split(ident, ":")
	if len(vr) != 2 || len(parts) < 3 {
		return ParsedHandle{}, false
	}
	return ParsedHandle{Bundle: parts[0], Name: strings.Join(parts[1:len(parts)-1], ":"), Kind: parts[len(parts)-1],
		Version: vr[0], Revision: vr[1], Position: pos}, true
}

// --- listing and search ------------------------------------------------------

// cursorDigest binds a cursor to its operation, query, filters and the
// snapshot of every searched bundle.
func cursorDigest(op, query, bundle, kind, version string, bs []Bundle) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s", op, query, bundle, kind, version)
	for _, b := range bs {
		fmt.Fprintf(h, "\x00%s@%s:%s", b.Name, b.Version, b.Revision)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// entry is one listing or search record with the bundle it came from.
type entry struct {
	rec    *output.Record
	bundle Bundle
	tokens int
}

// page returns the records from position pos (1-based) that fit the
// bounds, followed by a continuation record when entries remain.
func page(op, digest string, es []entry, pos, maxRecords int) []*output.Record {
	var out []*output.Record
	i := pos - 1
	for ; i < len(es) && len(out) < maxRecords; i++ {
		cand := append(append([]*output.Record{}, out...), es[i].rec)
		if i+1 < len(es) {
			cand = append(cand, continuation(op, digest, es, i+1))
		}
		if !fits(cand) && len(out) > 0 {
			break
		}
		out = append(out, es[i].rec)
	}
	if i < len(es) {
		out = append(out, continuation(op, digest, es, i))
	}
	return out
}

func continuation(op, digest string, es []entry, from int) *output.Record {
	tokens := 0
	for _, e := range es[from:] {
		tokens += e.tokens
	}
	b := es[from].bundle
	return output.New().Set("bundle", b.Name).Set("version", b.Version).Set("revision", b.Revision).
		Set("tokens_est", tokens).Set("next", fmt.Sprintf("%s@%s#%d", op, digest, from+1))
}

// firstChunkFloor is the smallest possible first response of an item: its
// caveat block alone, with a continuation handle, in the larger of the
// two serialized forms.
func firstChunkFloor(b Bundle, s Skill, kind string) int {
	rec := []*output.Record{chunkRecord(b, s, kind, caveatBlock(s), contentHandle(b, s, kind, 2))}
	max := 0
	for _, m := range []output.Mode{output.Compact, output.JSON} {
		var buf bytes.Buffer
		output.Write(&buf, m, rec)
		if buf.Len() > max {
			max = buf.Len()
		}
	}
	return max
}

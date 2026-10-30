package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
)

const maxMessage = 64 << 10
const timestampSkew = 5 * time.Minute

// Request is one signed command. Fields the catalog did not declare are
// rejected by DisallowUnknownFields before a signature is considered.
type Request struct {
	Verb      string `json:"verb"`
	Name      string `json:"name,omitempty"`
	Option    string `json:"option,omitempty"`
	Target    string `json:"target,omitempty"`
	PlanID    string `json:"plan_id,omitempty"`
	Soul      string `json:"soul,omitempty"`
	Harness   string `json:"harness,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Timestamp int64  `json:"timestamp"`
	Nonce     string `json:"nonce"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

// Response is signed by the daemon. Signature is over the other fields.
type Response struct {
	OK         bool            `json:"ok"`
	Diagnostic string          `json:"diagnostic,omitempty"`
	Message    string          `json:"message,omitempty"`
	State      string          `json:"state,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Timestamp  int64           `json:"timestamp"`
	Nonce      string          `json:"nonce"`
	Signature  string          `json:"signature"`
}

// Pairing is the agent-daemon public key. A staged key does not replace the
// live key until Confirm.
type Pairing struct {
	Live   ed25519.PublicKey
	Staged ed25519.PublicKey
}

// Stage records a replacement key and keeps Live authoritative.
func (p *Pairing) Stage(next ed25519.PublicKey) {
	p.Staged = append(ed25519.PublicKey(nil), next...)
}

// Confirm makes the staged key live. It does nothing when nothing is staged.
func (p *Pairing) Confirm() {
	if len(p.Staged) == ed25519.PublicKeySize {
		p.Live = p.Staged
		p.Staged = nil
	}
}

func (p Pairing) allows(key ed25519.PublicKey) bool {
	return len(p.Live) == ed25519.PublicKeySize && ed25519.PublicKey(p.Live).Equal(key)
}

// Gate verifies peer identity, signature, time, and nonce. Peer identity is
// necessary and never sufficient.
type Gate struct {
	Pairing Pairing
	// Dir is the daemon state directory. When set, verify reads the pairing
	// files before authorizing so a revocation or a confirmed replacement
	// takes effect without a restart.
	Dir     string
	PeerUID int
	Now     func() time.Time
	seen    map[string]struct{}
}

func (g *Gate) verify(peerUID int, raw []byte) (Request, string, string) {
	var req Request
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, diag.DaemonTampered, "request was not a single known object"
	}
	if peerUID != g.PeerUID {
		return req, diag.DaemonPeerRefused, "caller is not the paired agent-daemon"
	}
	if req.Signature == "" || req.PublicKey == "" {
		return req, diag.DaemonUnsigned, "request is not signed"
	}
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return req, diag.DaemonUnknownKey, "request key is not a known pairing"
	}
	if g.Dir != "" {
		pairing, err := loadPairing(g.Dir)
		if err != nil {
			return req, diag.DaemonUnknownKey, "pairing state could not be read"
		}
		g.Pairing = pairing
	}
	if !g.Pairing.allows(ed25519.PublicKey(pub)) {
		return req, diag.DaemonUnknownKey, "request key is not the live pairing"
	}
	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), canonical(req), sig) {
		return req, diag.DaemonTampered, "request signature does not match"
	}
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	when := time.Unix(req.Timestamp, 0)
	if when.Before(now.Add(-timestampSkew)) || when.After(now.Add(timestampSkew)) {
		return req, diag.DaemonStale, "request timestamp is outside the allowed window"
	}
	if req.Nonce == "" {
		return req, diag.DaemonReplay, "request nonce is missing"
	}
	if g.seen == nil {
		g.seen = map[string]struct{}{}
	}
	if _, ok := g.seen[req.Nonce]; ok {
		return req, diag.DaemonReplay, "request nonce was already used"
	}
	g.seen[req.Nonce] = struct{}{}
	if !knownVerb(req.Verb) {
		return req, diag.DaemonVerbUnknown, "verb is not one of status, receipts, plan, apply, update"
	}
	return req, "", ""
}

func knownVerb(verb string) bool {
	switch verb {
	case "status", "receipts", "plan", "apply", "update":
		return true
	default:
		return false
	}
}

func canonical(r Request) []byte {
	return []byte(strings.Join([]string{
		r.Verb, r.Name, r.Option, r.Target, r.PlanID, r.Soul, r.Harness, r.SessionID,
		strconv.FormatInt(r.Timestamp, 10), r.Nonce,
	}, "\n"))
}

// Sign fills PublicKey and Signature using the agent-daemon private key.
func Sign(priv ed25519.PrivateKey, r *Request) error {
	pub := priv.Public().(ed25519.PublicKey)
	r.PublicKey = base64.StdEncoding.EncodeToString(pub)
	sig := ed25519.Sign(priv, canonical(*r))
	r.Signature = base64.StdEncoding.EncodeToString(sig)
	return nil
}

func signResponse(priv ed25519.PrivateKey, res *Response) {
	res.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, responsePayload(*res)))
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func refused(id, message string) Response {
	return Response{OK: false, Diagnostic: id, Message: message}
}

func formatLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > maxMessage {
		return nil, fmt.Errorf("message exceeds %d bytes", maxMessage)
	}
	return append(b, '\n'), nil
}

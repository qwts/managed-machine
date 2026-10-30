package daemon

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Listen opens the per-user socket under dir. The directory is mode 0700
// and the socket is mode 0600. A previous socket file is removed first.
func Listen(dir string) (net.Listener, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "daemon.sock")
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve accepts one JSON line per connection, checks the peer and the
// signature, and writes one signed JSON line back. It returns when ctx is
// cancelled.
func Serve(ctx context.Context, ln net.Listener, g *Gate, priv ed25519.PrivateKey, peer func(net.Conn) (int, error), hooks Hooks) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		handleConn(conn, g, priv, peer, hooks)
	}
}

func handleConn(conn net.Conn, g *Gate, priv ed25519.PrivateKey, peer func(net.Conn) (int, error), hooks Hooks) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(io.LimitReader(conn, maxMessage))
	line, err := reader.ReadBytes('\n')
	uid := -1
	if peer != nil && err == nil {
		uid, err = peer(conn)
	}
	var res Response
	if err != nil {
		res = refused("MM_DAEMON_TAMPERED", err.Error())
	} else {
		req, id, msg := g.verify(uid, line)
		if id != "" {
			res = refused(id, msg)
		} else {
			res = hooks.reply(req)
		}
	}
	res.Timestamp = time.Now().Unix()
	nonce, nerr := newNonce()
	if nerr == nil {
		res.Nonce = nonce
	}
	if priv != nil {
		signResponse(priv, &res)
	}
	body, merr := formatLine(res)
	if merr == nil {
		_, _ = conn.Write(body)
	}
}

// Hooks supplies the read-only bodies for status and receipts and the
// catalog plan and apply actions. A nil function keeps the previous
// placeholder for status and refuses the other verbs.
type Hooks struct {
	Status    func() (string, []byte, error)
	Receipts  func() ([]byte, error)
	Plan      func(name, option, target string) (string, string)
	Apply     func(planID, name, option, target string) string
	Update    func(name string) (string, string)
	OnReceipt func()
}

func (h Hooks) reply(req Request) Response {
	switch req.Verb {
	case "status":
		if h.Status == nil {
			return Response{OK: true, State: "idle"}
		}
		state, body, err := h.Status()
		if err != nil {
			return refused("MM_DAEMON_STATUS_FAILED", "status could not be read")
		}
		return Response{OK: true, State: state, Body: body}
	case "receipts":
		if h.Receipts == nil {
			return refused("MM_DAEMON_UNIMPLEMENTED", "receipts is not served yet")
		}
		body, err := h.Receipts()
		if err != nil {
			return refused("MM_DAEMON_STATUS_FAILED", "receipts could not be read")
		}
		return Response{OK: true, State: "listed", Body: body}
	case "plan":
		if h.Plan == nil {
			return refused("MM_DAEMON_UNIMPLEMENTED", "plan is not served yet")
		}
		id, diagID := h.Plan(req.Name, req.Option, req.Target)
		if diagID != "" {
			return refused(diagID, "selection is not a pinned catalog item")
		}
		return Response{OK: true, State: "saved", Message: id}
	case "apply":
		if h.Apply == nil {
			return refused("MM_DAEMON_UNIMPLEMENTED", "apply is not served yet")
		}
		if diagID := h.Apply(req.PlanID, req.Name, req.Option, req.Target); diagID != "" {
			return refused(diagID, "saved plan was not applied")
		}
		if h.OnReceipt != nil {
			h.OnReceipt()
		}
		return Response{OK: true, State: "applied", Message: req.PlanID}
	case "update":
		if h.Update == nil {
			return refused("MM_DAEMON_UNIMPLEMENTED", "update is not served yet")
		}
		ids, diagID := h.Update(req.Name)
		if diagID != "" {
			return refused(diagID, "flavor was not updated")
		}
		if h.OnReceipt != nil {
			h.OnReceipt()
		}
		return Response{OK: true, State: "updated", Message: ids}
	default:
		return refused("MM_DAEMON_UNIMPLEMENTED", req.Verb+" is not served yet")
	}
}

func responsePayload(res Response) []byte {
	return []byte(strings.Join([]string{
		strconv.FormatBool(res.OK), res.Diagnostic, res.Message, res.State, string(res.Body),
		strconv.FormatInt(res.Timestamp, 10), res.Nonce,
	}, "\n"))
}

// VerifyResponse checks the daemon's signature over a response.
func VerifyResponse(pub ed25519.PublicKey, res Response) bool {
	sig, err := base64.StdEncoding.DecodeString(res.Signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, responsePayload(res), sig)
}

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
func Serve(ctx context.Context, ln net.Listener, g *Gate, priv ed25519.PrivateKey, peer func(net.Conn) (int, error)) error {
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
		handleConn(conn, g, priv, peer)
	}
}

func handleConn(conn net.Conn, g *Gate, priv ed25519.PrivateKey, peer func(net.Conn) (int, error)) {
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
		} else if req.Verb == "status" {
			res = Response{OK: true, State: "idle"}
		} else {
			res = refused("MM_DAEMON_UNIMPLEMENTED", req.Verb+" is not served yet")
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

func responsePayload(res Response) []byte {
	return []byte(strings.Join([]string{
		strconv.FormatBool(res.OK), res.Diagnostic, res.Message, res.State,
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

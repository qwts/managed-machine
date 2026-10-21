// Package protocol is the client side of mm-extension-1: one JSON request
// line on the extension's stdin, one JSON response line on its stdout.
// Anything else on stdout is a protocol violation, never text to parse.
package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// Version is the protocol identifier carried on every message.
const Version = "mm-extension-1"

// MaxResponse bounds one response line, so a misbehaving extension cannot
// make the core buffer unbounded output.
const MaxResponse = 1 << 20

// Request is one call. Params is method-specific and already JSON.
type Request struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// Response carries exactly one of Result or Error.
type Response struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Fault          `json:"error,omitempty"`
}

// Fault is an extension-reported failure.
type Fault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrViolation marks a response that breaks the protocol, as distinct from
// a well-formed Fault the extension chose to return.
var ErrViolation = errors.New("protocol violation")

// Call starts argv with env as the whole environment, sends req, and reads
// one response. The process must exit 0 after writing exactly one line.
// The timeout bounds the whole exchange.
func Call(ctx context.Context, argv []string, env []string, timeout time.Duration, req Request) (Response, error) {
	if len(argv) == 0 {
		return Response{}, errors.New("protocol: empty command")
	}
	req.Protocol = Version
	if req.ID == "" || req.Method == "" {
		return Response{}, errors.New("protocol: request needs an id and a method")
	}
	line, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(append(line, '\n'))
	var stdout, stderr limited
	stdout.max, stderr.max = MaxResponse+1, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return Response{}, fmt.Errorf("protocol: %s did not answer within %s", argv[0], timeout)
	}
	if runErr != nil {
		return Response{}, fmt.Errorf("protocol: %s failed: %v", argv[0], runErr)
	}
	if stdout.over {
		return Response{}, fmt.Errorf("%w: response exceeds %d bytes", ErrViolation, MaxResponse)
	}
	return decode(stdout.buf.Bytes(), req.ID)
}

func decode(out []byte, id string) (Response, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 4096), MaxResponse+1)
	var lines [][]byte
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if len(lines) != 1 || !bytes.HasSuffix(out, []byte("\n")) {
		return Response{}, fmt.Errorf("%w: expected exactly one newline-terminated line, got %d", ErrViolation, len(lines))
	}
	dec := json.NewDecoder(bytes.NewReader(lines[0]))
	dec.DisallowUnknownFields()
	var r Response
	if err := dec.Decode(&r); err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrViolation, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Response{}, fmt.Errorf("%w: trailing data after the response object", ErrViolation)
	}
	switch {
	case r.Protocol != Version:
		return Response{}, fmt.Errorf("%w: protocol %q, want %q", ErrViolation, r.Protocol, Version)
	case r.ID != id:
		return Response{}, fmt.Errorf("%w: response id %q, request id %q", ErrViolation, r.ID, id)
	case string(r.Result) == "null":
		return Response{}, fmt.Errorf("%w: result must not be null", ErrViolation)
	case (r.Result == nil) == (r.Error == nil):
		return Response{}, fmt.Errorf("%w: exactly one of result and error is required", ErrViolation)
	case r.Error != nil && (r.Error.Code == "" || r.Error.Message == ""):
		return Response{}, fmt.Errorf("%w: error needs a code and a message", ErrViolation)
	}
	return r, nil
}

// limited keeps the first max bytes and records that more arrived.
type limited struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
			l.over = true
		} else {
			l.buf.Write(p)
		}
	} else if len(p) > 0 {
		l.over = true
	}
	return len(p), nil
}

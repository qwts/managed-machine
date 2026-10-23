package protocol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a misbehaving extension selected by MM_PROTOCOL_FIXTURE.
func TestMain(m *testing.M) {
	mode := os.Getenv("MM_PROTOCOL_FIXTURE")
	if mode == "" {
		os.Exit(m.Run())
	}
	ok := `{"protocol":"mm-extension-1","id":"r1","result":{"ok":true}}`
	switch mode {
	case "ok":
		fmt.Println(ok)
	case "fault":
		fmt.Println(`{"protocol":"mm-extension-1","id":"r1","error":{"code":"unsupported","message":"no such method"}}`)
	case "text":
		fmt.Println("installing...")
	case "two-lines":
		fmt.Println(ok)
		fmt.Println(ok)
	case "no-newline":
		fmt.Print(ok)
	case "wrong-id":
		fmt.Println(strings.Replace(ok, `"r1"`, `"r2"`, 1))
	case "wrong-protocol":
		fmt.Println(strings.Replace(ok, "mm-extension-1", "mm-extension-2", 1))
	case "both":
		fmt.Println(`{"protocol":"mm-extension-1","id":"r1","result":{},"error":{"code":"x","message":"y"}}`)
	case "null-result":
		fmt.Println(`{"protocol":"mm-extension-1","id":"r1","result":null}`)
	case "extra-field":
		fmt.Println(`{"protocol":"mm-extension-1","id":"r1","result":{},"shell":"rm"}`)
	case "trailing":
		fmt.Println(ok + ` {}`)
	case "exit-1":
		fmt.Println(ok)
		os.Exit(1)
	case "huge":
		fmt.Println(`{"protocol":"mm-extension-1","id":"r1","result":"` + strings.Repeat("x", MaxResponse) + `"}`)
	case "hang":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func call(t *testing.T, mode string, timeout time.Duration) (Response, error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"MM_PROTOCOL_FIXTURE=" + mode}
	if v := os.Getenv("SYSTEMROOT"); v != "" {
		env = append(env, "SYSTEMROOT="+v)
	}
	return Call(context.Background(), []string{self}, env, timeout, Request{ID: "r1", Method: "describe"})
}

func TestWellFormed(t *testing.T) {
	r, err := call(t, "ok", 10*time.Second)
	if err != nil || string(r.Result) != `{"ok":true}` {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = call(t, "fault", 10*time.Second)
	if err != nil || r.Error == nil || r.Error.Code != "unsupported" {
		t.Fatalf("fault: %+v %v", r, err)
	}
}

func TestViolations(t *testing.T) {
	for _, mode := range []string{"text", "two-lines", "no-newline", "wrong-id", "wrong-protocol", "both", "null-result", "extra-field", "trailing", "huge"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := call(t, mode, 10*time.Second); !errors.Is(err, ErrViolation) {
				t.Errorf("got %v, want a protocol violation", err)
			}
		})
	}
}

func TestProcessFailures(t *testing.T) {
	if _, err := call(t, "exit-1", 10*time.Second); err == nil || errors.Is(err, ErrViolation) {
		t.Errorf("nonzero exit: %v", err)
	}
	if _, err := call(t, "hang", 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("timeout: %v", err)
	}
}

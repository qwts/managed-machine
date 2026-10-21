// Disposable portability fixture. Not the managed-machine CLI or plugin protocol.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

type request struct {
	Value string `json:"value"`
	Fail  bool   `json:"fail"`
}

type response struct {
	Value string `json:"value"`
	State string `json:"state"`
}

func emit(w io.Writer, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func diagnostic(id string, code int) int {
	emit(os.Stderr, struct {
		Diagnostic string `json:"diagnostic"`
	}{id})
	return code
}

func run(args []string) int {
	if len(args) == 0 {
		return diagnostic("PROOF_USAGE", 2)
	}
	switch args[0] {
	case "launch":
		if len(args) != 1 {
			return diagnostic("PROOF_USAGE", 2)
		}
		native, emulated, err := nativeArchitecture()
		if err != nil {
			return diagnostic("PROOF_NATIVE_ARCH", 1)
		}
		emit(os.Stdout, struct {
			Fixture    bool   `json:"fixture"`
			OS         string `json:"os"`
			Arch       string `json:"arch"`
			NativeArch string `json:"native_arch"`
			Emulated   bool   `json:"emulated"`
		}{true, runtime.GOOS, runtime.GOARCH, native, emulated})
		return 0
	case "path":
		if len(args) != 2 {
			return diagnostic("PROOF_USAGE", 2)
		}
		content, err := os.ReadFile(args[1])
		if err != nil {
			return diagnostic("PROOF_PATH_READ", 1)
		}
		emit(os.Stdout, struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}{args[1], string(content)})
		return 0
	case "exchange":
		if len(args) != 2 || (args[1] != "success" && args[1] != "failure") {
			return diagnostic("PROOF_USAGE", 2)
		}
		self, err := os.Executable()
		if err != nil {
			return diagnostic("PROOF_EXECUTABLE", 1)
		}
		input, err := json.Marshal(request{Value: "space café 雪", Fail: args[1] == "failure"})
		if err != nil {
			return diagnostic("PROOF_ENCODING", 1)
		}
		cmd := exec.Command(self, "_child") // No shell; fixture process only.
		cmd.Stdin = bytes.NewReader(input)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err = cmd.Run()
		exit := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				exit = e.ExitCode()
			} else {
				return diagnostic("PROOF_CHILD_START", 1)
			}
		}
		payload := out.Bytes()
		if exit != 0 {
			payload = stderr.Bytes()
		}
		var result response
		if json.Unmarshal(payload, &result) != nil {
			return diagnostic("PROOF_CHILD_JSON", 1)
		}
		emit(os.Stdout, struct {
			ChildExit int      `json:"child_exit"`
			Response  response `json:"response"`
		}{exit, result})
		if exit != 0 {
			return diagnostic("PROOF_CHILD_FAILURE", 1)
		}
		return 0
	case "legacy-unsupported":
		// Explicit route rejection occurs before any exec call.
		return diagnostic("PROOF_UNSUPPORTED_LEGACY_ROUTE", 2)
	case "new-core-failure":
		// Deliberately no catch-and-retry or legacy fallback.
		return diagnostic("PROOF_NEW_CORE_FAILURE", 1)
	case "_child":
		var req request
		if json.NewDecoder(os.Stdin).Decode(&req) != nil {
			return diagnostic("PROOF_CHILD_INPUT", 2)
		}
		if req.Fail {
			emit(os.Stderr, response{req.Value, "failed"})
			return 7
		}
		emit(os.Stdout, response{req.Value, "ok"})
		return 0
	case "_legacy-fixture":
		// Positive control for the harness marker; never called by normal routes.
		path := os.Getenv("PROOF_LEGACY_MARKER")
		if path == "" {
			return diagnostic("PROOF_MARKER_MISSING", 2)
		}
		if os.WriteFile(path, []byte("fixture invoked\n"), 0600) != nil {
			return diagnostic("PROOF_MARKER_WRITE", 1)
		}
		return 0
	default:
		return diagnostic("PROOF_UNKNOWN_ROUTE", 2)
	}
}

func main() { os.Exit(run(os.Args[1:])) }

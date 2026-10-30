package daemon

import (
	"testing"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/diag"
)

func owner(root string) Deps {
	return Deps{
		GOOS: "darwin",
		Caller: func() (Caller, error) {
			return Caller{Username: "admin", UID: 501, Home: root, RegisteredHome: root}, nil
		},
		Root: func() (string, error) { return root, nil },
		Executable: func() (string, error) {
			return "/opt/managed-machine/bin/managed-machine", nil
		},
	}
}

func TestDaemonInstallAndStatus(t *testing.T) {
	root := t.TempDir()
	res := run(owner(root), cli.Context{}, []string{"install"})
	if res.Diagnostic != nil {
		t.Fatal(res.Diagnostic.Message)
	}
	res = run(owner(root), cli.Context{}, []string{"status"})
	if res.Diagnostic != nil || len(res.Records) != 1 {
		t.Fatalf("status %#v", res)
	}
	got, ok := res.Records[0].Get("state")
	if !ok || got != "installed" {
		t.Fatalf("state %v", got)
	}
	res = run(owner(root), cli.Context{}, []string{"uninstall"})
	if res.Diagnostic != nil {
		t.Fatal(res.Diagnostic.Message)
	}
}

func TestDaemonRefusesAgent(t *testing.T) {
	root := t.TempDir()
	deps := owner(root)
	deps.Caller = func() (Caller, error) {
		return Caller{Username: "qwts-cursor-agent", UID: 502, Home: root, RegisteredHome: root}, nil
	}
	res := run(deps, cli.Context{}, []string{"install"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.DaemonCallerRefused {
		t.Fatalf("diagnostic %#v", res.Diagnostic)
	}
}

func TestDaemonUsage(t *testing.T) {
	res := run(owner(t.TempDir()), cli.Context{}, []string{"reboot"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.Usage {
		t.Fatalf("diagnostic %#v", res.Diagnostic)
	}
}

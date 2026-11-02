package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	deps.Caller = func() (Caller, error) {
		return Caller{Username: "user", UID: 502, Home: root, RegisteredHome: root, PrefixOwner: "admin"}, nil
	}
	res = run(deps, cli.Context{}, []string{"install"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.DaemonCallerRefused {
		t.Fatalf("non-owner diagnostic %#v", res.Diagnostic)
	}
}

func TestInstallActivates(t *testing.T) {
	root := t.TempDir()
	deps := owner(root)
	var got string
	deps.Activate = func(path string) error {
		got = path
		return nil
	}
	res := run(deps, cli.Context{}, []string{"install"})
	if res.Diagnostic != nil {
		t.Fatal(res.Diagnostic.Message)
	}
	if got == "" {
		t.Fatal("install did not activate the unit")
	}
}

func TestRefusedInstallDoesNotActivate(t *testing.T) {
	root := t.TempDir()
	deps := owner(root)
	deps.Caller = func() (Caller, error) {
		return Caller{Username: "qwts-cursor-agent", UID: 502, Home: root, RegisteredHome: root}, nil
	}
	deps.Activate = func(string) error {
		t.Fatal("refused install activated the unit")
		return nil
	}
	res := run(deps, cli.Context{}, []string{"install"})
	if res.Diagnostic == nil {
		t.Fatal("expected a refusal")
	}
}

func TestDaemonUsage(t *testing.T) {
	res := run(owner(t.TempDir()), cli.Context{}, []string{"reboot"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.Usage {
		t.Fatalf("diagnostic %#v", res.Diagnostic)
	}
}

func TestDaemonPairAndUnpair(t *testing.T) {
	root := t.TempDir()
	res := run(owner(root), cli.Context{}, []string{"pair"})
	if res.Diagnostic != nil {
		t.Fatal(res.Diagnostic.Message)
	}
	dir := stateDir(root)
	pairing, err := loadPairing(dir)
	if err != nil || len(pairing.Terminal) == 0 {
		t.Fatalf("paired terminal key %v %v", pairing, err)
	}
	res = run(owner(root), cli.Context{}, []string{"unpair"})
	if res.Diagnostic != nil {
		t.Fatal(res.Diagnostic.Message)
	}
	pairing, err = loadPairing(dir)
	if err != nil || len(pairing.Terminal) != 0 {
		t.Fatalf("unpaired terminal key %v %v", pairing, err)
	}
}

func TestFailedActivationLeavesNoUnitBehind(t *testing.T) {
	root := t.TempDir()
	deps := owner(root)
	deps.GOOS = "linux"
	deps.Activate = func(string) error {
		return errors.New("Failed to connect to bus: No medium found")
	}
	res := run(deps, cli.Context{}, []string{"install"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.DaemonServiceFailed {
		t.Fatalf("diagnostic %#v", res.Diagnostic)
	}
	if !strings.Contains(res.Diagnostic.Message, "No medium found") {
		t.Fatalf("message %q", res.Diagnostic.Message)
	}
	res = run(deps, cli.Context{}, []string{"status"})
	if got, _ := res.Records[0].Get("state"); got != "absent" {
		t.Fatalf("status after a refused activation: %v", got)
	}
}

func TestFailedActivationRestoresThePreviousUnit(t *testing.T) {
	root := t.TempDir()
	deps := owner(root)
	deps.GOOS = "linux"
	path, err := unitPath("linux", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := []byte("[Service]\nExecStart=/old/managed-machine daemon run\n")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	deps.Activate = func(string) error { return errors.New("unit refused") }
	var reloadSaw []byte
	deps.Reload = func() error {
		reloadSaw, _ = os.ReadFile(path)
		return nil
	}
	res := run(deps, cli.Context{}, []string{"install"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.DaemonServiceFailed {
		t.Fatalf("diagnostic %#v", res.Diagnostic)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("unit after a refused activation: %q %v", got, err)
	}
	if !bytes.Equal(reloadSaw, old) {
		t.Fatalf("service manager was not reloaded with the restored unit: %q", reloadSaw)
	}
}

func TestServiceFailureIsNotACallerRefusal(t *testing.T) {
	root := t.TempDir()
	deps := owner(root)
	deps.Deactivate = func(string) error { return errors.New("launchctl: input/output error") }
	res := run(deps, cli.Context{}, []string{"uninstall"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.DaemonServiceFailed {
		t.Fatalf("diagnostic %#v", res.Diagnostic)
	}
}

func TestFailedReloadAfterRestoreIsReported(t *testing.T) {
	root := t.TempDir()
	deps := owner(root)
	deps.GOOS = "linux"
	deps.Activate = func(string) error { return errors.New("enable refused") }
	deps.Reload = func() error { return errors.New("reload refused") }
	res := run(deps, cli.Context{}, []string{"install"})
	if res.Diagnostic == nil || res.Diagnostic.ID != diag.DaemonServiceFailed ||
		!strings.Contains(res.Diagnostic.Message, "enable refused") || !strings.Contains(res.Diagnostic.Message, "reload refused") {
		t.Fatalf("diagnostic %#v", res.Diagnostic)
	}
}

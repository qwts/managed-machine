package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/output"
)

// Deps is the testable surface of daemon install, uninstall, run, and status.
type Deps struct {
	command.Deps
	GOOS       string
	Executable func() (string, error)
	Caller     func() (Caller, error)
	Root       func() (string, error)
	Activate   func(path string) error
	Deactivate func(path string) error
}

// Command is `managed-machine daemon <run|install|uninstall|status|pair|unpair>`.
func Command(deps Deps) cli.Command {
	return cli.Command{
		Name:    "daemon",
		Summary: "install and run the per-user agent-machine executor",
		Usage: `managed-machine daemon run|install|uninstall|status|pair|unpair
agent-machine daemon run|install|uninstall|status|pair|unpair

install and uninstall write a per-user service unit for the prefix owner.
pair and unpair manage the Terminal --update thin client key.
They refuse root, agent accounts, agent sessions, and a foreign home.
The unit is a LaunchAgent, a systemd user unit, or a Windows service
description. Nothing is registered with a system service manager.`,
		Run: func(ctx cli.Context, args []string) cli.Result {
			return run(deps, ctx, args)
		},
	}
}

func run(deps Deps, ctx cli.Context, args []string) cli.Result {
	var verb string
	if d := command.Parse(args, command.Spec{Positional: &verb, PositionalName: "daemon verb"}); d != nil {
		return cli.Result{Diagnostic: d}
	}
	switch verb {
	case "run", "install", "uninstall", "status", "pair", "unpair":
	default:
		return cli.Result{Diagnostic: command.UsageErr("daemon verb must be run, install, uninstall, status, pair, or unpair")}
	}
	goos := deps.GOOS
	if goos == "" {
		goos = hostOS()
	}
	if verb != "status" {
		caller, err := deps.caller()
		if err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		var msg string
		if verb == "pair" || verb == "unpair" {
			msg = caller.RefuseClient()
		} else {
			msg = caller.Refuse()
		}
		if msg != "" {
			d := diag.New(diag.DaemonCallerRefused, msg)
			return cli.Result{Diagnostic: &d}
		}
	}
	root, err := deps.root()
	if err != nil {
		d := diag.New(diag.DaemonCallerRefused, err.Error())
		return cli.Result{Diagnostic: &d}
	}
	switch verb {
	case "status":
		dir := stateDir(root)
		ensureUserChannelLink(root, dir)
		on, err := Installed(goos, root)
		if err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		if !on && isSharedPrefixDir(dir) {
			if _, err := os.Stat(filepath.Join(dir, "daemon.pub")); err == nil {
				on = true
			} else if _, err := os.Stat(filepath.Join(dir, "daemon.sock")); err == nil {
				on = true
			}
		}
		state := "absent"
		if on {
			state = "installed"
		}
		rec := output.New().Set("service", "agent-machine").Set("scope", "user").Set("state", state).Set("os", goos)
		return cli.Result{Records: []*output.Record{rec}}
	case "install":
		exec, err := deps.executable(ctx)
		if err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		dir := stateDir(root)
		if _, err := ensureStateDirAndPeers(dir); err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		ensureUserChannelLink(root, dir)
		path, err := Install(goos, root, exec, "")
		if err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		if deps.Activate != nil {
			if err := deps.Activate(path); err != nil {
				d := diag.New(diag.DaemonCallerRefused, err.Error())
				return cli.Result{Diagnostic: &d}
			}
		}
		rec := output.New().Set("service", "agent-machine").Set("scope", "user").Set("state", "installed").Set("path", path)
		return cli.Result{Records: []*output.Record{rec}}
	case "uninstall":
		if path, err := unitPath(goos, root); err == nil && deps.Deactivate != nil {
			if err := deps.Deactivate(path); err != nil {
				d := diag.New(diag.DaemonCallerRefused, err.Error())
				return cli.Result{Diagnostic: &d}
			}
		}
		if err := Uninstall(goos, root); err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		if dir := stateDir(root); isSharedPrefixDir(dir) {
			_ = os.Remove(filepath.Join(dir, "daemon.sock"))
			_ = os.Remove(filepath.Join(dir, "daemon.pub"))
		}
		rec := output.New().Set("service", "agent-machine").Set("scope", "user").Set("state", "absent")
		return cli.Result{Records: []*output.Record{rec}}
	case "run":
		if err := Run(context.Background(), root); err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		rec := output.New().Set("service", "agent-machine").Set("scope", "user").Set("state", "stopped")
		return cli.Result{Records: []*output.Record{rec}}
	case "pair":
		path, err := PairTerminal(root)
		if err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		rec := output.New().Set("service", "agent-machine").Set("client", "terminal").Set("state", "paired").Set("path", path)
		return cli.Result{Records: []*output.Record{rec}}
	case "unpair":
		if err := UnpairTerminal(root); err != nil {
			d := diag.New(diag.DaemonCallerRefused, err.Error())
			return cli.Result{Diagnostic: &d}
		}
		rec := output.New().Set("service", "agent-machine").Set("client", "terminal").Set("state", "revoked")
		return cli.Result{Records: []*output.Record{rec}}
	default:
		return cli.Result{Diagnostic: command.UsageErr("daemon verb must be run, install, uninstall, status, pair, or unpair")}
	}
}

func (d Deps) caller() (Caller, error) {
	if d.Caller != nil {
		return d.Caller()
	}
	return Caller{}, fmt.Errorf("daemon caller is not configured")
}

func (d Deps) root() (string, error) {
	if d.Root != nil {
		return d.Root()
	}
	return "", fmt.Errorf("daemon service root is not configured")
}

func (d Deps) executable(ctx cli.Context) (string, error) {
	if d.Executable != nil {
		return d.Executable()
	}
	if ctx.Executable != nil {
		return ctx.Executable()
	}
	return "", fmt.Errorf("daemon executable is not configured")
}

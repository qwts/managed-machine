package daemon

import (
	"context"
	"fmt"

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
		if msg := caller.Refuse(); msg != "" {
			d := diag.New(diag.DaemonCallerRefused, msg)
			return cli.Result{Diagnostic: &d}
		}
	}
	root, err := deps.root()
	if err != nil {
		return serviceFailed(err)
	}
	switch verb {
	case "status":
		on, err := Installed(goos, root)
		if err != nil {
			return serviceFailed(err)
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
			return serviceFailed(err)
		}
		previous, err := readUnit(goos, root)
		if err != nil {
			return serviceFailed(err)
		}
		path, err := Install(goos, root, exec, "")
		if err != nil {
			return serviceFailed(err)
		}
		if deps.Activate != nil {
			if err := deps.Activate(path); err != nil {
				if rerr := restoreUnit(path, previous); rerr != nil {
					return serviceFailed(fmt.Errorf("%v; restoring %s: %v", err, path, rerr))
				}
				return serviceFailed(err)
			}
		}
		rec := output.New().Set("service", "agent-machine").Set("scope", "user").Set("state", "installed").Set("path", path)
		return cli.Result{Records: []*output.Record{rec}}
	case "uninstall":
		if path, err := unitPath(goos, root); err == nil && deps.Deactivate != nil {
			if err := deps.Deactivate(path); err != nil {
				return serviceFailed(err)
			}
		}
		if err := Uninstall(goos, root); err != nil {
			return serviceFailed(err)
		}
		rec := output.New().Set("service", "agent-machine").Set("scope", "user").Set("state", "absent")
		return cli.Result{Records: []*output.Record{rec}}
	case "run":
		if err := Run(context.Background(), root); err != nil {
			return serviceFailed(err)
		}
		rec := output.New().Set("service", "agent-machine").Set("scope", "user").Set("state", "stopped")
		return cli.Result{Records: []*output.Record{rec}}
	case "pair":
		path, err := PairTerminal(root)
		if err != nil {
			return serviceFailed(err)
		}
		rec := output.New().Set("service", "agent-machine").Set("client", "terminal").Set("state", "paired").Set("path", path)
		return cli.Result{Records: []*output.Record{rec}}
	case "unpair":
		if err := UnpairTerminal(root); err != nil {
			return serviceFailed(err)
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

func serviceFailed(err error) cli.Result {
	d := diag.New(diag.DaemonServiceFailed, err.Error())
	return cli.Result{Diagnostic: &d}
}

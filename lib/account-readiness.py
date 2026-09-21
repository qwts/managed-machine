#!/usr/bin/env python3
import argparse
import json
import os
from pathlib import Path
import pwd
import re
import subprocess
import sys

NAME = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_.+-]*\Z")
DAEMON_IDS = {"daemon.supervisor", "daemon.health"}
BASE_PATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"


def run(argv, env, cwd):
    try:
        return subprocess.run(argv, env=env, cwd=cwd, stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                              text=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired, UnicodeError):
        return subprocess.CompletedProcess(argv, 127, "", "")


def within(path, home):
    try:
        Path(path).resolve().relative_to(Path(home).resolve())
        return True
    except (ValueError, OSError, RuntimeError):
        return False


def load_json(path):
    with open(path, encoding="utf-8") as stream:
        return json.load(stream)


def catalog_app(path, harness):
    data = load_json(path)
    apps = data.get("apps") if isinstance(data, dict) else None
    if not isinstance(apps, list):
        raise ValueError()
    requested = "codex" if harness == "codex-cli" else harness
    preferred = harness + "-cli" if harness in {"kiro", "codex"} else harness
    matches = []
    preferred_matches = []
    for row in apps:
        if not isinstance(row, dict):
            raise ValueError()
        aliases = row.get("aliases", [])
        if not isinstance(aliases, list):
            raise ValueError()
        names = [row.get("name"), row.get("token"), *aliases]
        if preferred in names:
            preferred_matches.append(row)
        if harness in names or requested in names:
            matches.append(row)
    matches = preferred_matches or matches
    if len(matches) != 1:
        raise ValueError()
    row = matches[0]
    kind = row.get("kind")
    if not isinstance(kind, str):
        raise ValueError()
    command = row.get("command") or (kind if kind in {"opencode", "devin"} else None)
    if not isinstance(row.get("name"), str) or not NAME.fullmatch(row["name"]):
        raise ValueError()
    return kind, command


def diagnose(args, runner=run, identity=None, environ=None):
    checks = []

    def add(id, status, code, message, action="", **evidence):
        checks.append(dict(id=id, status=status, code=code, message=message,
                           action=action, evidence=evidence))

    def finish():
        statuses = {check["status"] for check in checks}
        status = ("not_ready" if "failed" in statuses else
                  "pending_user_action" if "pending_user_action" in statuses else "ready")
        return dict(schema_version=1, command="account-doctor", account=args.account,
                    home=args.home, harness=args.harness, ready=status == "ready",
                    status=status, checks=checks, vendor_sign_in="not_tested")

    if args.setup_results:
        try:
            outcomes = load_json(args.setup_results)
            actions = {
                "setup.config": "Refresh the bundled configuration as its owner, then rerun account setup.",
                "setup.shell": "Review target shell profile conflicts and ownership, then rerun account setup.",
                "setup.local-bin": "Resolve target command collisions or refresh the immutable local-bin bundle as admin.",
                "setup.zsh-functions": "Refresh the reviewed zsh-functions bundle/pin as its owner, then rerun account setup.",
                "setup.harness": "Resolve the selected harness catalog, installer, or admin prerequisite, then rerun account setup.",
                "setup.identity": "Run agent-bot doctor in the target account; use add-agent for missing approved profile/key seeding.",
            }
            allowed = set(actions)
            statuses = {"ready", "failed", "pending_user_action"}
            if not isinstance(outcomes, list):
                raise ValueError()
            seen = set()
            for item in outcomes:
                if (not isinstance(item, dict) or item.get("id") not in allowed or
                        item["id"] in seen or item.get("status") not in statuses or
                        not isinstance(item.get("evidence"), dict) or
                        type(item["evidence"].get("exit_code")) is not int):
                    raise ValueError()
                code = item["evidence"]["exit_code"]
                expected = "ready" if code == 0 else "pending_user_action" if code in (75, 76) else "failed"
                if item["status"] != expected:
                    raise ValueError()
                seen.add(item["id"])
            for item in outcomes:
                status = item["status"]
                add(item["id"], status, item["id"].replace(".", "-") + "-" + status.replace("_", "-"),
                    "Current " + item["id"].split(".")[1] + " setup step: " + status.replace("_", " ") + ".",
                    actions[item["id"]] if status != "ready" else "",
                    exit_code=item["evidence"]["exit_code"])
        except (OSError, ValueError, TypeError):
            add("setup.results", "failed", "setup-results-invalid",
                "Current setup outcomes could not be validated.", "Rerun account setup.")
    environ = os.environ if environ is None else environ
    home = Path(args.home)
    try:
        user = pwd.getpwuid(os.getuid()) if identity is None else identity
        valid = (user.pw_name == args.account and
                 os.path.abspath(user.pw_dir) == os.path.abspath(args.home) and
                 environ.get("HOME") == args.home and home.is_dir())
    except (KeyError, OSError):
        valid = False
    add("account.identity", "ready" if valid else "failed",
        "account-verified" if valid else "account-context-mismatch",
        "OS account and HOME verified." if valid else "OS account or HOME does not match the target.",
        "" if valid else "Run doctor as the target OS account with its own HOME.")
    if not valid:
        return finish()
    env = dict(HOME=args.home, USER=args.account, LOGNAME=args.account,
               SHELL=args.shell, PATH=BASE_PATH, LANG="C", LC_ALL="C")
    invalid_override = False
    for key in ("ZDOTDIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR"):
        if key not in environ:
            continue
        value = environ[key]
        if not value or not os.path.isabs(value) or not within(value, home):
            invalid_override = True
        else:
            env[key] = value
    if invalid_override:
        add("account.environment", "failed", "foreign-home-override",
            "An account directory override is not inside the target HOME.",
            "Remove foreign or relative directory overrides and rerun account setup.")
        return finish()
    local_bin = home / ".local/bin"
    diagnostic_env = dict(env, PATH=str(local_bin) + ":" + BASE_PATH)
    add("shell.local_bin", "ready" if local_bin.is_dir() else "failed",
        "local-bin-present" if local_bin.is_dir() else "local-bin-missing",
        "Account local executable directory checked.", "Run account setup if missing.")
    probe = runner([args.shell, "-lc", 'printf "\\0%s\\0" "${ZDOTDIR:-$HOME}"',
                    "account-readiness"], env, args.home)
    fields = probe.stdout.split("\0")
    zdotdir = fields[-2] if len(fields) >= 3 else ""
    profiles_ok = (probe.returncode == 0 and os.path.isabs(zdotdir) and
                   within(zdotdir, home) and
                   all((Path(zdotdir) / name).is_file() for name in (".zshenv", ".zprofile", ".zshrc")))
    add("shell.profiles", "ready" if profiles_ok else "failed",
        "profiles-present" if profiles_ok else "profiles-missing",
        "Fresh-shell ZDOTDIR and zsh profiles checked.",
        "Run account shell setup to repair target zsh profiles.")
    manifest = home / ".config/managed-machine/local-bin.manifest"
    try:
        values = dict(line.split("=", 1) for line in manifest.read_text().splitlines() if "=" in line)
        checkout = Path(values.get("checkout", ""))
        commit = values.get("commit", "")
        manifest_ok = (values.get("schema_version") == "1" and bool(values.get("ref")) and
                       re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", commit) is not None and
                       checkout.is_absolute() and checkout.is_dir() and
                       str(checkout.resolve()) == values.get("checkout") and
                       within(checkout, home) and checkout.resolve() != home.resolve())
    except (OSError, UnicodeError, ValueError):
        manifest_ok = False
    add("local_bin.manifest", "ready" if manifest_ok else "failed",
        "manifest-present" if manifest_ok else "manifest-missing",
        "Managed local-bin installation record checked.", "Run account local-bin setup.")
    pin_ok = False
    tracked = set()
    if manifest_ok:
        git_env = dict(env, GIT_TERMINAL_PROMPT="0", GIT_OPTIONAL_LOCKS="0",
                       GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null", GIT_NO_REPLACE_OBJECTS="1",
                       GIT_CONFIG_SYSTEM="/dev/null", GIT_ASKPASS="/usr/bin/false",
                       SSH_ASKPASS="/usr/bin/false")
        def git(*arguments):
            return runner(["/usr/bin/git", "-c", "core.fsmonitor=false", "-c",
                           "credential.helper=", "-c", "core.hooksPath=/dev/null",
                           "-C", str(checkout), *arguments], git_env, args.home)
        root = git("rev-parse", "--show-toplevel")
        head = git("rev-parse", "--verify", "HEAD^{commit}")
        state = git("status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
        files = git("ls-tree", "-r", "-z", commit)
        pin_ok = (all(probe.returncode == 0 for probe in (root, head, state, files)) and
                  root.stdout.strip() == str(checkout) and head.stdout.strip() == commit and
                  not state.stdout)
        if pin_ok:
            for entry in files.stdout.split("\0"):
                if "\t" in entry:
                    metadata, filename = entry.split("\t", 1)
                    if metadata.startswith("100755 blob "):
                        tracked.add(filename)
    add("local_bin.pin", "ready" if pin_ok else "failed",
        "pin-verified" if pin_ok else "pin-unverified",
        "Managed checkout commit and clean Git state checked.",
        "Run account local-bin setup to restore the pinned checkout.")
    broken = 0
    try:
        linked = (home / ".config/local-bin/linked-commands").read_text().splitlines()
        links_ok = bool(linked) and all(NAME.fullmatch(name) for name in linked)
        if links_ok:
            broken = sum(1 for name in linked if (local_bin / name).is_symlink() and not (local_bin / name).exists())
            links_ok = pin_ok and all((local_bin / name).is_symlink() and
                           within(local_bin / name, checkout) and
                           (local_bin / name).resolve().relative_to(checkout).as_posix() in tracked and
                           (local_bin / name).resolve().name == name and
                           os.access(local_bin / name, os.X_OK) for name in linked)
        links_ok = links_ok and broken == 0
    except (OSError, UnicodeError, RuntimeError):
        links_ok = False
    add("local_bin.links", "ready" if links_ok else "failed",
        "links-valid" if links_ok else "broken-managed-link",
        "Managed executable directory links checked.", "Run account local-bin setup to repair links.",
        broken_links=broken)
    try:
        kind, command = catalog_app(args.catalog, args.harness)
    except (OSError, ValueError, TypeError):
        kind, command = None, None
    # npm globals live in the admin-owned npm prefix (Homebrew node), the
    # same shared posture as homebrew/core formulae: the owner installs once
    # with the registry-pinned engine, and accounts resolve the shared
    # command without owning it.
    supported = kind in {"official-cli", "opencode", "devin", "brew-formula", "npm"}
    if kind in {"signed-cask", "cask", "vendor-dmg"} or (kind in {"brew-formula", "npm"} and not command):
        add("harness.catalog", "ready", "catalog-resolved", "Shared harness resolved from catalog.")
        add("harness.shared_install", "pending_user_action", "pending_admin",
            "Shared or desktop harness requires administrator and attended verification.",
            "Have an administrator install and verify the selected shared harness, then complete its attended account setup.")
    elif not supported or not isinstance(command, str) or not NAME.fullmatch(command):
        add("harness.catalog", "failed", "unsupported",
            "Catalog does not define a verifiable CLI for this harness.",
            "Provide a supported CLI catalog row with a safe command name.")
    else:
        add("harness.catalog", "ready", "catalog-resolved", "Harness CLI resolved from catalog.")
        if kind in {"brew-formula", "npm"}:
            add("harness.shared_install", "pending_user_action", "pending_admin",
                "Shared formula provenance requires administrator verification.",
                "Have an administrator verify the shared catalog installation.")
        for mode, flags in (("login_interactive", "-lic"), ("noninteractive", "-c")):
            result = runner([args.shell, flags,
                             'p=$(command -v -- "$1"); printf "\\0%s\\0%s\\0" "$p" "$PATH"',
                             "account-readiness", command], env, args.home)
            fields = result.stdout.split("\0")
            path = fields[-3] if len(fields) >= 4 else ""
            shell_path = fields[-2].split(":") if len(fields) >= 4 else []
            path_ok = result.returncode == 0 and str(local_bin) in shell_path
            add("shell." + mode + ".path", "ready" if path_ok else "failed",
                "local-bin-on-path" if path_ok else "local-bin-not-on-path",
                "Fresh shell account-local PATH checked.", "Repair target zsh PATH profiles.")
            found = result.returncode == 0 and os.path.isabs(path) and os.access(path, os.X_OK)
            in_home = found and (kind in {"brew-formula", "npm"} or within(path, home))
            try:
                owned = in_home and (kind in {"brew-formula", "npm"} or os.stat(path).st_uid == user.pw_uid)
            except OSError:
                owned = False
            code = ("cli-missing" if not found else "cli-wrong-home" if not in_home else
                    "cli-wrong-owner" if not owned else "cli-resolved")
            add("harness." + mode, "ready" if owned else "failed", code,
                "Fresh shell CLI resolution checked.", "Repair the target account CLI installation and shell PATH.")
            if owned:
                version_env = dict(env, PATH=fields[-2])
                result = runner([path, "--version"], version_env, args.home)
                add("harness." + mode + ".version", "ready" if result.returncode == 0 else "failed",
                    "cli-runnable" if result.returncode == 0 else "cli-version-failed",
                    "CLI version execution checked.", "Repair the target account CLI installation.")
    result = runner([args.agent_bot, "doctor", "--machine-only", "--json",
                     "--require-schema-version", "1"], diagnostic_env, args.home)
    try:
        report = json.loads(result.stdout)
        machine = report["machine"]
        upstream = machine["checks"]
        apps = machine["apps"]
        if (report.get("schema_version") != 1 or type(report.get("ready")) is not bool or
                not isinstance(upstream, list) or not isinstance(apps, list)):
            raise ValueError()
        target_apps = [app for app in apps if app["slug"] == args.account]
        if len(target_apps) != 1:
            raise ValueError()
        all_credentials = [check for app in apps for check in (app["credential"], app["live_mint"])]
        if (not all(isinstance(c, dict) and c.get("status") in
                    {"ready", "warning", "failed", "skipped", "not_applicable"}
                    for c in upstream + all_credentials) or
                len({app["slug"] for app in apps}) != len(apps)):
            raise ValueError()
        global_failed = any(c["status"] == "failed" for c in upstream + all_credentials)
        expected_status = "not_ready" if global_failed else "ready"
        if (report["ready"] != (not global_failed) or
                result.returncode != (1 if global_failed else 0) or
                machine.get("status", expected_status) != expected_status or
                report.get("scope", "machine") != "machine" or
                report.get("errors") or machine.get("errors") or report.get("error") or
                machine.get("error")):
            raise ValueError()
        worktree = report.get("worktree", {"status": "not_requested", "checks": []})
        if (not isinstance(worktree, dict) or worktree.get("status") != "not_requested" or
                worktree.get("checks") != []):
            raise ValueError()
        credentials = [target_apps[0]["credential"], target_apps[0]["live_mint"]]
        upstream = upstream + credentials
        incomplete = any(c["status"] not in {"ready", "failed"} for c in credentials)
        if incomplete:
            add("identity.live_verification", "failed", "identity-verification-incomplete",
                "Agent-bot credential or live mint verification was not completed.",
                "Run agent-bot doctor with live credential verification in the target account.")
        failures = [c for c in upstream if c["status"] == "failed"]
        daemon_failed = any(c.get("id") in DAEMON_IDS for c in failures)
        identity_failed = (any(c.get("id") not in DAEMON_IDS for c in failures) or
                           any(c["status"] == "failed" for c in credentials))
        add("identity.machine", "failed" if identity_failed else "ready",
            "identity-failed" if identity_failed else "identity-verified",
            "Agent-bot machine identity checks failed." if identity_failed else "Agent-bot machine identity checks passed.",
            "Run agent-bot doctor in the target account to repair identity wiring." if identity_failed else "")
        if daemon_failed:
            gui = runner(["/bin/launchctl", "print", "gui/" + str(user.pw_uid)], env, args.home)
            pending = sys.platform == "darwin" and gui.returncode != 0
            add("identity.daemon", "pending_user_action" if pending else "failed",
                "gui-login-required" if pending else "daemon-health-failed",
                "Identity daemon requires a GUI login." if pending else "Identity daemon health checks failed.",
                "Log into the target account graphically and rerun doctor." if pending else
                "Repair the identity daemon in the target account and rerun doctor.")
    except (ValueError, KeyError, TypeError):
        add("identity.machine", "failed", "doctor-invalid-json",
            "Agent-bot did not return a valid readiness report.",
            "Verify the installed agent-bot runtime and rerun doctor.")
    return finish()


def exit_code(report):
    return 0 if report["ready"] else 75 if report["status"] == "pending_user_action" else 1


def main():
    parser = argparse.ArgumentParser()
    for key in ("account", "home", "harness", "catalog", "agent-bot"):
        parser.add_argument("--" + key, required=True)
    parser.add_argument("--shell", default="/bin/zsh")
    parser.add_argument("--setup-results")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()
    if not NAME.fullmatch(args.account) or not NAME.fullmatch(args.harness):
        parser.error("account and harness must be safe names")
    if not all(os.path.isabs(value) for value in (args.home, args.catalog, args.agent_bot, args.shell)):
        parser.error("home, catalog, agent-bot, and shell must be absolute paths")
    if args.setup_results and not os.path.isabs(args.setup_results):
        parser.error("setup-results must be an absolute path")
    report = diagnose(args)
    if args.json:
        print(json.dumps(report, sort_keys=True))
    else:
        print("Account doctor: " + report["status"])
        for check in report["checks"]:
            print("{}: {} ({})".format(check["status"], check["message"], check["code"]))
            if check["status"] != "ready" and check["action"]:
                print("  " + check["action"])
        print("Vendor application sign-in: not tested.")
    return exit_code(report)


if __name__ == "__main__":
    sys.exit(main())

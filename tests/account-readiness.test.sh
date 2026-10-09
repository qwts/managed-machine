#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 - "$ROOT" <<'PY'
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("readiness", Path(sys.argv[1]) / "lib/account-readiness.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
sys.argv = sys.argv[:1]


class ReadinessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name) / "home"
        self.home.mkdir()
        self.bin = self.home / ".local/bin"
        self.bin.mkdir(parents=True)
        for name in (".zshenv", ".zprofile", ".zshrc"):
            (self.home / name).write_text("")
        self.cli = self.bin / "sample-cli"
        self.cli.write_text("#!/bin/sh\nexit 0\n")
        self.cli.chmod(0o700)
        self.checkout = self.home / ".local/share/local-bin"
        tool = self.checkout / "bin/sample-tool"
        tool.parent.mkdir(parents=True)
        tool.write_text("#!/bin/sh\nexit 0\n")
        tool.chmod(0o700)
        (self.bin / "sample-tool").symlink_to(tool)
        state = self.home / ".config/managed-machine"
        state.mkdir(parents=True)
        self.git_env = dict(HOME=str(self.home), PATH="/usr/bin:/bin", GIT_CONFIG_NOSYSTEM="1",
                            GIT_CONFIG_GLOBAL="/dev/null", GIT_AUTHOR_NAME="Fixture",
                            GIT_COMMITTER_NAME="Fixture", GIT_AUTHOR_EMAIL="fixture@example.invalid",
                            GIT_COMMITTER_EMAIL="fixture@example.invalid")
        self.git("init", "-q")
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")
        self.commit = self.git("rev-parse", "HEAD").strip()
        self.manifest = state / "local-bin.manifest"
        self.manifest.write_text("schema_version=1\nref=v1.0.0\ncommit=" + self.commit +
                                 "\ncheckout=" + str(self.checkout.resolve()) + "\n")
        linked = self.home / ".config/local-bin"
        linked.mkdir()
        (linked / "linked-commands").write_text("sample-tool\n")
        self.catalog = Path(self.temp.name) / "apps.json"
        self.row = dict(name="future-agent", kind="official-cli", command="sample-cli", aliases=["future"])
        self.save_catalog()
        self.args = argparse.Namespace(account="test-agent", home=str(self.home), harness="future",
                                       catalog=str(self.catalog),
                                       shell="/fake/zsh", setup_results=None, json=True)
        self.identity = SimpleNamespace(pw_name="test-agent", pw_dir=str(self.home), pw_uid=os.getuid())
        self.env = dict(HOME=str(self.home), GH_TOKEN="never-forward-this",
                        GITHUB_TOKEN="never-forward-this", AGENT_BOT_SUPERVISOR_SKIP_LOAD="1",
                        AGENT_BOT_HOME="/private-human")
        self.version_code = 0
        self.missing_mode = None
        self.resolved = str(self.cli)
        self.zdotdir = str(self.home)
        self.calls = []

    def git(self, *args):
        return subprocess.run(["/usr/bin/git", "-C", str(self.checkout), *args],
                              env=self.git_env, check=True, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, text=True).stdout

    def test_desktop_with_command_remains_attended(self):
        for kind in ("signed-cask", "cask", "vendor-dmg"):
            self.row["kind"] = kind
            self.save_catalog()
            self.assertEqual(m.exit_code(self.report()), 75)
            self.assertIn("pending_admin", self.codes())

    def test_pin_record_required(self):
        for text in ("schema_version=1\nref=v1\n", "schema_version=1\nref=v1\ncommit=\ncheckout=" + str(self.checkout)):
            self.manifest.write_text(text)
            self.assertFalse(self.report()["ready"])

    def test_wrong_commit_and_dirty_checkout_fail(self):
        original = self.manifest.read_text()
        self.manifest.write_text(original.replace(self.commit, "a" * 40))
        self.assertFalse(self.report()["ready"])
        self.manifest.write_text(original)
        tool = self.checkout / "bin/sample-tool"
        tool.write_text("changed")
        self.assertFalse(self.report()["ready"])
        self.git("restore", "bin/sample-tool")
        (self.checkout / "untracked").write_text("extra")
        self.assertFalse(self.report()["ready"])

    def test_foreign_or_noncanonical_checkout_record_fails(self):
        original = self.manifest.read_text()
        for path in (str(Path(self.temp.name)), str(self.checkout) + "/../local-bin", "relative"):
            self.manifest.write_text(original.replace(str(self.checkout.resolve()), path))
            self.assertFalse(self.report()["ready"])

    def test_managed_link_to_wrong_tracked_command_fails(self):
        other = self.checkout / "bin/other-tool"
        other.write_text("exit 0")
        other.chmod(0o700)
        self.git("add", ".")
        self.git("commit", "-qm", "second command")
        self.manifest.write_text(self.manifest.read_text().replace(self.commit, self.git("rev-parse", "HEAD").strip()))
        link = self.bin / "sample-tool"
        link.unlink()
        link.symlink_to(other)
        self.assertFalse(self.report()["ready"])

    def test_arbitrary_home_link_not_pinned(self):
        tool = self.home / "arbitrary"
        tool.write_text("exit 0")
        tool.chmod(0o700)
        link = self.bin / "sample-tool"
        link.unlink()
        link.symlink_to(tool)
        self.assertFalse(self.report()["ready"])

    def save_catalog(self):
        self.catalog.write_text(json.dumps(dict(apps=[self.row])))

    def runner(self, argv, env, cwd):
        self.calls.append(argv)
        self.assertEqual(env["HOME"], str(self.home))
        self.assertEqual(cwd, str(self.home))
        self.assertFalse(set(env) & {"GH_TOKEN", "GITHUB_TOKEN", "AGENT_BOT_SUPERVISOR_SKIP_LOAD",
                                     "AGENT_BOT_HOME"})
        for key in ("ZDOTDIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR"):
            if key in self.env:
                self.assertEqual(env.get(key), self.env[key])
        code, output = 0, ""
        if argv[0] == "/usr/bin/git":
            self.assertEqual(env["GIT_TERMINAL_PROMPT"], "0")
            self.assertEqual(env["GIT_OPTIONAL_LOCKS"], "0")
            self.assertEqual(env["GIT_CONFIG_GLOBAL"], "/dev/null")
            self.assertIn("core.fsmonitor=false", argv)
            self.assertFalse(set(argv) & {"fetch", "checkout", "reset", "clean"})
            return m.run(argv, env, cwd)
        if argv[0] == "/fake/zsh":
            if argv[1] == "-lc":
                output = "\0" + self.zdotdir + "\0"
            elif argv[1] == self.missing_mode:
                code = 1
            else:
                self.assertEqual(argv[-1], self.row["command"])
                self.assertNotIn(self.row["command"], argv[2])
                output = "ignored profile text\n\0" + self.resolved + "\0" + str(self.bin) + ":/usr/bin:/bin\0"
        else:
            self.assertEqual(argv, [self.resolved, "--version"])
            code = self.version_code
            output = "never-output-secret"
        return subprocess.CompletedProcess(argv, code, output, "never-output-secret")

    def report(self):
        with patch.object(m.sys, "platform", "darwin"):
            result = m.diagnose(self.args, self.runner, self.identity, self.env)
        self.assertNotIn("never-output", json.dumps(result))
        self.assertNotIn("never-forward", json.dumps(result))
        for check in result["checks"]:
            self.assertEqual(set(check), {"id", "status", "code", "message", "action", "evidence"})
        return result

    def codes(self):
        return {c["code"] for c in self.report()["checks"]}

    @unittest.skipUnless(Path('/bin/zsh').is_file(), 'requires zsh')
    def test_real_fresh_shells_and_shadowing_alias(self):
        self.args.shell = '/bin/zsh'
        (self.home / '.zshenv').write_text('export PATH="$HOME/.local/bin:$PATH"\n')
        self.cli.write_text('#!/bin/sh\n[ -z "${GH_TOKEN-}${GITHUB_TOKEN-}" ]\n')
        result = m.diagnose(self.args, identity=self.identity, environ=self.env)
        self.assertTrue(result['ready'], result)
        (self.home / '.zshrc').write_text("alias sample-cli='/private-human/sample-cli'\n")
        result = m.diagnose(self.args, identity=self.identity, environ=self.env)
        self.assertFalse(result['ready'])
        self.assertTrue(any(c['id'] == 'harness.login_interactive' and c['status'] == 'failed' for c in result['checks']))

    def test_identity_is_outside_managed_machine_readiness(self):
        result = self.report()
        self.assertFalse(any(check["id"].startswith("identity.") for check in result["checks"]))
        self.assertFalse(any("agent-bot" in " ".join(call) for call in self.calls))

    def test_ready_live_no_vendor_login_claim(self):
        result = self.report()
        self.assertTrue(result["ready"])
        self.assertEqual(result["vendor_sign_in"], "not_tested")
        self.assertEqual(m.exit_code(result), 0)
        self.assertEqual(sum(call[-1] == "--version" for call in self.calls), 2)

    def test_stale_account_marker_does_not_override_cli_failure(self):
        (self.home / ".config/managed-machine/account-setup.manifest").write_text("ready=true\n")
        self.cli.unlink()
        self.assertIn("cli-missing", self.codes())
        self.assertFalse(self.report()["ready"])

    def test_wrong_home_executable(self):
        outside = Path(self.temp.name) / "outside-cli"
        outside.write_text("#!/bin/sh\nexit 0\n")
        outside.chmod(0o700)
        self.resolved = str(outside)
        self.assertIn("cli-wrong-home", self.codes())
        self.assertFalse(any(call[-1] == "--version" for call in self.calls))

    def test_symlink_escape(self):
        self.cli.unlink()
        self.cli.symlink_to("/bin/sh")
        self.assertIn("cli-wrong-home", self.codes())

    def test_broken_link(self):
        (self.bin / "sample-tool").unlink()
        (self.bin / "sample-tool").symlink_to(self.home / "missing")
        self.assertIn("broken-managed-link", self.codes())

    def test_unrelated_broken_link_is_not_a_managed_failure(self):
        (self.bin / "custom-tool").symlink_to(self.home / "missing")
        self.assertTrue(self.report()["ready"])

    def test_missing_manifest_tool(self):
        (self.bin / "sample-tool").unlink()
        self.assertIn("broken-managed-link", self.codes())

    def test_version_fails(self):
        self.version_code = 1
        self.assertIn("cli-version-failed", self.codes())

    def test_fresh_shell_path_differs(self):
        self.missing_mode = "-c"
        result = self.report()
        checks = {c["id"]: c for c in result["checks"]}
        self.assertEqual(checks["harness.login_interactive"]["status"], "ready")
        self.assertEqual(checks["harness.noninteractive"]["status"], "failed")

    def test_catalog_invalid_or_unsupported(self):
        for row in (dict(name="future-agent", aliases=["future"]),
                    dict(name="future-agent", aliases=["future"], kind="official-cli"),
                    dict(name="future-agent", aliases=["future"], kind="official-cli", command="x;env"),
                    dict(name="future-agent", aliases=["future"], kind={"invalid": True}, command="sample-cli")):
            self.row = row
            self.save_catalog()
            self.assertIn("unsupported", self.codes())
        self.catalog.write_text("invalid")
        self.assertIn("unsupported", self.codes())

    def test_codex_cli_mapping(self):
        self.row.update(name="codex", aliases=[])
        self.args.harness = "codex-cli"
        self.save_catalog()
        self.assertTrue(self.report()["ready"])

    def test_kind_defined_commands(self):
        for kind in ("opencode", "devin"):
            self.catalog.write_text(json.dumps(dict(apps=[dict(name=kind, kind=kind)])))
            self.assertEqual(m.catalog_app(str(self.catalog), kind), (kind, kind))

    def test_shared_formula_pending_admin(self):
        self.row["kind"] = "brew-formula"
        self.save_catalog()
        self.assertIn("pending_admin", self.codes())
        self.assertEqual(m.exit_code(self.report()), 75)

    def test_shared_npm_pending_admin(self):
        self.row["kind"] = "npm"
        self.save_catalog()
        self.assertIn("pending_admin", self.codes())
        self.assertEqual(m.exit_code(self.report()), 75)

    def test_shared_npm_resolves_outside_home(self):
        # npm globals live in the admin-owned prefix, like homebrew/core
        # formulae: the command resolves outside the account home and is not
        # account-owned, yet counts as resolved once the owner installs it.
        self.row["kind"] = "npm"
        self.save_catalog()
        outside = Path(self.temp.name) / "shared-cli"
        outside.write_text("#!/bin/sh\nexit 0\n")
        outside.chmod(0o700)
        self.resolved = str(outside)
        self.assertIn("cli-resolved", self.codes())
        self.assertIn("pending_admin", self.codes())

    def test_shared_harness_without_cli_contract_is_pending(self):
        for kind in ('brew-formula', 'npm', 'signed-cask', 'vendor-dmg'):
            self.row['kind'] = kind
            self.row.pop('command', None)
            self.save_catalog()
            self.assertIn('pending_admin', self.codes())
            self.assertEqual(m.exit_code(self.report()), 75)

    def test_wrong_os_user_or_home_stops_probes(self):
        for field in ("pw_name", "pw_dir"):
            with patch.object(self.identity, field, "wrong"):
                self.assertEqual(m.exit_code(self.report()), 1)
        self.env["HOME"] = "/wrong-home"
        self.assertEqual(m.exit_code(self.report()), 1)
        self.assertEqual(self.calls, [])

    def test_zdotdir_from_fresh_shell_not_parent(self):
        target = self.home / "custom-zsh"
        target.mkdir()
        for name in (".zshenv", ".zprofile", ".zshrc"):
            (target / name).write_text("")
            (self.home / name).unlink()
        self.zdotdir = str(target)
        self.assertIn("profiles-present", self.codes())
        (target / ".zshenv").unlink()
        self.assertIn("profiles-missing", self.codes())

    def test_setup_result_marker_cannot_override_live_failure(self):
        results = Path(self.temp.name) / "results.json"
        results.write_text('{"ready":true}')
        self.args.setup_results = str(results)
        self.cli.unlink()
        self.assertFalse(self.report()["ready"])

    def test_existing_external_runtime_is_not_invoked_or_reported_ready(self):
        external = self.bin / "agent-bot"
        log = Path(self.temp.name) / "external-runtime.log"
        external.write_text("#!/bin/sh\necho invoked >>\"" + str(log) + "\"\n")
        external.chmod(0o700)
        result = self.report()
        self.assertTrue(result["ready"])
        self.assertFalse(log.exists())
        self.assertFalse(any(check["id"].startswith("identity.") for check in result["checks"]))

    def test_setup_outcomes_are_merged_without_raw_strings(self):
        results = Path(self.temp.name) / "results.json"
        self.args.setup_results = str(results)
        for status, code in (("ready", 0), ("failed", 1), ("pending_user_action", 75)):
            results.write_text(json.dumps([dict(id="setup.harness", status=status,
                code="never-output-secret", message="never-output-secret", action="never-output-secret",
                evidence=dict(exit_code=code, raw="never-output-secret"))]))
            report = self.report()
            self.assertEqual(m.exit_code(report), code)
            check = next(c for c in report["checks"] if c["id"] == "setup.harness")
            self.assertEqual(check["evidence"], dict(exit_code=code))
        self.cli.unlink()
        self.assertEqual(m.exit_code(self.report()), 1)

    def test_setup_zsh_functions_outcome_is_merged(self):
        results = Path(self.temp.name) / "results.json"
        self.args.setup_results = str(results)
        results.write_text(json.dumps([dict(id="setup.zsh-functions", status="ready",
            evidence=dict(exit_code=0))]))
        report = self.report()
        check = next(c for c in report["checks"] if c["id"] == "setup.zsh-functions")
        self.assertEqual(check["status"], "ready")
        self.assertNotIn("setup-results-invalid", self.codes())

    def test_invalid_setup_outcomes_fail_closed(self):
        results = Path(self.temp.name) / "results.json"
        self.args.setup_results = str(results)
        for text in ("invalid", "{}", "[null]", '[{"id":"setup.secret","status":"ready"}]',
                     '[{"id":"setup.shell","status":"ready","evidence":{"exit_code":true}}]'):
            results.write_text(text)
            self.assertIn("setup-results-invalid", self.codes())
        results.unlink()
        self.assertIn("setup-results-invalid", self.codes())

    def test_safe_target_local_environment_preserved(self):
        for key in ("ZDOTDIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR"):
            self.env[key] = str(self.home / key.lower())
        self.assertTrue(self.report()["ready"])

    def test_foreign_or_relative_environment_rejected_before_probes(self):
        for key in ("ZDOTDIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR"):
            for path in ("/private-human", "relative"):
                self.env[key] = path
                self.assertIn("foreign-home-override", self.codes())
                self.assertEqual(self.calls, [])
            del self.env[key]

    def test_wrong_uid_executable(self):
        with patch.object(self.identity, "pw_uid", os.getuid() + 1000):
            self.assertIn("cli-wrong-owner", self.codes())
        self.assertFalse(any(call[-1] == "--version" for call in self.calls))

    def test_cli_alias_preferred_over_desktop(self):
        for name in ("kiro", "codex"):
            self.catalog.write_text(json.dumps(dict(apps=[
                dict(name=name, kind="signed-cask"),
                dict(name=name + "-command", aliases=[name + "-cli"], kind="official-cli", command="safe-cli")
            ])))
            self.assertEqual(m.catalog_app(str(self.catalog), name), ("official-cli", "safe-cli"))


unittest.main()
PY

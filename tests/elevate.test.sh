#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_BIN="$TEST_DIR/bin"
OSA_LOG="$TEST_DIR/osascript.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_BIN"
cat >"$TEST_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$OSA_LOG"
[[ -z "${OSA_STDERR:-}" ]] || printf '%s\n' "$OSA_STDERR" >&2
exit "${OSA_EXIT:-0}"
EOF
cat >"$TEST_BIN/uname" <<'EOF'
#!/usr/bin/env bash
echo Darwin
EOF
chmod +x "$TEST_BIN"/*

export PATH="$TEST_BIN:$PATH" OSA_LOG
# shellcheck source=lib/elevate.sh
source "$ROOT/lib/elevate.sh"

# Success: the label and every argument reach osascript, quoted per-argument
# through AppleScript argv (no shell string is built by the caller).
unset MANAGED_MACHINE_BOOTSTRAP_MODE || true
elevate_run 'install the test payload' /usr/bin/true "arg with spaces" >"$TEST_DIR/ok.out"
grep -Fq 'install the test payload' "$OSA_LOG"
grep -Fq 'arg with spaces' "$OSA_LOG"
grep -Fq 'with administrator privileges' "$OSA_LOG"
grep -Fq 'Requesting administrator authorization to install the test payload' "$TEST_DIR/ok.out"

# Cancelled dialog: clean failure, clear message, nonzero exit.
if OSA_EXIT=1 elevate_run 'do the cancelled thing' /usr/bin/true >"$TEST_DIR/cancel.out" 2>&1; then
    echo 'expected cancelled authorization to fail' >&2
    exit 1
fi
grep -Fq 'cancelled or failed — did not do the cancelled thing' "$TEST_DIR/cancel.out"

# osascript's own wording for a dismissed dialog is still a cancellation.
if OSA_EXIT=1 OSA_STDERR='execution error: User canceled. (-128)' elevate_run 'do the dismissed thing' /usr/bin/true >"$TEST_DIR/dismiss.out" 2>&1; then
    echo 'expected dismissed authorization to fail' >&2
    exit 1
fi
grep -Fq 'cancelled or failed — did not do the dismissed thing' "$TEST_DIR/dismiss.out"

# The elevated command failing after the operator approved the dialog is
# reported as that command's failure, with its stderr, not as a cancellation.
if OSA_EXIT=1 OSA_STDERR='execution error: add-agent: home directory /Users/x for x is missing and could not be created (1)' elevate_run 'converge the x account' /usr/bin/false >"$TEST_DIR/script-fail.out" 2>&1; then
    echo 'expected a failing elevated command to fail' >&2
    exit 1
fi
grep -Fq 'the elevated step to converge the x account failed after authorization: add-agent: home directory /Users/x for x is missing and could not be created (1)' "$TEST_DIR/script-fail.out"
if grep -Fq 'cancelled or failed' "$TEST_DIR/script-fail.out"; then
    echo 'a failing elevated command must not be reported as a cancelled dialog' >&2
    exit 1
fi

# The elevated command's raw detail is left for callers to translate.
[[ "$ELEVATE_RUN_DETAIL" == *'home directory /Users/x for x is missing'* ]]
OSA_EXIT=0 elevate_run 'succeed again' /usr/bin/true >/dev/null
[[ -z "$ELEVATE_RUN_DETAIL" ]]

# A run that tracks declines (--update, #174): a cancelled dialog is
# remembered, a failed command after approval is not, and once declined no
# later step of the run opens another dialog.
export MANAGED_MACHINE_ELEVATION_DECLINED_FILE="$TEST_DIR/declined"
OSA_EXIT=1 OSA_STDERR='execution error: boom (1)' elevate_run 'fail after approval' /usr/bin/false >/dev/null 2>&1 || true
[[ ! -e "$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" ]]
OSA_EXIT=1 OSA_STDERR='execution error: User canceled. (-128)' elevate_run 'first ask' /usr/bin/true >/dev/null 2>&1 || true
[[ -e "$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" ]]
osa_calls="$(wc -l <"$OSA_LOG")"
rc=0
elevate_run 'second ask' /usr/bin/true >"$TEST_DIR/declined.out" 2>&1 || rc=$?
[[ "$rc" == 76 ]]
grep -Fq 'declined earlier in this run — not asking again to second ask' "$TEST_DIR/declined.out"
[[ "$(wc -l <"$OSA_LOG")" == "$osa_calls" ]]
! elevation_available
unset MANAGED_MACHINE_ELEVATION_DECLINED_FILE

# Agent-session detection mirrors the gh shim's markers: any harness marker,
# CODEX_* in the environment, or an agent account name; a plain human shell
# is none of those.
agent_env() { env -i HOME="$HOME" PATH="$PATH" "$@" bash -c 'source "$0"; managed_machine_agent_session' "$ROOT/lib/elevate.sh"; }
agent_env CLAUDECODE=1
agent_env QWEN_CODE=1
agent_env ANTIGRAVITY_AGENT=1
agent_env KIRO_AGENT=1
agent_env VIBE_AGENT=1
agent_env CURSOR_AGENT=1
agent_env COPILOT_AGENT=1
agent_env DEVIN_AGENT=1
agent_env WINDSURF_AGENT=1
agent_env MUSE_AGENT=1
agent_env CLAUDE_CODE_ENTRYPOINT=cli
agent_env AI_AGENT=claude-code_2-1-255_agent
agent_env GH_AGENT_APP=you-goose-agent
agent_env CODEX_SANDBOX=1
agent_env AGENT_BOT_ACCOUNT=you-goose-agent
if agent_env; then
    echo 'a plain shell must not read as an agent session' >&2
    exit 1
fi
if agent_env CLAUDECODE=0 AI_AGENT= AGENT_BOT_ACCOUNT=you; then
    echo 'unset or off markers must not read as an agent session' >&2
    exit 1
fi
if agent_env ANTIGRAVITY_AGENT=0; then
    echo 'ANTIGRAVITY_AGENT=0 must not read as an agent session' >&2
    exit 1
fi
if agent_env ANTIGRAVITY_AGENT=; then
    echo 'ANTIGRAVITY_AGENT= must not read as an agent session' >&2
    exit 1
fi
if agent_env OTHER_VAR=$'hello\nCURSOR_AGENT=1'; then
    echo 'multiline env var spoofing CURSOR_AGENT must not read as an agent session' >&2
    exit 1
fi
if env -i HOME="$HOME" PATH="$PATH" bash -c 'source "$0"; CODEX_BLOCK_BEGIN=1; managed_machine_agent_session' "$ROOT/lib/elevate.sh"; then
    echo 'unexported shell variable CODEX_BLOCK_BEGIN must not read as an agent session' >&2
    exit 1
fi
if ! env -i HOME="$HOME" PATH="$PATH" bash -c 'source "$0"; CODEX_SANDBOX=1 managed_machine_agent_session' "$ROOT/lib/elevate.sh"; then
    echo 'prefix-assigned CODEX_SANDBOX=1 must read as an agent session' >&2
    exit 1
fi
for proc_name in mirror-agent vibe-rs vibe-app-server; do
    cat >"$TEST_DIR/$proc_name" <<'EOF'
#!/bin/sh
"$@"
EOF
    chmod +x "$TEST_DIR/$proc_name"
    if ! env -i HOME="$HOME" PATH="$PATH" "$TEST_DIR/$proc_name" bash -c 'source "$0"; managed_machine_agent_session' "$ROOT/lib/elevate.sh"; then
        echo "$proc_name ancestor process without env markers must read as an agent session" >&2
        exit 1
    fi
done
if env -i HOME="$HOME" PATH="$PATH" bash -c 'source "$0"; managed_machine_agent_session' "$ROOT/lib/elevate.sh" mirror-agent vibe-rs; then
    echo 'mirror-agent or vibe-rs as a mere command argument must not read as an agent session' >&2
    exit 1
fi

# Noninteractive bootstrap never pops a dialog.
: >"$OSA_LOG"
if MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive elevate_run 'noninteractive thing' /usr/bin/true >"$TEST_DIR/nonint.out" 2>&1; then
    echo 'expected noninteractive elevation to fail' >&2
    exit 1
fi
grep -Fq 'authorization dialog unavailable' "$TEST_DIR/nonint.out"
[[ ! -s "$OSA_LOG" ]]

# A missing command is a usage error.
if elevate_run 'no command' >"$TEST_DIR/usage.out" 2>&1; then
    echo 'expected missing command to fail' >&2
    exit 1
fi
grep -Fq 'requires a command' "$TEST_DIR/usage.out"

# Same user: no dialog.
: >"$OSA_LOG"
elevate_as_user 'run as self' "$(id -un)" /usr/bin/true "keep me" >"$TEST_DIR/as-user.out"
[[ ! -s "$OSA_LOG" ]]

# Other user: drop privileges with an explicit PATH/HOME, not sudo -H.
: >"$OSA_LOG"
elevate_as_user 'run as other' otheradmin /opt/homebrew/bin/brew update >"$TEST_DIR/as-other.out"
grep -Fq '/usr/bin/sudo' "$OSA_LOG"
grep -Fq '/usr/bin/env' "$OSA_LOG"
grep -Fq 'PATH=/opt/homebrew/bin:/usr/local/bin:/usr/sbin:/usr/bin:/bin' "$OSA_LOG"
! grep -Fq -- '-H' "$OSA_LOG"

# elevate_as_user_with_github_auth: same user runs without dialog.
: >"$OSA_LOG"
elevate_as_user_with_github_auth 'run with gh auth self' "$(id -un)" /usr/bin/true "keep me" >"$TEST_DIR/gh-self.out"
[[ ! -s "$OSA_LOG" ]]

# Other user with gh token: passes token via file to brew-github-auth-run, never in argv.
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == auth && "$2" == token ]]; then
    echo 'gho_secretforwardtoken'
    exit 0
fi
exit 1
EOF
chmod +x "$TEST_BIN/gh"

: >"$OSA_LOG"
elevate_as_user_with_github_auth 'run with gh auth other' otheradmin git fetch --tags >"$TEST_DIR/gh-other.out"
grep -Fq 'brew-github-auth-run' "$OSA_LOG"
grep -Fq 'otheradmin' "$OSA_LOG"
grep -Fq 'git fetch --tags' "$OSA_LOG"
! grep -Fq 'gho_secretforwardtoken' "$OSA_LOG"

# Token file must be removed by RETURN trap after completion.
for token_cand in /tmp/mm-gh-token.*; do
    if [[ -f "$token_cand" ]] && grep -q 'gho_secretforwardtoken' "$token_cand" 2>/dev/null; then
        echo "token file was not cleaned up: $token_cand" >&2
        exit 1
    fi
done

# Other user without gh or git credential falls back to elevate_as_user.
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
cat >"$TEST_BIN/git" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == credential && "$2" == fill ]]; then
    exit 1
fi
exec /usr/bin/git "$@"
EOF
chmod +x "$TEST_BIN/git"

: >"$OSA_LOG"
elevate_as_user_with_github_auth 'run without gh auth' otheradmin git fetch --tags >"$TEST_DIR/gh-notoken.out"
grep -Fq '/usr/bin/sudo' "$OSA_LOG"
grep -Fq 'git fetch --tags' "$OSA_LOG"
! grep -Fq 'brew-github-auth-run' "$OSA_LOG"

# Approved plan execution (#174): multiple privileged actions execute under a
# single osascript call, brew actions drop to the prefix owner via sudo -u,
# matching elevate_run calls reuse the recorded status with zero extra dialogs,
# and any privileged step outside the approved plan is refused without opening
# a dialog.
PLAN_DIR="$TEST_DIR/plan-session"
mkdir -p "$PLAN_DIR"
chmod 700 "$PLAN_DIR"
PLAN_FILE="$PLAN_DIR/actions.tsv"
cat >"$PLAN_FILE" <<EOF
act-001	setup-taplo	brew-formula	otheradmin	homebrew/core/taplo	/opt/homebrew/Cellar/taplo	/opt/homebrew	install formula homebrew/core/taplo into /opt/homebrew/Cellar/taplo (brew as otheradmin)
act-002	setup-cursor	signed-cask	otheradmin	homebrew/cask/cursor	$TEST_DIR/Apps/Cursor.app	$TEST_DIR/Apps	install Cursor.app (homebrew/cask/cursor) into $TEST_DIR/Apps/Cursor.app (brew as otheradmin)
EOF
chmod 600 "$PLAN_FILE"

cat >"$TEST_BIN/brew" <<'EOF'
#!/usr/bin/env bash
printf 'brew:%s\n' "$*" >>"$TEST_DIR/plan-brew.log"
exit 0
EOF
chmod +x "$TEST_BIN/brew"
export TEST_DIR

: >"$OSA_LOG"
elevate_run_plan "bootstrap-test-plan-1" "apply approved bootstrap plan bootstrap-test-plan-1" "$PLAN_FILE" "$PLAN_DIR" >"$TEST_DIR/plan-run.out" 2>&1
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "1" ]]
grep -Fq 'apply approved bootstrap plan bootstrap-test-plan-1' "$OSA_LOG"
grep -Fq '/usr/bin/sudo -u otheradmin' "$OSA_LOG"
[[ "$(cat "$PLAN_DIR/status/act-001")" == "0" ]]
[[ "$(cat "$PLAN_DIR/status/act-002")" == "0" ]]

export MANAGED_MACHINE_APPROVED_PLAN_FILE="$PLAN_FILE"
export MANAGED_MACHINE_PLAN_SESSION_DIR="$PLAN_DIR"

# Matching in-plan steps do not invoke osascript again.
MANAGED_MACHINE_CURRENT_STEP="setup-taplo" elevate_run "run brew install homebrew/core/taplo" /usr/bin/sudo -u otheradmin /opt/homebrew/bin/brew install homebrew/core/taplo >/dev/null
MANAGED_MACHINE_CURRENT_STEP="setup-cursor" elevate_run "run brew install --cask homebrew/cask/cursor --appdir=$TEST_DIR/Apps" /usr/bin/sudo -u otheradmin /opt/homebrew/bin/brew install --cask homebrew/cask/cursor "--appdir=$TEST_DIR/Apps" >/dev/null
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "1" ]]

# A step outside the approved plan is refused without opening a dialog.
if MANAGED_MACHINE_CURRENT_STEP="setup-rogue" elevate_run "install unapproved package" /usr/bin/true >"$TEST_DIR/rogue.out" 2>&1; then
    echo 'expected privileged step outside the approved plan to be refused' >&2
    exit 1
fi
grep -Fq 'Error: refusing privileged step outside the approved plan: install unapproved package' "$TEST_DIR/rogue.out"
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "1" ]]

# Missing status file for a matched action fails closed instead of succeeding.
rm -f "$PLAN_DIR/status/act-001"
if MANAGED_MACHINE_CURRENT_STEP="setup-taplo" elevate_run "run brew install homebrew/core/taplo" /usr/bin/sudo -u otheradmin /opt/homebrew/bin/brew install homebrew/core/taplo >"$TEST_DIR/missing-status.out" 2>&1; then
    echo 'expected missing status file in approved plan to fail closed' >&2
    exit 1
fi
grep -Fq 'step did not record a completion status in the approved plan' "$TEST_DIR/missing-status.out"

unset MANAGED_MACHINE_APPROVED_PLAN_FILE MANAGED_MACHINE_PLAN_SESSION_DIR

# Tampering with the plan file after checksum calculation is refused by the runner.
if /bin/sh "$ROOT/lib/elevate-plan-run" "bootstrap-test-plan-1" "0000000000000000000000000000000000000000000000000000000000000000" "$PLAN_FILE" "$PLAN_DIR" "" >"$TEST_DIR/tamper.out" 2>&1; then
    echo 'expected checksum mismatch in elevate-plan-run to fail' >&2
    exit 1
fi
grep -Fq 'plan checksum mismatch' "$TEST_DIR/tamper.out"

# Root or UID 0 owner on a brew/npm plan row is refused by elevate-plan-run.
ROOT_PLAN_FILE="$PLAN_DIR/root-actions.tsv"
cat >"$ROOT_PLAN_FILE" <<EOF
act-001	setup-taplo	brew-formula	root	homebrew/core/taplo	/opt/homebrew/Cellar/taplo	/opt/homebrew	install taplo
EOF
root_sha="$(shasum -a 256 "$ROOT_PLAN_FILE" | awk '{print $1}')"
/bin/sh "$ROOT/lib/elevate-plan-run" "bootstrap-root-test" "$root_sha" "$ROOT_PLAN_FILE" "$PLAN_DIR" "" >"$TEST_DIR/root-owner.out" 2>&1 || true
[[ "$(cat "$PLAN_DIR/status/act-001")" != "0" ]]
grep -Fq 'refusing to run owner action as root' "$PLAN_DIR/detail/act-001"

# Failing vendor-dmg-retire-cask marks its companion vendor-dmg row failed without unbound-variable crash.
RETIRE_PLAN_DIR="$TEST_DIR/retire-plan-session"
mkdir -p "$RETIRE_PLAN_DIR"
chmod 700 "$RETIRE_PLAN_DIR"
RETIRE_PLAN_FILE="$RETIRE_PLAN_DIR/actions.tsv"
cat >"$RETIRE_PLAN_FILE" <<EOF
act-001	setup-muse-app	vendor-dmg-retire-cask	otheradmin	muse-app	$TEST_DIR/Apps/Muse.app	Muse.app|TEAMID1234|0|1.0.0	retire muse-app cask
act-002	setup-muse-app	vendor-dmg	root	https://example.com/Muse.dmg	$TEST_DIR/Apps/Muse.app	$TEST_DIR/Apps|TEAMID1234|0|1.0.0	install Muse.app
EOF
retire_sha="$(shasum -a 256 "$RETIRE_PLAN_FILE" | awk '{print $1}')"
/bin/sh "$ROOT/lib/elevate-plan-run" "bootstrap-retire-test" "$retire_sha" "$RETIRE_PLAN_FILE" "$RETIRE_PLAN_DIR" "" >"$TEST_DIR/retire-fail.out" 2>&1 || true
[[ "$(cat "$RETIRE_PLAN_DIR/status/act-001")" == "1" ]]
[[ "$(cat "$RETIRE_PLAN_DIR/status/act-002")" == "1" ]]
grep -Fq 'missing staged vendor DMG bundle for retire_cask action act-001' "$RETIRE_PLAN_DIR/detail/act-002"

echo 'elevate tests passed'


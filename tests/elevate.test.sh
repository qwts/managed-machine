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

# Agent-session detection mirrors the gh shim's markers: any harness marker,
# CODEX_* in the environment, or an agent account name; a plain human shell
# is none of those.
agent_env() { env -i HOME="$HOME" PATH="$PATH" "$@" bash -c 'source "$0"; managed_machine_agent_session' "$ROOT/lib/elevate.sh"; }
agent_env CLAUDECODE=1
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

echo 'elevate tests passed'

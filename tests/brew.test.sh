#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_BIN="$TEST_DIR/bin"
BREW_LOG="$TEST_DIR/brew.log"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_BIN"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"

user_in_admin_group "$(id -un)" || true
preferred="$(preferred_brew_owner)"
[[ -n "$preferred" ]]

cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
EOF
chmod +x "$TEST_BIN/brew"
PATH="$TEST_BIN:/usr/bin:/bin" brew_run install hello
grep -Fxq 'install hello' "$BREW_LOG"

# When brew runs as another user, the invoking gh token is forwarded via a
# 600 file so it never appears in osascript/sudo/env argv.
ELEVATE_LOG="$TEST_DIR/elevate.log"
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == auth && "$2" == token ]]; then
    echo 'gho_testtoken'
    exit 0
fi
exit 1
EOF
chmod +x "$TEST_BIN/gh"
brew_is_system_prefix() { return 0; }
brew_prefix_owner() { printf 'otheradmin\n'; }
elevate_run() {
    printf '%s\n' "$*" >>"$ELEVATE_LOG"
    local arg
    for arg in "$@"; do
        if [[ "$arg" == *gho_testtoken* ]]; then
            echo "token leaked into argv: $arg" >&2
            exit 1
        fi
        if [[ -f "$arg" ]] && grep -qxF 'gho_testtoken' "$arg" 2>/dev/null; then
            cp "$arg" "$TEST_DIR/captured-token"
        fi
    done
}
PATH="$TEST_BIN:/usr/bin:/bin" brew_run tap qwts/managed-machine
grep -Fq 'tap qwts/managed-machine' "$ELEVATE_LOG"
grep -Fq 'brew-github-auth-run' "$ELEVATE_LOG"
grep -Fq 'otheradmin' "$ELEVATE_LOG"
! grep -Fq 'gho_testtoken' "$ELEVATE_LOG"
[[ "$(cat "$TEST_DIR/captured-token")" == 'gho_testtoken' ]]

# gh supplies no token (the shim refusing an agent session the human's gh):
# brew still runs as the owner, with a note before the dialog in an agent
# session and none in a human shell (where an empty token is just "not
# logged in"). The stub echoes the refusal the installed agent-bot shim
# prints today, verbatim; managed-machine only sees the empty token and
# never matches this text. The wording is the shim's, from the ENG-0045
# directory rule, and changes when qwts/agent-bot-identity#187 ships the
# ENG-0339 account rule — update the echo then, nothing else here keys on it.
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == auth && "$2" == token ]]; then
    echo 'agent-bot: you-goose-agent is outside bot territory — refusing stock human gh' >&2
    exit 1
fi
exit 1
EOF
ELEVATE_RC=0
ELEVATE_STDERR=""
elevate_run() {
    printf '%s\n' "$*" >>"$ELEVATE_LOG"
    ELEVATE_RUN_DETAIL="$ELEVATE_STDERR"
    return "$ELEVATE_RC"
}
: >"$ELEVATE_LOG"
out="$(CLAUDECODE=1 PATH="$TEST_BIN:/usr/bin:/bin" brew_run update 2>&1)"
grep -Fq 'note: gh supplied no GitHub token to this agent session' <<<"$out"
grep -Fq 'sudo -u otheradmin' "$ELEVATE_LOG"
! grep -Fq 'brew-github-auth-run' "$ELEVATE_LOG"
: >"$ELEVATE_LOG"
out="$(env -u CLAUDECODE -u AI_AGENT -u CLAUDE_CODE_ENTRYPOINT PATH="$TEST_BIN:/usr/bin:/bin" bash -c 'source "$0/lib/brew.sh"; source "$0/lib/elevate.sh"; brew_is_system_prefix() { return 0; }; brew_prefix_owner() { printf "otheradmin\n"; }; elevate_run() { return 0; }; brew_run update' "$ROOT" 2>&1)"
! grep -Fq 'note: gh supplied no GitHub token' <<<"$out"

# The elevated brew failing on a private tap's authentication is explained
# as that, with the remedy for the session it happened in, and brew's own
# "does not exist! Run brew untap" advice is not the last word.
ELEVATE_RC=1
ELEVATE_STDERR="execution error: ==> Updating Homebrew... fatal: could not read Username for 'https://github.com': terminal prompts disabled
Error: qwts/homebrew-managed-machine does not exist! Run \`brew untap qwts/homebrew-managed-machine\` to remove it. (1)"
if out="$(CLAUDECODE=1 PATH="$TEST_BIN:/usr/bin:/bin" brew_run update 2>&1)"; then
    echo 'a failed elevated brew must fail brew_run' >&2
    exit 1
fi
grep -Fq 'GitHub authentication for a private tap failed while running brew as otheradmin' <<<"$out"
grep -Fq 'do not untap it' <<<"$out"
grep -Fq 'Run this from a human Terminal' <<<"$out"
if out="$(env -u CLAUDECODE -u AI_AGENT -u CLAUDE_CODE_ENTRYPOINT PATH="$TEST_BIN:/usr/bin:/bin" bash -c 'source "$0/lib/brew.sh"; source "$0/lib/elevate.sh"; brew_is_system_prefix() { return 0; }; brew_prefix_owner() { printf "otheradmin\n"; }; elevate_run() { ELEVATE_RUN_DETAIL="execution error: fatal: could not read Username for '"'"'https://github.com'"'"': terminal prompts disabled (1)"; return 1; }; brew_run update' "$ROOT" 2>&1)"; then
    echo 'a failed elevated brew must fail brew_run' >&2
    exit 1
fi
grep -Fq "Run 'gh auth login' as the invoking user" <<<"$out"
# An unrelated failure is left to the caller untranslated.
ELEVATE_STDERR="execution error: Error: No available formula with the name \"nope\" (1)"
if out="$(CLAUDECODE=1 PATH="$TEST_BIN:/usr/bin:/bin" brew_run install nope 2>&1)"; then
    echo 'a failed elevated brew must fail brew_run' >&2
    exit 1
fi
! grep -Fq 'private tap' <<<"$out"
# The token path translates the same failure (a token without access).
ELEVATE_RC=0
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == auth && "$2" == token ]] && { echo 'ghs_bottoken'; exit 0; }
exit 1
EOF
ELEVATE_RC=1
ELEVATE_STDERR="execution error: fatal: could not read Username for 'https://github.com': terminal prompts disabled (1)"
if out="$(CLAUDECODE=1 PATH="$TEST_BIN:/usr/bin:/bin" brew_run update 2>&1)"; then
    echo 'a failed elevated brew must fail brew_run' >&2
    exit 1
fi
grep -Fq 'GitHub authentication for a private tap failed' <<<"$out"

echo 'brew helper tests passed'

#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
BREW_PREFIX="$TEST_DIR/homebrew"
AGENT_BOT_LOG="$TEST_DIR/agent-bot.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$BREW_PREFIX/bin"
cat >"$TEST_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$AGENT_BOT_LOG"
[[ "${AGENT_BOT_FAIL:-0}" != "1" ]]
EOF
chmod +x "$TEST_BIN/agent-bot"
cat >"$BREW_PREFIX/bin/gh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$BREW_PREFIX/bin/gh"

export HOME="$TEST_HOME" HOMEBREW_PREFIX="$BREW_PREFIX" AGENT_BOT_LOG
export PATH="$TEST_BIN:/usr/bin:/bin"

MARKER="$TEST_HOME/.config/managed-machine/agent-bot-gh-interposer"
GH_PATH="$BREW_PREFIX/bin/gh"

# Explicit initial install records machine consent and the direct Homebrew path.
"$ROOT/setup-agent-bot-gh" >"$TEST_DIR/install.out"
grep -Fxq "install-gh-shim --codex-desktop-gh $GH_PATH" "$AGENT_BOT_LOG"
grep -Fxq "$GH_PATH" "$MARKER"
[[ "$(stat -c '%a' "$MARKER" 2>/dev/null || stat -f '%Lp' "$MARKER")" == '600' ]]

# Reinstall and update repair are idempotent calls into the runtime owner.
"$ROOT/setup-agent-bot-gh" >"$TEST_DIR/reinstall.out"
# shellcheck source=../lib/agent-bot-gh.sh
source "$ROOT/lib/agent-bot-gh.sh"
repair_agent_bot_gh_if_configured >"$TEST_DIR/repair.out"
[[ "$(grep -Fxc "install-gh-shim --codex-desktop-gh $GH_PATH" "$AGENT_BOT_LOG")" == '3' ]]
grep -Fq 'Reconciled explicit Codex desktop gh interposition' "$TEST_DIR/repair.out"

# Restore delegates to agent-bot first and removes consent only after success.
"$ROOT/setup-agent-bot-gh" --restore >"$TEST_DIR/restore.out"
tail -1 "$AGENT_BOT_LOG" | grep -Fxq "install-gh-shim --restore-codex-desktop-gh $GH_PATH"
[[ ! -e "$MARKER" ]]
lines_before="$(wc -l <"$AGENT_BOT_LOG" | tr -d ' ')"
repair_agent_bot_gh_if_configured
[[ "$(wc -l <"$AGENT_BOT_LOG" | tr -d ' ')" == "$lines_before" ]]

# A failed runtime install never records opt-in, and a failed restore keeps it.
AGENT_BOT_FAIL=1 "$ROOT/setup-agent-bot-gh" >"$TEST_DIR/failed-install.out" 2>&1 && {
    echo 'expected runtime install failure' >&2
    exit 1
}
[[ ! -e "$MARKER" ]]
AGENT_BOT_FAIL=0 "$ROOT/setup-agent-bot-gh" >/dev/null
AGENT_BOT_FAIL=1 "$ROOT/setup-agent-bot-gh" --restore >"$TEST_DIR/failed-restore.out" 2>&1 && {
    echo 'expected runtime restore failure' >&2
    exit 1
}
[[ -f "$MARKER" ]]

# Prefix drift and malformed markers fail closed instead of selecting a new gh.
HOMEBREW_PREFIX="$TEST_DIR/other-homebrew" repair_agent_bot_gh_if_configured >"$TEST_DIR/drift.out" 2>&1 && {
    echo 'expected Homebrew prefix drift to fail' >&2
    exit 1
}
grep -Fq 'does not match the current Homebrew prefix' "$TEST_DIR/drift.out"
printf 'relative/gh\n' >"$MARKER"
read_agent_bot_gh_marker >"$TEST_DIR/malformed.out" 2>&1 && {
    echo 'expected malformed marker to fail' >&2
    exit 1
}
grep -Fq 'marker is malformed' "$TEST_DIR/malformed.out"

# A symlink marker is a configured-but-invalid state, never a silent opt-out.
rm -f "$MARKER"
ln -s "$TEST_DIR/missing-marker" "$MARKER"
agent_bot_gh_is_configured
repair_agent_bot_gh_if_configured >"$TEST_DIR/symlink.out" 2>&1 && {
    echo 'expected symlink marker to fail closed' >&2
    exit 1
}
grep -Fq 'not explicitly configured' "$TEST_DIR/symlink.out"

# Explicit reinstall validates existing consent instead of overwriting it.
rm -f "$MARKER"
printf '/different/prefix/bin/gh\n' >"$MARKER"
"$ROOT/setup-agent-bot-gh" >"$TEST_DIR/reinstall-drift.out" 2>&1 && {
    echo 'expected explicit reinstall path drift to fail' >&2
    exit 1
}
grep -Fq 'does not match the current Homebrew prefix' "$TEST_DIR/reinstall-drift.out"
grep -Fxq '/different/prefix/bin/gh' "$MARKER"

"$ROOT/setup-agent-bot-gh" --restore extra >"$TEST_DIR/extra-args.out" 2>&1 && {
    echo 'expected extra setup arguments to fail' >&2
    exit 1
}
grep -Fq 'usage: setup-agent-bot-gh' "$TEST_DIR/extra-args.out"

# An interrupted restore remains configured state and makes updates fail loud.
rm -f "$MARKER"
PENDING="${MARKER}.restore-pending"
printf '%s\n' "$GH_PATH" >"$PENDING"
agent_bot_gh_is_configured
repair_agent_bot_gh_if_configured >"$TEST_DIR/pending.out" 2>&1 && {
    echo 'expected interrupted restore to fail closed' >&2
    exit 1
}
grep -Fq 'restore is incomplete' "$TEST_DIR/pending.out"

# Prefix discovery repairs PATH through the shared Homebrew helper, while
# invalid prefixes and discovery failures propagate without inventing /bin/gh.
unset HOMEBREW_PREFIX
FALLBACK_BREW_BIN="$TEST_DIR/fallback-brew/bin"
mkdir -p "$FALLBACK_BREW_BIN"
cat >"$FALLBACK_BREW_BIN/brew" <<EOF
#!/usr/bin/env bash
[[ "\${1:-}" == '--prefix' ]] && printf '%s\n' '$BREW_PREFIX'
EOF
chmod +x "$FALLBACK_BREW_BIN/brew"
ensure_brew_on_path() {
    export PATH="$FALLBACK_BREW_BIN:$PATH"
}
[[ "$(agent_bot_homebrew_gh_path)" == "$GH_PATH" ]]

HOMEBREW_PREFIX=relative agent_bot_homebrew_gh_path >"$TEST_DIR/relative-prefix.out" 2>&1 && {
    echo 'expected relative Homebrew prefix to fail' >&2
    exit 1
}
grep -Fq 'non-absolute prefix' "$TEST_DIR/relative-prefix.out"
! grep -Fq '/bin/gh' "$TEST_DIR/relative-prefix.out"

unset HOMEBREW_PREFIX
ensure_brew_on_path() { return 1; }
agent_bot_homebrew_gh_path >"$TEST_DIR/missing-brew.out" 2>&1 && {
    echo 'expected missing Homebrew to fail' >&2
    exit 1
}
grep -Fq 'brew required — run setup-brew first' "$TEST_DIR/missing-brew.out"

# Update convergence is conditional; machines without the marker remain stock.
grep -Fq 'if agent_bot_gh_is_configured; then' "$ROOT/scripts/update"
grep -Fq 'repair_agent_bot_gh_if_configured' "$ROOT/scripts/update"

echo 'agent-bot gh convergence tests passed'

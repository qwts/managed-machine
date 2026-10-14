#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
FIXTURE="$TEST_ROOT/fixture"
TEST_HOME="$TEST_ROOT/home"
RUN_LOG="$TEST_ROOT/run.log"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$FIXTURE/scripts" "$FIXTURE/lib" "$TEST_HOME"
cp "$ROOT/scripts/bootstrap" "$FIXTURE/scripts/bootstrap"
cp "$ROOT"/lib/*.sh "$FIXTURE/lib/"
chmod +x "$FIXTURE/scripts/bootstrap"
cat >>"$FIXTURE/lib/apps.sh" <<EOF
install_catalog_app() {
    printf 'catalog:%s\n' "\$1" >>'$RUN_LOG'
    return 0
}
EOF

CONFIG_REPO="$TEST_ROOT/managed-machine-config"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'managed-machine test'
git -C "$CONFIG_REPO" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
printf '{"schema_version":1,"apps":[]}\n' >"$CONFIG_REPO/apps.json"
printf 'v0.1.0\n' >"$CONFIG_REPO/local-bin.ref"
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

SETUP_SCRIPTS=(
    setup-hostname
    setup-zsh
    setup-brew
    setup-nvm
    setup-git-hooks
    setup-gh
    setup-bin
    setup-zsh-functions
    setup-rust
)

# The real list must match this fixture, so a step added to one is added to
# the other, and in the same position.
real_scripts="$(sed -n '/^SETUP_SCRIPTS=(/,/^)/p' "$ROOT/scripts/bootstrap" | sed -n 's/^    \(setup-[a-z-]*\)$/\1/p')"
[[ "$real_scripts" == "$(printf '%s\n' "${SETUP_SCRIPTS[@]}")" ]] || {
    echo 'scripts/bootstrap SETUP_SCRIPTS drifted from the test fixture' >&2
    exit 1
}

for name in "${SETUP_SCRIPTS[@]}"; do
    cat >"$FIXTURE/$name" <<EOF
#!/usr/bin/env bash
printf '%s\n' '$name' >>'$RUN_LOG'
if [[ '$name' == 'setup-nvm' && "\${MOCK_FAIL_NVM:-0}" == '1' ]]; then
    exit 42
fi
if [[ '$name' == 'setup-zsh' && "\${MOCK_DEFER_ZSH:-0}" == '1' ]]; then
    exit 75
fi
if [[ '$name' == 'setup-git-hooks' && "\${MOCK_SKIP_GIT_HOOKS:-0}" == '1' ]]; then
    exit 76
fi
if [[ '$name' == 'setup-hostname' && "\${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == 'noninteractive' ]]; then
    exit 75
fi
EOF
    chmod +x "$FIXTURE/$name"
done

STATUS_FILE="$TEST_HOME/.config/managed-machine/bootstrap.manifest"
STUB_BIN="$TEST_ROOT/bin"
mkdir -p "$STUB_BIN"
AGENT_BOT_LOG="$TEST_ROOT/agent-bot.log"
export AGENT_BOT_LOG
# Existing independently managed runtime and interposer state are stale
# inputs to this fixture. Bootstrap must leave them untouched.
mkdir -p "$TEST_HOME/.local/bin" "$TEST_HOME/.config/managed-machine"
cat >"$STUB_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
printf called >>"$AGENT_BOT_LOG"
EOF
chmod +x "$STUB_BIN/agent-bot"
printf '%s\n' "$TEST_HOME/.local/bin/gh" >"$TEST_HOME/.config/managed-machine/agent-bot-gh-interposer"
cp "$TEST_HOME/.config/managed-machine/agent-bot-gh-interposer" "$TEST_ROOT/interposer.before"
export PATH="$STUB_BIN:$PATH"

file_mode() {
    if stat -c '%a' "$1" >/dev/null 2>&1; then
        stat -c '%a' "$1"
    else
        stat -f '%Lp' "$1"
    fi
}

# Noninteractive mode skips only steps that need a dialog, runs the rest,
# and does not assign follow-up setup commands.
HOME="$TEST_HOME" "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/noninteractive.out" 2>&1
grep -Fq 'Bootstrap mode: noninteractive' "$TEST_ROOT/noninteractive.out"
grep -Fq 'skip: setup-hostname' "$TEST_ROOT/noninteractive.out"
! grep -Fq 'defer: setup-gh' "$TEST_ROOT/noninteractive.out"
! grep -Fq 'managed-machine setup' "$TEST_ROOT/noninteractive.out"
grep -qxF 'setup-gh' "$RUN_LOG"
grep -qxF 'setup-bin' "$RUN_LOG"
! grep -qxF 'setup-hostname' "$RUN_LOG"
grep -qxF 'setup-zsh' "$RUN_LOG"
grep -qxF 'setup-rust' "$RUN_LOG"
grep -q $'^complete\tsetup-gh\t' "$STATUS_FILE"
grep -q $'^complete\tsetup-bin\t' "$STATUS_FILE"
grep -q $'^skipped\tsetup-hostname\t' "$STATUS_FILE"
grep -qxF 'mode=noninteractive' "$STATUS_FILE"
[[ "$(file_mode "$STATUS_FILE")" == '600' ]]

# The runtime is never a bootstrap step, even when an existing executable is
# on PATH. Unrelated setup continues to run.
grep -qxF 'setup-gh' "$RUN_LOG"
grep -qxF 'setup-bin' "$RUN_LOG"
! grep -q 'agent-bot' "$RUN_LOG"
! grep -q 'agent-bot' "$STATUS_FILE"
[[ ! -e "$TEST_ROOT/agent-bot.log" ]]
cmp -s "$TEST_HOME/.config/managed-machine/agent-bot-gh-interposer" "$TEST_ROOT/interposer.before"

# A failed step does not prevent later independent setup, but makes the final
# bootstrap result fail and records both outcomes.
: >"$RUN_LOG"
if HOME="$TEST_HOME" MOCK_FAIL_NVM=1 "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/failed.out" 2>&1; then
    echo 'expected bootstrap with a failed setup step to exit nonzero' >&2
    exit 1
fi
grep -qxF 'setup-rust' "$RUN_LOG"
grep -qxF $'failed\tsetup-nvm\texit status 42' "$STATUS_FILE"
grep -q $'^complete\tsetup-rust\t' "$STATUS_FILE"
grep -Fq 'Bootstrap finished with failed steps.' "$TEST_ROOT/failed.out"

# Reserved 75/76 exit codes mean the step is not part of this install.
: >"$RUN_LOG"
HOME="$TEST_HOME" MOCK_DEFER_ZSH=1 "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/deferred.out" 2>&1
grep -q $'^skipped\tsetup-zsh\tnot part of this install$' "$STATUS_FILE"

# A skipped step is recorded without failing bootstrap.
: >"$RUN_LOG"
HOME="$TEST_HOME" MOCK_SKIP_GIT_HOOKS=1 "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/skipped.out" 2>&1
grep -q $'^skipped\tsetup-git-hooks\tnot part of this install$' "$STATUS_FILE"
grep -Fq 'setup-git-hooks' "$TEST_ROOT/skipped.out"
grep -qxF 'setup-zsh' "$RUN_LOG"

# Mode resolution fails closed when interactive mode is explicitly requested
# without a terminal, while auto mode selects noninteractive.
# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/bootstrap.sh
source "$ROOT/lib/bootstrap.sh"
bootstrap_has_interactive_terminal() { return 1; }
[[ "$(bootstrap_resolve_mode auto)" == 'noninteractive' ]]
if bootstrap_resolve_mode interactive >"$TEST_ROOT/no-terminal.out" 2>&1; then
    echo 'expected explicit interactive mode without a terminal to fail' >&2
    exit 1
fi
grep -Fq 'no usable terminal is attached' "$TEST_ROOT/no-terminal.out"

# Ownership migration that needs a dialog is skipped in noninteractive mode,
# not recorded as a failed bootstrap step.
cat >>"$FIXTURE/lib/migrate.sh" <<'EOF'

run_managed_machine_migrations() {
    if [[ "${MOCK_DEFER_MIGRATIONS:-0}" == "1" ]]; then
        echo "Skipped: restoring Homebrew prefix ownership needs the administrator dialog" >&2
        return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
    fi
    return 0
}
EOF
: >"$RUN_LOG"
HOME="$TEST_HOME" MOCK_DEFER_MIGRATIONS=1 "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/migrate-deferred.out" 2>&1
grep -q $'^skipped\tmigrations\tnot part of this install$' "$STATUS_FILE"
! grep -Fq 'managed-machine --bootstrap' "$STATUS_FILE"
grep -qxF 'setup-zsh' "$RUN_LOG"
grep -Fq 'skipped:' "$TEST_ROOT/migrate-deferred.out"

# auto:false catalog rows are skipped by bootstrap; omitted auto still runs.
printf '%s\n' '{"schema_version":1,"apps":[{"name":"always","kind":"devin"},{"name":"sometimes","kind":"devin","auto":false}]}' >"$CONFIG_REPO/apps.json"
: >"$RUN_LOG"
HOME="$TEST_HOME" "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/catalog-auto.out" 2>&1
grep -qxF 'catalog:always' "$RUN_LOG"
! grep -qxF 'catalog:sometimes' "$RUN_LOG"
grep -q $'^complete\tsetup-always\t' "$STATUS_FILE"
! grep -q $'\tsetup-sometimes\t' "$STATUS_FILE"

# Interactive bootstrap (#174): prints the execution plan up front (every step,
# which steps need administrator access, and what packages, /Applications
# bundles, owners, and paths they change), confirms once in the terminal,
# requests one authorization dialog whose prompt names the plan, runs all
# privileged steps under that single approval (dropping to the brew owner),
# logs the plan in bootstrap.manifest, and refuses out-of-plan privileged steps.
cp "$ROOT/lib/elevate-plan-run" "$FIXTURE/lib/elevate-plan-run"
chmod +x "$FIXTURE/lib/elevate-plan-run"
cp "$ROOT/lib/apps.sh" "$FIXTURE/lib/apps.sh"

OSA_LOG="$TEST_ROOT/osascript.log"
BREW_LOG="$TEST_ROOT/brew.log"
APPDIR="$TEST_ROOT/Applications"
mkdir -p "$APPDIR"
export OSA_LOG BREW_LOG

cat >"$STUB_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$OSA_LOG"
[[ -z "${OSA_STDERR:-}" ]] || printf '%s\n' "$OSA_STDERR" >&2
exit "${OSA_EXIT:-0}"
EOF
cat >"$STUB_BIN/uname" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "-s" ]]; then
    echo Darwin
else
    /usr/bin/uname "$@"
fi
EOF
cat >"$STUB_BIN/brew" <<EOF
#!/usr/bin/env bash
printf 'brew:%s\n' "\$*" >>'$BREW_LOG'
case "\$1" in
    --prefix)
        echo /opt/homebrew
        ;;
    info)
        if [[ "\$*" == *homebrew/core/taplo* ]]; then
            printf '%s\n' '{"formulae":[{"name":"taplo","tap":"homebrew/core"}]}'
        elif [[ "\$*" == *homebrew/cask/cursor* ]]; then
            printf '%s\n' '{"casks":[{"token":"cursor","tap":"homebrew/cask","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","url":"https://downloader.cursor.sh/Cursor.dmg","homepage":"https://cursor.com"}]}'
        fi
        ;;
    list)
        if [[ "\$*" == *--cask* && -f '$TEST_ROOT/cursor.receipt' ]]; then
            echo 'cursor 1.0.0'
            exit 0
        elif [[ "\$*" != *--cask* && -f '$TEST_ROOT/taplo.receipt' ]]; then
            echo 'taplo 0.9.3'
            exit 0
        fi
        exit 1
        ;;
    install)
        if [[ "\$*" == *homebrew/core/taplo* ]]; then
            : >'$TEST_ROOT/taplo.receipt'
        elif [[ "\$*" == *homebrew/cask/cursor* ]]; then
            mkdir -p '$APPDIR/Cursor.app'
            : >'$TEST_ROOT/cursor.receipt'
        fi
        ;;
esac
EOF
cat >"$STUB_BIN/codesign" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "-dv" ]]; then
    echo "Authority=Developer ID Application: Anysphere (VDXQ22DGB9)"
    echo "TeamIdentifier=VDXQ22DGB9"
    exit 0
fi
exit 0
EOF
chmod +x "$STUB_BIN/osascript" "$STUB_BIN/uname" "$STUB_BIN/brew" "$STUB_BIN/codesign"

cat >"$CONFIG_REPO/apps.json" <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {
      "name": "taplo",
      "kind": "brew-formula",
      "formula": "taplo"
    },
    {
      "name": "cursor",
      "kind": "signed-cask",
      "token": "cursor",
      "app_name": "Cursor.app",
      "team_id": "VDXQ22DGB9",
      "url_hosts": ["downloader.cursor.sh"],
      "homepage_hosts": ["cursor.com"]
    }
  ]
}
EOF

# 1. Declining the plan in the terminal changes nothing and opens no dialog.
: >"$RUN_LOG"
: >"$OSA_LOG"
: >"$BREW_LOG"
rm -f "$STATUS_FILE" "$TEST_HOME/.config/managed-machine/declined-apps.manifest"
if HOME="$TEST_HOME" \
    MANAGED_MACHINE_PLAN_CONFIRM=n \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$APPDIR" \
    "$FIXTURE/scripts/bootstrap" --interactive >"$TEST_ROOT/declined-plan.out" 2>&1; then
    echo 'expected declined bootstrap plan to exit nonzero' >&2
    exit 1
fi
grep -Fq 'Managed-machine bootstrap plan (' "$TEST_ROOT/declined-plan.out"
grep -Fq '[admin] setup-taplo' "$TEST_ROOT/declined-plan.out"
grep -Fq '[admin] setup-cursor' "$TEST_ROOT/declined-plan.out"
grep -Fq 'homebrew/core/taplo' "$TEST_ROOT/declined-plan.out"
grep -Fq 'Cursor.app' "$TEST_ROOT/declined-plan.out"
grep -Fq 'owner: otheradmin' "$TEST_ROOT/declined-plan.out"
grep -Fq 'declined — no changes made.' "$TEST_ROOT/declined-plan.out"
[[ ! -s "$RUN_LOG" ]]
[[ ! -s "$OSA_LOG" ]]
[[ ! -e "$STATUS_FILE" ]]
# Clear declined-apps.manifest before testing dialog cancellation and approval.
rm -f "$TEST_HOME/.config/managed-machine/declined-apps.manifest"

# 2. Cancelling the single authorization dialog makes no change and runs no steps.
: >"$RUN_LOG"
: >"$OSA_LOG"
if HOME="$TEST_HOME" \
    MANAGED_MACHINE_PLAN_CONFIRM=y \
    OSA_EXIT=1 \
    OSA_STDERR='execution error: User canceled. (-128)' \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$APPDIR" \
    "$FIXTURE/scripts/bootstrap" --interactive >"$TEST_ROOT/cancel-dialog.out" 2>&1; then
    echo 'expected cancelled bootstrap authorization dialog to exit nonzero' >&2
    exit 1
fi
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "1" ]]
[[ ! -s "$RUN_LOG" ]]
[[ ! -d "$APPDIR/Cursor.app" ]]
rm -f "$TEST_HOME/.config/managed-machine/declined-apps.manifest"

# 3. Approving the plan runs multiple privileged steps under ONE osascript
# invocation, drops Homebrew to otheradmin (never root), and records the plan
# in bootstrap.manifest.
: >"$RUN_LOG"
: >"$OSA_LOG"
: >"$BREW_LOG"
HOME="$TEST_HOME" \
    MANAGED_MACHINE_PLAN_CONFIRM=y \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$APPDIR" \
    "$FIXTURE/scripts/bootstrap" --interactive >"$TEST_ROOT/approved-plan.out" 2>&1
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "1" ]]
grep -Fq 'apply approved bootstrap plan bootstrap-' "$OSA_LOG"
grep -Fq '/usr/bin/sudo -u otheradmin' "$OSA_LOG"
[[ -f "$TEST_ROOT/taplo.receipt" ]]
[[ -d "$APPDIR/Cursor.app" ]]
plan_id_logged="$(sed -n 's/^plan_id=//p' "$STATUS_FILE")"
[[ -n "$plan_id_logged" ]]
grep -Fq "plan	$plan_id_logged	setup-taplo	brew-formula	otheradmin	homebrew/core/taplo" "$STATUS_FILE"
grep -Fq "plan	$plan_id_logged	setup-cursor	signed-cask	otheradmin	homebrew/cask/cursor" "$STATUS_FILE"
grep -Fq "complete	setup-taplo	plan=$plan_id_logged:" "$STATUS_FILE"
grep -Fq "complete	setup-cursor	plan=$plan_id_logged:" "$STATUS_FILE"

# 4. A step that attempts a privileged action outside the approved plan is
# refused rather than prompted for.
cat >"$FIXTURE/setup-rust" <<EOF
#!/usr/bin/env bash
source "$FIXTURE/lib/elevate.sh"
elevate_run "unplanned privileged action in setup-rust" /usr/bin/true
EOF
chmod +x "$FIXTURE/setup-rust"
: >"$OSA_LOG"
if HOME="$TEST_HOME" \
    MANAGED_MACHINE_PLAN_CONFIRM=y \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$APPDIR" \
    "$FIXTURE/scripts/bootstrap" --interactive >"$TEST_ROOT/out-of-plan.out" 2>&1; then
    echo 'expected out-of-plan privileged step to fail bootstrap' >&2
    exit 1
fi
grep -Fq 'Error: refusing privileged step outside the approved plan: unplanned privileged action in setup-rust' "$TEST_ROOT/out-of-plan.out"
[[ ! -s "$OSA_LOG" ]]
grep -qxF $'failed\tsetup-rust\texit status 1' "$STATUS_FILE"

echo 'Bootstrap contract tests passed'

#!/usr/bin/env bash
set -euo pipefail

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
    setup-agent-bot
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

# The agent-bot runtime is present on PATH for the default runs, so the
# noninteractive preflight lets setup-agent-bot run; a run without it
# exercises the skip below.
STUB_BIN="$TEST_ROOT/bin"
mkdir -p "$STUB_BIN"
printf '#!/usr/bin/env bash\nexit 0\n' >"$STUB_BIN/agent-bot"
chmod +x "$STUB_BIN/agent-bot"
export PATH="$STUB_BIN:$PATH"

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

# setup-agent-bot is part of bootstrap (#75): it runs after setup-gh, whose
# GitHub auth its tap fetch rides on, and before setup-bin.
grep -q $'^complete\tsetup-agent-bot\t' "$STATUS_FILE"
[[ "$(grep -nxF -e setup-gh -e setup-agent-bot -e setup-bin "$RUN_LOG" | cut -d: -f2 | tr '\n' ' ')" == 'setup-gh setup-agent-bot setup-bin ' ]]

# Without the runtime installed, a noninteractive bootstrap skips
# setup-agent-bot up front — the brew install needs the dialog — and
# records the skip instead of spending a step on it. A bare system PATH
# has neither agent-bot nor brew.
: >"$RUN_LOG"
HOME="$TEST_HOME" PATH="/usr/bin:/bin" "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/no-runtime.out" 2>&1
grep -Fq 'skip: setup-agent-bot (installing the agent-bot runtime needs the administrator dialog)' "$TEST_ROOT/no-runtime.out"
! grep -qxF 'setup-agent-bot' "$RUN_LOG"
grep -q $'^skipped\tsetup-agent-bot\tinstalling the agent-bot runtime needs the administrator dialog$' "$STATUS_FILE"
grep -qxF 'setup-bin' "$RUN_LOG"

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

echo 'Bootstrap contract tests passed'

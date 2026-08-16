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

CONFIG_REPO="$TEST_ROOT/managed-machine-config"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'managed-machine test'
git -C "$CONFIG_REPO" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
printf '{"schema_version":1,"apps":[]}\n' >"$CONFIG_REPO/apps.json"
printf 'v0.1.0\n' >"$CONFIG_REPO/local-bin.ref"
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

SETUP_SCRIPTS=(
    setup-brew
    setup-hostname
    setup-zsh
    setup-nvm
    setup-git-hooks
    setup-gh
    setup-bin
    setup-rust
)

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

# Explicit noninteractive mode preflights prompt-capable steps, skips them,
# runs later independent steps, and exits successfully when only deferrals remain.
HOME="$TEST_HOME" "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/noninteractive.out" 2>&1
grep -Fq 'Bootstrap mode: noninteractive' "$TEST_ROOT/noninteractive.out"
grep -Fq 'defer: setup-gh' "$TEST_ROOT/noninteractive.out"
grep -Fq 'defer: setup-bin' "$TEST_ROOT/noninteractive.out"
preflight_line="$(grep -nF 'defer: setup-gh' "$TEST_ROOT/noninteractive.out" | cut -d: -f1)"
first_setup_line="$(grep -nF '==> setup-' "$TEST_ROOT/noninteractive.out" | head -1 | cut -d: -f1)"
[[ "$preflight_line" -lt "$first_setup_line" ]]
! grep -qxF 'setup-gh' "$RUN_LOG"
! grep -qxF 'setup-bin' "$RUN_LOG"
! grep -qxF 'setup-hostname' "$RUN_LOG"
grep -qxF 'setup-zsh' "$RUN_LOG"
grep -qxF 'setup-rust' "$RUN_LOG"
grep -q $'^deferred\tsetup-gh\t.*managed-machine setup gh$' "$STATUS_FILE"
grep -q $'^deferred\tsetup-bin\t.*managed-machine setup bin$' "$STATUS_FILE"
grep -q $'^deferred\tsetup-hostname\t' "$STATUS_FILE"
grep -qxF 'mode=noninteractive' "$STATUS_FILE"
[[ "$(file_mode "$STATUS_FILE")" == '600' ]]

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

# The reserved deferral exit code is pending work, not a hard failure.
: >"$RUN_LOG"
HOME="$TEST_HOME" MOCK_DEFER_ZSH=1 "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/deferred.out" 2>&1
grep -q $'^deferred\tsetup-zsh\tsetup requested interactive follow-up' "$STATUS_FILE"

# A skipped step is recorded without failing bootstrap.
: >"$RUN_LOG"
HOME="$TEST_HOME" MOCK_SKIP_GIT_HOOKS=1 "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/skipped.out" 2>&1
grep -q $'^skipped\tsetup-git-hooks\tstep does not apply in this install layout$' "$STATUS_FILE"
grep -Fq 'skipped: 1' "$TEST_ROOT/skipped.out"
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

# Ownership migration that needs a dialog is deferred in noninteractive mode,
# not recorded as a failed bootstrap step.
cat >>"$FIXTURE/lib/migrate.sh" <<'EOF'

run_managed_machine_migrations() {
    if [[ "${MOCK_DEFER_MIGRATIONS:-0}" == "1" ]]; then
        echo "Deferred: restoring Homebrew prefix ownership requires administrator authorization" >&2
        echo "Complete later with: managed-machine --bootstrap --interactive" >&2
        return "${MANAGED_MACHINE_DEFERRED_EXIT:-75}"
    fi
    return 0
}
EOF
: >"$RUN_LOG"
HOME="$TEST_HOME" MOCK_DEFER_MIGRATIONS=1 "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_ROOT/migrate-deferred.out" 2>&1
grep -q $'^deferred\tmigrations\t' "$STATUS_FILE"
grep -Fq 'managed-machine --bootstrap --interactive' "$STATUS_FILE"
grep -qxF 'setup-zsh' "$RUN_LOG"
grep -Fq 'deferred:' "$TEST_ROOT/migrate-deferred.out"

echo 'Bootstrap contract tests passed'

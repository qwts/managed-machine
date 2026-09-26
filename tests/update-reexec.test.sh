#!/usr/bin/env bash
# scripts/update upgrades the very tree it is running from: ROOT resolves
# through $prefix/opt/managed-machine/libexec, and `brew upgrade` repoints that
# symlink mid-run. Sourcing libraries on both sides of the swap leaves one
# process holding two versions at once, so any release that changes a contract
# between two libraries breaks the upgrading run — and only that run. This test
# pins the invariant: every library a single update pass sources comes from one
# version of the tree.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

# The suite may itself run inside a harness; the update refuses agent
# sessions (scenario 7), so the fixture's one marker is cleared here and
# set explicitly where the test wants it.
unset CLAUDECODE

export LIBEXEC="$TEST_DIR/libexec"
export LOG="$TEST_DIR/sourced.log"
export PASSFILE="$TEST_DIR/pass.counter"
export CONFIG_DIR="$TEST_DIR/config"
export WRITE_TREE="$TEST_DIR/write-tree"
STUB_BIN="$TEST_DIR/bin"
mkdir -p "$LIBEXEC/lib" "$LIBEXEC/scripts" "$STUB_BIN" "$CONFIG_DIR"

# The script under test, run against a fixture tree of stub libraries. Only
# scripts/update is real: the stubs stand in for every library it sources and
# stamp themselves with the tree version they were written at.
cp "$ROOT/scripts/update" "$LIBEXEC/scripts/update"
chmod +x "$LIBEXEC/scripts/update"

# Writes a whole fixture tree at version $1. Each library appends one log line
# tagged with the pass that sourced it and the version it came from.
#
# UPDATE_PASS is deliberately NOT exported: exec cannot carry a shell variable
# across, so a line's pass number identifies the process that wrote it even
# though exec preserves the PID.
cat >"$WRITE_TREE" <<'GEN'
#!/usr/bin/env bash
set -euo pipefail
v="$1"
for lib in install agent-bot-gh bootstrap migrate apps hostname; do
    {
        printf 'if [[ -z "${UPDATE_PASS:-}" ]]; then\n'
        printf '    UPDATE_PASS=$(( $(cat "$PASSFILE" 2>/dev/null || echo 0) + 1 ))\n'
        printf '    printf %%s "$UPDATE_PASS" >"$PASSFILE"\n'
        printf 'fi\n'
        printf 'printf "pass=%%s %s.sh=%s\\n" "$UPDATE_PASS" >>"$LOG"\n' "$lib" "$v"
    } >"$LIBEXEC/lib/$lib.sh"
done

# The helpers scripts/update calls, split across the libraries that really
# define them so the fixture exercises the same source ordering.
cat >>"$LIBEXEC/lib/install.sh" <<'HELPERS'
brew_run() { brew "$@"; }
managed_machine_config_repo_dir() { printf '%s\n' "$CONFIG_DIR"; }
managed_machine_config_dir() { printf '%s\n' "$CONFIG_DIR"; }
# The real detector lives in lib/elevate.sh (tested there); the fixture
# mirrors the one marker this test drives.
managed_machine_agent_session() { [[ "${CLAUDECODE:-}" == 1 ]]; }
catalog_auto_app_names() { :; }
install_catalog_app() { :; }
HELPERS
printf 'MANAGED_MACHINE_DEFERRED_EXIT=75\nMANAGED_MACHINE_SKIPPED_EXIT=76\n' >>"$LIBEXEC/lib/bootstrap.sh"
printf 'run_managed_machine_migrations() { :; }\n' >>"$LIBEXEC/lib/migrate.sh"
printf 'agent_bot_gh_is_configured() { return 1; }\n' >>"$LIBEXEC/lib/agent-bot-gh.sh"

# Steps exit as $STEP_EXIT_<name> says (default 0), so a failing or a
# deferring step can be modelled per run.
for step in setup-gh setup-agent-bot setup-zsh setup-bin setup-zsh-functions; do
    {
        printf '#!/usr/bin/env bash\n'
        printf 'printf "step %s=%s\\n" >>"$LOG"\n' "$step" "$v"
        printf 'v="STEP_EXIT_%s"; exit "${!v:-0}"\n' "${step//-/_}"
    } >"$LIBEXEC/$step"
    chmod +x "$LIBEXEC/$step"
done
GEN
chmod +x "$WRITE_TREE"

# `brew upgrade` swaps the tree under the running process, exactly as the real
# upgrade repoints the opt symlink.
cat >"$STUB_BIN/brew" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "upgrade" ]]; then
    "$WRITE_TREE" 2
fi
exit 0
STUB
chmod +x "$STUB_BIN/brew"

"$WRITE_TREE" 1
PATH="$STUB_BIN:$PATH" "$LIBEXEC/scripts/update" >"$TEST_DIR/update.out" 2>&1 || {
    echo 'update failed' >&2
    cat "$TEST_DIR/update.out" >&2
    exit 1
}

# 1. No pass mixes versions. This is the regression: before the re-exec, the
# single pass sourced install.sh (and its catalog) from the old tree and
# apps.sh (and its cask helpers) from the new one.
mixed="$(awk '
    $1 ~ /^pass=/ {
        pass = $1
        version = $2
        sub(/^.*=/, "", version)
        if (!(pass in seen)) { seen[pass] = version }
        else if (seen[pass] != version) { bad[pass] = 1 }
    }
    END { for (p in bad) print p }
' "$LOG")"
if [[ -n "$mixed" ]]; then
    echo "update sourced two tree versions in one process: $mixed" >&2
    cat "$LOG" >&2
    exit 1
fi

# 2. The upgrade actually landed, and the work after it ran against the new
# tree — including the libraries that were already sourced before it.
grep -Fq 'pass=2 install.sh=2' "$LOG"
grep -Fq 'pass=2 agent-bot-gh.sh=2' "$LOG"
grep -Fq 'pass=2 migrate.sh=2' "$LOG"
grep -Fq 'pass=2 apps.sh=2' "$LOG"
grep -Fq 'pass=2 hostname.sh=2' "$LOG"

# 3. The pre-upgrade pass sourced only what it needed to run brew, and did no
# post-upgrade work: no migrations, apps, or hostname handling at the old
# version.
! grep -Eq 'pass=1 (migrate|apps|hostname)\.sh=' "$LOG"

# 4. The re-exec neither loops nor drops the rest of the run: the safe steps
# execute once, from the upgraded tree, and the script reaches its end.
for step in setup-gh setup-agent-bot setup-zsh setup-bin setup-zsh-functions; do
    [[ "$(grep -Fc "step $step=2" "$LOG")" == 1 ]]
    ! grep -Fq "step $step=1" "$LOG"
done
[[ "$(grep -Fc 'Update complete.' "$TEST_DIR/update.out")" == 1 ]]
[[ "$(cat "$PASSFILE")" == 2 ]]

# 4b. setup-agent-bot is a safe step (#75), run right after setup-gh whose
# GitHub auth its tap fetch rides on; the real list matches the fixture.
[[ "$(grep -o '^step setup-[a-z-]*' "$LOG" | sed 's/^step //' | tr '\n' ' ')" == 'setup-gh setup-agent-bot setup-zsh setup-bin setup-zsh-functions ' ]]
real_steps="$(sed -n '/^SAFE_STEPS=(/,/^)/p' "$ROOT/scripts/update" | sed -n 's/^    \(setup-[a-z-]*\)$/\1/p' | tr '\n' ' ')"
[[ "$real_steps" == 'setup-gh setup-agent-bot setup-zsh setup-bin setup-zsh-functions ' ]]

# 5. With no brew installed there is nothing to upgrade, but the run still
# completes and stays single-version. A bare system PATH has the tools the
# script needs and no Homebrew.
: >"$LOG"
rm -f "$PASSFILE"
"$WRITE_TREE" 1
PATH="/usr/bin:/bin" "$LIBEXEC/scripts/update" >"$TEST_DIR/nobrew.out" 2>&1 || {
    echo 'update without brew failed' >&2
    cat "$TEST_DIR/nobrew.out" >&2
    exit 1
}
grep -Fq 'skip: brew not found' "$TEST_DIR/nobrew.out"
grep -Fq 'Update complete.' "$TEST_DIR/nobrew.out"
! grep -Eq '\.sh=2' "$LOG"

# 6. Every step's outcome is recorded, and a failed safe step neither stops
# the steps after it nor hides: the run finishes, names the failure, exits
# nonzero, and the manifest keeps it for status.
: >"$LOG"
rm -f "$PASSFILE"
"$WRITE_TREE" 1
if STEP_EXIT_setup_gh=1 PATH="$STUB_BIN:$PATH" "$LIBEXEC/scripts/update" >"$TEST_DIR/failstep.out" 2>&1; then
    echo 'a failed safe step must make the update exit nonzero' >&2
    cat "$TEST_DIR/failstep.out" >&2
    exit 1
fi
for step in setup-gh setup-agent-bot setup-zsh setup-bin setup-zsh-functions; do
    [[ "$(grep -Fc "step $step=2" "$LOG")" == 1 ]]
done
grep -Fq 'failed: setup-gh exited with status 1' "$TEST_DIR/failstep.out"
grep -Fq 'Update finished with failures: setup-gh' "$TEST_DIR/failstep.out"
! grep -Fq 'Update complete.' "$TEST_DIR/failstep.out"
manifest="$CONFIG_DIR/update.manifest"
[[ -f "$manifest" ]]
[[ "$(stat -f '%Lp' "$manifest")" == 600 ]]
grep -qx 'schema_version=1' "$manifest"
grep -q '^started_at=' "$manifest"
grep -q '^finished_at=' "$manifest"
grep -qx $'complete\tmigrations\t' "$manifest"
grep -qx $'failed\tsetup-gh\texit status 1' "$manifest"
grep -qx $'complete\tsetup-agent-bot\t' "$manifest"
grep -qx $'complete\tsetup-zsh\t' "$manifest"
grep -qx $'complete\tsetup-bin\t' "$manifest"

# A deferring step (exit 75/76) is a skip, not a failure: the run is clean.
: >"$LOG"
rm -f "$PASSFILE"
"$WRITE_TREE" 1
STEP_EXIT_setup_bin=76 PATH="$STUB_BIN:$PATH" "$LIBEXEC/scripts/update" >"$TEST_DIR/skipstep.out" 2>&1 || {
    echo 'a skipped step must not fail the update' >&2
    cat "$TEST_DIR/skipstep.out" >&2
    exit 1
}
grep -Fq 'Skipped: setup-bin' "$TEST_DIR/skipstep.out"
grep -Fq 'Update complete.' "$TEST_DIR/skipstep.out"
grep -qx $'skipped\tsetup-bin\tnot part of this install' "$manifest"
! grep -q '^failed' "$manifest"

# 7. An agent session is refused before any brew call or dialog, with the
# remedy, and exits as skipped; nothing is recorded because nothing ran.
: >"$LOG"
rm -f "$PASSFILE" "$manifest"
"$WRITE_TREE" 1
BREW_CALLS="$TEST_DIR/brew-calls.log"
export BREW_CALLS
cat >"$STUB_BIN/brew" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$BREW_CALLS"
if [[ "${1:-}" == "upgrade" ]]; then
    "$WRITE_TREE" 2
fi
exit 0
STUB
if CLAUDECODE=1 PATH="$STUB_BIN:$PATH" "$LIBEXEC/scripts/update" >"$TEST_DIR/agent.out" 2>&1; then
    echo 'an agent session must not run the update' >&2
    exit 1
fi
rc=0
CLAUDECODE=1 PATH="$STUB_BIN:$PATH" "$LIBEXEC/scripts/update" >/dev/null 2>&1 || rc=$?
[[ "$rc" == 76 ]]
grep -Fq 'does not run from an agent session' "$TEST_DIR/agent.out"
grep -Fq 'Run it from a human Terminal' "$TEST_DIR/agent.out"
[[ ! -e "$BREW_CALLS" ]]
[[ ! -s "$LOG" ]]
[[ ! -e "$manifest" ]]
# A human shell on the same tree still runs end to end.
: >"$LOG"
rm -f "$PASSFILE"
"$WRITE_TREE" 1
env -u CLAUDECODE PATH="$STUB_BIN:$PATH" "$LIBEXEC/scripts/update" >"$TEST_DIR/human.out" 2>&1
grep -Fq 'Update complete.' "$TEST_DIR/human.out"
grep -Fxq 'update' "$BREW_CALLS"

echo 'update re-exec tests passed'

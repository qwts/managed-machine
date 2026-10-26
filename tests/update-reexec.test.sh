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
# sessions (scenario 7), so ambient agent markers are cleared here and
# set explicitly where the test wants them.
for v in $(compgen -v); do
    [[ "$v" =~ ^[A-Z0-9]+_AGENT$ || "$v" == CODEX_* ]] && unset "$v"
done
unset -v QWEN_CODE CLAUDECODE AI_AGENT CLAUDE_CODE_ENTRYPOINT GH_AGENT_APP 2>/dev/null || true

export LIBEXEC="$TEST_DIR/libexec"
export LOG="$TEST_DIR/sourced.log"
export PASSFILE="$TEST_DIR/pass.counter"
export CONFIG_DIR="$TEST_DIR/config"
export WRITE_TREE="$TEST_DIR/write-tree"
export PHASES="$TEST_DIR/phases.log"
STUB_BIN="$TEST_DIR/bin"
mkdir -p "$LIBEXEC/lib" "$LIBEXEC/scripts" "$STUB_BIN" "$CONFIG_DIR" "$TEST_DIR/home/.local/bin" "$TEST_DIR/home/.config/managed-machine"
export HOME="$TEST_DIR/home"
cat >"$STUB_BIN/agent-bot" <<EOF
#!/usr/bin/env bash
printf 'called\n' >>"$TEST_DIR/agent-bot.log"
EOF
chmod +x "$STUB_BIN/agent-bot"
printf '%s\n' "$TEST_DIR/home/.local/bin/gh" >"$HOME/.config/managed-machine/agent-bot-gh-interposer"
cp "$HOME/.config/managed-machine/agent-bot-gh-interposer" "$TEST_DIR/interposer.before"

# The script under test, run against a fixture tree of stub libraries. Only
# scripts/update is real: the stubs stand in for every library it sources and
# stamp themselves with the tree version they were written at.
cp "$ROOT/legacy/scripts/update" "$LIBEXEC/scripts/update"
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
for lib in install bootstrap migrate apps hostname; do
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
# One authorization phase: record each call, then run it in-process.
brew_run_script() { printf 'phase %s\n' "$1" >>"$PHASES"; /bin/sh -c "$1"; }
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

# Steps exit as $STEP_EXIT_<name> says (default 0), so a failing or a
# deferring step can be modelled per run.
for step in setup-gh setup-zsh setup-bin setup-zsh-functions; do
    {
        printf '#!/usr/bin/env bash\n'
        printf 'printf "step %s=%s\\n" >>"$LOG"\n' "$step" "$v"
        printf 'printf "declined-file %s=%%s\\n" "${MANAGED_MACHINE_ELEVATION_DECLINED_FILE:-}" >>"$LOG"\n' "$step"
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
grep -Fq 'pass=2 migrate.sh=2' "$LOG"
grep -Fq 'pass=2 apps.sh=2' "$LOG"
grep -Fq 'pass=2 hostname.sh=2' "$LOG"

# 3. The pre-upgrade pass sourced only what it needed to run brew, and did no
# post-upgrade work: no migrations, apps, or hostname handling at the old
# version.
! grep -Eq 'pass=1 (migrate|apps|hostname)\.sh=' "$LOG"

# 4. The re-exec neither loops nor drops the rest of the run: the safe steps
# execute once, from the upgraded tree, and the script reaches its end.
for step in setup-gh setup-zsh setup-bin setup-zsh-functions; do
    [[ "$(grep -Fc "step $step=2" "$LOG")" == 1 ]]
    ! grep -Fq "step $step=1" "$LOG"
done
[[ "$(grep -Fc 'Update complete.' "$TEST_DIR/update.out")" == 1 ]]
[[ "$(cat "$PASSFILE")" == 2 ]]
[[ ! -e "$TEST_DIR/agent-bot.log" ]]
cmp -s "$HOME/.config/managed-machine/agent-bot-gh-interposer" "$TEST_DIR/interposer.before"

# 4a. brew update and the managed-machine upgrade share one authorization
# phase, and every step of the run sees the same declined-dialog marker,
# which is gone once the run ends (#174).
[[ "$(grep -c '^phase ' "$PHASES")" == 1 ]]
grep -q '^phase .*brew update && { .*brew upgrade managed-machine' "$PHASES"
declined_file="$(sed -n 's/^declined-file setup-gh=//p' "$LOG")"
[[ "$declined_file" == */declined ]]
for step in setup-zsh setup-bin setup-zsh-functions; do
    grep -Fxq "declined-file $step=$declined_file" "$LOG"
done
[[ ! -e "$(dirname "$declined_file")" ]]

# 4b. The safe step list contains only independently managed setup.
[[ "$(grep -o '^step setup-[a-z-]*' "$LOG" | sed 's/^step //' | tr '\n' ' ')" == 'setup-gh setup-zsh setup-bin setup-zsh-functions ' ]]
real_steps="$(sed -n '/^SAFE_STEPS=(/,/^)/p' "$ROOT/legacy/scripts/update" | sed -n 's/^    \(setup-[a-z-]*\)$/\1/p' | tr '\n' ' ')"
[[ "$real_steps" == 'setup-gh setup-zsh setup-bin setup-zsh-functions ' ]]

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
for step in setup-gh setup-zsh setup-bin setup-zsh-functions; do
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
! grep -q "setup-agent-bot" "$manifest"
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

# 8. Post-re-exec plan, single authorization, and persistent declined catalog
# rows (#174):
# - With multiple privileged catalog rows, --update uses one osascript call
#   for the pre-re-exec brew update/upgrade and one osascript call after
#   re-exec for the entire approved update plan (2 total across the run).
# - Cancelling the post-re-exec dialog stops privileged work and records the
#   declined catalog rows in declined-apps.manifest so the next --update lists
#   them as `not installed (declined)` and does not prompt for them again,
#   until the owner runs `managed-machine setup <name>`.
PLAN_TEST="$TEST_DIR/plan-test"
PLAN_LIBEXEC="$PLAN_TEST/libexec"
PLAN_HOME="$PLAN_TEST/home"
PLAN_CONFIG_REPO="$PLAN_TEST/managed-machine-config"
PLAN_BIN="$PLAN_TEST/bin"
PLAN_APPS="$PLAN_TEST/Applications"
OSA_LOG="$PLAN_TEST/osascript.log"
export OSA_LOG
mkdir -p "$PLAN_LIBEXEC/lib" "$PLAN_LIBEXEC/scripts" "$PLAN_HOME/.config/managed-machine" "$PLAN_CONFIG_REPO" "$PLAN_BIN" "$PLAN_APPS"
cp "$ROOT/legacy/scripts/update" "$PLAN_LIBEXEC/scripts/update"
cp "$ROOT"/legacy/lib/*.sh "$PLAN_LIBEXEC/lib/"
cp "$ROOT/legacy/lib/elevate-plan-run" "$PLAN_LIBEXEC/lib/elevate-plan-run"
cp "$ROOT/legacy/lib/brew-github-auth-run" "$PLAN_LIBEXEC/lib/brew-github-auth-run"
cp "$ROOT/legacy/lib/npm-public-run" "$PLAN_LIBEXEC/lib/npm-public-run"
chmod +x "$PLAN_LIBEXEC/scripts/update" "$PLAN_LIBEXEC/lib/elevate-plan-run" "$PLAN_LIBEXEC/lib/brew-github-auth-run" "$PLAN_LIBEXEC/lib/npm-public-run"
for step in setup-gh setup-zsh setup-bin setup-zsh-functions; do
    cat >"$PLAN_LIBEXEC/$step" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
    chmod +x "$PLAN_LIBEXEC/$step"
done

git init --quiet "$PLAN_CONFIG_REPO"
git -C "$PLAN_CONFIG_REPO" config user.name 'managed-machine test'
git -C "$PLAN_CONFIG_REPO" config user.email 'managed-machine-test@example.invalid'
git -C "$PLAN_CONFIG_REPO" config commit.gpgsign false
printf 'v0.1.0\n' >"$PLAN_CONFIG_REPO/local-bin.ref"
cat >"$PLAN_CONFIG_REPO/apps.json" <<'EOF'
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
git -C "$PLAN_CONFIG_REPO" add . && git -C "$PLAN_CONFIG_REPO" commit --quiet -m seed

cat >"$PLAN_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$OSA_LOG"
count="$(wc -l <"$OSA_LOG" | tr -d '[:space:]')"
if [[ -n "${OSA_CANCEL_ON_CALL:-}" && "$count" == "$OSA_CANCEL_ON_CALL" ]]; then
    printf 'execution error: User canceled. (-128)\n' >&2
    exit 1
fi
exit 0
EOF
cat >"$PLAN_BIN/uname" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "-s" ]]; then
    echo Darwin
else
    /usr/bin/uname "$@"
fi
EOF
cat >"$PLAN_BIN/gh" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "auth" && "${2:-}" == "token" ]]; then
    echo "gho_testtoken"
    exit 0
fi
exit 0
EOF
cat >"$PLAN_BIN/brew" <<EOF
#!/usr/bin/env bash
case "\$1" in
    --prefix) echo /opt/homebrew ;;
    update|upgrade) exit 0 ;;
    info)
        if [[ "\$*" == *homebrew/core/taplo* ]]; then
            printf '%s\n' '{"formulae":[{"name":"taplo","tap":"homebrew/core"}]}'
        elif [[ "\$*" == *homebrew/cask/cursor* ]]; then
            printf '%s\n' '{"casks":[{"token":"cursor","tap":"homebrew/cask","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","url":"https://downloader.cursor.sh/Cursor.dmg","homepage":"https://cursor.com"}]}'
        fi
        ;;
    list)
        if [[ "\$*" == *managed-machine* ]]; then
            echo 'managed-machine 0.7.11'
            exit 0
        elif [[ "\$*" == *--cask* && -f '$PLAN_TEST/cursor.receipt' ]]; then
            echo 'cursor 1.0.0'
            exit 0
        elif [[ "\$*" != *--cask* && -f '$PLAN_TEST/taplo.receipt' ]]; then
            echo 'taplo 0.9.3'
            exit 0
        fi
        exit 1
        ;;
    install)
        if [[ "\$*" == *homebrew/core/taplo* ]]; then
            : >'$PLAN_TEST/taplo.receipt'
        elif [[ "\$*" == *homebrew/cask/cursor* ]]; then
            mkdir -p '$PLAN_APPS/Cursor.app'
            : >'$PLAN_TEST/cursor.receipt'
        fi
        ;;
esac
EOF
cat >"$PLAN_BIN/codesign" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "-dv" ]]; then
    echo "Authority=Developer ID Application: Anysphere (VDXQ22DGB9)"
    echo "TeamIdentifier=VDXQ22DGB9"
    exit 0
fi
exit 0
EOF
chmod +x "$PLAN_BIN/osascript" "$PLAN_BIN/uname" "$PLAN_BIN/gh" "$PLAN_BIN/brew" "$PLAN_BIN/codesign"

# 8a. Cancelling the post-re-exec authorization dialog (call 2) stops the rest
# of the run's privileged work without prompting again and records the declined
# catalog rows in declined-apps.manifest.
: >"$OSA_LOG"
env -u CLAUDECODE \
    HOME="$PLAN_HOME" \
    MANAGED_MACHINE_CONFIG_DIR="$PLAN_CONFIG_REPO" \
    MANAGED_MACHINE_PLAN_CONFIRM=y \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$PLAN_APPS" \
    OSA_CANCEL_ON_CALL=2 \
    PATH="$PLAN_BIN:$PATH" \
    "$PLAN_LIBEXEC/scripts/update" >"$PLAN_TEST/cancel-update.out" 2>&1
# Call 1 = pre-re-exec brew update/upgrade; Call 2 = post-re-exec plan dialog (cancelled).
# Neither taplo nor cursor opens a 3rd dialog.
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "2" ]]
[[ -f "$PLAN_HOME/.config/managed-machine/declined-apps.manifest" ]]
grep -q '^taplo	' "$PLAN_HOME/.config/managed-machine/declined-apps.manifest"
grep -q '^cursor	' "$PLAN_HOME/.config/managed-machine/declined-apps.manifest"

# 8b. Re-running --update lists declined rows as `not installed (declined)` in
# the plan and does not offer or prompt for them again (only 1 osascript call,
# for the pre-re-exec brew update/upgrade).
: >"$OSA_LOG"
env -u CLAUDECODE \
    HOME="$PLAN_HOME" \
    MANAGED_MACHINE_CONFIG_DIR="$PLAN_CONFIG_REPO" \
    MANAGED_MACHINE_PLAN_CONFIRM=y \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$PLAN_APPS" \
    PATH="$PLAN_BIN:$PATH" \
    "$PLAN_LIBEXEC/scripts/update" >"$PLAN_TEST/second-update.out" 2>&1
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "1" ]]
grep -Fq 'setup-taplo' "$PLAN_TEST/second-update.out"
grep -Fq 'not installed (declined)' "$PLAN_TEST/second-update.out"

# 8c. Explicitly running `managed-machine setup <name>` clears the decline;
# when both rows are cleared and --update runs with approval, all privileged
# post-re-exec steps run under a single osascript dialog (2 total across the
# entire --update run) and record the plan in update.manifest.
(
    export HOME="$PLAN_HOME" CONFIG_REPO_ROOT="$PLAN_CONFIG_REPO"
    # shellcheck source=lib/install.sh
    source "$PLAN_LIBEXEC/lib/install.sh"
    # shellcheck source=lib/apps.sh
    source "$PLAN_LIBEXEC/lib/apps.sh"
    clear_declined_app taplo
    clear_declined_app cursor
)
: >"$OSA_LOG"
env -u CLAUDECODE \
    HOME="$PLAN_HOME" \
    MANAGED_MACHINE_CONFIG_DIR="$PLAN_CONFIG_REPO" \
    MANAGED_MACHINE_PLAN_CONFIRM=y \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$PLAN_APPS" \
    PATH="$PLAN_BIN:$PATH" \
    "$PLAN_LIBEXEC/scripts/update" >"$PLAN_TEST/approved-update.out" 2>&1
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "2" ]]
grep -Fq 'apply approved update plan update-' "$OSA_LOG"
[[ -f "$PLAN_TEST/taplo.receipt" ]]
[[ -d "$PLAN_APPS/Cursor.app" ]]
update_manifest="$PLAN_HOME/.config/managed-machine/update.manifest"
update_plan_id="$(sed -n 's/^plan_id=//p' "$update_manifest")"
[[ -n "$update_plan_id" ]]
grep -Fq "complete	setup-taplo	plan=$update_plan_id:" "$update_manifest"
grep -Fq "complete	setup-cursor	plan=$update_plan_id:" "$update_manifest"

# 8d. Running --update </dev/null with privileged actions and no
# MANAGED_MACHINE_PLAN_CONFIRM fails closed on the terminal confirmation
# instead of defaulting to 'y' or opening the post-re-exec authorization dialog.
rm -f "$PLAN_TEST/taplo.receipt"
rm -rf "$PLAN_APPS/Cursor.app"
: >"$OSA_LOG"
if env -u CLAUDECODE \
    HOME="$PLAN_HOME" \
    MANAGED_MACHINE_CONFIG_DIR="$PLAN_CONFIG_REPO" \
    MANAGED_MACHINE_ASSUME_SYSTEM_BREW=1 \
    MANAGED_MACHINE_BREW_OWNER=otheradmin \
    MANAGED_MACHINE_BREW_PREFIX_OWNER=otheradmin \
    MANAGED_MACHINE_SYSTEM_APPDIR="$PLAN_APPS" \
    PATH="$PLAN_BIN:$PATH" \
    "$PLAN_LIBEXEC/scripts/update" </dev/null >"$PLAN_TEST/eof-update.out" 2>&1; then
    echo 'expected update </dev/null with privileged plan and no confirmation to exit nonzero' >&2
    exit 1
fi
[[ "$(wc -l <"$OSA_LOG" | tr -d '[:space:]')" == "1" ]]
grep -Fq 'declined — no changes made.' "$PLAN_TEST/eof-update.out"
[[ ! -f "$PLAN_TEST/taplo.receipt" ]]

# 9. S8 D3(e) cross-layout upgrade (v0.10.x flat libexec -> v0.11.0 inverted
# libexec/legacy with the transitional libexec/scripts/update shim):
# Pass 1 starts from $LIBEXEC/scripts/update with libraries at $LIBEXEC/lib;
# `brew upgrade` swaps $LIBEXEC to the inverted layout (removing $LIBEXEC/lib
# and $LIBEXEC/setup-*, placing the real tree under $LIBEXEC/legacy/ and the
# transitional shim at $LIBEXEC/scripts/update). The re-exec of $ROOT/scripts/update
# lands on the shim, which execs $LIBEXEC/legacy/scripts/update so Pass 2 runs
# cleanly from $LIBEXEC/legacy with zero version mixing.
: >"$LOG"
: >"$PHASES"
rm -f "$PASSFILE"
"$WRITE_TREE" 1

WRITE_INVERTED_TREE="$TEST_DIR/write-inverted-tree"
export WRITE_INVERTED_TREE
cat >"$WRITE_INVERTED_TREE" <<'GEN_INV'
#!/usr/bin/env bash
set -euo pipefail
v="$1"
rm -rf "$LIBEXEC/lib" "$LIBEXEC"/setup-*
mkdir -p "$LIBEXEC/legacy/lib" "$LIBEXEC/legacy/scripts" "$LIBEXEC/scripts"
cp "$LIBEXEC/scripts/update" "$LIBEXEC/legacy/scripts/update"
chmod +x "$LIBEXEC/legacy/scripts/update"
cat >"$LIBEXEC/scripts/update" <<'SHIM'
#!/usr/bin/env bash
exec "$(dirname "$0")/../legacy/scripts/update" "$@"
SHIM
chmod +x "$LIBEXEC/scripts/update"

for lib in install bootstrap migrate apps hostname; do
    {
        printf 'if [[ -z "${UPDATE_PASS:-}" ]]; then\n'
        printf '    UPDATE_PASS=$(( $(cat "$PASSFILE" 2>/dev/null || echo 0) + 1 ))\n'
        printf '    printf %%s "$UPDATE_PASS" >"$PASSFILE"\n'
        printf 'fi\n'
        printf 'printf "pass=%%s %s.sh=%s root=%%s\\n" "$UPDATE_PASS" "$ROOT" >>"$LOG"\n' "$lib" "$v"
    } >"$LIBEXEC/legacy/lib/$lib.sh"
done
cat >>"$LIBEXEC/legacy/lib/install.sh" <<'HELPERS'
brew_run() { brew "$@"; }
brew_run_script() { printf 'phase %s\n' "$1" >>"$PHASES"; /bin/sh -c "$1"; }
managed_machine_config_repo_dir() { printf '%s\n' "$CONFIG_DIR"; }
managed_machine_config_dir() { printf '%s\n' "$CONFIG_DIR"; }
managed_machine_agent_session() { [[ "${CLAUDECODE:-}" == 1 ]]; }
catalog_auto_app_names() { :; }
install_catalog_app() { :; }
HELPERS
printf 'MANAGED_MACHINE_DEFERRED_EXIT=75\nMANAGED_MACHINE_SKIPPED_EXIT=76\n' >>"$LIBEXEC/legacy/lib/bootstrap.sh"
printf 'run_managed_machine_migrations() { :; }\n' >>"$LIBEXEC/legacy/lib/migrate.sh"

for step in setup-gh setup-zsh setup-bin setup-zsh-functions; do
    {
        printf '#!/usr/bin/env bash\n'
        printf 'printf "step %s=%s\\n" >>"$LOG"\n' "$step" "$v"
        printf 'exit 0\n'
    } >"$LIBEXEC/legacy/$step"
    chmod +x "$LIBEXEC/legacy/$step"
done
GEN_INV
chmod +x "$WRITE_INVERTED_TREE"

cat >"$STUB_BIN/brew" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "upgrade" ]]; then
    "$WRITE_INVERTED_TREE" 2-inverted
fi
exit 0
STUB
chmod +x "$STUB_BIN/brew"

env -u CLAUDECODE PATH="$STUB_BIN:$PATH" "$LIBEXEC/scripts/update" >"$TEST_DIR/cross-layout.out" 2>&1 || {
    echo 'cross-layout update failed' >&2
    cat "$TEST_DIR/cross-layout.out" >&2
    exit 1
}
grep -Fq 'pass=1 install.sh=1' "$LOG"
! grep -Eq 'pass=1 (migrate|apps|hostname)\.sh=' "$LOG"
for lib in install bootstrap migrate apps hostname; do
    grep -Fq "pass=2 $lib.sh=2-inverted root=$LIBEXEC/legacy" "$LOG"
done
for step in setup-gh setup-zsh setup-bin setup-zsh-functions; do
    [[ "$(grep -Fc "step $step=2-inverted" "$LOG")" == 1 ]]
done
[[ "$(grep -Fc 'Update complete.' "$TEST_DIR/cross-layout.out")" == 1 ]]
[[ "$(cat "$PASSFILE")" == 2 ]]

echo 'update re-exec tests passed'

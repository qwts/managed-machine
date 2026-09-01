#!/usr/bin/env bash
# scripts/update upgrades the very tree it is running from: ROOT resolves
# through $prefix/opt/managed-machine/libexec, and `brew upgrade` repoints that
# symlink mid-run. Sourcing libraries on both sides of the swap leaves one
# process holding two versions at once, so any release that changes a contract
# between two libraries breaks the upgrading run — and only that run. This test
# pins the invariant: every library a single update pass sources comes from one
# version of the tree.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

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
for lib in install agent-bot-gh migrate apps hostname; do
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
catalog_auto_app_names() { :; }
install_catalog_app() { :; }
HELPERS
printf 'run_managed_machine_migrations() { :; }\n' >>"$LIBEXEC/lib/migrate.sh"
printf 'agent_bot_gh_is_configured() { return 1; }\n' >>"$LIBEXEC/lib/agent-bot-gh.sh"

for step in setup-gh setup-zsh setup-bin; do
    {
        printf '#!/usr/bin/env bash\n'
        printf 'printf "step %s=%s\\n" >>"$LOG"\n' "$step" "$v"
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
for step in setup-gh setup-zsh setup-bin; do
    [[ "$(grep -Fc "step $step=2" "$LOG")" == 1 ]]
    ! grep -Fq "step $step=1" "$LOG"
done
[[ "$(grep -Fc 'Update complete.' "$TEST_DIR/update.out")" == 1 ]]
[[ "$(cat "$PASSFILE")" == 2 ]]

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

echo 'update re-exec tests passed'

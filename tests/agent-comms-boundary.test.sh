#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

FIXTURE="$TEST_DIR/tree"
HOME_DIR="$TEST_DIR/home"
CONFIG_DIR="$HOME_DIR/.config/managed-machine"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
BIN="$TEST_DIR/bin"
RUN_LOG="$TEST_DIR/setup.log"
COMMS_LOG="$TEST_DIR/agent-comms.log"
BREW_LOG="$TEST_DIR/brew.log"

mkdir -p "$FIXTURE/scripts" "$FIXTURE/lib" "$HOME_DIR" "$CONFIG_DIR" "$CONFIG_REPO" "$BIN"
cp "$ROOT/scripts/bootstrap" "$FIXTURE/scripts/bootstrap"
cp "$ROOT/scripts/update" "$FIXTURE/scripts/update"
chmod +x "$FIXTURE/scripts/bootstrap" "$FIXTURE/scripts/update"

# Keep the real entrypoints while supplying the small library contract they
# need. Setup commands are fixture recorders: this tests entrypoint scheduling
# and command dispatch, not the internals of production setup scripts.
cat >"$FIXTURE/lib/install.sh" <<'LIB'
managed_machine_config_repo_dir() { printf '%s\n' "$CONFIG_REPO_ROOT"; }
managed_machine_config_dir() { printf '%s\n' "$CONFIG_DIR"; }
managed_machine_agent_session() { return 1; }
brew_run() { brew "$@"; }
LIB
cat >"$FIXTURE/lib/bootstrap.sh" <<'LIB'
MANAGED_MACHINE_DEFERRED_EXIT=75
MANAGED_MACHINE_SKIPPED_EXIT=76
bootstrap_resolve_mode() { printf '%s\n' noninteractive; }
bootstrap_status_file() { printf '%s\n' "$CONFIG_DIR/bootstrap.manifest"; }
bootstrap_noninteractive_skip_reason() { return 1; }
LIB
cat >"$FIXTURE/lib/hostname.sh" <<'LIB'
:
LIB
cat >"$FIXTURE/lib/migrate.sh" <<'LIB'
run_managed_machine_migrations() { return 0; }
LIB
cat >"$FIXTURE/lib/apps.sh" <<'LIB'
catalog_auto_app_names() { :; }
install_catalog_app() { printf 'catalog:%s\n' "$1" >>"$RUN_LOG"; }
LIB
cat >"$FIXTURE/lib/agent-bot-gh.sh" <<'LIB'
agent_bot_gh_is_configured() { return 1; }
LIB

# Derive fixture setup stubs from the production lists so unrelated setup
# additions or agent-bot removals do not freeze this test's scope.
real_bootstrap_steps="$(sed -n '/^SETUP_SCRIPTS=(/,/^)/p' "$ROOT/scripts/bootstrap" | sed -n 's/^    \(setup-[a-z-]*\)$/\1/p')"
real_update_steps="$(sed -n '/^SAFE_STEPS=(/,/^)/p' "$ROOT/scripts/update" | sed -n 's/^    \(setup-[a-z-]*\)$/\1/p')"
for step in $real_bootstrap_steps $real_update_steps; do
    case "$step" in
        *agent-comms*|*agent_comms*)
            echo "forbidden provisioning step is scheduled: $step" >&2
            exit 1
            ;;
    esac
done
SETUP_SCRIPTS=()
for step in $real_bootstrap_steps $real_update_steps; do
    found=0
    for seen in "${SETUP_SCRIPTS[@]:-}"; do
        [[ "$seen" == "$step" ]] && found=1
    done
    [[ "$found" == 1 ]] || SETUP_SCRIPTS+=("$step")
done
for name in "${SETUP_SCRIPTS[@]}"; do
    cat >"$FIXTURE/$name" <<SCRIPT
#!/usr/bin/env bash
    printf '%s\\n' '$name' >>"\$RUN_LOG"
case "\${MOCK_PROVISION_MODE:-}" in
    direct) agent-comms service install ;;
    brew) brew install agent-comms ;;
esac
SCRIPT
    chmod +x "$FIXTURE/$name"
done

cat >"$BIN/brew" <<'BREW'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$BREW_LOG"
if [[ "${1:-}" == list ]]; then exit 0; fi
exit 0
BREW
chmod +x "$BIN/brew"

run_entrypoints() {
    local label="$1" path="$2"
    : >"$RUN_LOG"
    : >"$COMMS_LOG"
    : >"$BREW_LOG"
    HOME="$HOME_DIR" CONFIG_REPO_ROOT="$CONFIG_REPO" CONFIG_DIR="$CONFIG_DIR" \
        RUN_LOG="$RUN_LOG" COMMS_LOG="$COMMS_LOG" BREW_LOG="$BREW_LOG" \
        PATH="$path" \
        "$FIXTURE/scripts/bootstrap" --non-interactive >"$TEST_DIR/$label-bootstrap.out" 2>&1
    grep -qxF setup-bin "$RUN_LOG"
    grep -q $'^complete\tsetup-bin\t' "$CONFIG_DIR/bootstrap.manifest"
    ! grep -Eiq 'agent[-_]comms' "$RUN_LOG"

    : >"$RUN_LOG"
    HOME="$HOME_DIR" CONFIG_REPO_ROOT="$CONFIG_REPO" CONFIG_DIR="$CONFIG_DIR" \
        RUN_LOG="$RUN_LOG" COMMS_LOG="$COMMS_LOG" BREW_LOG="$BREW_LOG" \
        MANAGED_MACHINE_UPDATE_REEXECED=1 \
        MOCK_PROVISION_MODE="${MOCK_PROVISION_MODE:-}" PATH="$path" \
        "$FIXTURE/scripts/update" >"$TEST_DIR/$label-update.out" 2>&1
    grep -qxF setup-bin "$RUN_LOG"
    grep -q $'^complete\tsetup-bin\t' "$CONFIG_DIR/update.manifest"
    ! grep -Eiq 'agent[-_]comms' "$RUN_LOG"
}

assert_no_provisioning() {
    if [[ -s "$COMMS_LOG" ]] || \
        grep -Eiq '(^|[[:space:]])(install|upgrade|tap)([[:space:]]|$).*(agent-comms)|(agent-comms).*(^|[[:space:]])(install|upgrade|tap)([[:space:]]|$)' "$BREW_LOG"; then
        echo 'entrypoint fixture attempted agent-comms provisioning' >&2
        cat "$COMMS_LOG" "$BREW_LOG" >&2
        return 1
    fi
}

# A new machine has no agent-comms command available, yet both entrypoints
# complete unrelated setup without trying to provision it.
if PATH="$BIN:/usr/bin:/bin" command -v agent-comms >/dev/null 2>&1; then
    echo 'agent-comms unexpectedly available in the absent fixture' >&2
    exit 1
fi
run_entrypoints absent "$BIN:/usr/bin:/bin"
assert_no_provisioning

# Model an independently managed installation. Any accidental call records
# itself; the existing executable's contents must survive both setup paths.
cat >"$BIN/agent-comms" <<EOF
#!/usr/bin/env bash
printf '%s\\n' "\$*" >>"$COMMS_LOG"
exit 0
EOF
chmod +x "$BIN/agent-comms"
cp "$BIN/agent-comms" "$TEST_DIR/agent-comms.before"
run_entrypoints external "$BIN:/usr/bin:/bin"
assert_no_provisioning
cmp -s "$TEST_DIR/agent-comms.before" "$BIN/agent-comms"

# Mutation checks prove the recorder assertions reject both a direct service
# command and Homebrew installation injected into a scheduled setup step.
for mutation in direct brew; do
    MOCK_PROVISION_MODE="$mutation" run_entrypoints "mutation-$mutation" "$BIN:/usr/bin:/bin"
    if assert_no_provisioning >"$TEST_DIR/mutation-$mutation.assert" 2>&1; then
        echo "boundary assertion failed to detect the $mutation mutation" >&2
        exit 1
    fi
    grep -Fq 'entrypoint fixture attempted' "$TEST_DIR/mutation-$mutation.assert"
    case "$mutation" in
        direct) grep -Fq 'service install' "$TEST_DIR/mutation-$mutation.assert" ;;
        brew) grep -Fq 'install agent-comms' "$TEST_DIR/mutation-$mutation.assert" ;;
    esac
done

echo 'agent-comms provisioning boundary tests passed'

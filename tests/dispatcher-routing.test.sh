#!/usr/bin/env bash
# S6 R03 (#329): characterize bin/managed-machine routing per D5 using a
# MANAGED_MACHINE_CORE stub that records argv and environment, plus default
# $ROOT/core/managed-machine resolution, MANAGED_MACHINE_POLICY_DIR export,
# core-missing error envelopes, and terminal-only deprecation notices.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

FIX="$TEST_ROOT/fixture"
EMPTY_HOME="$TEST_ROOT/home"
CORE_STUB="$TEST_ROOT/core-stub"
CORE_ARGV_LOG="$TEST_ROOT/core-argv.log"
CORE_ENV_LOG="$TEST_ROOT/core-env.log"
LEGACY_LOG="$TEST_ROOT/legacy.log"
SOURCED_LOG="$TEST_ROOT/sourced.log"

mkdir -p "$FIX/bin" "$FIX/lib" "$FIX/scripts" "$FIX/skills/managed-machine" "$EMPTY_HOME"
cp "$ROOT/bin/managed-machine" "$FIX/bin/managed-machine"
cp "$ROOT/lib/setup-cutover.sh" "$FIX/lib/setup-cutover.sh"
chmod +x "$FIX/bin/managed-machine"
printf '0.9.0\n' >"$FIX/VERSION"
printf '0123456789abcdef0123456789abcdef01234567\n' >"$FIX/RELEASE_COMMIT"
printf '# fixture skill\n' >"$FIX/skills/managed-machine/SKILL.md"

cat >"$FIX/lib/install.sh" <<'EOF'
printf 'install.sh sourced\n' >>"${MM_SOURCED_LOG:-/dev/null}"
managed_machine_config_repo_dir() { printf '%s\n' "$REPO_ROOT/no-config"; }
refresh_managed_machine_config_repo() { :; }
setup_name_installed() { return 1; }
EOF

for s in status catalog fleet adopt add-agent account ssh bootstrap update; do
    cat >"$FIX/scripts/$s" <<EOF
#!/usr/bin/env bash
printf 'legacy:%s' '$s' >>'$LEGACY_LOG'
for a in "\$@"; do
    printf '\t%s' "\$a" >>'$LEGACY_LOG'
done
printf '\n' >>'$LEGACY_LOG'
EOF
    chmod +x "$FIX/scripts/$s"
done

for s in setup-alpha setup-cursor; do
    cat >"$FIX/$s" <<EOF
#!/usr/bin/env bash
printf 'legacy:%s' '$s' >>'$LEGACY_LOG'
for a in "\$@"; do
    printf '\t%s' "\$a" >>'$LEGACY_LOG'
done
printf '\n' >>'$LEGACY_LOG'
EOF
    chmod +x "$FIX/$s"
done

cat >"$CORE_STUB" <<EOF
#!/usr/bin/env bash
printf 'argc=%d\n' "\$#" >'$CORE_ARGV_LOG'
for a in "\$@"; do
    printf 'arg=%s\n' "\$a" >>'$CORE_ARGV_LOG'
done
if [[ -n "\${MANAGED_MACHINE_POLICY_DIR+x}" ]]; then
    printf 'policy=%s\n' "\$MANAGED_MACHINE_POLICY_DIR" >'$CORE_ENV_LOG'
else
    printf 'policy=<unset>\n' >'$CORE_ENV_LOG'
fi
if [[ -n "\${MANAGED_MACHINE_SOURCE_DIR+x}" ]]; then
    printf 'source=%s\n' "\$MANAGED_MACHINE_SOURCE_DIR" >>'$CORE_ENV_LOG'
else
    printf 'source=<unset>\n' >>'$CORE_ENV_LOG'
fi
EOF
chmod +x "$CORE_STUB"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

reset_logs() {
    : >"$CORE_ARGV_LOG"
    : >"$CORE_ENV_LOG"
    : >"$LEGACY_LOG"
    : >"$SOURCED_LOG"
}

assert_new_core_route() {
    reset_logs
    HOME="$EMPTY_HOME" MANAGED_MACHINE_CORE="$CORE_STUB" \
        MM_SOURCED_LOG="$SOURCED_LOG" \
        "$FIX/bin/managed-machine" "$@" >/dev/null 2>&1
    [[ ! -s "$LEGACY_LOG" ]] || fail "new-core route '$*' unexpectedly ran legacy script: $(cat "$LEGACY_LOG")"
    [[ ! -s "$SOURCED_LOG" ]] || fail "new-core route '$*' unexpectedly sourced lib/install.sh"
    local expected="$TEST_ROOT/expected-argv"
    printf 'argc=%d\n' "$#" >"$expected"
    local a
    for a in "$@"; do
        printf 'arg=%s\n' "$a" >>"$expected"
    done
    diff -u "$expected" "$CORE_ARGV_LOG" || fail "new-core route '$*' altered argv"
}

assert_legacy_route() {
    reset_logs
    HOME="$EMPTY_HOME" MANAGED_MACHINE_CORE="$CORE_STUB" \
        MM_SOURCED_LOG="$SOURCED_LOG" \
        "$FIX/bin/managed-machine" "$@" >/dev/null 2>&1 || true
    [[ ! -s "$CORE_ARGV_LOG" ]] || fail "legacy route '$*' unexpectedly invoked core stub: $(cat "$CORE_ARGV_LOG")"
}

# --- 1. D5 new-core routes receive the untouched argument vector --------------
assert_new_core_route software
assert_new_core_route software --available --json
assert_new_core_route software ripgrep --recipes --human
assert_new_core_route software --install ripgrep --user
assert_new_core_route software --refresh
assert_new_core_route config
assert_new_core_route config --available --json
assert_new_core_route config gitconfig --plan --user --custom "$TEST_ROOT/path with spaces"
assert_new_core_route config --apply --from-plan plan-1
assert_new_core_route config --check --user
assert_new_core_route config --refresh
assert_new_core_route status --section software
assert_new_core_route status --section config --user
assert_new_core_route status --section flavor workstation --json
assert_new_core_route status --section fleet authorized-keys --human
assert_new_core_route status --section=software
assert_new_core_route status --section=fleet --json
assert_new_core_route fleet --keys
assert_new_core_route fleet --access --json
assert_new_core_route fleet --refresh
assert_new_core_route fleet --plan
assert_new_core_route fleet --apply --from-plan plan-1
assert_new_core_route fleet --check
assert_new_core_route fleet --publish --owner alice --key "$TEST_ROOT/id_ed25519.pub" --label "work laptop"
assert_new_core_route fleet --unpublish SHA256:abc --owner alice
assert_new_core_route fleet --bogus --json
assert_new_core_route fleet alice
assert_new_core_route fleet alice --keys --human
assert_new_core_route skill
assert_new_core_route skill --json
assert_new_core_route skill list
assert_new_core_route skill list --json
assert_new_core_route skill managed-machine --kind reference
assert_new_core_route skill --search "ssh enroll"
assert_new_core_route platform
assert_new_core_route platform --json
assert_new_core_route version
assert_new_core_route version --json
assert_new_core_route version --human

# S7 D9: one forwarded argv per SetupCutover table row when <software>.yaml
# exists in the effective MANAGED_MACHINE_SOURCE_DIR, plus aliases, setup-
# prefixes, presentation flags, and setup cursor ahead of the setup-cursor script.
CUTOVER_ROWS=(
    antigravity-app antigravity-ide ast-grep brave chatgpt-app chrome
    claude-app cursor devin-app discord docker fd grokbot kiro kiro-cli
    lmstudio minikube opencode-app ripgrep slack taplo telegram uv vscode
    vscode-insiders warp zcode
)
CUTOVER_SOURCE_DIR="$TEST_ROOT/cutover-source"
mkdir -p "$CUTOVER_SOURCE_DIR"
for row in "${CUTOVER_ROWS[@]}"; do
    : >"$CUTOVER_SOURCE_DIR/$row.yaml"
done
for row in "${CUTOVER_ROWS[@]}"; do
    MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup "$row"
done
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup setup-warp
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup warp-terminal
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup setup-warp-terminal
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup sg
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup rg
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup code-insiders
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup visual-studio-code --json
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_new_core_route setup setup-cursor --human

# --- 2. D5 legacy routes never start the core stub ----------------------------
# S7 D9 safety guard: when <software>.yaml is absent from the effective
# MANAGED_MACHINE_SOURCE_DIR, every cutover name and alias stays on legacy Bash.
for row in "${CUTOVER_ROWS[@]}"; do
    assert_legacy_route setup "$row"
done
assert_legacy_route setup warp-terminal
assert_legacy_route setup sg
assert_legacy_route setup rg
assert_legacy_route setup code-insiders
assert_legacy_route setup visual-studio-code --json
assert_legacy_route setup cursor
[[ "$(cat "$LEGACY_LOG")" == "legacy:setup-cursor" ]] || fail "setup cursor without recipe did not run legacy setup-cursor script: $(cat "$LEGACY_LOG")"

# Even when all recipes exist, unconverted names, setup --list, and extra args stay legacy.
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup alpha
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup grok-build
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup qwen
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup --list
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup --list --json
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup warp extra
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup cursor extra
[[ "$(cat "$LEGACY_LOG")" == $'legacy:setup-cursor\textra' ]] || fail "setup cursor extra did not run legacy setup-cursor script: $(cat "$LEGACY_LOG")"
MANAGED_MACHINE_SOURCE_DIR="$CUTOVER_SOURCE_DIR" assert_legacy_route setup warp --user
assert_legacy_route catalog list --no-refresh
assert_legacy_route catalog resolve vscode --no-refresh
assert_legacy_route adopt
assert_legacy_route adopt vscode
assert_legacy_route add-agent claude
assert_legacy_route account setup qwts-claude-agent
assert_legacy_route account doctor qwts-claude-agent --json
assert_legacy_route ssh enroll --authentication
assert_legacy_route ssh status --json
assert_legacy_route --bootstrap --non-interactive
assert_legacy_route --update
assert_legacy_route --version
assert_legacy_route --help
assert_legacy_route -h
assert_legacy_route help
assert_legacy_route
assert_legacy_route skill path
assert_legacy_route skill path --json
assert_legacy_route fleet
assert_legacy_route fleet list
assert_legacy_route fleet list --json
assert_legacy_route fleet remove machine-1 --dry-run --json
assert_legacy_route status
assert_legacy_route status --json
assert_legacy_route status --section ssh,gh
assert_legacy_route status --section software,config
assert_legacy_route status --section software --section config

LEGACY_SECTIONS=(
    managed-machine machine ssh bootstrap update agent-accounts local-bin
    zsh-functions pins toolchain homebrew gh nvm node hostname rustup rustc
    cargo catalog
)
for sec in "${LEGACY_SECTIONS[@]}"; do
    assert_legacy_route status --section "$sec"
    assert_legacy_route status --section="$sec" --json
done

# --- 3. MANAGED_MACHINE_POLICY_DIR and MANAGED_MACHINE_SOURCE_DIR export rules
# 3a. Unset and computed checkout directory does not exist -> stays unset and is never created.
reset_logs
HOME="$EMPTY_HOME" MANAGED_MACHINE_CORE="$CORE_STUB" \
    "$FIX/bin/managed-machine" fleet --keys
[[ "$(cat "$CORE_ENV_LOG")" == $'policy=<unset>\nsource=<unset>' ]] || fail "expected policy/source=<unset> when checkout is absent, got $(cat "$CORE_ENV_LOG")"
[[ ! -e "$EMPTY_HOME/.local/share/managed-machine/managed-machine-config" ]] || fail "dispatcher created policy checkout directory"

# 3b. Unset and default $HOME/.local/share/managed-machine/managed-machine-config exists without software/ -> policy exported, source stays unset.
DEFAULT_POLICY="$EMPTY_HOME/.local/share/managed-machine/managed-machine-config"
mkdir -p "$DEFAULT_POLICY"
reset_logs
HOME="$EMPTY_HOME" MANAGED_MACHINE_CORE="$CORE_STUB" \
    "$FIX/bin/managed-machine" fleet --keys
[[ "$(cat "$CORE_ENV_LOG")" == $'policy='"$DEFAULT_POLICY"$'\nsource=<unset>' ]] || fail "expected policy=$DEFAULT_POLICY and source=<unset>, got $(cat "$CORE_ENV_LOG")"
[[ ! -e "$DEFAULT_POLICY/software" ]] || fail "dispatcher created software directory"

# 3b-source. Default checkout software/ directory exists -> both exported;
# cutover setup names route to core only when <software>.yaml exists in that directory.
mkdir -p "$DEFAULT_POLICY/software"
reset_logs
HOME="$EMPTY_HOME" MANAGED_MACHINE_CORE="$CORE_STUB" \
    "$FIX/bin/managed-machine" software --available
[[ "$(cat "$CORE_ENV_LOG")" == $'policy='"$DEFAULT_POLICY"$'\nsource='"$DEFAULT_POLICY/software" ]] || fail "expected policy and source exported for default checkout, got $(cat "$CORE_ENV_LOG")"
assert_legacy_route setup warp
assert_legacy_route setup warp-terminal
: >"$DEFAULT_POLICY/software/warp.yaml"
assert_new_core_route setup warp
assert_new_core_route setup warp-terminal
assert_legacy_route setup vscode
assert_legacy_route setup visual-studio-code

# 3c. Unset and XDG_DATA_HOME checkout + software/ exist -> exported.
XDG_DIR="$TEST_ROOT/xdg"
XDG_POLICY="$XDG_DIR/managed-machine/managed-machine-config"
mkdir -p "$XDG_POLICY/software"
reset_logs
HOME="$EMPTY_HOME" XDG_DATA_HOME="$XDG_DIR" MANAGED_MACHINE_CORE="$CORE_STUB" \
    "$FIX/bin/managed-machine" fleet --keys
[[ "$(cat "$CORE_ENV_LOG")" == $'policy='"$XDG_POLICY"$'\nsource='"$XDG_POLICY/software" ]] || fail "expected policy=$XDG_POLICY and source=$XDG_POLICY/software, got $(cat "$CORE_ENV_LOG")"

# 3d. Unset and CONFIG_REPO_ROOT + software/ exist -> exported.
CFG_OVERRIDE="$TEST_ROOT/cfg-override"
mkdir -p "$CFG_OVERRIDE/software"
reset_logs
HOME="$EMPTY_HOME" CONFIG_REPO_ROOT="$CFG_OVERRIDE" MANAGED_MACHINE_CORE="$CORE_STUB" \
    "$FIX/bin/managed-machine" fleet --keys
[[ "$(cat "$CORE_ENV_LOG")" == $'policy='"$CFG_OVERRIDE"$'\nsource='"$CFG_OVERRIDE/software" ]] || fail "expected policy=$CFG_OVERRIDE and source=$CFG_OVERRIDE/software, got $(cat "$CORE_ENV_LOG")"

# 3e. Pre-set MANAGED_MACHINE_POLICY_DIR and MANAGED_MACHINE_SOURCE_DIR are preserved untouched.
EXPLICIT_POLICY="$TEST_ROOT/explicit-policy"
EXPLICIT_SOURCE="$TEST_ROOT/explicit-source"
reset_logs
HOME="$EMPTY_HOME" CONFIG_REPO_ROOT="$CFG_OVERRIDE" MANAGED_MACHINE_POLICY_DIR="$EXPLICIT_POLICY" \
    MANAGED_MACHINE_SOURCE_DIR="$EXPLICIT_SOURCE" \
    MANAGED_MACHINE_CORE="$CORE_STUB" \
    "$FIX/bin/managed-machine" fleet --keys
[[ "$(cat "$CORE_ENV_LOG")" == $'policy='"$EXPLICIT_POLICY"$'\nsource='"$EXPLICIT_SOURCE" ]] || fail "expected explicit policy and source preserved, got $(cat "$CORE_ENV_LOG")"

# --- 4. Missing or non-executable core yields core-missing (exit 1, no fallback)
MISSING_CORE="$TEST_ROOT/no-such-core"
NONEXEC_CORE="$TEST_ROOT/nonexec-core"
printf '#!/usr/bin/env bash\nexit 0\n' >"$NONEXEC_CORE"
chmod 0644 "$NONEXEC_CORE"

for bad_core in "$MISSING_CORE" "$NONEXEC_CORE"; do
    for cmd in "platform" "status --section software" "fleet --keys" "version"; do
        reset_logs
        set +e
        # shellcheck disable=SC2086
        HOME="$EMPTY_HOME" MANAGED_MACHINE_CORE="$bad_core" \
            "$FIX/bin/managed-machine" $cmd >"$TEST_ROOT/out" 2>"$TEST_ROOT/err"
        rc=$?
        set -e
        [[ "$rc" -eq 1 ]] || fail "missing core plain ($cmd): expected exit 1, got $rc"
        [[ ! -s "$TEST_ROOT/out" ]] || fail "missing core plain ($cmd): expected empty stdout"
        [[ ! -s "$LEGACY_LOG" ]] || fail "missing core plain ($cmd): fell back to legacy"
        grep -Fq "Error: the managed-machine core is missing at $bad_core; run brew reinstall managed-machine" "$TEST_ROOT/err" \
            || fail "missing core plain ($cmd): unexpected stderr $(cat "$TEST_ROOT/err")"

        reset_logs
        set +e
        # shellcheck disable=SC2086
        HOME="$EMPTY_HOME" MANAGED_MACHINE_CORE="$bad_core" \
            "$FIX/bin/managed-machine" $cmd --json >"$TEST_ROOT/out.json" 2>"$TEST_ROOT/err"
        rc=$?
        set -e
        [[ "$rc" -eq 1 ]] || fail "missing core --json ($cmd): expected exit 1, got $rc"
        [[ ! -s "$TEST_ROOT/err" ]] || fail "missing core --json ($cmd): expected empty stderr, got $(cat "$TEST_ROOT/err")"
        [[ ! -s "$LEGACY_LOG" ]] || fail "missing core --json ($cmd): fell back to legacy"
        python3 - "$TEST_ROOT/out.json" "$bad_core" <<'PY' || fail "missing core --json envelope ($cmd)"
import json, sys
doc = json.load(open(sys.argv[1]))
want_path = sys.argv[2]
assert doc["schema_version"] == 1
assert doc["ready"] is False
assert doc["status"] == "not_ready"
assert doc["error"]["code"] == "core-missing", doc["error"]
assert doc["error"]["evidence"] == {"path": want_path}, doc["error"]
assert doc["checks"][0]["code"] == "core-missing"
assert doc["checks"][0]["evidence"] == {"path": want_path}
PY
    done
done

# --- 5. Default path $ROOT/core/managed-machine -------------------------------
# Before $FIX/core/managed-machine exists, unset MANAGED_MACHINE_CORE fails with core-missing at $FIX/core/managed-machine.
set +e
HOME="$EMPTY_HOME" "$FIX/bin/managed-machine" platform --json >"$TEST_ROOT/default-missing.json" 2>"$TEST_ROOT/default-missing.err"
rc=$?
set -e
[[ "$rc" -eq 1 && ! -s "$TEST_ROOT/default-missing.err" ]] || fail "default core missing should exit 1 on --json"
python3 - "$TEST_ROOT/default-missing.json" "$FIX/core/managed-machine" <<'PY' || fail "default core-missing path"
import json, sys
doc = json.load(open(sys.argv[1]))
assert doc["error"]["code"] == "core-missing"
assert doc["error"]["evidence"] == {"path": sys.argv[2]}
PY

mkdir -p "$FIX/core"
cp "$CORE_STUB" "$FIX/core/managed-machine"
chmod +x "$FIX/core/managed-machine"
reset_logs
HOME="$EMPTY_HOME" "$FIX/bin/managed-machine" version --json >/dev/null 2>&1
grep -Fq 'arg=version' "$CORE_ARGV_LOG" || fail "default $ROOT/core/managed-machine was not invoked"
grep -Fq 'arg=--json' "$CORE_ARGV_LOG" || fail "default $ROOT/core/managed-machine missed --json"

# --- 6. Terminal-only deprecation notices (D5, v0.11.0) -----------------------
# 6a. Absent under a pipe / non-terminal stderr.
for cmd in "status" "status --section ssh" "status --section catalog" "fleet list" "skill path"; do
    # shellcheck disable=SC2086
    HOME="$EMPTY_HOME" "$FIX/bin/managed-machine" $cmd >"$TEST_ROOT/pipe.out" 2>"$TEST_ROOT/pipe.err"
    [[ ! -s "$TEST_ROOT/pipe.err" ]] || fail "deprecation notice leaked to non-tty stderr for '$cmd': $(cat "$TEST_ROOT/pipe.err")"
done

# 6b. Present on a pseudo-terminal (`script` on macOS) only for bare status,
# status --section <legacy>, fleet list, and skill path, with exact D5 wording.
run_in_pty() {
    local out_file="$1"
    shift
    script -q "$out_file" env HOME="$EMPTY_HOME" "$FIX/bin/managed-machine" "$@" </dev/null >/dev/null 2>&1
    tr -d '\r' <"$out_file" >"${out_file}.clean"
}

run_in_pty "$TEST_ROOT/pty-status" status
grep -Fq "managed-machine: 'status' is a legacy route kept for compatibility until v0.11.0; use 'managed-machine status --section software|config|flavor|fleet' (see docs/plans/f01-caller-characterization.md)" \
    "$TEST_ROOT/pty-status.clean" || fail "missing PTY deprecation notice for bare status: $(cat "$TEST_ROOT/pty-status.clean")"

for sec in ssh gh catalog managed-machine; do
    run_in_pty "$TEST_ROOT/pty-status-$sec" status --section "$sec"
    grep -Fq "managed-machine: 'status --section $sec' is a legacy route kept for compatibility until v0.11.0; use 'managed-machine status --section software|config|flavor|fleet' (see docs/plans/f01-caller-characterization.md)" \
        "$TEST_ROOT/pty-status-$sec.clean" || fail "missing PTY deprecation notice for status --section $sec"
done

run_in_pty "$TEST_ROOT/pty-fleet-list" fleet list
grep -Fq "managed-machine: 'fleet list' is a legacy route kept for compatibility until v0.11.0; use 'managed-machine fleet --keys' (see docs/plans/f01-caller-characterization.md)" \
    "$TEST_ROOT/pty-fleet-list.clean" || fail "missing PTY deprecation notice for fleet list"

run_in_pty "$TEST_ROOT/pty-skill-path" skill path
grep -Fq "managed-machine: 'skill path' is a legacy route kept for compatibility until v0.11.0; use 'managed-machine skill' (see docs/plans/f01-caller-characterization.md)" \
    "$TEST_ROOT/pty-skill-path.clean" || fail "missing PTY deprecation notice for skill path"

# 6c. Routes without a v1 core replacement and new-core routes emit no deprecation notice on a PTY.
for cmd in "setup alpha" "catalog list" "adopt" "add-agent claude" "account doctor x" "ssh status" "fleet remove machine-1 --dry-run" "--bootstrap" "--update" "--version" "platform" "version" "status --section software" "fleet --keys" "skill"; do
    # shellcheck disable=SC2086
    run_in_pty "$TEST_ROOT/pty-no-dep" $cmd
    if grep -Fq "is a legacy route kept for compatibility until v0.11.0" "$TEST_ROOT/pty-no-dep.clean"; then
        fail "unexpected deprecation notice on PTY for '$cmd': $(cat "$TEST_ROOT/pty-no-dep.clean")"
    fi
done

echo "dispatcher-routing: ok"

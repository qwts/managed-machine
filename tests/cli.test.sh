#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
FIXTURE="$TEST_ROOT/fixture"
CLI="$FIXTURE/bin/managed-machine"
RUN_LOG="$TEST_ROOT/run.log"
REFRESH_LOG="$TEST_ROOT/refresh.log"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$FIXTURE/bin" "$FIXTURE/lib" "$FIXTURE/scripts"
cp "$ROOT/legacy/bin/managed-machine" "$CLI"
chmod +x "$CLI"
cat >"$FIXTURE/lib/install.sh" <<'EOF'
# Fixture lib: the real install detection lives in lib/apps.sh, which this
# fixture does not carry. Stub the probe so the ✓ marker path is exercised.
setup_name_installed() {
    [[ "$2" == "alpha" ]]
}
# Stand-ins for lib/config-repo.sh: a config refresh is recorded, never run.
managed_machine_config_repo_dir() {
    printf '%s\n' "$REPO_ROOT/../managed-machine-config"
}
refresh_managed_machine_config_repo() {
    printf 'refresh\n' >>"$REFRESH_LOG"
}
EOF
export REFRESH_LOG
cat >"$FIXTURE/scripts/bootstrap" <<EOF
#!/usr/bin/env bash
printf 'bootstrap' >>'$RUN_LOG'
printf '\t%s' "\$@" >>'$RUN_LOG'
printf '\n' >>'$RUN_LOG'
EOF
chmod +x "$FIXTURE/scripts/bootstrap"

write_setup_fixture() {
    local name="$1"
    cat >"$FIXTURE/setup-$name" <<EOF
#!/usr/bin/env bash
printf '%s' '$name' >>'$RUN_LOG'
if [[ \$# -gt 0 ]]; then
    printf '\t%s' "\$@" >>'$RUN_LOG'
fi
printf '\n' >>'$RUN_LOG'
EOF
    chmod +x "$FIXTURE/setup-$name"
}

write_setup_fixture alpha
write_setup_fixture beta-tool
for setup_script in "$ROOT"/legacy/setup-*; do
    [[ -f "$setup_script" && -x "$setup_script" ]] || continue
    setup_name="${setup_script##*/}"
    write_setup_fixture "${setup_name#setup-}"
done
printf '#!/usr/bin/env bash\n' >"$FIXTURE/setup-hidden"

"$CLI" setup alpha
"$CLI" setup setup-beta-tool
[[ "$(sed -n '1p' "$RUN_LOG")" == 'alpha' ]]
[[ "$(sed -n '2p' "$RUN_LOG")" == 'beta-tool' ]]

"$CLI" --bootstrap --non-interactive
[[ "$(sed -n '3p' "$RUN_LOG")" == $'bootstrap\t--non-interactive' ]]

cat >"$FIXTURE/scripts/adopt" <<EOF
#!/usr/bin/env bash
printf 'adopt' >>'$RUN_LOG'
if [[ \$# -gt 0 ]]; then
    printf '\t%s' "\$@" >>'$RUN_LOG'
fi
printf '\n' >>'$RUN_LOG'
EOF
chmod +x "$FIXTURE/scripts/adopt"
"$CLI" adopt
"$CLI" adopt vscode
[[ "$(sed -n '4p' "$RUN_LOG")" == 'adopt' ]]
[[ "$(sed -n '5p' "$RUN_LOG")" == $'adopt\tvscode' ]]

"$CLI" setup alpha --restore
[[ "$(sed -n '6p' "$RUN_LOG")" == $'alpha\t--restore' ]]

cat >"$FIXTURE/scripts/account" <<EOF
#!/usr/bin/env bash
printf 'account' >>'$RUN_LOG'
if [[ \$# -gt 0 ]]; then
    printf '\t%s' "\$@" >>'$RUN_LOG'
fi
printf '\n' >>'$RUN_LOG'
printf '%s' "\${ACCOUNT_OUTPUT:-}"
exit "\${ACCOUNT_EXIT:-0}"
EOF
chmod +x "$FIXTURE/scripts/account"
"$CLI" account setup qwts-claude-agent
"$CLI" account setup qwts-claude-agent --json
"$CLI" account doctor qwts-claude-agent
"$CLI" account doctor qwts-claude-agent --json
"$CLI" account --help
"$CLI" account
"$CLI" account doctor 'argument with spaces' --json
[[ "$(sed -n '7p' "$RUN_LOG")" == $'account\tsetup\tqwts-claude-agent' ]]
[[ "$(sed -n '8p' "$RUN_LOG")" == $'account\tsetup\tqwts-claude-agent\t--json' ]]
[[ "$(sed -n '9p' "$RUN_LOG")" == $'account\tdoctor\tqwts-claude-agent' ]]
[[ "$(sed -n '10p' "$RUN_LOG")" == $'account\tdoctor\tqwts-claude-agent\t--json' ]]
[[ "$(sed -n '11p' "$RUN_LOG")" == $'account\t--help' ]]
[[ "$(sed -n '12p' "$RUN_LOG")" == 'account' ]]
[[ "$(sed -n '13p' "$RUN_LOG")" == $'account\tdoctor\targument with spaces\t--json' ]]
for account_exit in 0 1 75; do
    actual_exit=0
    account_output="$(ACCOUNT_EXIT="$account_exit" ACCOUNT_OUTPUT='{"status":"stub"}' "$CLI" account doctor qwts-claude-agent --json)" || actual_exit=$?
    [[ "$actual_exit" -eq "$account_exit" ]]
    [[ "$account_output" == '{"status":"stub"}' ]]
done

cat >"$FIXTURE/scripts/ssh" <<EOF
#!/usr/bin/env bash
printf 'ssh' >>'$RUN_LOG'
if [[ \$# -gt 0 ]]; then
    printf '\t%s' "\$@" >>'$RUN_LOG'
fi
printf '\n' >>'$RUN_LOG'
EOF
chmod +x "$FIXTURE/scripts/ssh"
"$CLI" ssh enroll --authentication --fleet
"$CLI" ssh status
[[ "$(sed -n '14p' "$RUN_LOG")" == $'ssh\tenroll\t--authentication\t--fleet' ]]
[[ "$(sed -n '15p' "$RUN_LOG")" == $'ssh\tstatus' ]]

cat >"$FIXTURE/scripts/catalog" <<EOF
#!/usr/bin/env bash
printf 'catalog' >>'$RUN_LOG'
if [[ \$# -gt 0 ]]; then
    printf '\t%s' "\$@" >>'$RUN_LOG'
fi
printf '\n' >>'$RUN_LOG'
EOF
chmod +x "$FIXTURE/scripts/catalog"
"$CLI" catalog list --json
"$CLI" catalog resolve vscode --json
[[ "$(sed -n '16p' "$RUN_LOG")" == $'catalog\tlist\t--json' ]]
[[ "$(sed -n '17p' "$RUN_LOG")" == $'catalog\tresolve\tvscode\t--json' ]]

# S8 D3: non-legacy argv reaching legacy/bin/managed-machine exits 2 with an
# internal error without running anything or touching the config checkout.
cp "$RUN_LOG" "$TEST_ROOT/run.before"
for non_legacy in '' 'help' '-h' '--help' 'helpme' 'bootstrap' 'update' 'status' 'skill path'; do
    rm -f "$REFRESH_LOG"
    rc=0
    if [[ -z "$non_legacy" ]]; then
        "$CLI" >"$TEST_ROOT/bad.out" 2>"$TEST_ROOT/bad.err" || rc=$?
    else
        # shellcheck disable=SC2086
        "$CLI" $non_legacy >"$TEST_ROOT/bad.out" 2>"$TEST_ROOT/bad.err" || rc=$?
    fi
    [[ "$rc" -eq 2 ]]
    grep -Fq 'Error: internal error: non-legacy route reached legacy entry:' "$TEST_ROOT/bad.err"
    [[ ! -s "$TEST_ROOT/bad.out" ]]
    [[ ! -e "$REFRESH_LOG" ]]
done
if "$CLI" --update now >"$TEST_ROOT/bad.out" 2>"$TEST_ROOT/bad.err"; then
    echo "expected '--update now' to fail" >&2
    exit 1
fi
grep -Fq 'Error: --update does not accept arguments' "$TEST_ROOT/bad.err"
[[ ! -s "$TEST_ROOT/bad.out" ]]
[[ ! -e "$REFRESH_LOG" ]]
cmp -s "$RUN_LOG" "$TEST_ROOT/run.before"

assert_invalid_setup() {
    local expected="$1"
    shift
    if "$CLI" setup "$@" >"$TEST_ROOT/invalid.out" 2>&1; then
        echo "expected setup command to fail: $*" >&2
        exit 1
    fi
    grep -Fq "$expected" "$TEST_ROOT/invalid.out"
    grep -Fq 'Available setup names' "$TEST_ROOT/invalid.out"
    grep -Fq '✓ alpha' "$TEST_ROOT/invalid.out"
    # The setup command's own listing still pulls merged catalog rows.
    [[ -s "$REFRESH_LOG" ]]
    rm -f "$REFRESH_LOG"
}

assert_invalid_setup 'unknown setup name: missing' missing
assert_invalid_setup 'invalid setup name: ../alpha' ../alpha
assert_invalid_setup 'setup requires a script name'
# Retired commands explain the new ownership boundary without running or
# mutating a pre-existing external identity installation.
mkdir -p "$FIXTURE/bin"
cat >"$FIXTURE/bin/agent-bot" <<EOF
#!/usr/bin/env bash
printf 'invoked\n' >>'$RUN_LOG'
EOF
chmod +x "$FIXTURE/bin/agent-bot"
cp "$FIXTURE/bin/agent-bot" "$TEST_ROOT/agent-bot.before"
for retired in agent-bot agent-bot-gh; do
    if PATH="$FIXTURE/bin:$PATH" "$CLI" setup "$retired" >"$TEST_ROOT/retired.out" 2>&1; then
        echo "expected $retired setup to be retired" >&2
        exit 1
    fi
    grep -Fq 'is retired; managed-machine no longer installs, wires, or repairs agent-bot' "$TEST_ROOT/retired.out"
done
! grep -q '^invoked$' "$RUN_LOG"
cmp -s "$FIXTURE/bin/agent-bot" "$TEST_ROOT/agent-bot.before"

# setup --list and setup --json succeed with exit 0 and refresh the checkout.
rm -f "$REFRESH_LOG"
"$CLI" setup --list >"$TEST_ROOT/setup-list.out" 2>"$TEST_ROOT/setup-list.err"
[[ ! -s "$TEST_ROOT/setup-list.err" ]]
[[ -s "$REFRESH_LOG" ]]
grep -Fq 'Available setup names' "$TEST_ROOT/setup-list.out"
grep -Fq '✓ alpha' "$TEST_ROOT/setup-list.out"

for json_args in 'setup --list --json' 'setup --json'; do
    rm -f "$REFRESH_LOG"
    # shellcheck disable=SC2086
    "$CLI" $json_args >"$TEST_ROOT/setup-list.json" 2>"$TEST_ROOT/setup-list.err"
    [[ ! -s "$TEST_ROOT/setup-list.err" ]]
    [[ -s "$REFRESH_LOG" ]]
    python3 - "$TEST_ROOT/setup-list.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["schema_version"] == 1
assert data["command"] == "setup-list"
by_name = {item["name"]: item["installed"] for item in data["names"]}
assert by_name["alpha"] is True
assert "bin" in by_name
PY
done

# Truncated pipelines exit 0 cleanly on SIGPIPE.
"$CLI" setup --list | head -n 1 >/dev/null
"$CLI" setup --list --json | head -n 1 >/dev/null

# Invalid setup and CLI invocations with --json emit structured error envelopes.
assert_cli_json_error() {
    local expected_code="$1"
    local expected_evidence="$2"
    shift 2
    if "$CLI" "$@" >"$TEST_ROOT/err.json" 2>"$TEST_ROOT/err.stderr"; then
        echo "expected CLI --json call to fail: $*" >&2
        exit 1
    fi
    [[ ! -s "$TEST_ROOT/err.stderr" ]]
    python3 - "$TEST_ROOT/err.json" "$expected_code" "$expected_evidence" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
expected = sys.argv[2]
expected_ev = json.loads(sys.argv[3]) if sys.argv[3] else None
assert data["schema_version"] == 1
assert data["ready"] is False
assert data["status"] == "not_ready"
assert data["error"]["code"] == expected, (data["error"]["code"], expected)
assert data["error"]["action"]
assert data["checks"][0]["code"] == expected
if expected_ev is not None:
    assert data["error"]["evidence"] == expected_ev, (data["error"]["evidence"], expected_ev)
    assert data["checks"][0]["evidence"] == expected_ev, (data["checks"][0]["evidence"], expected_ev)
PY
}

assert_cli_json_error 'setup-name-unknown' '{"requested": "missing"}' setup missing --json
assert_cli_json_error 'setup-name-invalid' '{"requested": "../alpha"}' setup ../alpha --json
assert_cli_json_error 'setup-name-retired' '{"name": "agent-bot"}' setup agent-bot --json
assert_cli_json_error 'setup-invalid-arguments' '{}' setup --list --bogus --json
assert_cli_json_error 'setup-invalid-arguments' '{}' setup --list --json --bogus
assert_cli_json_error 'setup-invalid-arguments' '{}' setup --json --bogus
assert_cli_json_error 'update-invalid-arguments' '{}' --update now --json
assert_cli_json_error 'update-invalid-arguments' '{}' --update --json now

# Execute every concrete setup example published by the skill. This keeps the
# documentation and accepted CLI forms coupled without running real installers.
SKILL_EXAMPLES=0
while IFS= read -r example; do
    [[ -n "$example" ]] || continue
    "$CLI" setup "$example" >/dev/null
    SKILL_EXAMPLES=$((SKILL_EXAMPLES + 1))
done < <(sed -n 's/^managed-machine setup \([^ #<]*\).*/\1/p' "$ROOT/skills/managed-machine/SKILL.md")
[[ "$SKILL_EXAMPLES" -gt 0 ]]

# Every setup script documented in the skill table must exist and be executable.
while IFS= read -r script; do
    [[ -x "$ROOT/legacy/$script" ]]
done < <(sed -n 's/^| \(setup-[a-z0-9-]*\) |.*/\1/p' "$ROOT/skills/managed-machine/SKILL.md")

echo 'CLI tests passed'

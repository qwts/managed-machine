#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT
TEST_DIR="$(cd "$TEST_DIR" && pwd -P)"
mkdir -p "$TEST_DIR/home" "$TEST_DIR/config"
source "$ROOT/scripts/account"
export HOME="$TEST_DIR/home" MANAGED_MACHINE_ACCOUNT_CONTEXT=1
CURRENT=you-claude-agent
OS_HOME="$HOME"
ROSTER_STATUS=active
EXISTS=true
ADMIN=false
HARNESS=claude
CROSS_RESULT=0
DOCTOR_RESULT=0
REPORT_STATUS=ready
SETUP_CALLED="$TEST_DIR/setup-called"
DOCTOR_CALLED="$TEST_DIR/doctor-called"
CROSS_CALLED="$TEST_DIR/cross-called"
TARGET_UID=601
account_current_user() { printf '%s\n' "$CURRENT"; }
account_uid() { printf '%s\n' "$TARGET_UID"; }
account_groups() { printf '%s\n' 'staff agents'; }
agent_roster_source() { printf '%s\n' "$TEST_DIR/profile.json"; }
agent_roster_query() {
    case "$1" in status) printf '%s\n' "$ROSTER_STATUS" ;; harness) printf '%s\n' "$HARNESS" ;; esac
}
agent_account_exists() { "$EXISTS"; }
agent_account_is_admin() { "$ADMIN"; }
agent_account_home() { printf '%s\n' "$OS_HOME"; }
account_config_source() { printf '%s\n' "$TEST_DIR/config"; }
report() {
    python3 - "$REPORT_STATUS" <<'PY'
import json, sys
status = sys.argv[1]
check_status = "failed" if status == "not_ready" else status
print(json.dumps({"schema_version":1, "command":"account-doctor", "account":"you-claude-agent", "home":"fixture", "harness":"claude", "ready":status == "ready", "status":status, "checks":[{"id":"account.identity", "status":check_status, "message":"Account check"}]}))
PY
}
account_collect_json() { printf '%s\n' "$*" >>"$DOCTOR_CALLED"; report; return "$DOCTOR_RESULT"; }
account_setup_current() { [[ "$(pwd -P)" == "$HOME" ]] || return 99; printf '%s\n' "$*" >>"$SETUP_CALLED"; report; return "$DOCTOR_RESULT"; }
account_cross_account() {
    printf '%s\n' "$*" >>"$CROSS_CALLED"
    [[ "$CROSS_RESULT" == 0 ]] || return "$CROSS_RESULT"
    report
}

out="$(account_main doctor you-claude-agent --json)"
python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["ready"] and r["command"] == "account-doctor"' <<<"$out"
[[ -s "$DOCTOR_CALLED" && ! -e "$SETUP_CALLED" && ! -e "$CROSS_CALLED" ]]
out="$(account_main setup you-claude-agent --json)"
python3 -c 'import json,sys; assert json.load(sys.stdin)["command"] == "account-setup"' <<<"$out"
[[ "$(cat "$SETUP_CALLED")" == 'you-claude-agent claude' ]]

expect_failure() {
    local code="$1" expected_status="${2:-not_ready}" rc=0 output
    output="$(account_main doctor you-claude-agent --json)" || rc=$?
    [[ "$rc" != 0 ]]
    python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["checks"][0]["code"] == sys.argv[1]; assert r["status"] == sys.argv[2]' "$code" "$expected_status" <<<"$output"
}
ROSTER_STATUS=retired; expect_failure account-not-active; ROSTER_STATUS=active
EXISTS=false; expect_failure account-missing; EXISTS=true
ADMIN=true; expect_failure account-is-admin; ADMIN=false
TARGET_UID=0; expect_failure account-uid-invalid; TARGET_UID=601
OS_HOME="$TEST_DIR/absent"; expect_failure account-home-invalid; OS_HOME="$HOME"
HOME="$TEST_DIR"; expect_failure account-home-mismatch; HOME="$OS_HOME"
HARNESS='bad;command'; expect_failure harness-missing; HARNESS=claude
if account_main setup '../unsafe' --json >"$TEST_DIR/invalid"; then exit 1; fi
python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["account"] is None' <"$TEST_DIR/invalid"
if account_main setup you-claude-agent --all >/dev/null 2>&1; then exit 1; fi
if account_main update you-claude-agent >/dev/null 2>&1; then exit 1; fi

CURRENT=owner
out="$(account_main doctor you-claude-agent --json)"
[[ "$(tail -1 "$CROSS_CALLED")" == "doctor you-claude-agent $HOME" ]]
CROSS_RESULT=75; expect_failure account-authorization-unavailable pending_user_action
CROSS_RESULT=1; expect_failure account-authorization-unavailable
CURRENT=you-claude-agent; CROSS_RESULT=0
REPORT_STATUS=pending_user_action; DOCTOR_RESULT=75
rc=0; out="$(account_main doctor you-claude-agent --json)" || rc=$?
[[ "$rc" == 75 ]]
REPORT_STATUS=not_ready; DOCTOR_RESULT=1
rc=0; out="$(account_main doctor you-claude-agent --json)" || rc=$?
[[ "$rc" == 1 ]]
REPORT_STATUS=ready; DOCTOR_RESULT=0
out="$(account_main doctor you-claude-agent)"
[[ "$out" == *'Account status: ready'* ]]

source "$ROOT/scripts/account"
account_current_user() { printf 'you-claude-agent\n'; }
account_uid() { printf '601\n'; }
account_groups() { printf '%s\n' 'staff agents'; }
agent_roster_source() { printf '%s\n' "$TEST_DIR/profile.json"; }
agent_roster_query() { [[ "$1" != status ]] || printf 'active\n'; [[ "$1" != harness ]] || printf 'claude\n'; return 0; }
agent_account_exists() { return 0; }
agent_account_is_admin() { return 1; }
agent_account_home() { printf '%s\n' "$HOME"; }
account_prepare_config() { export CONFIG_REPO_ROOT="$TEST_DIR/config"; printf config >>"$TEST_DIR/steps"; }
account_prepare_shell() { printf shell >>"$TEST_DIR/steps"; return 1; }
account_prepare_local_bin() { printf bin >>"$TEST_DIR/steps"; return 75; }
account_prepare_zsh_functions() { printf zshfuncs >>"$TEST_DIR/steps"; return 75; }
account_harness_setup() { printf harness >>"$TEST_DIR/steps"; }
account_collect_json() {
    python3 -c 'import json,sys; rows=json.load(open(sys.argv[1])); assert [c["status"] for c in rows] == ["ready","failed","pending_user_action","pending_user_action","ready"]; print(json.dumps({"schema_version":1,"account":"you-claude-agent","ready":False,"status":"not_ready","checks":rows}))' "$4" || return 99
    return 1
}
rc=0; out="$(account_main setup you-claude-agent --json)" || rc=$?
[[ "$rc" == 1 && "$(cat "$TEST_DIR/steps")" == configshellbinzshfuncsharness ]]
python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["checks"][1]["code"] == "account-shell-incomplete"; assert r["checks"][3]["code"] == "account-zsh-functions-incomplete"' <<<"$out"

account_prepare_config() { return 75; }
account_config_source() { printf '%s\n' "$TEST_DIR/config"; }
account_collect_json() {
    [[ "$3" == "$TEST_DIR/config/apps.json" ]] || return 99
    python3 -c 'import json,sys; rows=json.load(open(sys.argv[1])); assert [c["status"] for c in rows] == ["pending_user_action"]; print(json.dumps({"schema_version":1,"account":"you-claude-agent","ready":False,"status":"pending_user_action","checks":rows}))' "$4" || return 99
    return 75
}
rc=0; out="$(CONFIG_REPO_ROOT= account_main setup you-claude-agent --json)" || rc=$?
[[ "$rc" == 75 ]]
python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["status"] == "pending_user_action" and len(r["checks"]) == 1 and r["checks"][0]["status"] == "pending_user_action"' <<<"$out"

source "$ROOT/scripts/account"
account_uid() { printf '601\n'; }
account_installed_cli() { printf '%s\n' "$TEST_DIR/installed cli/managed-machine"; }
managed_machine_agent_session() { return 0; }
elevation_available() { return 0; }
osascript() { echo 'unexpected authorization' >&2; return 99; }
if account_cross_account doctor you-claude-agent "$HOME" >/dev/null 2>&1; then exit 1; fi
managed_machine_agent_session() { return 1; }
elevation_available() { return 1; }
rc=0; account_cross_account doctor you-claude-agent "$HOME" >/dev/null 2>&1 || rc=$?
[[ "$rc" == 75 ]]
elevation_available() { return 0; }
if [[ -x /usr/bin/osascript ]]; then
    osascript() {
        local code
        local -a scripts=()
        while [[ "${1:-}" == -e ]]; do
            code="$2"
            case "$code" in 'do shell script cmd'*) code='return cmd' ;; esac
            scripts+=(-e "$code")
            shift 2
        done
        /usr/bin/osascript "${scripts[@]}" "$@" >"$TEST_DIR/authorized-command"
        report
    }
    account_cross_account doctor you-claude-agent "$TEST_DIR/home with spaces" >"$TEST_DIR/cross-report"
    rendered="$(cat "$TEST_DIR/authorized-command")"
    [[ "$rendered" == *'/bin/launchctl asuser'* && "$rendered" == *'/usr/bin/sudo -u'* && "$rendered" == *'/usr/bin/env -i'* ]]
    [[ "$rendered" == *"'HOME=$TEST_DIR/home with spaces'"* ]]
    /bin/sh -n -c "$rendered"
fi
osascript() { return 1; }
if account_cross_account setup you-claude-agent "$HOME" >/dev/null 2>&1; then exit 1; fi

# npm harness routing in account setup: npm globals are admin-owned shared
# installs like homebrew/core formulae, so the account defers to the owner
# instead of installing account-locally or failing as unsupported. Without a
# system npm there is no shared prefix (an NVM-only owner install would land
# where the account cannot resolve it), so the message names that
# prerequisite instead of the owner setup run.
source "$ROOT/scripts/account"
account_catalog_name() { printf 'copilot\n'; }
catalog_app_kind() { printf 'npm\n'; }
shared_npm_available() { return 0; }
rc=0; out="$(account_harness_setup copilot 2>&1)" || rc=$?
[[ "$rc" == 75 ]]
[[ "$out" == *'Shared harness copilot requires admin-owned installation'* ]]
[[ "$out" == *'run managed-machine setup copilot from the owner account first'* ]]
shared_npm_available() { return 1; }
rc=0; out="$(account_harness_setup copilot 2>&1)" || rc=$?
[[ "$rc" == 75 ]]
[[ "$out" == *'Shared harness copilot needs an admin-owned npm prefix'* ]]
[[ "$out" == *'install Homebrew node'* ]]
catalog_app_kind() { printf 'weird-kind\n'; }
rc=0; out="$(account_harness_setup copilot 2>&1)" || rc=$?
[[ "$rc" == 1 ]]
[[ "$out" == *'Unsupported account harness installation kind.'* ]]

printf '%s\n' 'Account orchestration tests passed'

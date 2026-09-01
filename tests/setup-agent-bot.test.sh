#!/usr/bin/env bash
# setup-agent-bot: reviewed self-tap install, stdin-only profile wiring,
# fail-closed doctor gate, and provider-code deferral. No network; brew,
# curl, and agent-bot are stubs in TEST_BIN.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
BREW_LOG="$TEST_ROOT/brew.log"
CURL_LOG="$TEST_ROOT/curl.log"
AGENT_BOT_LOG="$TEST_ROOT/agent-bot.log"
BREW_STATE="$TEST_ROOT/brew-state"
PROFILE_JSON='{"schema_version":1,"org":"qwts","marker":"mm-profile-marker"}'
PROFILE_URL='https://raw.githubusercontent.com/qwts/playbook-engineering/main/governance/organization-profile.json'
export BREW_LOG CURL_LOG AGENT_BOT_LOG BREW_STATE
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$BREW_STATE"
CONFIG_REPO="$TEST_ROOT/managed-machine-config"
mkdir -p "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
: >"$BREW_LOG"
: >"$CURL_LOG"
: >"$AGENT_BOT_LOG"

# brew: records every invocation; tap/install state lives in BREW_STATE so
# idempotency is observable. `commands` advertises trust unless disabled.
cat >"$TEST_BIN/brew" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$BREW_LOG"
case "${1:-}" in
    commands)
        printf 'install\nlist\ntap\npin\n'
        [[ "${MOCK_BREW_HAS_TRUST:-1}" == '1' ]] && printf 'trust\n'
        exit 0
        ;;
    tap)
        if [[ $# -eq 1 ]]; then
            [[ ! -f "$BREW_STATE/tapped" ]] || cat "$BREW_STATE/tapped"
        else
            printf '%s\n' "$2" >>"$BREW_STATE/tapped"
        fi
        ;;
    trust|pin) ;;
    install) touch "$BREW_STATE/installed" ;;
    list)
        [[ -f "$BREW_STATE/installed" ]] || exit 1
        echo 'agent-bot 0.2.0'
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

# curl: never touches the network; serves the profile on stdout only.
cat >"$TEST_BIN/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$CURL_LOG"
if [[ "${MOCK_CURL_FAIL:-0}" == '1' ]]; then
    exit 22
fi
printf '%s' "$MOCK_PROFILE_JSON"
EOF
chmod +x "$TEST_BIN/curl"

# agent-bot: bootstrap records its argv and stdin; doctor passes only once
# the wired marker exists, so the doctor gate is exercised independently of
# the bootstrap exit code.
cat >"$TEST_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$AGENT_BOT_LOG"
case "${1:-}" in
    bootstrap)
        stdin="$(cat)"
        printf 'bootstrap stdin: %s\n' "$stdin" >>"$AGENT_BOT_LOG"
        if [[ -n "${MOCK_AGENT_BOT_BOOTSTRAP_JSON:-}" ]]; then
            printf '%s\n' "$MOCK_AGENT_BOT_BOOTSTRAP_JSON"
        fi
        if [[ "${MOCK_AGENT_BOT_BOOTSTRAP_RESULT:-0}" != '0' ]]; then
            exit "$MOCK_AGENT_BOT_BOOTSTRAP_RESULT"
        fi
        if [[ "${MOCK_AGENT_BOT_BOOTSTRAP_WIRES:-1}" == '1' ]]; then
            touch "$HOME/.mock-agent-bot-wired"
        fi
        ;;
    doctor)
        [[ -f "$HOME/.mock-agent-bot-wired" ]]
        ;;
esac
EOF
chmod +x "$TEST_BIN/agent-bot"

run_setup() {
    env HOME="$TEST_HOME" \
        PATH="$TEST_BIN:/usr/bin:/bin" \
        CONFIG_REPO_ROOT="$CONFIG_REPO" \
        MOCK_PROFILE_JSON="$PROFILE_JSON" \
        "$@" \
        /bin/bash "$ROOT/setup-agent-bot"
}

reset_logs() {
    : >"$BREW_LOG"
    : >"$CURL_LOG"
    : >"$AGENT_BOT_LOG"
}

# 1. Fresh install and wiring: tap URL, trust, tagged formula, pin; the
#    profile arrives on agent-bot's stdin and never lands on disk; the doctor
#    gate runs after bootstrap.
run_setup >"$TEST_ROOT/install.out" 2>&1
grep -qxF 'tap qwts/agent-bot-identity https://github.com/qwts/agent-bot-identity.git' "$BREW_LOG"
grep -qxF 'trust qwts/agent-bot-identity' "$BREW_LOG"
grep -qxF 'install qwts/agent-bot-identity/agent-bot' "$BREW_LOG"
grep -qxF 'pin agent-bot' "$BREW_LOG"
grep -qxF -- "-fsSL $PROFILE_URL" "$CURL_LOG"
grep -qxF 'bootstrap --profile - --with-gh-shim --machine-only --json' "$AGENT_BOT_LOG"
grep -qxF "bootstrap stdin: $PROFILE_JSON" "$AGENT_BOT_LOG"
grep -qxF 'doctor --machine-only --json --require-schema-version 1' "$AGENT_BOT_LOG"
grep -Fq 'machine wiring verified' "$TEST_ROOT/install.out"
# stdin only: curl was never asked to write a file, and the governance data
# exists nowhere under HOME.
! grep -Fq -- '-o' "$CURL_LOG"
! grep -rFq 'mm-profile-marker' "$TEST_HOME"

# 2. Idempotent re-run against a wired machine: doctor vouches, so no brew
#    command, no fetch, no re-wiring.
reset_logs
run_setup >"$TEST_ROOT/rerun.out" 2>&1
grep -Fq 'already installed and the machine wiring is verified' "$TEST_ROOT/rerun.out"
[[ ! -s "$BREW_LOG" ]]
[[ ! -s "$CURL_LOG" ]]
! grep -q '^bootstrap' "$AGENT_BOT_LOG"

# 3. Homebrew without `brew trust`: detected, not assumed.
rm -rf "$BREW_STATE" "$TEST_HOME/.mock-agent-bot-wired"
mkdir -p "$BREW_STATE"
reset_logs
run_setup MOCK_BREW_HAS_TRUST=0 >"$TEST_ROOT/notrust.out" 2>&1
! grep -q '^trust' "$BREW_LOG"
grep -qxF 'install qwts/agent-bot-identity/agent-bot' "$BREW_LOG"

# 4. Failed profile fetch: refused, nonzero, and no wiring attempted.
rm -f "$TEST_HOME/.mock-agent-bot-wired"
reset_logs
set +e
run_setup MOCK_CURL_FAIL=1 >"$TEST_ROOT/fetch-fail.out" 2>&1
result=$?
set -e
[[ "$result" -ne 0 && "$result" -ne 76 ]]
grep -Fq 'could not fetch the organization profile' "$TEST_ROOT/fetch-fail.out"
grep -Fq 'Refusing to wire agent-bot' "$TEST_ROOT/fetch-fail.out"
! grep -q '^bootstrap' "$AGENT_BOT_LOG"
! grep -Fq 'machine wiring verified' "$TEST_ROOT/fetch-fail.out"

# 5. Doctor gate fails even though bootstrap exited 0: refused, never
#    reported as wired.
reset_logs
set +e
run_setup MOCK_AGENT_BOT_BOOTSTRAP_WIRES=0 >"$TEST_ROOT/doctor-fail.out" 2>&1
result=$?
set -e
[[ "$result" -ne 0 && "$result" -ne 76 ]]
grep -q '^bootstrap --profile' "$AGENT_BOT_LOG"
grep -Fq 'doctor did not pass the machine gate' "$TEST_ROOT/doctor-fail.out"
! grep -Fq 'machine wiring verified' "$TEST_ROOT/doctor-fail.out"

# 6. Noninteractive first run on a machine whose secret provider is not
#    ready: agent-bot refuses with a typed provider code, and the step defers
#    with the shared status for bootstrap to record — it does not fail and
#    does not report success.
reset_logs
set +e
run_setup MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive \
    MOCK_AGENT_BOT_BOOTSTRAP_RESULT=1 \
    MOCK_AGENT_BOT_BOOTSTRAP_JSON='{"apps":[{"credential":{"local":"failed","code":"provider-session-required"}}]}' \
    >"$TEST_ROOT/deferral.out" 2>&1
result=$?
set -e
[[ "$result" -eq 76 ]]
grep -Fq 'Skipped:' "$TEST_ROOT/deferral.out"
grep -Fq 'pass-cli' "$TEST_ROOT/deferral.out"
! grep -Fq 'machine wiring verified' "$TEST_ROOT/deferral.out"

# 7. A non-provider bootstrap failure (profile rejected) stays fail-closed —
#    the readiness codes, not the exit code, decide deferral.
reset_logs
set +e
run_setup MOCK_AGENT_BOT_BOOTSTRAP_RESULT=1 \
    MOCK_AGENT_BOT_BOOTSTRAP_JSON='{"config":{"runtime":"failed","code":"config-missing"}}' \
    >"$TEST_ROOT/rejected.out" 2>&1
result=$?
set -e
[[ "$result" -ne 0 && "$result" -ne 76 ]]
grep -Fq 'bootstrap refused the machine wiring' "$TEST_ROOT/rejected.out"
! grep -Fq 'machine wiring verified' "$TEST_ROOT/rejected.out"

# 8. ~/.local/bin/agent-bot pointing into a git checkout: reported clearly,
#    nothing installed or wired.
CHECKOUT="$TEST_ROOT/agent-bot-checkout"
mkdir -p "$CHECKOUT/.git" "$TEST_HOME/.local/bin"
cat >"$CHECKOUT/agent-bot" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "$CHECKOUT/agent-bot"
ln -s "$CHECKOUT/agent-bot" "$TEST_HOME/.local/bin/agent-bot"
reset_logs
set +e
run_setup >"$TEST_ROOT/checkout.out" 2>&1
result=$?
set -e
[[ "$result" -ne 0 && "$result" -ne 76 ]]
grep -Fq 'points into a git checkout' "$TEST_ROOT/checkout.out"
grep -Fq "$CHECKOUT" "$TEST_ROOT/checkout.out"
! grep -q '^install' "$BREW_LOG"
! grep -q '^bootstrap' "$AGENT_BOT_LOG"

echo 'setup-agent-bot tests passed'

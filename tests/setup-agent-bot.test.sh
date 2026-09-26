#!/usr/bin/env bash
# setup-agent-bot: reviewed self-tap install, stdin-only profile wiring,
# fail-closed doctor gate, and provider-code deferral. No network; brew,
# curl, and agent-bot are stubs in TEST_BIN.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
BREW_LOG="$TEST_ROOT/brew.log"
CURL_LOG="$TEST_ROOT/curl.log"
AGENT_BOT_LOG="$TEST_ROOT/agent-bot.log"
BREW_STATE="$TEST_ROOT/brew-state"
PROFILE_JSON='{"schema_version":1,"org":"qwts","marker":"mm-profile-marker"}'
PROFILE_URL='https://raw.githubusercontent.com/qwts/qwts-agent-org/main/governance/organization-profile.json'
FORMULA_URL='https://raw.githubusercontent.com/qwts/agent-bot-identity/main/Formula/agent-bot.rb'
export BREW_LOG CURL_LOG AGENT_BOT_LOG BREW_STATE FORMULA_URL
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
    trust) ;;
    pin) touch "$BREW_STATE/pinned" ;;
    unpin) rm -f "$BREW_STATE/pinned" ;;
    install) touch "$BREW_STATE/installed"; echo '0.2.0' >"$BREW_STATE/version" ;;
    update)
        # Advances the on-disk tap to whatever the tap repo publishes
        # (MOCK_TAP_STUCK=1 models a checkout that will not move).
        if [[ "${MOCK_TAP_STUCK:-0}" != '1' && -f "$BREW_STATE/published-formula.rb" ]]; then
            mkdir -p "$BREW_STATE/tap/Formula"
            cp "$BREW_STATE/published-formula.rb" "$BREW_STATE/tap/Formula/agent-bot.rb"
            sed -n 's|^  url ".*/refs/tags/v\([0-9][0-9.]*\)\.tar\.gz"$|\1|p' "$BREW_STATE/published-formula.rb" >"$BREW_STATE/tap-version"
        fi
        ;;
    upgrade)
        [[ -f "$BREW_STATE/installed" ]] || exit 1
        [[ ! -f "$BREW_STATE/pinned" ]] || { echo 'agent-bot is pinned. You must unpin it to upgrade.' >&2; exit 1; }
        cp "$BREW_STATE/tap-version" "$BREW_STATE/version"
        ;;
    --repository) echo "$BREW_STATE/tap" ;;
    list)
        if [[ "${2:-}" == '--pinned' ]]; then
            [[ ! -f "$BREW_STATE/pinned" ]] || echo 'agent-bot'
            exit 0
        fi
        [[ -f "$BREW_STATE/installed" ]] || exit 1
        echo "agent-bot $(cat "$BREW_STATE/version" 2>/dev/null || echo 0.2.0)"
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

# The formula file names the published tag, and that is all the version
# check reads (no formula load, no trust). Two copies exist: the tap as brew
# keeps it on disk (advanced only by `brew update`) and the one the tap repo
# publishes on its main branch (what the network fetch returns).
formula_text() {
    cat <<EOF
class AgentBot < Formula
  url "https://github.com/qwts/agent-bot-identity/archive/refs/tags/v$1.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
end
EOF
}
publish_tap_version() {
    mkdir -p "$BREW_STATE/tap/Formula"
    printf '%s\n' "$1" >"$BREW_STATE/tap-version"
    formula_text "$1" >"$BREW_STATE/tap/Formula/agent-bot.rb"
}
publish_release_version() {
    formula_text "$1" >"$BREW_STATE/published-formula.rb"
}
publish_tap_version 0.2.0
publish_release_version 0.2.0

# curl: never touches the network; serves the published formula for the tap
# repo URL and the profile for everything else, on stdout only.
cat >"$TEST_BIN/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$CURL_LOG"
if [[ "${MOCK_CURL_FAIL:-0}" == '1' ]]; then
    exit 22
fi
if [[ "$*" == *"$FORMULA_URL"* ]]; then
    [[ -f "$BREW_STATE/published-formula.rb" ]] || exit 22
    cat "$BREW_STATE/published-formula.rb"
    exit 0
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
# The tap lives on a custom remote, so the URL is the reference the trust
# gate actually matches; the name alone is a no-op there.
grep -qxF 'trust qwts/agent-bot-identity https://github.com/qwts/agent-bot-identity.git' "$BREW_LOG"
# Order matters: `brew tap` validates formulae by loading them, which the
# trust gate refuses while the tap is untrusted — trust must precede tap.
[[ "$(grep -E '^(trust|tap) ' "$BREW_LOG" | head -1)" == 'trust qwts/agent-bot-identity https://github.com/qwts/agent-bot-identity.git' ]]
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
#    mutation, no profile fetch, no re-wiring. (The version check reads
#    brew's install list and the published formula file; it never loads the
#    formula and never asks for authorization.)
reset_logs
run_setup >"$TEST_ROOT/rerun.out" 2>&1
grep -Fq 'already installed and the machine wiring is verified' "$TEST_ROOT/rerun.out"
! grep -q '^\(install\|upgrade\|update\|tap \|trust\|pin\|unpin\)' "$BREW_LOG"
grep -qF -- "$FORMULA_URL" "$CURL_LOG"
! grep -qF -- "$PROFILE_URL" "$CURL_LOG"
! grep -q '^bootstrap' "$AGENT_BOT_LOG"

# 2b. The tap repo publishes a newer tag while the tap checkout on disk is
#     still behind (no `brew update` ran since): the stale checkout is
#     refreshed and the pinned runtime moved in one update/unpin/upgrade/pin
#     script, and the wiring re-runs even though doctor still vouched for
#     the old daemon; the pin holds afterwards.
publish_release_version 0.3.0
reset_logs
run_setup >"$TEST_ROOT/upgrade.out" 2>&1
grep -Fq 'Upgrading agent-bot 0.2.0 -> 0.3.0' "$TEST_ROOT/upgrade.out"
grep -qxF 'update' "$BREW_LOG"
grep -qxF 'unpin agent-bot' "$BREW_LOG"
grep -qxF 'upgrade qwts/agent-bot-identity/agent-bot' "$BREW_LOG"
grep -qxF 'pin agent-bot' "$BREW_LOG"
! grep -q '^install' "$BREW_LOG"
[[ "$(cat "$BREW_STATE/version")" == '0.3.0' ]]
[[ "$(cat "$BREW_STATE/tap-version")" == '0.3.0' ]]
[[ -f "$BREW_STATE/pinned" ]]
grep -qxF 'bootstrap --profile - --with-gh-shim --machine-only --json' "$AGENT_BOT_LOG"
grep -Fq 'machine wiring verified' "$TEST_ROOT/upgrade.out"
# The four brew mutations arrived as one script (one authorization), not
# four separate brew_run calls: the stub logs them, the log order is fixed.
[[ "$(grep -n '^update\|^unpin\|^upgrade\|^pin' "$BREW_LOG" | cut -d: -f2 | tr '\n' ' ')" == 'update unpin agent-bot upgrade qwts/agent-bot-identity/agent-bot pin agent-bot ' ]]

# 2c. Up to date again: back to the converged path, nothing mutates.
reset_logs
run_setup >"$TEST_ROOT/rerun2.out" 2>&1
grep -Fq 'already installed and the machine wiring is verified' "$TEST_ROOT/rerun2.out"
! grep -q '^\(install\|upgrade\|update\|unpin\|pin\)' "$BREW_LOG"

# 2d. The tap checkout is already current (--update's `brew update` ran
#     first): no redundant refresh, just unpin/upgrade/pin.
publish_tap_version 0.4.0
publish_release_version 0.4.0
reset_logs
run_setup >"$TEST_ROOT/upgrade2.out" 2>&1
grep -Fq 'Upgrading agent-bot 0.3.0 -> 0.4.0' "$TEST_ROOT/upgrade2.out"
! grep -qx 'update' "$BREW_LOG"
[[ "$(grep -n '^unpin\|^upgrade\|^pin' "$BREW_LOG" | cut -d: -f2 | tr '\n' ' ')" == 'unpin agent-bot upgrade qwts/agent-bot-identity/agent-bot pin agent-bot ' ]]
[[ "$(cat "$BREW_STATE/version")" == '0.4.0' ]]

# 2e. Offline: the published formula cannot be fetched, so the tap checkout
#     on disk is the answer; a current install stays on the converged path
#     without any brew mutation. (The profile is never needed on this path.)
reset_logs
run_setup MOCK_CURL_FAIL=1 >"$TEST_ROOT/offline.out" 2>&1
grep -Fq 'already installed and the machine wiring is verified' "$TEST_ROOT/offline.out"
! grep -q '^\(install\|upgrade\|update\|unpin\|pin\)' "$BREW_LOG"

# 2f. The upgrade lands a different version than the published tag (a tap
#     checkout that will not advance): reported and nonzero rather than
#     declared done, and no wiring runs on the wrong runtime.
publish_release_version 0.5.0
reset_logs
if run_setup MOCK_TAP_STUCK=1 >"$TEST_ROOT/stuck.out" 2>&1; then
    echo 'expected a version mismatch after upgrade to fail' >&2
    exit 1
fi
grep -Fq 'Upgrading agent-bot 0.4.0 -> 0.5.0' "$TEST_ROOT/stuck.out"
grep -Fq 'agent-bot is 0.4.0 after the upgrade, expected 0.5.0' "$TEST_ROOT/stuck.out"
! grep -q '^bootstrap' "$AGENT_BOT_LOG"
publish_tap_version 0.2.0
publish_release_version 0.2.0
echo '0.2.0' >"$BREW_STATE/version"

# 2g. The fetched formula lags the release (a cache still serving the
#     previous tag) while `brew update` already advanced the tap checkout:
#     the newer of the two wins, so the runtime moves without a redundant
#     refresh instead of reading as current.
publish_tap_version 0.3.0
reset_logs
run_setup >"$TEST_ROOT/lag.out" 2>&1
grep -Fq 'Upgrading agent-bot 0.2.0 -> 0.3.0' "$TEST_ROOT/lag.out"
! grep -qx 'update' "$BREW_LOG"
[[ "$(grep -n '^unpin\|^upgrade\|^pin' "$BREW_LOG" | cut -d: -f2 | tr '\n' ' ')" == 'unpin agent-bot upgrade qwts/agent-bot-identity/agent-bot pin agent-bot ' ]]
[[ "$(cat "$BREW_STATE/version")" == '0.3.0' ]]
grep -Fq 'machine wiring verified' "$TEST_ROOT/lag.out"
publish_tap_version 0.2.0
echo '0.2.0' >"$BREW_STATE/version"

# 3. Homebrew without `brew trust`: detected, not assumed.
rm -rf "$BREW_STATE" "$TEST_HOME/.mock-agent-bot-wired"
mkdir -p "$BREW_STATE"
reset_logs
run_setup MOCK_BREW_HAS_TRUST=0 >"$TEST_ROOT/notrust.out" 2>&1
! grep -q '^trust' "$BREW_LOG"
grep -qxF 'install qwts/agent-bot-identity/agent-bot' "$BREW_LOG"

# 3b. Installed but unpinned (a prior run interrupted between install and
#     pin): the re-run re-applies the pin instead of reporting converged
#     state, and does not reinstall.
rm -f "$BREW_STATE/pinned" "$TEST_HOME/.mock-agent-bot-wired"
reset_logs
run_setup >"$TEST_ROOT/repin.out" 2>&1
grep -Fq 'agent-bot already installed' "$TEST_ROOT/repin.out"
grep -qxF 'pin agent-bot' "$BREW_LOG"
! grep -q '^install' "$BREW_LOG"

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

# 8. ~/.local/bin/agent-bot pointing into a git checkout (#91): the link is
#    parked next to itself, reversibly and with the checkout untouched, the
#    run says where, and the install and wiring go on.
CHECKOUT="$TEST_ROOT/agent-bot-checkout"
mkdir -p "$CHECKOUT/.git" "$TEST_HOME/.local/bin"
cat >"$CHECKOUT/agent-bot" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "$CHECKOUT/agent-bot"
ln -s "$CHECKOUT/agent-bot" "$TEST_HOME/.local/bin/agent-bot"
rm -f "$TEST_HOME/.mock-agent-bot-wired"
reset_logs
run_setup >"$TEST_ROOT/checkout.out" 2>&1
grep -Fq "pointed into a git checkout ($CHECKOUT); parked it at $TEST_HOME/.local/bin/agent-bot.devlink-" "$TEST_ROOT/checkout.out"
parked="$(ls "$TEST_HOME/.local/bin"/agent-bot.devlink-* | head -1)"
[[ -L "$parked" && "$(readlink "$parked")" == "$CHECKOUT/agent-bot" ]]
[[ -x "$CHECKOUT/agent-bot" ]]
grep -q '^bootstrap --profile' "$AGENT_BOT_LOG"
grep -Fq 'machine wiring verified' "$TEST_ROOT/checkout.out"
! grep -Fq 'Error:' "$TEST_ROOT/checkout.out"
rm -f "$parked"

# 9. The wired state itself: ~/.local/bin/agent-bot linked at the brew
#    stable entrypoint, under a prefix that is a git checkout (as
#    /opt/homebrew is on ARM Macs). The deferred-provider retry must
#    re-wire, not hard-stop on the prefix's .git.
FAKE_PREFIX="$TEST_ROOT/homebrew-prefix"
mkdir -p "$FAKE_PREFIX/.git" "$FAKE_PREFIX/opt/agent-bot/bin"
cp "$TEST_BIN/agent-bot" "$FAKE_PREFIX/opt/agent-bot/bin/agent-bot"
ln -sf "$FAKE_PREFIX/opt/agent-bot/bin/agent-bot" "$TEST_HOME/.local/bin/agent-bot"
rm -f "$TEST_HOME/.mock-agent-bot-wired"
reset_logs
run_setup HOMEBREW_PREFIX="$FAKE_PREFIX" >"$TEST_ROOT/brew-link.out" 2>&1
! grep -Fq 'points into a git checkout' "$TEST_ROOT/brew-link.out"
grep -q '^bootstrap --profile' "$AGENT_BOT_LOG"
grep -Fq 'machine wiring verified' "$TEST_ROOT/brew-link.out"

echo 'setup-agent-bot tests passed'

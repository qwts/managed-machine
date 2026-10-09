#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
FAKE_BIN="$TEST_DIR/bin"
STATE="$TEST_DIR/state"
APPS_DIR="$TEST_DIR/Applications"
SHARED_ROOT="$TEST_DIR/shared"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_HOME" "$FAKE_BIN" "$STATE/users" "$APPS_DIR"
export STATE

# The roster fixture covers all three answers add-agent must give: active
# provisions, retired fails closed, unknown fails closed.
PROFILE="$TEST_DIR/organization-profile.json"
cat >"$PROFILE" <<'EOF'
{
  "identities": [
    { "slug": "you-goose-agent", "harness": "goose", "status": "active" },
    { "slug": "you-codex-agent", "harness": "codex", "status": "active" },
    { "slug": "you-bare-agent", "status": "active" },
    { "slug": "you-vscode-agent", "harness": "vscode", "status": "retired" }
  ]
}
EOF

# osascript stub: execute the elevated command directly (drop the -e script
# pairs and the label) so the account-mutation stubs actually run.
cat >"$FAKE_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
while [[ "${1:-}" == "-e" ]]; do shift 2; done
shift # the label
exec "$@"
EOF

# sysadminctl stub: record the invocation (password redacted) and mark the
# account created.
cat >"$FAKE_BIN/sysadminctl" <<'EOF'
#!/usr/bin/env bash
name="" full=""
args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -addUser) name="$2"; args+=("$1" "$2"); shift 2 ;;
        -fullName) full="$2"; args+=("$1" "$2"); shift 2 ;;
        -password) args+=("$1" '<redacted>'); shift 2 ;;
        *) args+=("$1"); shift ;;
    esac
done
printf '%s\n' "${args[*]}" >>"$STATE/sysadminctl.log"
[[ -n "$name" ]] || exit 1
printf '%s\n' "$full" >"$STATE/users/$name"
EOF

cat >"$FAKE_BIN/createhomedir" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STATE/createhomedir.log"
[[ ! -e "$STATE/createhomedir-fail" ]] || exit 0   # a home that never appears
while [[ $# -gt 0 ]]; do
    case "$1" in
        -u) mkdir -p "$STATE/homes/$2"; shift 2 ;;
        *) shift ;;
    esac
done
EOF

# dsmemberutil stub: the OS membership verdict, driven by the state dir.
# admin membership lives in $STATE/admins; every other group is a member
# list under $STATE/groups/<group>, which the dseditgroup stub maintains.
cat >"$FAKE_BIN/dsmemberutil" <<'EOF'
#!/usr/bin/env bash
user="" group=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -U) user="$2"; shift 2 ;;
        -G) group="$2"; shift 2 ;;
        *) shift ;;
    esac
done
case "$group" in
    admin) members="$STATE/admins" ;;
    *) members="$STATE/groups/$group" ;;
esac
if grep -Fxq "$user" "$members" 2>/dev/null; then
    echo 'user is a member of the group'
else
    echo 'user is not a member of the group'
fi
EOF

# dseditgroup stub: record every operation and keep the member lists the
# dsmemberutil stub answers from. `read` fails until `create` has run, the
# way the real tool reports an unknown group.
cat >"$FAKE_BIN/dseditgroup" <<'EOF'
#!/usr/bin/env bash
op="" group="" member=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) op="$2"; shift 2 ;;
        -a) member="$2"; shift 2 ;;
        -t|-r) shift 2 ;;
        *) group="$1"; shift ;;
    esac
done
printf '%s %s %s\n' "$op" "$group" "$member" >>"$STATE/dseditgroup.log"
mkdir -p "$STATE/groups"
case "$op" in
    read) [[ -f "$STATE/groups/$group" ]] ;;
    create) : >"$STATE/groups/$group" ;;
    edit) [[ -f "$STATE/groups/$group" ]] || exit 1; printf '%s\n' "$member" >>"$STATE/groups/$group" ;;
    *) exit 1 ;;
esac
EOF

# dscl stub: answer the exact reads the helpers make, from the state dir,
# and honor the one write the elevated phase performs (the Picture record).
cat >"$FAKE_BIN/dscl" <<'EOF'
#!/usr/bin/env bash
if [[ "${2:-}" == "-create" && "${4:-}" == "Picture" ]]; then
    name="${3#/Users/}"
    [[ -f "$STATE/users/$name" ]] || exit 56
    mkdir -p "$STATE/picture-attr"
    printf '%s\n' "$5" >"$STATE/picture-attr/$name"
    exit 0
fi
if [[ "${2:-}" == "-read" ]]; then
    case "$3" in
        /Users/*)
            name="${3#/Users/}"
            [[ -f "$STATE/users/$name" ]] || exit 56
            case "${4:-}" in
                NFSHomeDirectory) printf 'NFSHomeDirectory: %s\n' "$STATE/homes/$name" ;;
                RealName) printf 'RealName:\n %s\n' "$(cat "$STATE/users/$name")" ;;
                UniqueID) printf 'UniqueID: 601\n' ;;
                Picture)
                    if [[ -f "$STATE/picture-attr/$name" ]]; then
                        printf 'Picture: %s\n' "$(cat "$STATE/picture-attr/$name")"
                    else
                        echo 'No such key: Picture'
                    fi
                    ;;
            esac
            ;;
        /Groups/admin)
            printf 'GroupMembership: root you %s\n' "$(cat "$STATE/admins" 2>/dev/null || true)"
            ;;
        /Groups/*)
            group="${3#/Groups/}"
            [[ -f "$STATE/groups/$group" ]] || exit 56
            printf 'GroupMembership: %s\n' "$(tr '\n' ' ' <"$STATE/groups/$group")"
            ;;
        *) exit 56 ;;
    esac
fi
EOF

# curl stub: the avatar download and the organization profile fetch. Serves
# the fixture bytes in $STATE/avatar (or the roster fixture for the profile
# URL), records the URL, and fails while $STATE/curl-fail exists.
cat >"$FAKE_BIN/curl" <<'EOF'
#!/usr/bin/env bash
dest="" url=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) dest="$2"; shift 2 ;;
        --max-time) shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
printf '%s\n' "$url" >>"$STATE/curl.log"
[[ ! -e "$STATE/curl-fail" ]] || exit 22
case "$url" in
    */organization-profile.json) cp "$STATE/profile" "$dest" ;;
    https://api.github.com/users/*)
        # The anonymous users API: fails while $STATE/curl-api-fail exists
        # (rate limit), answers on stdout otherwise.
        [[ ! -e "$STATE/curl-api-fail" ]] || exit 22
        printf '{"login":"you-goose-agent[bot]","avatar_url":"https://avatars.githubusercontent.com/in/777?v=4"}\n'
        ;;
    *) cp "$STATE/avatar" "$dest" ;;
esac
EOF

# sudo stub: the elevated phase drops to the account for the agent-bot
# wiring. Record the target user, give it that account's home, and run.
cat >"$FAKE_BIN/sudo" <<'EOF'
#!/usr/bin/env bash
user=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -u) user="$2"; shift 2 ;;
        -H) shift ;;
        *) break ;;
    esac
done
printf '%s\n' "$user" >>"$STATE/sudo.log"
HOME="$STATE/homes/$user" exec "$@"
EOF

# chown stub: the test user owns nothing but itself, so record the intent.
cat >"$FAKE_BIN/chown" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STATE/chown.log"
EOF

# Existing runtime stub is an invocation tripwire. The setup flow must leave
# it, its credentials, and stale markers alone.
cat >"$FAKE_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "$HOME" "$*" >>"$STATE/agent-bot.log"
exit 99
EOF
# managed-machine stub: the harness install the elevated phase runs as the
# account (`managed-machine setup <name>`). Records the HOME it ran under,
# the bootstrap mode, and its arguments; installs a fake CLI into that home's
# ~/.local/bin; fails with a vendor-installer error while $STATE/harness-fail
# exists. stdin must be closed: the install runs headless behind the dialog.
cat >"$FAKE_BIN/managed-machine" <<'EOF'
#!/usr/bin/env bash
printf '%s %s %s\n' "$HOME" "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" "$*" >>"$STATE/managed-machine.log"
if [[ -t 0 ]]; then echo "a headless harness install must not read a terminal" >&2; exit 3; fi
[[ "${GIT_TERMINAL_PROMPT:-}" == 0 ]] || { echo "git prompts must be disabled for a headless install" >&2; exit 3; }
[[ "$#" == 4 && "$1" == account && "$2" == setup && "$3" == "${HOME##*/}" && "$4" == --json ]] || exit 3
[[ -r "$STATE/markers/$3.profile.json" ]] || exit 4
status=ready ready=true rc=0
if [[ -e "$STATE/harness-fail" ]]; then
    echo "Error: sensitive installer output" >&2
    status=not_ready ready=false rc=7
elif [[ -e "$STATE/harness-pending" ]]; then
    status=pending_user_action ready=false rc=75
else
    harness="${3#you-}"; harness="${harness%-agent}"
    mkdir -p "$HOME/.local/bin"
    if [[ ! -x "$HOME/.local/bin/$harness" ]]; then
        printf '#!/bin/sh\necho %s 1.0\n' "$harness" >"$HOME/.local/bin/$harness"
        chmod +x "$HOME/.local/bin/$harness"
    fi
fi
check_status="$status"
[[ "$status" != not_ready ]] || check_status=failed
printf '{"schema_version":1,"command":"account-setup","account":"%s","status":"%s","ready":%s,"checks":[{"id":"account.identity","status":"ready"},{"id":"shell.profiles","status":"ready"},{"id":"local_bin.links","status":"ready"},{"id":"identity.machine","status":"ready"},{"id":"harness.cli","status":"%s"}]}\n' "$3" "$status" "$ready" "$check_status"
exit "$rc"
EOF

# gh stub: the users API fallback for the avatar URL.
cat >"$FAKE_BIN/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STATE/gh.log"
[[ "$1" == "api" && "$2" == users/*%5Bbot%5D ]] || exit 1
echo 'https://avatars.githubusercontent.com/in/777?v=4'
EOF
chmod +x "$FAKE_BIN"/*

# The helpers call /usr/bin/dscl absolutely; put the stub there via a fake
# /usr/bin overlay is not possible, so point dscl through PATH by shadowing
# the helper's absolute call with a function is not either — instead the lib
# uses /usr/bin/dscl, so route through a private DYLD-free wrapper: a copy of
# the lib with the absolute path rewritten. The rewrite is mechanical and
# keeps the code under test byte-identical otherwise.
PICTURES="$STATE/pictures"
MARKERS="$STATE/markers"
mkdir -p "$MARKERS"
printf 'stale external doctor record\n' >"$MARKERS/you-goose-agent.doctor.json"
printf 'stale external key fingerprint\n' >"$MARKERS/you-goose-agent.keys.sha256"
cp "$MARKERS/you-goose-agent.doctor.json" "$STATE/expected-doctor-marker"
cp "$MARKERS/you-goose-agent.keys.sha256" "$STATE/expected-key-marker"
mkdir -p "$TEST_DIR/lib-under-test"
sed -e 's|/usr/bin/dscl|dscl|g' -e 's|/usr/bin/dsmemberutil|dsmemberutil|g' \
    -e "s|/Library/User Pictures/agents|$PICTURES|g" \
    -e "s|/Library/Application Support/managed-machine/agents|$MARKERS|g" \
    "$ROOT/lib/agent-account.sh" >"$TEST_DIR/lib-under-test/agent-account.sh"
sed -e "s|^REPO_ROOT=.*|REPO_ROOT=\"$ROOT\"|" \
    -e "s|source \"\$REPO_ROOT/lib/agent-account.sh\"|source \"$TEST_DIR/lib-under-test/agent-account.sh\"|" \
    -e 's|/usr/sbin/sysadminctl|sysadminctl|' \
    -e 's|/usr/sbin/createhomedir|createhomedir|' \
    -e 's|/usr/sbin/dseditgroup|dseditgroup|g' \
    -e 's|/usr/bin/dscl|dscl|g' \
    -e 's|/usr/bin/sudo|sudo|g' \
    -e 's|/usr/sbin/chown|chown|g' \
    -e "s|/opt/homebrew/opt/agent-bot/bin/agent-bot|$FAKE_BIN/agent-bot|" \
    -e "s|/opt/homebrew/bin/managed-machine|$FAKE_BIN/managed-machine|" \
    -e "s|/Library/User Pictures/agents|$PICTURES|g" \
    -e "s|/Library/Application Support/managed-machine/agents|$MARKERS|g" \
    "$ROOT/scripts/add-agent" >"$TEST_DIR/add-agent"

# The production script must keep the elevated tool paths, the group name,
# and the pictures directory hardcoded absolute: anything env-chosen would
# run as root (or be written by root) behind a generic prompt.
grep -Fq '/usr/sbin/sysadminctl -addUser' "$ROOT/scripts/add-agent"
grep -Fq '/usr/sbin/createhomedir -c -u' "$ROOT/scripts/add-agent"
grep -Fq '/usr/sbin/dseditgroup -o edit -a "$1" -t user agents' "$ROOT/scripts/add-agent"
# The uid is the OS's to assign; the script must not try to pick or fix one
# (macOS refuses to modify or delete local records even for elevated root).
if grep -q -- '-UID\|UniqueID' "$ROOT/scripts/add-agent"; then
    echo 'add-agent must not depend on a particular uid' >&2
    exit 1
fi
grep -Fq 'PIC="/Library/User Pictures/agents/$1.png"' "$ROOT/scripts/add-agent"
grep -Fq '/usr/bin/dscl . -create "/Users/$1" Picture "$PIC"' "$ROOT/scripts/add-agent"
# Identity operations and private-key copying are outside managed-machine.
if rg -n 'agent-bot (bootstrap|doctor|ensure-private-key)|AGENT_BOT_SUPERVISOR|private-key\.pem|keys\.sha256|/opt/[^ ]*agent-bot' "$ROOT/scripts/add-agent"; then
    echo 'add-agent must not seed credentials or wire the identity runtime' >&2
    exit 1
fi
# The harness install is the Homebrew managed-machine run as the account,
# headless (noninteractive mode, no git prompts, stdin closed), never as root.
grep -Fq 'MM=/opt/homebrew/bin/managed-machine' "$ROOT/scripts/add-agent"
grep -Fq '/usr/bin/sudo -u "$1" -H /usr/bin/env PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive GIT_TERMINAL_PROMPT=0 "$MM" account setup "$1" --json' "$ROOT/scripts/add-agent"
if grep -E '"\$MM" [a-z]' "$ROOT/scripts/add-agent" | grep -Fvq '/usr/bin/sudo -u "$1"'; then
    echo 'the harness install must run as the account, via sudo -u' >&2
    exit 1
fi
if grep -E '"\$MM" account setup' "$ROOT/scripts/add-agent" | grep -Fvq '</dev/null'; then
    echo 'the harness install must run with stdin closed' >&2
    exit 1
fi
if grep -q 'MANAGED_MACHINE_SYSADMINCTL\|MANAGED_MACHINE_CREATEHOMEDIR\|MANAGED_MACHINE_DSEDITGROUP\|MANAGED_MACHINE_USER_PICTURES' "$ROOT/scripts/add-agent"; then
    echo 'elevated tool paths must not be environment-overridable' >&2
    exit 1
fi
chmod +x "$TEST_DIR/add-agent"

# The avatar fixture: the URL agent-bot cached for the App in the operator's
# own ~/.config/<slug>, and the bytes curl will serve for it.
mkdir -p "$TEST_HOME/.config/you-goose-agent"
echo 'https://avatars.githubusercontent.com/in/4321?v=4' >"$TEST_HOME/.config/you-goose-agent/bot-avatar-url"
printf 'PNG-A' >"$STATE/avatar"
# Stale external credentials and identity markers must remain untouched.
KEY_DIR="$TEST_HOME/.config/you-goose-agent"
AGENT_HOME="$STATE/homes/you-goose-agent"
mkdir -p "$KEY_DIR"
printf '4321\n' >"$KEY_DIR/app-id"
printf 'existing external key\n' >"$KEY_DIR/private-key.pem"
chmod 0600 "$KEY_DIR/app-id" "$KEY_DIR/private-key.pem"
cp "$PROFILE" "$STATE/profile"

run_add_agent() {
    HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:/usr/bin:/bin" \
    MANAGED_MACHINE_ORG_PROFILE="$PROFILE" \
    MANAGED_MACHINE_APPLICATIONS_DIR="$APPS_DIR" \
    MANAGED_MACHINE_AGENT_SHARED_ROOT="$SHARED_ROOT" \
    bash "$TEST_DIR/add-agent" "$@"
}

# --- fail closed: unknown slug ---
if out="$(run_add_agent you-mystery-agent 2>&1)"; then
    echo 'expected unknown slug to fail closed' >&2
    exit 1
fi
grep -Fq 'not in the active roster' <<<"$out"
[[ ! -f "$STATE/sysadminctl.log" ]]

# --- fail closed: retired slug ---
if out="$(run_add_agent you-vscode-agent 2>&1)"; then
    echo 'expected retired slug to fail closed' >&2
    exit 1
fi
grep -Fq 'retired in the roster' <<<"$out"
[[ ! -f "$STATE/sysadminctl.log" ]]

# --- fail closed: malformed slug never reaches the roster ---
if run_add_agent 'bad;slug' >/dev/null 2>&1; then
    echo 'expected malformed slug to fail' >&2
    exit 1
fi

# --- active slug provisions the account ---
out="$(run_add_agent you-goose-agent)"
grep -Fq 'creating standard account you-goose-agent ("Goose")' <<<"$out"
grep -Fq 'ok: account you-goose-agent exists' <<<"$out"
grep -Fq 'ok: account is standard (not admin)' <<<"$out"
grep -Fq "ok: full name is 'Goose'" <<<"$out"
! grep -q 'identity.*ready\|agent-bot.*wired' <<<"$out"
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]
grep -q -- '-addUser you-goose-agent' "$STATE/sysadminctl.log"
grep -q -- '-fullName Goose' "$STATE/sysadminctl.log"

# The generated password never appears in output or in the recorded args.
if grep -E 'password [^<]' "$STATE/sysadminctl.log" | grep -vq '<redacted>'; then
    echo 'the generated password leaked into the log' >&2
    exit 1
fi

# The shared coordination space converged: sticky root, non-sticky lock area.
[[ -d "$SHARED_ROOT/agent-locks" ]]
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT")" == '1777' ]]
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT/agent-locks")" == '0777' ]]

# The same elevated phase joined the agents group (creating it first) and
# installed the cached avatar as the account picture, referenced by
# sysadminctl -picture and recorded in the directory.
grep -Fq 'ok: account is a member of the agents group' <<<"$out"
grep -Fq "ok: account picture is $PICTURES/you-goose-agent.png" <<<"$out"
grep -Fxq 'create agents ' "$STATE/dseditgroup.log"
grep -Fxq 'edit agents you-goose-agent' "$STATE/dseditgroup.log"
grep -q -- "-picture $PICTURES/you-goose-agent.png" "$STATE/sysadminctl.log"
[[ "$(cat "$PICTURES/you-goose-agent.png")" == 'PNG-A' ]]
[[ "$(cat "$STATE/picture-attr/you-goose-agent")" == "$PICTURES/you-goose-agent.png" ]]
grep -Fxq 'https://avatars.githubusercontent.com/in/4321?v=4' "$STATE/curl.log"
[[ ! -f "$STATE/gh.log" ]]   # the cached URL made the API call unnecessary
# The staged download never lingers outside the private temp dir.
[[ ! -e "$TEST_HOME/avatar" ]]

# Credentials and old identity markers remain byte-for-byte intact, and the
# runtime is never invoked or reported ready.
[[ "$(cat "$KEY_DIR/private-key.pem")" == 'existing external key' ]]
[[ "$(cat "$KEY_DIR/app-id")" == '4321' ]]
cmp -s "$MARKERS/you-goose-agent.doctor.json" "$STATE/expected-doctor-marker"
cmp -s "$MARKERS/you-goose-agent.keys.sha256" "$STATE/expected-key-marker"
[[ ! -e "$AGENT_HOME/.config/you-goose-agent" ]]
[[ ! -e "$STATE/agent-bot.log" ]]
[[ ! -e "$MARKERS/you-goose-agent.profile.json" ]]

# --- idempotent: second run verifies without creating again ---
out2="$(run_add_agent you-goose-agent)"
grep -Fq 'already exists — verifying' <<<"$out2"
grep -Fq 'ok: account you-goose-agent exists' <<<"$out2"
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]
# Converged account and picture mean no second elevated phase.
if grep -Fq 'converging the you-goose-agent agent account' <<<"$out2"; then
    echo 'a converged account must not be re-elevated' >&2
    exit 1
fi
[[ "$(wc -l <"$STATE/dseditgroup.log")" -eq 2 ]]
[[ ! -e "$STATE/sudo.log" ]]

# --- a changed avatar is a drift: one repair phase, no re-creation ---
printf 'PNG-B' >"$STATE/avatar"
out_drift="$(run_add_agent you-goose-agent)"
grep -Fq 'converging the you-goose-agent agent account' <<<"$out_drift"
[[ "$(cat "$PICTURES/you-goose-agent.png")" == 'PNG-B' ]]
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]

# --- lost group membership is repaired the same way ---
: >"$STATE/groups/agents"
out_group="$(run_add_agent you-goose-agent)"
grep -Fq 'converging the you-goose-agent agent account' <<<"$out_group"
grep -Fxq 'you-goose-agent' "$STATE/groups/agents"
grep -Fq 'ok: account is a member of the agents group' <<<"$out_group"

# --- a pre-existing record whose home is missing: account/harness setup can
#     request home creation, without seeding external identity credentials ---
rm -rf "$AGENT_HOME" "$STATE/markers/you-goose-agent".*
: >"$STATE/createhomedir.log"
out_home="$(run_add_agent you-goose-agent 2>&1)"
grep -Fq 'converging the you-goose-agent agent account' <<<"$out_home"
grep -Fxq -e '-c -u you-goose-agent' "$STATE/createhomedir.log"
[[ -d "$AGENT_HOME" ]]
[[ ! -e "$AGENT_HOME/.config/you-goose-agent" ]]
if grep -Fq 'cancelled or failed' <<<"$out_home"; then
    echo 'an approved phase that did its work must not be reported as cancelled' >&2
    exit 1
fi
# When the home cannot be created the phase says so, with the reason, and
# the report shows the key as not seeded rather than blaming the dialog.
rm -rf "$AGENT_HOME" "$STATE/markers/you-goose-agent".*
touch "$STATE/createhomedir-fail"
if out_nohome="$(run_add_agent you-goose-agent 2>&1)"; then
    echo 'a home that cannot be created must fail add-agent' >&2
    exit 1
fi
rm "$STATE/createhomedir-fail"
grep -Fq "home directory $AGENT_HOME for you-goose-agent is missing and could not be created" <<<"$out_nohome"
grep -Fq 'the elevated step to converge the you-goose-agent agent account failed after authorization' <<<"$out_nohome"
if grep -Fq 'cancelled or failed' <<<"$out_nohome"; then
    echo 'a failing elevated command must not be reported as a cancelled dialog' >&2
    exit 1
fi
run_add_agent you-goose-agent >/dev/null   # back to converged for what follows

# --- without a cached URL the public users API supplies the avatar over
#     curl, and the URL is cached for later runs ---
rm "$TEST_HOME/.config/you-goose-agent/bot-avatar-url"
rm -f "$STATE/gh.log"
run_add_agent you-goose-agent >/dev/null
grep -Fxq 'https://api.github.com/users/you-goose-agent%5Bbot%5D' "$STATE/curl.log"
[[ ! -f "$STATE/gh.log" ]]
[[ "$(tail -1 "$STATE/curl.log")" == 'https://avatars.githubusercontent.com/in/777?v=4' ]]
[[ "$(cat "$TEST_HOME/.config/you-goose-agent/bot-avatar-url")" == 'https://avatars.githubusercontent.com/in/777?v=4' ]]
# The cached URL then short-circuits the API on the next run.
api_calls="$(grep -c 'api.github.com' "$STATE/curl.log")"
run_add_agent you-goose-agent >/dev/null
[[ "$(grep -c 'api.github.com' "$STATE/curl.log")" -eq "$api_calls" ]]

# --- with the anonymous API unavailable, gh is the last resort ---
rm "$TEST_HOME/.config/you-goose-agent/bot-avatar-url"
touch "$STATE/curl-api-fail"
run_add_agent you-goose-agent >/dev/null
rm "$STATE/curl-api-fail"
grep -Fq 'api users/you-goose-agent%5Bbot%5D' "$STATE/gh.log"
[[ "$(tail -1 "$STATE/curl.log")" == 'https://avatars.githubusercontent.com/in/777?v=4' ]]

# --- an avatar off GitHub's avatar host is refused, never downloaded ---
echo 'https://evil.example/avatar.png' >"$TEST_HOME/.config/you-goose-agent/bot-avatar-url"
rm -f "$STATE/gh.log"
curl_lines="$(wc -l <"$STATE/curl.log")"
out_host="$(run_add_agent you-goose-agent)"
grep -Fq 'warn: no avatar resolved for you-goose-agent' <<<"$out_host"
[[ "$(wc -l <"$STATE/curl.log")" -eq "$curl_lines" ]]
[[ ! -f "$STATE/gh.log" ]]   # a cached (if unusable) URL still short-circuits the API
echo 'https://avatars.githubusercontent.com/in/4321?v=4' >"$TEST_HOME/.config/you-goose-agent/bot-avatar-url"

# --- an unreachable avatar is a warning; the installed picture stays ---
touch "$STATE/curl-fail"
out_down="$(run_add_agent you-goose-agent)"
grep -Fq 'warn: no avatar resolved for you-goose-agent' <<<"$out_down"
grep -Fq "ok: account picture is $PICTURES/you-goose-agent.png" <<<"$out_down"
rm "$STATE/curl-fail"

# --- a missing picture record is only a warning in the report ---
rm "$STATE/picture-attr/you-goose-agent"
touch "$STATE/curl-fail"
out_nopic="$(run_add_agent you-goose-agent)"
rm "$STATE/curl-fail"
grep -Fq 'warn: account picture is unset' <<<"$out_nopic"
run_add_agent you-goose-agent >/dev/null   # the next reachable run restores it
[[ "$(cat "$STATE/picture-attr/you-goose-agent")" == "$PICTURES/you-goose-agent.png" ]]

# --- an operator-supplied persona name is compared, not silently accepted ---
out3="$(run_add_agent you-goose-agent --full-name 'Goose McCloud')"
grep -Fq "warn: full name is 'Goose' (expected 'Goose McCloud')" <<<"$out3"

# --- admin membership is a hard compliance failure ---
printf 'you-goose-agent' >"$STATE/admins"
if out4="$(run_add_agent you-goose-agent)"; then
    echo 'expected admin membership to fail compliance' >&2
    exit 1
fi
grep -Fq 'is an administrator — agent accounts must be standard' <<<"$out4"
rm -f "$STATE/admins"

# --- Little Snitch presence surfaces the headless-hang warning ---
mkdir -p "$APPS_DIR/Little Snitch.app"
out5="$(run_add_agent you-goose-agent)"
grep -Fq 'Little Snitch is active' <<<"$out5"

# --- pre-existing wrong modes on the shared space are corrected on rerun ---
chmod 0700 "$SHARED_ROOT" "$SHARED_ROOT/agent-locks"
run_add_agent you-goose-agent >/dev/null
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT")" == '1777' ]]
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT/agent-locks")" == '0777' ]]

# --- uncorrectable wrong modes are a compliance failure, not an "ok" ---
# Simulate another owner's directory: report against a root this run cannot
# chmod by checking the report path directly with a bad, unowned-looking mode.
BAD_ROOT="$TEST_DIR/bad-shared"
mkdir -p "$BAD_ROOT/agent-locks"
chmod 0700 "$BAD_ROOT" "$BAD_ROOT/agent-locks"
if out6="$(HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    MANAGED_MACHINE_AGENT_SHARED_ROOT="$BAD_ROOT" \
    MANAGED_MACHINE_APPLICATIONS_DIR="$APPS_DIR" \
    bash -c 'source "'"$ROOT"'/lib/install.sh"; source "'"$TEST_DIR"'/lib-under-test/agent-account.sh"; agent_compliance_report you-goose-agent Goose')"; then
    echo 'expected wrong shared-space modes to fail compliance' >&2
    exit 1
fi
grep -Fq 'fail: shared agent space modes are 0700/0700' <<<"$out6"

# --- --with-harness installs the roster harness as the account (ENG-0339:
#     each agent account needs its own harness in its own home), records the
#     verdict, and converges like everything else; without the flag the
#     report only warns that the install is missing ---
run_add_agent you-goose-agent >/dev/null   # reconverge after the marker removal above
out_noharness="$(run_add_agent you-goose-agent)"
grep -Fq "warn: harness goose installation unverified for you-goose-agent — run 'managed-machine add-agent you-goose-agent --with-harness'" <<<"$out_noharness"
[[ ! -f "$STATE/managed-machine.log" ]]
[[ ! -e "$MARKERS/you-goose-agent.harness" ]]
if [[ -f "$STATE/sudo.log" ]]; then sudo_lines="$(wc -l <"$STATE/sudo.log")"; else sudo_lines=0; fi
out_harness="$(run_add_agent you-goose-agent --with-harness)"
grep -Fq 'converging the you-goose-agent agent account (group, picture, harness goose)' <<<"$out_harness"
grep -Fq 'ok: harness goose setup snapshot for you-goose-agent (historical success, not live readiness)' <<<"$out_harness"
grep -Fxq "$AGENT_HOME noninteractive account setup you-goose-agent --json" "$STATE/managed-machine.log"
[[ -x "$AGENT_HOME/.local/bin/goose" ]]
[[ "$(cat "$MARKERS/you-goose-agent.harness")" == 'ok goose' ]]
[[ "$(stat -f '%Lp' "$MARKERS/you-goose-agent.harness")" == '644' ]]
grep -Fq 'account-setup' "$MARKERS/you-goose-agent.account.json"
[[ ! -e "$MARKERS/you-goose-agent.harness.log" ]]
[[ "$(wc -l <"$STATE/sudo.log")" -eq $((sudo_lines + 1)) ]]   # harness setup runs as the account
[[ "$(sort -u "$STATE/sudo.log")" == 'you-goose-agent' ]]
# A recorded install makes the next --with-harness run a no-op…
rm "$AGENT_HOME/.local/bin/goose"
out_harness2="$(run_add_agent you-goose-agent --with-harness)"
grep -Fq 'converging the you-goose-agent agent account' <<<"$out_harness2"
grep -Fq 'ok: harness goose setup snapshot for you-goose-agent' <<<"$out_harness2"
[[ -x "$AGENT_HOME/.local/bin/goose" ]]
[[ "$(wc -l <"$STATE/managed-machine.log")" -eq 2 ]]
# …and a plain run reads the same record.
out_harness3="$(run_add_agent you-goose-agent)"
grep -Fq 'ok: harness goose setup snapshot for you-goose-agent' <<<"$out_harness3"

# A failed install is recorded with its exit status, reported as a warning
# that quotes the installer and names the log, flagged in the status summary,
# and retried by the next --with-harness run (a plain run leaves it alone).
rm "$MARKERS/you-goose-agent.harness"
touch "$STATE/harness-fail"
out_hfail="$(run_add_agent you-goose-agent --with-harness)"
grep -Fq "warn: harness goose setup snapshot for you-goose-agent failed" <<<"$out_hfail"
grep -Fq "$MARKERS/you-goose-agent.account.json" <<<"$out_hfail"
grep -Fq "$MARKERS/you-goose-agent.account.json" "$MARKERS/you-goose-agent.harness.err"
! grep -rq 'sensitive installer output' "$MARKERS"
! grep -q 'sensitive installer output' <<<"$out_hfail"
[[ "$(cat "$MARKERS/you-goose-agent.harness")" == 'failed goose 7' ]]
summary="$(HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    MANAGED_MACHINE_AGENT_SHARED_ROOT="$SHARED_ROOT" \
    bash -c 'source "'"$ROOT"'/lib/install.sh"; source "'"$TEST_DIR"'/lib-under-test/agent-account.sh"; agent_account_summary you-goose-agent')"
[[ "$summary" == 'account not-ready snapshot, harness install failed' ]]
mm_lines="$(wc -l <"$STATE/managed-machine.log")"
run_add_agent you-goose-agent >/dev/null
[[ "$(wc -l <"$STATE/managed-machine.log")" -eq "$mm_lines" ]]
rm "$STATE/harness-fail"
out_hretry="$(run_add_agent you-goose-agent --with-harness)"
grep -Fq 'ok: harness goose setup snapshot for you-goose-agent' <<<"$out_hretry"
[[ "$(cat "$MARKERS/you-goose-agent.harness")" == 'ok goose' ]]

# A harness whose CLI has its own setup-<harness>-cli script installs through
# it: the bare name is the dotfile step (codex) or the IDE (kiro).
[[ -x "$ROOT/setup-codex-cli" ]]
mkdir -p "$TEST_HOME/.config/you-codex-agent"
echo '4322' >"$TEST_HOME/.config/you-codex-agent/app-id"
printf -- '-----BEGIN PRIVATE KEY-----\nkey-codex\n-----END PRIVATE KEY-----\n' >"$TEST_HOME/.config/you-codex-agent/private-key.pem"
out_codex="$(run_add_agent you-codex-agent --with-harness)"
grep -Fq 'ok: harness codex-cli setup snapshot for you-codex-agent' <<<"$out_codex"
grep -Fxq "$STATE/homes/you-codex-agent noninteractive account setup you-codex-agent --json" "$STATE/managed-machine.log"
[[ -x "$STATE/homes/you-codex-agent/.local/bin/codex" ]]
mv "$KEY_DIR/private-key.pem" "$STATE/private-key.pem.aside"
rm "$MARKERS/you-goose-agent.profile.json"
touch "$STATE/harness-pending"
out_pending="$(run_add_agent you-goose-agent --with-harness)"
[[ "$(cat "$MARKERS/you-goose-agent.harness")" == 'pending goose 75' ]]
grep -Fq 'account setup snapshot: pending' <<<"$out_pending"
grep -Fq "$MARKERS/you-goose-agent.account.json" <<<"$out_pending"
grep -Fq "$MARKERS/you-goose-agent.account.json" "$MARKERS/you-goose-agent.harness.err"
cmp -s "$PROFILE" "$MARKERS/you-goose-agent.profile.json"
rm "$STATE/harness-pending"
mv "$STATE/private-key.pem.aside" "$KEY_DIR/private-key.pem"

# A roster row with no harness has nothing to install: --with-harness fails
# closed before any account work, and a plain run provisions without a
# harness line.
if out_bare="$(run_add_agent you-bare-agent --with-harness 2>&1)"; then
    echo 'expected --with-harness to fail closed without a roster harness' >&2
    exit 1
fi
grep -Fq 'the roster names no harness for you-bare-agent' <<<"$out_bare"
! grep -q 'you-bare-agent' "$STATE/sysadminctl.log"
touch "$STATE/curl-fail"   # no avatar, no profile: the bare provision only
out_bare_plain="$(run_add_agent you-bare-agent)"
rm "$STATE/curl-fail"
grep -Fq 'ok: account you-bare-agent exists' <<<"$out_bare_plain"
if grep -Eq '^(ok|warn): (the recorded )?harness ' <<<"$out_bare_plain"; then
    echo 'a slug without a roster harness must not get a harness line:' >&2
    grep -E '^(ok|warn): (the recorded )?harness ' <<<"$out_bare_plain" >&2
    exit 1
fi

# --- the CLI dispatches the verb ---
usage_out="$("$ROOT/bin/managed-machine" --help)"
grep -Fq 'add-agent <slug>' <<<"$usage_out"
grep -Fq -- '--with-harness' <<<"$usage_out"
grep -Fq -- '--with-harness' "$(bash "$TEST_DIR/add-agent" --help >"$TEST_DIR/usage" && echo "$TEST_DIR/usage")"

[[ ! -e "$STATE/agent-bot.log" ]]

echo 'add-agent tests passed'

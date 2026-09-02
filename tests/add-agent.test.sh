#!/usr/bin/env bash
set -euo pipefail

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

# agent-bot stub: records every invocation with the HOME it ran under, and
# answers doctor with a ready verdict unless $STATE/doctor-fail exists.
cat >"$FAKE_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "$HOME" "$*" >>"$STATE/agent-bot.log"
[[ "${AGENT_BOT_SUPERVISOR_SKIP_LOAD:-}" == 1 ]] || { echo "supervisor load must be skipped for a session-less account" >&2; exit 3; }
case "$1" in
    bootstrap)
        mkdir -p "$HOME/.config/agent-bot"
        echo '{"schema_version":1,"command":"bootstrap","ready":true}'
        ;;
    doctor)
        if [[ -e "$STATE/doctor-fail" ]]; then
            echo '{"schema_version":1,"command":"doctor","ready":false,"first_actionable_failure":{"scope":"machine","code":"supervisor-not-loaded","message":"the identity daemon supervisor unit is present but not loaded","action":"run: agent-bot install"}}'
        else
            echo '{"schema_version":1,"command":"doctor","ready":true,"first_actionable_failure":null}'
        fi
        ;;
    *) exit 2 ;;
esac
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
# The key seed and the wiring: root copies and chowns with absolute tools,
# the markers land in a fixed system directory, and agent-bot is the
# Homebrew binary run as the account (sudo -u), never as root.
grep -Fq 'MARK="/Library/Application Support/managed-machine/agents"' "$ROOT/scripts/add-agent"
grep -Fq '/usr/sbin/chown -R "$1:staff" "$H/.config/$1"' "$ROOT/scripts/add-agent"
grep -Fq 'AB=/opt/homebrew/opt/agent-bot/bin/agent-bot' "$ROOT/scripts/add-agent"
grep -Fq '/usr/bin/sudo -u "$1" -H /usr/bin/env PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin AGENT_BOT_SUPERVISOR_SKIP_LOAD=1 "$AB" bootstrap' "$ROOT/scripts/add-agent"
grep -Fq '/usr/bin/sudo -u "$1" -H /usr/bin/env PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin AGENT_BOT_SUPERVISOR_SKIP_LOAD=1 "$AB" doctor' "$ROOT/scripts/add-agent"
# Every agent-bot invocation in the elevated script goes through sudo -u.
if grep -E '"\$AB" [a-z]' "$ROOT/scripts/add-agent" | grep -Fvq '/usr/bin/sudo -u "$1"'; then
    echo 'agent-bot must run as the account, via sudo -u' >&2
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
# The App key fixture the operator holds, and the profile curl will serve.
KEY_DIR="$TEST_HOME/.config/you-goose-agent"
AGENT_HOME="$STATE/homes/you-goose-agent"
echo '4321' >"$KEY_DIR/app-id"
printf -- '-----BEGIN PRIVATE KEY-----\nkey-one\n-----END PRIVATE KEY-----\n' >"$KEY_DIR/private-key.pem"
chmod 0600 "$KEY_DIR/app-id" "$KEY_DIR/private-key.pem"
cp "$PROFILE" "$STATE/profile"
key_fingerprint() { cat "$1/app-id" "$1/private-key.pem" | shasum -a 256 | cut -d ' ' -f 1; }

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
grep -Fq 'ok: agent-bot is installed machine-wide' <<<"$out"
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

# The same phase seeded this App's key material into the agent home — only
# this slug's directory, owned by the account, private modes — recorded its
# fingerprint, and ran agent-bot's machine wiring as the account with the
# published profile, the roster scoped to the one App, and the supervisor
# load skipped (no login session to load it into).
grep -Fq 'ok: App key material for you-goose-agent is seeded from ~/.config/you-goose-agent' <<<"$out"
grep -Fq 'ok: agent-bot is wired for you-goose-agent (doctor --machine-only ready)' <<<"$out"
[[ "$(cat "$AGENT_HOME/.config/you-goose-agent/app-id")" == '4321' ]]
cmp -s "$KEY_DIR/private-key.pem" "$AGENT_HOME/.config/you-goose-agent/private-key.pem"
cmp -s "$KEY_DIR/bot-avatar-url" "$AGENT_HOME/.config/you-goose-agent/bot-avatar-url"
[[ "$(stat -f '%Lp' "$AGENT_HOME/.config")" == '700' ]]
[[ "$(stat -f '%Lp' "$AGENT_HOME/.config/you-goose-agent")" == '700' ]]
[[ "$(stat -f '%Lp' "$AGENT_HOME/.config/you-goose-agent/private-key.pem")" == '600' ]]
[[ "$(stat -f '%Lp' "$AGENT_HOME/.config/you-goose-agent/app-id")" == '600' ]]
grep -Fxq "you-goose-agent:staff $AGENT_HOME/.config" "$STATE/chown.log"
grep -Fxq -e "-R you-goose-agent:staff $AGENT_HOME/.config/you-goose-agent" "$STATE/chown.log"
[[ "$(cat "$MARKERS/you-goose-agent.keys.sha256")" == "$(key_fingerprint "$KEY_DIR")" ]]
[[ "$(stat -f '%Lp' "$MARKERS/you-goose-agent.keys.sha256")" == '644' ]]
grep -Fq 'organization-profile.json' "$STATE/curl.log"
cmp -s "$PROFILE" "$MARKERS/you-goose-agent.profile.json"
[[ "$(wc -l <"$STATE/sudo.log")" -eq 2 ]]
[[ "$(sort -u "$STATE/sudo.log")" == 'you-goose-agent' ]]
grep -Fxq "$AGENT_HOME bootstrap --profile $MARKERS/you-goose-agent.profile.json --scope-app you-goose-agent --with-gh-shim --machine-only --json" "$STATE/agent-bot.log"
grep -Fxq "$AGENT_HOME doctor --machine-only --json" "$STATE/agent-bot.log"
[[ "$(stat -f '%Lp' "$MARKERS/you-goose-agent.doctor.json")" == '644' ]]
# The key material itself never reaches the world-readable marker directory.
if grep -rq 'key-one' "$MARKERS"; then
    echo 'the private key leaked into the marker directory' >&2
    exit 1
fi

# --- idempotent: second run verifies without creating again ---
out2="$(run_add_agent you-goose-agent)"
grep -Fq 'already exists — verifying' <<<"$out2"
grep -Fq 'ok: account you-goose-agent exists' <<<"$out2"
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]
# Converged group, picture, key, and wiring mean no second elevated phase.
if grep -Fq 'converging the you-goose-agent agent account' <<<"$out2"; then
    echo 'a converged account must not be re-elevated' >&2
    exit 1
fi
[[ "$(wc -l <"$STATE/dseditgroup.log")" -eq 2 ]]
[[ "$(wc -l <"$STATE/sudo.log")" -eq 2 ]]

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

# --- a pre-existing record whose home is missing (no markers yet, as on a
#     machine where the record predates add-agent): the home is created in
#     the same elevated phase, the key seeded, nothing exits silently ---
rm -rf "$AGENT_HOME" "$STATE/markers/you-goose-agent".*
: >"$STATE/createhomedir.log"
out_home="$(run_add_agent you-goose-agent 2>&1)"
grep -Fq 'converging the you-goose-agent agent account' <<<"$out_home"
grep -Fxq -e '-c -u you-goose-agent' "$STATE/createhomedir.log"
[[ -f "$AGENT_HOME/.config/you-goose-agent/app-id" ]]
grep -Fq 'ok: App key material for you-goose-agent is seeded' <<<"$out_home"
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
#     curl, gh is never consulted, and the URL is cached for later runs ---
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

# --- a rotated key in the operator's ~/.config is a drift: reseeded, rewired ---
sudo_lines="$(wc -l <"$STATE/sudo.log")"
printf -- '-----BEGIN PRIVATE KEY-----\nkey-two\n-----END PRIVATE KEY-----\n' >"$KEY_DIR/private-key.pem"
out_rotate="$(run_add_agent you-goose-agent)"
grep -Fq 'converging the you-goose-agent agent account' <<<"$out_rotate"
grep -Fq 'ok: App key material for you-goose-agent is seeded' <<<"$out_rotate"
cmp -s "$KEY_DIR/private-key.pem" "$AGENT_HOME/.config/you-goose-agent/private-key.pem"
[[ "$(cat "$MARKERS/you-goose-agent.keys.sha256")" == "$(key_fingerprint "$KEY_DIR")" ]]
[[ "$(wc -l <"$STATE/sudo.log")" -eq $((sudo_lines + 2)) ]]
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]

# --- a not-ready verdict is reported with agent-bot's own code and fix ---
# A previous wiring left the account not ready; the rerun repairs (one more
# elevated phase) and, while the stub keeps failing, reports the verdict.
touch "$STATE/doctor-fail"
sudo_lines="$(wc -l <"$STATE/sudo.log")"
cp "$MARKERS/you-goose-agent.doctor.json" "$STATE/doctor-ready.json"
echo '{"ready":false,"first_actionable_failure":{"code":"daemon-not-running","message":"identity daemon is not running","action":"run: agent-bot install"}}' >"$MARKERS/you-goose-agent.doctor.json"
out_notready="$(run_add_agent you-goose-agent)"
grep -Fq 'converging the you-goose-agent agent account' <<<"$out_notready"
[[ "$(wc -l <"$STATE/sudo.log")" -eq $((sudo_lines + 2)) ]]
grep -Fq 'warn: agent-bot wiring for you-goose-agent is not-ready: supervisor-not-loaded: the identity daemon supervisor unit is present but not loaded (fix: run: agent-bot install)' <<<"$out_notready"
rm "$STATE/doctor-fail"
out_repaired="$(run_add_agent you-goose-agent)"   # the next run wires it
grep -Fq 'ok: agent-bot is wired for you-goose-agent' <<<"$out_repaired"

# --- a garbled verdict record is "pending", never "ready" ---
echo 'not json' >"$MARKERS/you-goose-agent.doctor.json"
touch "$STATE/curl-fail"   # no profile: report only, no repair attempt
out_garbled="$(run_add_agent you-goose-agent)"
rm "$STATE/curl-fail"
grep -Fq 'warn: agent-bot bootstrap pending for you-goose-agent' <<<"$out_garbled"
grep -Fq 'warn: could not fetch the organization profile' <<<"$out_garbled"
run_add_agent you-goose-agent >/dev/null

# --- without key material in the operator's home nothing is seeded or run ---
mv "$KEY_DIR/private-key.pem" "$STATE/private-key.pem.aside"
sudo_lines="$(wc -l <"$STATE/sudo.log")"
curl_lines="$(wc -l <"$STATE/curl.log")"
out_nokey="$(run_add_agent you-goose-agent)"
grep -Fq "warn: no App key material in $KEY_DIR — run 'agent-bot ensure-private-key --app you-goose-agent' as yourself" <<<"$out_nokey"
grep -Fq "warn: App key pending — run 'agent-bot ensure-private-key --app you-goose-agent' as yourself" <<<"$out_nokey"
if grep -Fq 'converging the you-goose-agent agent account' <<<"$out_nokey"; then
    echo 'a missing key source must not trigger an elevated phase' >&2
    exit 1
fi
[[ "$(wc -l <"$STATE/sudo.log")" -eq "$sudo_lines" ]]
[[ "$(wc -l <"$STATE/curl.log")" -eq "$curl_lines" ]]   # no profile fetch either
mv "$STATE/private-key.pem.aside" "$KEY_DIR/private-key.pem"

# --- the report never claims a seed it cannot fingerprint ---
rm "$MARKERS/you-goose-agent.keys.sha256"
out_nomark="$(HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    MANAGED_MACHINE_AGENT_SHARED_ROOT="$SHARED_ROOT" \
    MANAGED_MACHINE_APPLICATIONS_DIR="$APPS_DIR" \
    bash -c 'source "'"$ROOT"'/lib/install.sh"; source "'"$TEST_DIR"'/lib-under-test/agent-account.sh"; agent_compliance_report you-goose-agent Goose' || true)"
grep -Fq 'warn: App key material for you-goose-agent is not seeded, or differs' <<<"$out_nomark"

# --- the CLI dispatches the verb ---
usage_out="$("$ROOT/bin/managed-machine" --help)"
grep -Fq 'add-agent <slug>' <<<"$usage_out"

echo 'add-agent tests passed'

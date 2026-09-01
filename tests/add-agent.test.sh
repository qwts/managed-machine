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

# curl stub: the avatar download. Serves the fixture bytes in $STATE/avatar,
# records the URL, and fails while $STATE/curl-fail exists.
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
cp "$STATE/avatar" "$dest"
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
mkdir -p "$TEST_DIR/lib-under-test"
sed -e 's|/usr/bin/dscl|dscl|g' -e 's|/usr/bin/dsmemberutil|dsmemberutil|g' \
    -e "s|/Library/User Pictures/agents|$PICTURES|g" \
    "$ROOT/lib/agent-account.sh" >"$TEST_DIR/lib-under-test/agent-account.sh"
sed -e "s|^REPO_ROOT=.*|REPO_ROOT=\"$ROOT\"|" \
    -e "s|source \"\$REPO_ROOT/lib/agent-account.sh\"|source \"$TEST_DIR/lib-under-test/agent-account.sh\"|" \
    -e 's|/usr/sbin/sysadminctl|sysadminctl|' \
    -e 's|/usr/sbin/createhomedir|createhomedir|' \
    -e 's|/usr/sbin/dseditgroup|dseditgroup|g' \
    -e 's|/usr/bin/dscl|dscl|g' \
    -e "s|/Library/User Pictures/agents|$PICTURES|g" \
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
grep -Fq 'warn: agent-bot is not installed' <<<"$out"
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

# --- idempotent: second run verifies without creating again ---
out2="$(run_add_agent you-goose-agent)"
grep -Fq 'already exists — verifying' <<<"$out2"
grep -Fq 'ok: account you-goose-agent exists' <<<"$out2"
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]
# Converged group and picture mean no second elevated phase at all.
if grep -Fq 'converging group membership and picture' <<<"$out2"; then
    echo 'a converged account must not be re-elevated' >&2
    exit 1
fi
[[ "$(wc -l <"$STATE/dseditgroup.log")" -eq 2 ]]

# --- a changed avatar is a drift: one repair phase, no re-creation ---
printf 'PNG-B' >"$STATE/avatar"
out_drift="$(run_add_agent you-goose-agent)"
grep -Fq 'converging group membership and picture' <<<"$out_drift"
[[ "$(cat "$PICTURES/you-goose-agent.png")" == 'PNG-B' ]]
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]

# --- lost group membership is repaired the same way ---
: >"$STATE/groups/agents"
out_group="$(run_add_agent you-goose-agent)"
grep -Fq 'converging group membership and picture' <<<"$out_group"
grep -Fxq 'you-goose-agent' "$STATE/groups/agents"
grep -Fq 'ok: account is a member of the agents group' <<<"$out_group"

# --- without a cached URL the users API supplies the avatar ---
rm "$TEST_HOME/.config/you-goose-agent/bot-avatar-url"
run_add_agent you-goose-agent >/dev/null
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

# --- an untraversable agent home reads as unverifiable, not as pending ---
cat >"$FAKE_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$FAKE_BIN/agent-bot"
mkdir -p "$STATE/homes/you-goose-agent/.config"
chmod 0000 "$STATE/homes/you-goose-agent"
out7="$(run_add_agent you-goose-agent)"
chmod 0755 "$STATE/homes/you-goose-agent"
grep -Fq 'cannot inspect' <<<"$out7"
if grep -Fq 'bootstrap pending' <<<"$out7"; then
    echo 'an unreadable home must not be reported as pending' >&2
    exit 1
fi
rm -f "$FAKE_BIN/agent-bot"

# --- the CLI dispatches the verb ---
usage_out="$("$ROOT/bin/managed-machine" --help)"
grep -Fq 'add-agent <slug>' <<<"$usage_out"

echo 'add-agent tests passed'

#!/usr/bin/env bash
# add-agent --all: provision every active roster identity in roster order so
# fleet Macs never need a hand-written per-account loop. Retired identities
# are skipped, failures are collected with a summary, and --with-harness is
# passed through to each account. The single-slug flow is untouched: the
# loop re-execs the same script once per slug, so each account keeps its own
# administrator prompt and verdicts.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
FAKE_BIN="$TEST_DIR/bin"
STATE="$TEST_DIR/state"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_HOME" "$FAKE_BIN" "$STATE/users" "$STATE/homes"
export STATE MANAGED_MACHINE_AGENT_SHARED_ROOT="$STATE/shared"

PROFILE="$TEST_DIR/organization-profile.json"
cat >"$PROFILE" <<'EOF'
{
  "identities": [
    { "slug": "you-goose-agent", "harness": "goose", "status": "active" },
    { "slug": "you-codex-agent", "harness": "codex", "status": "active" },
    { "slug": "you-vscode-agent", "harness": "vscode", "status": "retired" }
  ]
}
EOF
export MANAGED_MACHINE_ORG_PROFILE="$PROFILE"

# osascript stub: execute the elevated command directly so the
# account-mutation stubs actually run.
cat >"$FAKE_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
while [[ "${1:-}" == "-e" ]]; do shift 2; done
shift # the label
exec "$@"
EOF

# sysadminctl stub: record the invocation and mark the account created.
# Fails for $SYSADMINCTL_FAIL_SLUG to simulate a per-account failure.
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
[[ "$name" != "${SYSADMINCTL_FAIL_SLUG:-}" ]] || exit 9
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
            printf 'GroupMembership: root %s\n' "$(cat "$STATE/admins" 2>/dev/null || true)"
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

# curl stub: the avatar download and the organization profile fetch.
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
case "$url" in
    */organization-profile.json) cp "$STATE/profile" "$dest" ;;
    https://api.github.com/users/*)
        printf '{"login":"you-goose-agent[bot]","avatar_url":"https://avatars.githubusercontent.com/in/777?v=4"}\n'
        ;;
    *) printf 'PNG\n' >"$dest" ;;
esac
EOF
cp "$PROFILE" "$STATE/profile"
printf 'PNG-A\n' >"$STATE/avatar"

# sudo stub: run the command as the target account's home.
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

cat >"$FAKE_BIN/chown" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STATE/chown.log"
EOF

# agent-bot stub: answers doctor ready; wiring is skipped here (no key
# source) but the stub must exist on PATH for the compliance report.
cat >"$FAKE_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "$HOME" "$*" >>"$STATE/agent-bot.log"
case "$1" in
    doctor) echo '{"schema_version":1,"command":"doctor","ready":true,"first_actionable_failure":null}' ;;
    *) exit 2 ;;
esac
EOF

# managed-machine stub: the headless harness install per account.
cat >"$FAKE_BIN/managed-machine" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "$HOME" "$*" >>"$STATE/managed-machine.log"
[[ "$#" == 4 && "$1" == account && "$2" == setup && "$3" == "${HOME##*/}" && "$4" == --json ]] || exit 3
[[ -r "$STATE/markers/$3.profile.json" ]] || exit 4
printf '{"schema_version":1,"command":"account-setup","account":"%s","status":"ready","ready":true,"checks":[]}\n' "$3"
EOF

# gh stub: the users API fallback for the avatar URL.
cat >"$FAKE_BIN/gh" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == "api" && "$2" == users/*%5Bbot%5D ]] || exit 1
echo 'https://avatars.githubusercontent.com/in/777?v=4'
EOF
chmod +x "$FAKE_BIN"/*

# Rewrites under test: absolute tool paths and system directories become the
# state-backed stubs, exactly like tests/add-agent.test.sh. The --all loop's
# self-call is rewired to the test copy as well; production keeps the
# hardcoded "$REPO_ROOT/scripts/add-agent" path (asserted below).
PICTURES="$STATE/pictures"
MARKERS="$STATE/markers"
mkdir -p "$TEST_DIR/lib-under-test"
sed -e 's|/usr/bin/dscl|dscl|g' -e 's|/usr/bin/dsmemberutil|dsmemberutil|g' \
    -e "s|/Library/User Pictures/agents|$PICTURES|g" \
    -e "s|/Library/Application Support/managed-machine/agents|$MARKERS|g" \
    "$ROOT/lib/agent-account.sh" >"$TEST_DIR/lib-under-test/agent-account.sh"
sed -e "s|^REPO_ROOT=.*|REPO_ROOT=\"$ROOT\"|" \
    -e "s|source \"\$REPO_ROOT/lib/agent-account.sh\"|source \"$TEST_DIR/lib-under-test/agent-account.sh\"|" \
    -e "s|\"\$REPO_ROOT/scripts/add-agent\" \"\$slug\"|\"$TEST_DIR/add-agent\" \"\$slug\"|" \
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
chmod +x "$TEST_DIR/add-agent"

# Production posture: the loop delegates through the hardcoded script path —
# never an environment-overridable self path that would run behind the
# operator's shell before elevation.
grep -Fq '"$REPO_ROOT/scripts/add-agent" "$slug"' "$ROOT/scripts/add-agent"
if grep -q 'ADD_AGENT_SELF\|ADD_AGENT_BIN\|ADD_AGENT_PATH' "$ROOT/scripts/add-agent"; then
    echo 'the --all self-call must stay a hardcoded path' >&2
    exit 1
fi

run_all() {
    HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
        /bin/bash "$TEST_DIR/add-agent" --all "$@"
}

# 1. --all provisions every active identity in roster order, skips the
# retired one, and reports the count.
run_all >"$TEST_DIR/all.out" 2>&1
grep -Fq '==> provisioning 2 agent accounts: you-goose-agent you-codex-agent' "$TEST_DIR/all.out"
grep -Fq 'each account approves its own administrator prompt' "$TEST_DIR/all.out"
grep -q -- '-addUser you-goose-agent' "$STATE/sysadminctl.log"
grep -q -- '-addUser you-codex-agent' "$STATE/sysadminctl.log"
if grep -q -- '-addUser you-vscode-agent' "$STATE/sysadminctl.log"; then
    echo 'retired identity must not be provisioned' >&2
    exit 1
fi
grep -Fq 'add-agent --all: 2 provisioned, 0 failed' "$TEST_DIR/all.out"
grep -Fq 'ok: account you-goose-agent exists' "$TEST_DIR/all.out"
grep -Fq 'ok: account you-codex-agent exists' "$TEST_DIR/all.out"

# 2. Rerun converges without duplicating: still exit 0 and the same summary.
run_all >"$TEST_DIR/rerun.out" 2>&1
grep -Fq 'add-agent --all: 2 provisioned, 0 failed' "$TEST_DIR/rerun.out"

# 3. One failing account is collected, named, and fails the run; the other
# account is still provisioned.
rm -rf "$STATE/users" "$STATE/homes" "$STATE/groups" "$STATE/sysadminctl.log"
mkdir -p "$STATE/users" "$STATE/homes"
SYSADMINCTL_FAIL_SLUG=you-codex-agent \
    HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    /bin/bash "$TEST_DIR/add-agent" --all >"$TEST_DIR/partial.out" 2>"$TEST_DIR/partial.err" || rc=$?
[[ "${rc:-0}" -ne 0 ]]
grep -Fq 'add-agent --all: 1 provisioned, 1 failed: you-codex-agent' "$TEST_DIR/partial.err"
grep -q -- '-addUser you-goose-agent' "$STATE/sysadminctl.log"
unset SYSADMINCTL_FAIL_SLUG
rm -rf "$STATE/users" "$STATE/homes" "$STATE/groups" "$STATE/sysadminctl.log"
mkdir -p "$STATE/users" "$STATE/homes"

# 4. --all takes no slug and no --full-name.
if run_all you-goose-agent >"$TEST_DIR/conflict.out" 2>&1; then
    echo 'expected --all with a slug to fail' >&2
    exit 1
fi
grep -Fq -- '--all takes no slug' "$TEST_DIR/conflict.out"
if HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    /bin/bash "$TEST_DIR/add-agent" --all --full-name Goose >"$TEST_DIR/conflict2.out" 2>&1; then
    echo 'expected --all with --full-name to fail' >&2
    exit 1
fi
grep -Fq -- '--full-name takes no effect with --all' "$TEST_DIR/conflict2.out"

# 5. No roster source fails closed before any account is touched.
if env -u MANAGED_MACHINE_ORG_PROFILE HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    /bin/bash "$TEST_DIR/add-agent" --all >"$TEST_DIR/noroster.out" 2>&1; then
    echo 'expected --all without a roster to fail' >&2
    exit 1
fi
grep -Fq 'no roster source found' "$TEST_DIR/noroster.out"
[[ ! -e "$STATE/sysadminctl.log" ]]

# 6. A roster with no active identities fails closed.
cat >"$TEST_DIR/retired-profile.json" <<'EOF'
{"identities": [{ "slug": "you-vscode-agent", "harness": "vscode", "status": "retired" }]}
EOF
if MANAGED_MACHINE_ORG_PROFILE="$TEST_DIR/retired-profile.json" \
    HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    /bin/bash "$TEST_DIR/add-agent" --all >"$TEST_DIR/none.out" 2>&1; then
    echo 'expected --all with no active identities to fail' >&2
    exit 1
fi
grep -Fq 'no active agent identities' "$TEST_DIR/none.out"
[[ ! -e "$STATE/sysadminctl.log" ]]

# 7. --with-harness passes through: each account runs headless account
# setup and records a harness verdict.
rm -rf "$STATE/users" "$STATE/homes" "$STATE/groups" "$STATE/markers"
mkdir -p "$STATE/users" "$STATE/homes"
HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    /bin/bash "$TEST_DIR/add-agent" --all --with-harness >"$TEST_DIR/harness.out" 2>&1
grep -Fq 'add-agent --all: 2 provisioned, 0 failed' "$TEST_DIR/harness.out"
grep -Fq "account setup you-goose-agent --json" "$STATE/managed-machine.log"
grep -Fq "account setup you-codex-agent --json" "$STATE/managed-machine.log"
grep -qxF 'ok goose' "$MARKERS/you-goose-agent.harness"
# codex resolves through the shipped setup-codex-cli script, exercising the
# agent_harness_setup_name -cli branch.
grep -qxF 'ok codex-cli' "$MARKERS/you-codex-agent.harness"

echo 'add-agent --all tests passed'

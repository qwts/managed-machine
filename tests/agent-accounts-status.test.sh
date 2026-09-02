#!/usr/bin/env bash
# `managed-machine status` lists the agent accounts (#93): one line per
# roster slug from the directory and the markers root recorded, read-only,
# never elevating and never reading an agent home.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
STATE="$TEST_DIR/state"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
FIXTURE="$TEST_DIR/fixture"
export STATE
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$STATE/users" "$STATE/groups" "$STATE/homes" "$CONFIG_REPO" \
    "$FIXTURE/scripts" "$FIXTURE/lib"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"

# Keep status off the host Homebrew/rustup, and give it an agent-bot on PATH.
for cmd in brew rustup rustc cargo; do
    printf '%s\n' '#!/usr/bin/env bash' 'exit 1' >"$TEST_BIN/$cmd"
done
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$TEST_BIN/agent-bot"

# Directory stubs, driven by $STATE: users/<name> exists = account exists,
# groups/<group> lists members, admins lists admin members, picture-attr/<name>
# is the Picture record.
cat >"$TEST_BIN/dscl" <<'EOF'
#!/usr/bin/env bash
[[ "${2:-}" == "-read" ]] || exit 1
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
    /Groups/admin) printf 'GroupMembership: root you %s\n' "$(cat "$STATE/admins" 2>/dev/null || true)" ;;
    /Groups/*)
        group="${3#/Groups/}"
        [[ -f "$STATE/groups/$group" ]] || exit 56
        printf 'GroupMembership: %s\n' "$(tr '\n' ' ' <"$STATE/groups/$group")"
        ;;
    *) exit 56 ;;
esac
EOF
cat >"$TEST_BIN/dsmemberutil" <<'EOF'
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
chmod +x "$TEST_BIN"/*

# The helpers call /usr/bin/dscl absolutely and use the system pictures and
# markers directories; run a copy of the tree with those rewritten, the code
# otherwise byte-identical (the same rewrite add-agent.test.sh uses).
PICTURES="$STATE/pictures"
MARKERS="$STATE/markers"
mkdir -p "$PICTURES" "$MARKERS" "$STATE/picture-attr"
cp "$ROOT"/lib/*.sh "$FIXTURE/lib/"
sed -e 's|/usr/bin/dscl|dscl|g' -e 's|/usr/bin/dsmemberutil|dsmemberutil|g' \
    -e "s|/Library/User Pictures/agents|$PICTURES|g" \
    -e "s|/Library/Application Support/managed-machine/agents|$MARKERS|g" \
    "$ROOT/lib/agent-account.sh" >"$FIXTURE/lib/agent-account.sh"
cp "$ROOT/scripts/status" "$FIXTURE/scripts/status"
chmod +x "$FIXTURE/scripts/status"

ROSTER="$TEST_DIR/organization-profile.json"
cat >"$ROSTER" <<'EOF'
{"schema_version":1,"identities":[
  {"slug":"you-goose-agent","harness":"goose","status":"active"},
  {"slug":"you-devin-agent","harness":"devin","status":"active"},
  {"slug":"you-cline-agent","harness":"cline","status":"active"},
  {"slug":"you-aider-agent","harness":"aider","status":"active"},
  {"slug":"you-old-agent","harness":"old","status":"retired"},
  {"slug":"you-gone-agent","harness":"gone","status":"retired"}
]}
EOF

# Accounts: goose fully converged; devin without a picture and with a
# not-ready doctor verdict; cline admin, outside the agents group, with no
# markers and no key material; aider not provisioned; old retired but
# lingering; gone retired and absent.
for slug in you-goose-agent you-devin-agent you-cline-agent you-old-agent; do
    printf '%s\n' "${slug#you-}" >"$STATE/users/$slug"
    mkdir -p "$STATE/homes/$slug"
done
printf '%s\n' you-goose-agent you-devin-agent you-old-agent >"$STATE/groups/agents"
printf '%s\n' you-cline-agent >"$STATE/admins"
for slug in you-goose-agent you-old-agent; do
    printf 'png\n' >"$PICTURES/$slug.png"
    printf '%s\n' "$PICTURES/$slug.png" >"$STATE/picture-attr/$slug"
done
for slug in you-goose-agent you-devin-agent; do
    mkdir -p "$TEST_HOME/.config/$slug"
    printf 'id-%s\n' "$slug" >"$TEST_HOME/.config/$slug/app-id"
    printf 'key-%s\n' "$slug" >"$TEST_HOME/.config/$slug/private-key.pem"
    cat "$TEST_HOME/.config/$slug/app-id" "$TEST_HOME/.config/$slug/private-key.pem" \
        | shasum -a 256 | cut -d ' ' -f 1 >"$MARKERS/$slug.keys.sha256"
done
echo '{"schema_version":1,"command":"doctor","ready":true,"first_actionable_failure":null}' >"$MARKERS/you-goose-agent.doctor.json"
echo '{"schema_version":1,"command":"doctor","ready":false,"first_actionable_failure":{"scope":"machine","code":"supervisor-not-loaded","message":"the identity daemon supervisor unit is present but not loaded","action":"run: agent-bot install"}}' >"$MARKERS/you-devin-agent.doctor.json"

run_status() {
    HOME="$TEST_HOME" \
    NVM_DIR="$TEST_HOME/.nvm" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$TEST_DIR/system-apps" \
    CARGO_HOME="$TEST_HOME/.cargo" \
    RUSTUP_HOME="$TEST_HOME/.rustup" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    "$@" /bin/bash "$FIXTURE/scripts/status"
}

snapshot() {
    find "$TEST_HOME" "$STATE" -print | LC_ALL=C sort
}

# 1. One line per account, from the roster order; counts in the header;
# retired-and-absent slugs are not listed; status exits 0 and writes nothing.
BEFORE="$(snapshot)"
run_status env MANAGED_MACHINE_ORG_PROFILE="$ROSTER" >"$TEST_DIR/full.out"
[[ "$BEFORE" == "$(snapshot)" ]]
grep -qE '^agent-accounts +1 of 4 active roster accounts ready$' "$TEST_DIR/full.out"
grep -qE '^  you-goose-agent +ready$' "$TEST_DIR/full.out"
grep -qE '^  you-devin-agent +not-ready: supervisor-not-loaded, no picture$' "$TEST_DIR/full.out"
grep -qE '^  you-cline-agent +unwired, admin \(fail\), not in agents group, no picture, key pending$' "$TEST_DIR/full.out"
grep -qE '^  you-aider-agent +not provisioned$' "$TEST_DIR/full.out"
grep -qE '^  you-old-agent +retired, account lingers' "$TEST_DIR/full.out"
! grep -q 'you-gone-agent' "$TEST_DIR/full.out"
# The section sits between the update block and local-bin, and the rest of
# the report still follows it.
[[ "$(grep -n '^agent-accounts' "$TEST_DIR/full.out" | cut -d: -f1)" -lt "$(grep -n '^local-bin' "$TEST_DIR/full.out" | cut -d: -f1)" ]]
grep -qE '^homebrew ' "$TEST_DIR/full.out"
# Nothing from an agent home or a key file leaks into the report.
! grep -q 'key-you\|id-you\|private-key' "$TEST_DIR/full.out"

# 2. Rotated key material in ~/.config/<slug> shows as not seeded.
printf 'rotated\n' >"$TEST_HOME/.config/you-goose-agent/private-key.pem"
run_status env MANAGED_MACHINE_ORG_PROFILE="$ROSTER" >"$TEST_DIR/rotated.out"
grep -qE '^  you-goose-agent +ready, key not seeded$' "$TEST_DIR/rotated.out"
grep -qE '^agent-accounts +0 of 4 active roster accounts ready$' "$TEST_DIR/rotated.out"

# 3. No roster anywhere: one line saying so, and the report goes on.
run_status env -u MANAGED_MACHINE_ORG_PROFILE >"$TEST_DIR/no-roster.out"
grep -qE '^agent-accounts +no roster \(install agent-bot, or set MANAGED_MACHINE_ORG_PROFILE\)$' "$TEST_DIR/no-roster.out"
grep -qE '^local-bin ' "$TEST_DIR/no-roster.out"

# 4. The installed agent-bot config is the default roster source.
mkdir -p "$TEST_HOME/.config/agent-bot"
printf '{"profile":{"identities":[{"slug":"you-goose-agent","harness":"goose","status":"active"}]}}\n' >"$TEST_HOME/.config/agent-bot/config.json"
run_status env -u MANAGED_MACHINE_ORG_PROFILE >"$TEST_DIR/config.out"
grep -qE '^agent-accounts +0 of 1 active roster accounts ready$' "$TEST_DIR/config.out"
grep -qE '^  you-goose-agent +ready, key not seeded$' "$TEST_DIR/config.out"

echo 'agent-accounts status tests passed'

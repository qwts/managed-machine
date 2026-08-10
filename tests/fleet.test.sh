#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
CONFIG_REPO_ROOT="$TEST_ROOT/managed-machine-config"
REPO_ROOT="$ROOT"
HOME="$TEST_HOME"
export HOME CONFIG_REPO_ROOT REPO_ROOT
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_HOME/.ssh" "$CONFIG_REPO_ROOT/ssh"
printf 'v0.9.0\n' >"$CONFIG_REPO_ROOT/local-bin.ref"

ssh-keygen -q -t ed25519 -N '' -f "$TEST_ROOT/key-one"
ssh-keygen -q -t ed25519 -N '' -f "$TEST_ROOT/key-two"
KEY_ONE="$(awk '{print $1 " " $2}' "$TEST_ROOT/key-one.pub")"
KEY_TWO="$(awk '{print $1 " " $2}' "$TEST_ROOT/key-two.pub")"

{
    echo '# Public keys for host-to-host SSH among your machines.'
    echo '# first-mac 2026-08-01'
    echo "$KEY_ONE"
    echo '# second-mac 2026-08-02'
    echo "$KEY_TWO"
} >"$CONFIG_REPO_ROOT/ssh/authorized_keys"

{
    echo 'ssh-ed25519 AAAAuserowned unrelated@example'
    echo '# BEGIN home-bin new-machine'
    echo 'ssh-ed25519 AAAAlegacy legacy@example'
    echo '# END home-bin new-machine'
} >"$TEST_HOME/.ssh/authorized_keys"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/fleet.sh
source "$ROOT/lib/fleet.sh"
FLEET_CHANGED=0

import_legacy_fleet_entries
[[ "$(find "$CONFIG_REPO_ROOT/fleet/machines" -type f -name '*.toml' | wc -l | tr -d ' ')" == "2" ]]

register_current_machine "$TEST_ROOT/key-one.pub"
CURRENT_RECORD="$CONFIG_REPO_ROOT/fleet/machines/$CURRENT_MACHINE_ID.toml"
[[ -f "$CURRENT_RECORD" ]]
[[ "$(toml_value "$CURRENT_RECORD" managed_machine_ref)" != "legacy" ]]
grep -qxF 'managed_at = "2026-08-01T00:00:00Z"' "$CURRENT_RECORD"

generate_fleet_authorized_keys
sync_local_authorized_keys
[[ "$(grep -c '^ssh-' "$CONFIG_REPO_ROOT/ssh/authorized_keys")" == "2" ]]
[[ "$(grep -c '^# BEGIN managed-machine$' "$TEST_HOME/.ssh/authorized_keys")" == "1" ]]
grep -qxF 'ssh-ed25519 AAAAuserowned unrelated@example' "$TEST_HOME/.ssh/authorized_keys"
! grep -q 'AAAAlegacy' "$TEST_HOME/.ssh/authorized_keys"
! grep -q '^public_key' "$(local_machine_state_file)"

FIRST_RECORD_HASH="$(shasum -a 256 "$CURRENT_RECORD")"
FLEET_CHANGED=0
import_legacy_fleet_entries
register_current_machine "$TEST_ROOT/key-one.pub"
generate_fleet_authorized_keys
[[ "$FLEET_CHANGED" == "0" ]]
[[ "$(shasum -a 256 "$CURRENT_RECORD")" == "$FIRST_RECORD_HASH" ]]

LIST_OUTPUT="$(list_fleet_machines)"
CURRENT_HOSTNAME="$(hostname -s)"
[[ "$LIST_OUTPUT" == *$'MACHINE_ID\tHOSTNAME\tMANAGED_AT'* ]]
[[ "$LIST_OUTPUT" == *"$CURRENT_HOSTNAME"* ]]
[[ "$LIST_OUTPUT" == *'second-mac'* ]]

SECOND_FINGERPRINT="$(ssh_fingerprint "$TEST_ROOT/key-two.pub")"
SECOND_MACHINE_ID="$(machine_id_from_fingerprint "$SECOND_FINGERPRINT")"
remove_fleet_machine "$SECOND_MACHINE_ID"
[[ ! -f "$CONFIG_REPO_ROOT/fleet/machines/$SECOND_MACHINE_ID.toml" ]]
grep -qxF "$KEY_ONE" "$CONFIG_REPO_ROOT/ssh/authorized_keys"
! grep -qxF "$KEY_TWO" "$CONFIG_REPO_ROOT/ssh/authorized_keys"

if remove_fleet_machine 'sha256-does-not-exist' >/dev/null 2>&1; then
    echo 'expected unknown machine removal to fail' >&2
    exit 1
fi
if remove_fleet_machine '../unsafe' >/dev/null 2>&1; then
    echo 'expected invalid machine ID to fail' >&2
    exit 1
fi

GH_LOG="$TEST_ROOT/gh.log"
GH_FAIL_SIGNING=0
gh() {
    if [[ "$*" == *'-X DELETE'* ]]; then
        printf '%s\n' "$*" >>"$GH_LOG"
    elif [[ "$*" == *'user/ssh_signing_keys'* ]]; then
        if [[ "$GH_FAIL_SIGNING" == "1" ]]; then
            return 1
        fi
        printf '22\t%s\n' "$KEY_ONE"
    elif [[ "$*" == *'user/keys'* ]]; then
        printf '11\t%s\n' "$KEY_ONE"
    fi
}
GH_FAIL_SIGNING=1
if revoke_github_public_key "$KEY_ONE" >/dev/null 2>&1; then
    echo 'expected GitHub signing-key lookup failure to propagate' >&2
    exit 1
fi
[[ -f "$CURRENT_RECORD" ]]
GH_FAIL_SIGNING=0
revoke_github_public_key "$KEY_ONE"
[[ "$(wc -l <"$GH_LOG" | tr -d ' ')" == "2" ]]

git init -q "$CONFIG_REPO_ROOT"
git -C "$CONFIG_REPO_ROOT" config user.name 'managed-machine test'
git -C "$CONFIG_REPO_ROOT" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO_ROOT" config commit.gpgsign false
if HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    /bin/bash "$ROOT/scripts/fleet" remove "$CURRENT_MACHINE_ID" >"$TEST_ROOT/noninteractive.out" 2>&1; then
    echo 'expected noninteractive removal without --yes to fail' >&2
    exit 1
fi
grep -Fq 'refusing noninteractive removal without --yes' "$TEST_ROOT/noninteractive.out"

echo 'fleet tests passed'

#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/../legacy" && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
REMOTE="$TEST_ROOT/managed-machine-config.git"
SOURCE="$TEST_ROOT/source"
CHECKOUT="$TEST_ROOT/checkout"
FAKE_BIN="$TEST_ROOT/bin"
GH_LOG="$TEST_ROOT/gh.log"
export TEST_REMOTE="$REMOTE" GH_LOG
trap 'rm -rf "$TEST_ROOT"' EXIT

configure_test_repo() {
    git -C "$1" config user.name 'managed-machine test'
    git -C "$1" config user.email 'managed-machine-test@example.invalid'
    git -C "$1" config commit.gpgsign false
}

write_machine() {
    local id="$1"
    local public_key="$2"
    cat >"$SOURCE/fleet/machines/$id.toml" <<EOF
schema_version = 1
machine_id = "$id"
hostname = "$id"
managed_at = "2026-08-10T00:00:00Z"
public_key = "$public_key"
EOF
}

mkdir -p "$TEST_HOME/.ssh" "$SOURCE/fleet/machines" "$SOURCE/ssh" "$FAKE_BIN"
ssh-keygen -q -t ed25519 -N '' -f "$TEST_ROOT/key-one"
ssh-keygen -q -t ed25519 -N '' -f "$TEST_ROOT/key-two"
KEY_ONE="$(awk '{print $1 " " $2}' "$TEST_ROOT/key-one.pub")"
KEY_TWO="$(awk '{print $1 " " $2}' "$TEST_ROOT/key-two.pub")"
MACHINE_ONE='sha256-removal-one'
MACHINE_TWO='sha256-removal-two'
write_machine "$MACHINE_ONE" "$KEY_ONE"
write_machine "$MACHINE_TWO" "$KEY_TWO"
printf '# generated\n%s\n%s\n' "$KEY_ONE" "$KEY_TWO" >"$SOURCE/ssh/authorized_keys"
printf 'v0.9.0\n' >"$SOURCE/local-bin.ref"

git init --quiet --bare "$REMOTE"
git init --quiet "$SOURCE"
configure_test_repo "$SOURCE"
git -C "$SOURCE" add .
git -C "$SOURCE" commit --quiet -m 'Seed removal test fleet'
git -C "$SOURCE" branch -M main
git -C "$SOURCE" remote add origin "$REMOTE"
git -C "$SOURCE" push --quiet -u origin main
git --git-dir="$REMOTE" symbolic-ref HEAD refs/heads/main
git clone --quiet "$REMOTE" "$CHECKOUT"
configure_test_repo "$CHECKOUT"

cat >"$FAKE_BIN/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "$*" == *'-X DELETE'* ]]; then
    if git --git-dir="$TEST_REMOTE" cat-file -e "main:fleet/machines/$TEST_MACHINE_ID.toml" 2>/dev/null; then
        echo 'GitHub key revocation occurred before fleet removal was published' >&2
        exit 90
    fi
    if [[ "${GH_DELETE_FAIL:-0}" == "1" ]]; then
        exit 91
    fi
    printf '%s\n' "$*" >>"$GH_LOG"
elif [[ "$*" == *'user/ssh_signing_keys'* ]]; then
    printf '22\t%s\n' "$TEST_PUBLIC_KEY"
elif [[ "$*" == *'user/keys'* ]]; then
    printf '11\t%s\n' "$TEST_PUBLIC_KEY"
fi
EOF
chmod +x "$FAKE_BIN/gh"

run_remove() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CHECKOUT" \
    PATH="$FAKE_BIN:$PATH" \
    TEST_MACHINE_ID="$1" \
    TEST_PUBLIC_KEY="$2" \
    GH_DELETE_FAIL="${3:-0}" \
        /bin/bash "$ROOT/scripts/fleet" remove "$1" --yes --revoke-github
}

# Dry-run before removal: requires no --yes on noninteractive stdin, mutates
# nothing locally or remotely, and reports the exact plan and qualified remote
# outcome in text and JSON.
BEFORE_COMMIT="$(git -C "$CHECKOUT" rev-parse HEAD)"
HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CHECKOUT" PATH="$FAKE_BIN:$PATH" \
    /bin/bash "$ROOT/scripts/fleet" remove "$MACHINE_ONE" --dry-run </dev/null >"$TEST_ROOT/dry.out" 2>"$TEST_ROOT/dry.err"
[ ! -s "$TEST_ROOT/dry.err" ]
grep -Fq "Dry run: fleet remove $MACHINE_ONE (no changes made; local preflight passed; live exit depends on remote sync)" "$TEST_ROOT/dry.out"
grep -Fq "would remove $CHECKOUT/fleet/machines/$MACHINE_ONE.toml" "$TEST_ROOT/dry.out"
grep -Fq "would synchronize $CHECKOUT (requires remote fetch/rebase/push)" "$TEST_ROOT/dry.out"
[[ -f "$CHECKOUT/fleet/machines/$MACHINE_ONE.toml" ]]
[[ "$(git -C "$CHECKOUT" rev-parse HEAD)" == "$BEFORE_COMMIT" ]]
[ ! -e "$GH_LOG" ]

HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CHECKOUT" PATH="$FAKE_BIN:$PATH" \
    /bin/bash "$ROOT/scripts/fleet" remove "$MACHINE_ONE" --revoke-github --dry-run --json </dev/null >"$TEST_ROOT/dry.json" 2>"$TEST_ROOT/dry.err"
[ ! -s "$TEST_ROOT/dry.err" ]
[[ -f "$CHECKOUT/fleet/machines/$MACHINE_ONE.toml" ]]
[[ "$(git -C "$CHECKOUT" rev-parse HEAD)" == "$BEFORE_COMMIT" ]]
[ ! -e "$GH_LOG" ]
python3 - "$TEST_ROOT/dry.json" "$MACHINE_ONE" "$CHECKOUT" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["schema_version"] == 1
assert data["command"] == "fleet-remove"
assert data["ready"] is True
assert data["dry_run"] is True
assert "would_exit" not in data
assert data["local_preflight_passed"] is True
assert data["expected_exit_if_remote_succeeds"] == 0
assert data["remote_outcomes_verified"] is False
assert data["remote_dependencies"] == ["config_repo_sync", "github_key_revocation"]
assert "remote git fetch/rebase/push" in data["outcome_qualification"]
assert data["machine_id"] == sys.argv[2]
assert data["record_present"] is True
assert data["would_sync_config_repo"] is True
assert data["would_commit_and_push_config_repo"] is True
assert data["would_revoke_github_keys"] is True
assert data["config_checkout"]["path"] == sys.argv[3]
PY

# Fresh-HOME and legacy-origin non-materialization tests for fleet remove --dry-run:
ISOLATED_ROOT="$TEST_ROOT/isolated-root/libexec"
mkdir -p "$ISOLATED_ROOT"
ln -s "$ROOT/lib" "$ISOLATED_ROOT/lib"
ln -s "$ROOT/scripts" "$ISOLATED_ROOT/scripts"
git clone --quiet "$REMOTE" "$ISOLATED_ROOT/managed-machine-config"

FRESH_HOME="$TEST_ROOT/fresh-home"
mkdir -p "$FRESH_HOME"
if env -u CONFIG_REPO_ROOT HOME="$FRESH_HOME" PATH="$FAKE_BIN:$PATH" \
    MANAGED_MACHINE_CONFIG_REPO_URL="$REMOTE" \
    /bin/bash "$ISOLATED_ROOT/scripts/fleet" remove "$MACHINE_ONE" --dry-run --json </dev/null >"$TEST_ROOT/fresh-dry.json" 2>"$TEST_ROOT/fresh-dry.err"; then
    echo 'expected fleet remove --dry-run on fresh HOME without checkout to fail' >&2
    exit 1
fi
[ ! -s "$TEST_ROOT/fresh-dry.err" ]
[ ! -e "$FRESH_HOME/.local/share/managed-machine" ]
python3 - "$TEST_ROOT/fresh-dry.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["error"]["code"] == "fleet-config-repo-unavailable"
assert data["error"]["evidence"]["dry_run"] is True
PY

LEGACY_HOME="$TEST_ROOT/legacy-home"
LEGACY_CHECKOUT="$LEGACY_HOME/.local/share/managed-machine/managed-machine-config"
mkdir -p "$(dirname "$LEGACY_CHECKOUT")"
git clone --quiet "$REMOTE" "$LEGACY_CHECKOUT"
configure_test_repo "$LEGACY_CHECKOUT"
git -C "$LEGACY_CHECKOUT" remote set-url origin 'git@github.com:qwts/managed-machine-config.git'
env -u CONFIG_REPO_ROOT HOME="$LEGACY_HOME" PATH="$FAKE_BIN:$PATH" \
    MANAGED_MACHINE_CONFIG_REPO_URL='https://github.com/qwts/managed-machine-config.git' \
    /bin/bash "$ISOLATED_ROOT/scripts/fleet" remove "$MACHINE_ONE" --dry-run --json </dev/null >"$TEST_ROOT/legacy-dry.json" 2>"$TEST_ROOT/legacy-dry.err"
[ ! -s "$TEST_ROOT/legacy-dry.err" ]
[[ "$(git -C "$LEGACY_CHECKOUT" remote get-url origin)" == 'git@github.com:qwts/managed-machine-config.git' ]]

# Local sync preflight parity: missing origin and wrong branch fail dry-run
# just as live sync_managed_machine_config_repo fails.
NO_ORIGIN="$TEST_ROOT/no-origin"
git clone --quiet "$REMOTE" "$NO_ORIGIN"
configure_test_repo "$NO_ORIGIN"
git -C "$NO_ORIGIN" remote remove origin
if HOME="$TEST_HOME" CONFIG_REPO_ROOT="$NO_ORIGIN" PATH="$FAKE_BIN:$PATH" \
    /bin/bash "$ROOT/scripts/fleet" remove "$MACHINE_ONE" --dry-run --json </dev/null >"$TEST_ROOT/no-origin.json" 2>"$TEST_ROOT/no-origin.err"; then
    echo 'expected dry-run on checkout without origin remote to fail' >&2
    exit 1
fi
[ ! -s "$TEST_ROOT/no-origin.err" ]
python3 - "$TEST_ROOT/no-origin.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["error"]["code"] == "fleet-config-repo-no-origin"
assert data["error"]["evidence"]["dry_run"] is True
PY

WRONG_BRANCH="$TEST_ROOT/wrong-branch"
git clone --quiet "$REMOTE" "$WRONG_BRANCH"
configure_test_repo "$WRONG_BRANCH"
git -C "$WRONG_BRANCH" checkout --quiet -b other-branch
if HOME="$TEST_HOME" CONFIG_REPO_ROOT="$WRONG_BRANCH" PATH="$FAKE_BIN:$PATH" \
    /bin/bash "$ROOT/scripts/fleet" remove "$MACHINE_ONE" --dry-run --json </dev/null >"$TEST_ROOT/wrong-branch.json" 2>"$TEST_ROOT/wrong-branch.err"; then
    echo 'expected dry-run on checkout on wrong branch to fail' >&2
    exit 1
fi
[ ! -s "$TEST_ROOT/wrong-branch.err" ]
python3 - "$TEST_ROOT/wrong-branch.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["error"]["code"] == "fleet-config-repo-wrong-branch"
assert data["error"]["evidence"]["dry_run"] is True
PY

# Dry-run on an unknown machine ID exits 1 with fleet-machine-not-found.
if HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CHECKOUT" PATH="$FAKE_BIN:$PATH" \
    /bin/bash "$ROOT/scripts/fleet" remove "sha256-missing-machine" --dry-run --json </dev/null >"$TEST_ROOT/dry-missing.json" 2>"$TEST_ROOT/dry-missing.err"; then
    echo 'expected dry-run on missing machine to fail' >&2
    exit 1
fi
[ ! -s "$TEST_ROOT/dry-missing.err" ]
python3 - "$TEST_ROOT/dry-missing.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["error"]["code"] == "fleet-machine-not-found"
assert data["error"]["evidence"]["dry_run"] is True
PY

# Successful revocation observes that the canonical record is already absent.
run_remove "$MACHINE_ONE" "$KEY_ONE"
if git --git-dir="$REMOTE" cat-file -e "main:fleet/machines/$MACHINE_ONE.toml" 2>/dev/null; then
    echo 'expected first fleet record to be removed remotely' >&2
    exit 1
fi
[ ! -f "$TEST_HOME/.config/managed-machine/pending-github-key-revocations/$MACHINE_ONE.pub" ]

# A remote revocation failure leaves only the public key locally. The same
# command retries revocation after the fleet removal has already been pushed.
if run_remove "$MACHINE_TWO" "$KEY_TWO" 1 >"$TEST_ROOT/revoke-failure.out" 2>&1; then
    echo 'expected synthetic GitHub revocation failure' >&2
    exit 1
fi
if git --git-dir="$REMOTE" cat-file -e "main:fleet/machines/$MACHINE_TWO.toml" 2>/dev/null; then
    echo 'expected second fleet record to be removed before failed revocation' >&2
    exit 1
fi
PENDING="$TEST_HOME/.config/managed-machine/pending-github-key-revocations/$MACHINE_TWO.pub"
[[ -f "$PENDING" ]]
grep -qxF "$KEY_TWO" "$PENDING"

# Pending-only revocation still synchronizes the config repo first during live
# removal, so dry-run must also reject unrelated dirty files in that state.
printf 'unrelated edit\n' >"$CHECKOUT/unrelated.txt"
if HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CHECKOUT" PATH="$FAKE_BIN:$PATH" \
    /bin/bash "$ROOT/scripts/fleet" remove "$MACHINE_TWO" --revoke-github --dry-run --json </dev/null >"$TEST_ROOT/dry-pending-dirty.json" 2>"$TEST_ROOT/dry-pending-dirty.err"; then
    echo 'expected pending-only dry-run with dirty config checkout to fail' >&2
    exit 1
fi
[ ! -s "$TEST_ROOT/dry-pending-dirty.err" ]
python3 - "$TEST_ROOT/dry-pending-dirty.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["error"]["code"] == "fleet-config-repo-dirty"
assert data["error"]["evidence"]["dry_run"] is True
PY
rm -f "$CHECKOUT/unrelated.txt"

# Dry-run when the fleet record was already removed and the checkout is clean.
HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CHECKOUT" PATH="$FAKE_BIN:$PATH" \
    /bin/bash "$ROOT/scripts/fleet" remove "$MACHINE_TWO" --revoke-github --dry-run --json </dev/null >"$TEST_ROOT/dry-pending.json"
python3 - "$TEST_ROOT/dry-pending.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["dry_run"] is True
assert data["local_preflight_passed"] is True
assert data["record_present"] is False
assert data["pending_revocation_present"] is True
assert data["would_sync_config_repo"] is True
assert data["would_commit_and_push_config_repo"] is False
assert data["would_revoke_github_keys"] is True
PY
[[ -f "$PENDING" ]]

run_remove "$MACHINE_TWO" "$KEY_TWO"
[ ! -f "$PENDING" ]
[[ "$(wc -l <"$GH_LOG" | tr -d ' ')" == "4" ]]

echo 'fleet removal tests passed'

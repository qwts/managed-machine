#!/usr/bin/env bash
# S5 L05 characterization test (#315): once fleet/cutover.yaml (kind:
# fleet-cutover) lands in the managed-machine-config checkout, every legacy
# Git-writing fleet path refuses before any authorization dialog and before any
# mutation, while read-only inspection (fleet list, ssh status, status) keeps
# working unchanged.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

FIX="$TEST_ROOT/fixture"
REMOTE="$TEST_ROOT/origin.git"
SEED="$TEST_ROOT/seed"
CONFIG="$TEST_ROOT/managed-machine-config"
HOME_DIR="$TEST_ROOT/home"
STATE="$HOME_DIR/.config/managed-machine"
STUB_BIN="$TEST_ROOT/bin"
SENTINEL_LOG="$TEST_ROOT/sentinel.log"

mkdir -p "$FIX/bin" "$FIX/scripts" "$FIX/Formula" "$SEED/fleet/machines" "$SEED/ssh" \
    "$STATE" "$HOME_DIR/.ssh" "$STUB_BIN"
chmod 700 "$HOME_DIR/.ssh"
: >"$SENTINEL_LOG"

cp "$ROOT/bin/managed-machine" "$FIX/bin/"
cp "$ROOT/scripts/fleet" "$ROOT/scripts/ssh" "$ROOT/scripts/status" "$ROOT/scripts/catalog" "$FIX/scripts/"
cp -R "$ROOT/lib" "$FIX/lib"
chmod +x "$FIX/bin/managed-machine" "$FIX/scripts/"*
printf '0.0.0-fixture\n' >"$FIX/VERSION"
printf 'class ManagedMachine < Formula\n  version "0.0.0"\nend\n' >"$FIX/Formula/managed-machine.rb"

# Sentinel stubs: none of these external binaries may be invoked during cutover
# refusal or read-only fleet/ssh/status inspection.
for cmd in ssh-keygen ssh-add ssh osascript sudo security brew; do
    cat >"$STUB_BIN/$cmd" <<EOF
#!/usr/bin/env bash
printf '%s %s\n' "$cmd" "\$*" >>"$SENTINEL_LOG"
exit 99
EOF
    chmod +x "$STUB_BIN/$cmd"
done
printf '#!/usr/bin/env bash\nprintf "fixture-host\\n"\n' >"$STUB_BIN/hostname"
printf '#!/usr/bin/env bash\nprintf "fixture-host\\n"\n' >"$STUB_BIN/scutil"
chmod +x "$STUB_BIN/hostname" "$STUB_BIN/scutil"
ln -s "$(command -v python3)" "$STUB_BIN/python3"
ln -s "$(command -v git)" "$STUB_BIN/git"

# Redirect any hardcoded /usr/bin/osascript in the fixture copy to the sentinel
# stub so an accidental dialog invocation is caught offline without touching macOS.
sed -i.bak "s|/usr/bin/osascript|$STUB_BIN/osascript|g" "$FIX/lib/ssh-enroll.sh"
rm -f "$FIX/lib/ssh-enroll.sh.bak"
cat >>"$FIX/lib/agent-account.sh" <<'STUB'
agent_account_exists() { return 1; }
agent_account_in_group() { return 1; }
STUB

MIN_PATH="$STUB_BIN:/usr/bin:/bin"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

configure_test_repo() {
    git -C "$1" config user.name 'managed-machine test'
    git -C "$1" config user.email 'managed-machine-test@example.invalid'
    git -C "$1" config commit.gpgsign false
}

# Seed two registry records in the shape write_machine_record produces.
cat >"$SEED/fleet/machines/sha256-fixture_machine_aaaa.toml" <<'TOML'
schema_version = 1
machine_id = "sha256-fixture_machine_aaaa"
hostname = "fixture-mac"
managed_at = "2026-10-15T01:00:00Z"
managed_machine_ref = "v0.0.0"
local_bin_ref = "v1.2.3"
ssh_key_fingerprint = "SHA256:fixture/machine+aaaa"
public_key = "ssh-ed25519 AAAAfixtureaaaa"
TOML

cat >"$SEED/fleet/machines/sha256-fixture_machine_bbbb.toml" <<'TOML'
schema_version = 1
machine_id = "sha256-fixture_machine_bbbb"
hostname = "second-mac"
managed_at = "2026-10-16T02:00:00Z"
managed_machine_ref = "v0.0.0"
local_bin_ref = "v1.2.4"
ssh_key_fingerprint = "SHA256:fixture/machine+bbbb"
public_key = "ssh-ed25519 AAAAfixturebbbb"
TOML

cat >"$SEED/ssh/authorized_keys" <<'EOF'
# Public keys for host-to-host SSH among your machines.
# Generated from managed-machine-config/fleet/machines by managed-machine.
# fixture-mac 2026-10-15 sha256-fixture_machine_aaaa
ssh-ed25519 AAAAfixtureaaaa
# second-mac 2026-10-16 sha256-fixture_machine_bbbb
ssh-ed25519 AAAAfixturebbbb
EOF
printf 'v1.2.4\n' >"$SEED/local-bin.ref"

git init --quiet --bare "$REMOTE"
git init --quiet "$SEED"
configure_test_repo "$SEED"
git -C "$SEED" add .
git -C "$SEED" commit --quiet -m 'Seed fixture fleet'
git -C "$SEED" branch -M main
git -C "$SEED" remote add origin "$REMOTE"
git -C "$SEED" push --quiet -u origin main
git --git-dir="$REMOTE" symbolic-ref HEAD refs/heads/main
git clone --quiet "$REMOTE" "$CONFIG"
configure_test_repo "$CONFIG"

# 1. Verify fleet_cut_over is false before cutover.yaml exists and when kind
#    does not match fleet-cutover.
(
    export CONFIG_REPO_ROOT="$CONFIG"
    # shellcheck source=/dev/null
    source "$FIX/lib/install.sh"
    # shellcheck source=/dev/null
    source "$FIX/lib/fleet.sh"
    if fleet_cut_over; then
        fail "fleet_cut_over succeeded before fleet/cutover.yaml existed"
    fi
    printf 'schema_version: 1\nkind: other-document\n' >"$CONFIG/fleet/cutover.yaml"
    if fleet_cut_over; then
        fail "fleet_cut_over succeeded for non-fleet-cutover document"
    fi
    rm -f "$CONFIG/fleet/cutover.yaml"
)

# Write and commit the cutover document to the fixture checkout and fake origin.
cat >"$CONFIG/fleet/cutover.yaml" <<'YAML'
schema_version: 1
kind: fleet-cutover
cut_over_at: 2026-10-16T03:00:00Z
reviewed_by:
  - alice
YAML
git -C "$CONFIG" add fleet/cutover.yaml
git -C "$CONFIG" commit --quiet -m 'Cut over fleet management'
git -C "$CONFIG" push --quiet origin main

# Snapshot fixture checkout HEAD, index, working tree, fake origin, and HOME.
snapshot_state() {
    local dest="$1"
    python3 - "$CONFIG" "$REMOTE" "$HOME_DIR" "$dest" <<'PY'
import hashlib, json, os, subprocess, sys

config, remote, home_dir, dest = sys.argv[1:]

def tree_digest(root):
    entries = {}
    for base, dirs, files in os.walk(root):
        dirs.sort()
        files.sort()
        for name in files:
            full = os.path.join(base, name)
            rel = os.path.relpath(full, root)
            with open(full, "rb") as f:
                entries[rel] = hashlib.sha256(f.read()).hexdigest()
    return entries

state = {
    "config_head": subprocess.check_output(["git", "-C", config, "rev-parse", "HEAD"], text=True).strip(),
    "config_index": subprocess.check_output(["git", "-C", config, "ls-files", "--stage"], text=True),
    "config_status": subprocess.check_output(["git", "-C", config, "status", "--porcelain", "--ignored"], text=True),
    "config_tree": tree_digest(config),
    "origin_head": subprocess.check_output(["git", "--git-dir=" + remote, "rev-parse", "HEAD"], text=True).strip(),
    "origin_refs": subprocess.check_output(["git", "--git-dir=" + remote, "show-ref"], text=True),
    "origin_tree": tree_digest(remote),
    "home_tree": tree_digest(home_dir),
}
with open(dest, "w", encoding="utf-8") as f:
    json.dump(state, f, indent=2, sort_keys=True)
PY
}

BEFORE_SNAP="$TEST_ROOT/before.snap.json"
AFTER_SNAP="$TEST_ROOT/after.snap.json"
snapshot_state "$BEFORE_SNAP"

assert_unchanged() {
    local label="$1"
    if [[ -s "$SENTINEL_LOG" ]]; then
        fail "$label: invoked forbidden external binary: $(cat "$SENTINEL_LOG")"
    fi
    snapshot_state "$AFTER_SNAP"
    if ! diff -u "$BEFORE_SNAP" "$AFTER_SNAP"; then
        fail "$label: mutated fixture checkout, fake origin, or HOME"
    fi
}

assert_plain_refusal() {
    local label="$1" out_file="$2" err_file="$3" rc="$4"
    [[ "$rc" -eq 1 ]] || fail "$label: expected exit 1, got $rc"
    [[ ! -s "$out_file" ]] || fail "$label: expected empty stdout, got: $(cat "$out_file")"
    local line_count
    line_count="$(wc -l <"$err_file" | tr -d '[:space:]')"
    [[ "$line_count" == "1" ]] || fail "$label: expected 1 stderr line, got $line_count: $(cat "$err_file")"
    grep -q '^Error: ' "$err_file" || fail "$label: stderr does not start with Error:: $(cat "$err_file")"
    grep -Fq 'managed-machine fleet --plan --user' "$err_file" || fail "$label: stderr does not name 'managed-machine fleet --plan --user': $(cat "$err_file")"
}

assert_json_refusal() {
    local label="$1" expected_cmd="$2" out_file="$3" err_file="$4" rc="$5"
    [[ "$rc" -eq 1 ]] || fail "$label: expected exit 1, got $rc"
    [[ ! -s "$err_file" ]] || fail "$label: expected empty stderr under --json, got: $(cat "$err_file")"
    python3 - "$label" "$expected_cmd" "$out_file" <<'PY'
import json, sys
label, expected_cmd, path = sys.argv[1:]
with open(path, encoding="utf-8") as f:
    data = json.load(f)
assert data["schema_version"] == 1, f"{label}: unexpected schema_version: {data}"
assert data["command"] == expected_cmd, f"{label}: expected command {expected_cmd}, got {data.get('command')}"
assert data["ready"] is False, f"{label}: expected ready=False"
assert data["status"] == "not_ready", f"{label}: expected status=not_ready"
assert data["error"]["code"] == "fleet-cut-over", f"{label}: expected code fleet-cut-over, got {data['error']}"
assert data["error"]["message"].startswith("Error: "), f"{label}: expected Error: prefix in {data['error']['message']}"
assert "managed-machine fleet --plan --user" in data["error"]["message"], f"{label}: missing replacement command in message"
assert "managed-machine fleet --plan --user" in data["error"]["action"], f"{label}: missing replacement command in action"
assert len(data["checks"]) == 1 and data["checks"][0]["code"] == "fleet-cut-over", f"{label}: unexpected checks: {data['checks']}"
PY
}

run_mm() {
    local out_file="$1" err_file="$2"
    shift 2
    local rc=0
    set +e
    env -i HOME="$HOME_DIR" PATH="$MIN_PATH" LANG=C.UTF-8 LC_ALL=C.UTF-8 \
        GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
        CONFIG_REPO_ROOT="$CONFIG" MANAGED_MACHINE_CONFIG_REPO_URL="$REMOTE" \
        bash "$FIX/bin/managed-machine" "$@" </dev/null >"$out_file" 2>"$err_file"
    rc=$?
    set -e
    return "$rc"
}

# 2. fleet_cut_over returns 0 and sync_managed_machine_config_repo refuses
#    before commit_managed_fleet_changes.
OUT="$TEST_ROOT/sync.out"
ERR="$TEST_ROOT/sync.err"
COMMIT_CALLED="$TEST_ROOT/commit_called"
rc=0
set +e
(
    export HOME="$HOME_DIR" PATH="$MIN_PATH" CONFIG_REPO_ROOT="$CONFIG" MANAGED_MACHINE_CONFIG_REPO_URL="$REMOTE"
    # shellcheck source=/dev/null
    source "$FIX/lib/install.sh"
    # shellcheck source=/dev/null
    source "$FIX/lib/fleet.sh"
    fleet_cut_over || exit 91
    commit_managed_fleet_changes() {
        printf 'called\n' >"$COMMIT_CALLED"
        return 0
    }
    sync_managed_machine_config_repo "$CONFIG" "Synchronize managed machine fleet"
) >"$OUT" 2>"$ERR"
rc=$?
set -e
[[ ! -e "$COMMIT_CALLED" ]] || fail "sync_managed_machine_config_repo reached commit_managed_fleet_changes"
assert_plain_refusal "sync_managed_machine_config_repo" "$OUT" "$ERR" "$rc"
assert_unchanged "sync_managed_machine_config_repo"

# 3. fleet remove refusals (with and without --dry-run, plain and --json, with
#    and without --yes / --revoke-github).
MACHINE_ID="sha256-fixture_machine_bbbb"

for flags in \
    "--dry-run" \
    "" \
    "--yes" \
    "--yes --revoke-github"
do
    label="fleet remove $flags (plain)"
    rc=0
    # shellcheck disable=SC2086
    if run_mm "$OUT" "$ERR" fleet remove "$MACHINE_ID" $flags; then
        rc=0
    else
        rc=$?
    fi
    assert_plain_refusal "$label" "$OUT" "$ERR" "$rc"
    assert_unchanged "$label"
done

for flags in \
    "--dry-run --json" \
    "--json" \
    "--yes --json" \
    "--yes --revoke-github --json"
do
    label="fleet remove $flags"
    rc=0
    # shellcheck disable=SC2086
    if run_mm "$OUT" "$ERR" fleet remove "$MACHINE_ID" $flags; then
        rc=0
    else
        rc=$?
    fi
    assert_json_refusal "$label" "fleet-remove" "$OUT" "$ERR" "$rc"
    assert_unchanged "$label"
done

# 4. ssh enroll --fleet refusals (plain and --json, alone or combined with
#    --authentication / --signing) before the authorization dialog and before
#    any mutation.
for flags in \
    "--fleet" \
    "--authentication --fleet" \
    "--signing --fleet" \
    "--authentication --signing --fleet"
do
    label="ssh enroll $flags (plain)"
    rc=0
    # shellcheck disable=SC2086
    if run_mm "$OUT" "$ERR" ssh enroll $flags; then
        rc=0
    else
        rc=$?
    fi
    assert_plain_refusal "$label" "$OUT" "$ERR" "$rc"
    assert_unchanged "$label"

    label="ssh enroll $flags --json"
    rc=0
    # shellcheck disable=SC2086
    if run_mm "$OUT" "$ERR" ssh enroll $flags --json; then
        rc=0
    else
        rc=$?
    fi
    assert_json_refusal "$label" "ssh-enroll" "$OUT" "$ERR" "$rc"
    assert_unchanged "$label"
done

# 5. Read-only fleet list (--json and plain) still lists both machines and
#    leaves the fixture checkout, fake origin, and HOME byte-identical.
run_mm "$OUT" "$ERR" fleet list --json
[[ ! -s "$ERR" ]] || fail "fleet list --json wrote to stderr: $(cat "$ERR")"
python3 - "$OUT" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
assert data["schema_version"] == 1
assert data["command"] == "fleet-list"
assert data["ready"] is True
ids = [m["machine_id"] for m in data["machines"]]
assert ids == ["sha256-fixture_machine_aaaa", "sha256-fixture_machine_bbbb"], f"unexpected machines: {ids}"
PY
assert_unchanged "fleet list --json"

run_mm "$OUT" "$ERR" fleet list
[[ ! -s "$ERR" ]] || fail "fleet list wrote to stderr: $(cat "$ERR")"
grep -Fq 'sha256-fixture_machine_aaaa	fixture-mac' "$OUT" || fail "fleet list missing first machine"
grep -Fq 'sha256-fixture_machine_bbbb	second-mac' "$OUT" || fail "fleet list missing second machine"
assert_unchanged "fleet list"

run_mm "$OUT" "$ERR" ssh status --json
[[ ! -s "$ERR" ]] || fail "ssh status --json wrote to stderr: $(cat "$ERR")"
python3 - "$OUT" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
assert data["schema_version"] == 1
assert data["command"] == "ssh-status"
assert data["ready"] is True
PY
assert_unchanged "ssh status --json"

run_mm "$OUT" "$ERR" status --section ssh --json
[[ ! -s "$ERR" ]] || fail "status --section ssh --json wrote to stderr: $(cat "$ERR")"
python3 - "$OUT" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
assert data["schema_version"] == 1
assert data["command"] == "status"
assert "ssh" in data["sections"]
PY
assert_unchanged "status --section ssh --json"

echo "fleet cutover characterization: all refusals and read-only checks passed"

#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
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

# Successful revocation observes that the canonical record is already absent.
run_remove "$MACHINE_ONE" "$KEY_ONE"
if git --git-dir="$REMOTE" cat-file -e "main:fleet/machines/$MACHINE_ONE.toml" 2>/dev/null; then
    echo 'expected first fleet record to be removed remotely' >&2
    exit 1
fi
[[ ! -f "$TEST_HOME/.config/managed-machine/pending-github-key-revocations/$MACHINE_ONE.pub" ]]

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

run_remove "$MACHINE_TWO" "$KEY_TWO"
[[ ! -f "$PENDING" ]]
[[ "$(wc -l <"$GH_LOG" | tr -d ' ')" == "4" ]]

echo 'fleet removal tests passed'

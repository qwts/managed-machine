#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
XDG_DATA_HOME="$TEST_ROOT/data"
REMOTE="$TEST_ROOT/managed-machine-config.git"
SEED_SOURCE="$TEST_ROOT/seed-source"
REPO_ROOT="$TEST_ROOT/libexec"
MANAGED_MACHINE_CONFIG_REPO_URL="$REMOTE"
export HOME="$TEST_HOME" XDG_DATA_HOME REPO_ROOT MANAGED_MACHINE_CONFIG_REPO_URL
trap 'rm -rf "$TEST_ROOT"' EXIT

configure_test_repo() {
    git -C "$1" config user.name 'managed-machine test'
    git -C "$1" config user.email 'managed-machine-test@example.invalid'
    git -C "$1" config commit.gpgsign false
}

write_machine() {
    local repo="$1"
    local id="$2"
    local key="$3"
    mkdir -p "$repo/fleet/machines"
    cat >"$repo/fleet/machines/$id.toml" <<EOF
machine_id = "$id"
hostname = "$id"
managed_at = "2026-08-10T00:00:00Z"
public_key = "$key"
EOF
}

regenerate_test_authorized_keys() {
    local file
    mkdir -p "$CONFIG_REPO_ROOT/ssh"
    {
        echo '# generated test fleet keys'
        while IFS= read -r file; do
            sed -n 's/^public_key = "\(.*\)"$/\1/p' "$file"
        done < <(find "$CONFIG_REPO_ROOT/fleet/machines" -type f -name '*.toml' | LC_ALL=C sort)
    } >"$CONFIG_REPO_ROOT/ssh/authorized_keys"
}

mkdir -p "$TEST_HOME" "$REPO_ROOT"
git init --quiet --bare "$REMOTE"
git init --quiet "$SEED_SOURCE"
configure_test_repo "$SEED_SOURCE"
mkdir -p "$SEED_SOURCE/dotfiles/zsh" "$SEED_SOURCE/ssh"
printf 'v0.9.0\n' >"$SEED_SOURCE/local-bin.ref"
printf '# seed\n' >"$SEED_SOURCE/ssh/authorized_keys"
printf '# zshrc\n' >"$SEED_SOURCE/dotfiles/zsh/.zshrc"
git -C "$SEED_SOURCE" add .
git -C "$SEED_SOURCE" commit --quiet -m 'Seed private config'
git -C "$SEED_SOURCE" branch -M main
git -C "$SEED_SOURCE" remote add origin "$REMOTE"
git -C "$SEED_SOURCE" push --quiet -u origin main
git --git-dir="$REMOTE" symbolic-ref HEAD refs/heads/main
git clone --quiet "$REMOTE" "$REPO_ROOT/managed-machine-config"

SEED_HEAD="$(git -C "$REPO_ROOT/managed-machine-config" rev-parse HEAD)"
SEED_STATUS="$(git -C "$REPO_ROOT/managed-machine-config" status --porcelain)"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"

# GNU stat accepts -c while BSD stat does not. Ensure the GNU path is selected
# without invoking the incompatible BSD form first.
stat() {
    if [[ "$1" == "-c" ]]; then
        id -un
        return 0
    fi
    return 99
}
[[ "$(config_repo_owner "$REPO_ROOT/managed-machine-config")" == "$(id -un)" ]]
unset -f stat

if materialize_managed_machine_config_repo \
    "$REPO_ROOT/managed-machine-config" \
    "$TEST_ROOT/unsafe-checkout" \
    'https://synthetic-token@example.invalid/private.git' >"$TEST_ROOT/materialize-credentials.out" 2>&1; then
    echo 'expected token-bearing clone URL to be rejected' >&2
    exit 1
fi
[[ ! -e "$TEST_ROOT/unsafe-checkout" ]]

CONFIG_REPO_ROOT="$(managed_machine_config_repo_dir)"
export CONFIG_REPO_ROOT
EXPECTED_CHECKOUT="$XDG_DATA_HOME/managed-machine/managed-machine-config"
[[ "$CONFIG_REPO_ROOT" == "$EXPECTED_CHECKOUT" ]]
[[ "$(git -C "$CONFIG_REPO_ROOT" remote get-url origin)" == "$REMOTE" ]]
configure_test_repo "$CONFIG_REPO_ROOT"

write_machine "$CONFIG_REPO_ROOT" 'sha256-first' 'ssh-ed25519 AAAAfirst first@example'
regenerate_test_authorized_keys
sync_managed_machine_config_repo \
    "$CONFIG_REPO_ROOT" \
    'Register first test machine' \
    regenerate_test_authorized_keys
[[ -z "$(git -C "$CONFIG_REPO_ROOT" status --porcelain)" ]]
git --git-dir="$REMOTE" show main:fleet/machines/sha256-first.toml >/dev/null

FIRST_SYNC_HEAD="$(git -C "$CONFIG_REPO_ROOT" rev-parse HEAD)"
sync_managed_machine_config_repo \
    "$CONFIG_REPO_ROOT" \
    'Synchronize test fleet' \
    regenerate_test_authorized_keys
[[ "$(git -C "$CONFIG_REPO_ROOT" rev-parse HEAD)" == "$FIRST_SYNC_HEAD" ]]
[[ -z "$(git -C "$CONFIG_REPO_ROOT" status --porcelain)" ]]

# Two stale checkouts may join concurrently. The second push rebases its unique
# machine record and regenerates authorized_keys from the combined registry.
CLONE_A="$TEST_ROOT/concurrent-a"
CLONE_B="$TEST_ROOT/concurrent-b"
git clone --quiet "$REMOTE" "$CLONE_A"
git clone --quiet "$REMOTE" "$CLONE_B"
configure_test_repo "$CLONE_A"
configure_test_repo "$CLONE_B"

CONFIG_REPO_ROOT="$CLONE_A"
write_machine "$CONFIG_REPO_ROOT" 'sha256-alpha' 'ssh-ed25519 AAAAalpha alpha@example'
regenerate_test_authorized_keys
sync_managed_machine_config_repo \
    "$CONFIG_REPO_ROOT" \
    'Register alpha test machine' \
    regenerate_test_authorized_keys

CONFIG_REPO_ROOT="$CLONE_B"
write_machine "$CONFIG_REPO_ROOT" 'sha256-bravo' 'ssh-ed25519 AAAAbravo bravo@example'
regenerate_test_authorized_keys
sync_managed_machine_config_repo \
    "$CONFIG_REPO_ROOT" \
    'Register bravo test machine' \
    regenerate_test_authorized_keys

VERIFY="$TEST_ROOT/verify"
git clone --quiet "$REMOTE" "$VERIFY"
[[ -f "$VERIFY/fleet/machines/sha256-alpha.toml" ]]
[[ -f "$VERIFY/fleet/machines/sha256-bravo.toml" ]]
grep -q 'AAAAalpha' "$VERIFY/ssh/authorized_keys"
grep -q 'AAAAbravo' "$VERIFY/ssh/authorized_keys"

# Unrelated private config edits are never staged or pushed by fleet sync.
printf 'user edit\n' >"$CLONE_A/unrelated.txt"
CONFIG_REPO_ROOT="$CLONE_A"
if sync_managed_machine_config_repo \
    "$CONFIG_REPO_ROOT" \
    'Unsafe test change' \
    regenerate_test_authorized_keys >"$TEST_ROOT/unrelated.out" 2>&1; then
    echo 'expected unrelated config changes to block synchronization' >&2
    exit 1
fi
grep -q 'refusing to synchronize managed-machine-config with unrelated changes' "$TEST_ROOT/unrelated.out"

# Clean but unpushed commits outside fleet state are not smuggled into a fleet
# push from a development checkout.
COMMITTED_CLONE="$TEST_ROOT/committed-unrelated"
git clone --quiet "$REMOTE" "$COMMITTED_CLONE"
configure_test_repo "$COMMITTED_CLONE"
printf 'private work in progress\n' >"$COMMITTED_CLONE/dotfiles/zsh/.zshrc"
git -C "$COMMITTED_CLONE" add dotfiles/zsh/.zshrc
git -C "$COMMITTED_CLONE" commit --quiet -m 'Unrelated private config work'
REMOTE_BEFORE="$(git --git-dir="$REMOTE" rev-parse main)"
CONFIG_REPO_ROOT="$COMMITTED_CLONE"
if sync_managed_machine_config_repo \
    "$CONFIG_REPO_ROOT" \
    'Unsafe committed test change' \
    regenerate_test_authorized_keys >"$TEST_ROOT/committed-unrelated.out" 2>&1; then
    echo 'expected unrelated ahead commit to block synchronization' >&2
    exit 1
fi
grep -q 'refusing to push an unrelated committed config path' "$TEST_ROOT/committed-unrelated.out"
[[ "$(git --git-dir="$REMOTE" rev-parse main)" == "$REMOTE_BEFORE" ]]

# Token-bearing HTTP remotes are rejected before any network or git write.
git -C "$CLONE_B" remote set-url origin 'https://bot:synthetic-token@example.invalid/private.git'
CONFIG_REPO_ROOT="$CLONE_B"
if sync_managed_machine_config_repo \
    "$CONFIG_REPO_ROOT" \
    'Unsafe credential test' \
    regenerate_test_authorized_keys >"$TEST_ROOT/credentials.out" 2>&1; then
    echo 'expected embedded remote credentials to block synchronization' >&2
    exit 1
fi
grep -q 'refusing managed-machine-config remote with embedded credentials' "$TEST_ROOT/credentials.out"

# Legacy SSH origins for the same GitHub repository migrate to the configured
# HTTPS URL; foreign remotes keep failing the strict assertion.
LEGACY="$TEST_ROOT/legacy-ssh"
git clone --quiet "$REMOTE" "$LEGACY"
configure_test_repo "$LEGACY"
git -C "$LEGACY" remote set-url origin 'git@github.com:qwts/managed-machine-config.git'
materialize_managed_machine_config_repo \
    "" \
    "$LEGACY" \
    'https://github.com/qwts/managed-machine-config.git' >"$TEST_ROOT/legacy.out" 2>&1
[[ "$(git -C "$LEGACY" remote get-url origin)" == 'https://github.com/qwts/managed-machine-config.git' ]]
grep -q 'Migrating managed-machine-config origin' "$TEST_ROOT/legacy.out"

git -C "$LEGACY" remote set-url origin 'git@github.com:someone-else/other-repo.git'
if materialize_managed_machine_config_repo \
    "" \
    "$LEGACY" \
    'https://github.com/qwts/managed-machine-config.git' >"$TEST_ROOT/foreign.out" 2>&1; then
    echo 'expected foreign remote to be rejected' >&2
    exit 1
fi
grep -q 'origin does not match' "$TEST_ROOT/foreign.out"

# The Homebrew-bundled seed remains byte-for-byte at its original commit and clean.
[[ "$(git -C "$REPO_ROOT/managed-machine-config" rev-parse HEAD)" == "$SEED_HEAD" ]]
[[ "$(git -C "$REPO_ROOT/managed-machine-config" status --porcelain)" == "$SEED_STATUS" ]]

# A seed owned by the prefix owner (not the invoking user) is still a trusted
# read-only input; the writable checkout is what must be owned by this user.
FOREIGN_SEED="$TEST_ROOT/admin-owned-seed"
git clone --quiet "$REMOTE" "$FOREIGN_SEED"
config_repo_owner() {
    if [[ "$1" == "$FOREIGN_SEED" ]]; then
        printf 'otheradmin\n'
        return 0
    fi
    id -un
}
materialize_managed_machine_config_repo \
    "$FOREIGN_SEED" \
    "$TEST_ROOT/from-admin-seed" \
    "$REMOTE" >"$TEST_ROOT/admin-seed.out" 2>&1
[[ -d "$TEST_ROOT/from-admin-seed" ]]
[[ "$(git -C "$TEST_ROOT/from-admin-seed" rev-parse HEAD)" == "$(git -C "$FOREIGN_SEED" rev-parse HEAD)" ]]
unset -f config_repo_owner

echo 'config repo tests passed'

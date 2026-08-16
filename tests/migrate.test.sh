#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_HOME" "$TEST_DIR/prefix"

export HOME="$TEST_HOME"
# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/bootstrap.sh
source "$ROOT/lib/bootstrap.sh"
# shellcheck source=lib/migrate.sh
source "$ROOT/lib/migrate.sh"

user_in_admin_group() { return 1; }
preferred_brew_owner() { printf 'admin\n'; }
brew_prefix_path() { printf '%s\n' "$TEST_DIR/prefix"; }
brew_prefix_owner() { id -un; }

set +e
MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive migrate_brew_owner_v1 \
    >"$TEST_DIR/defer.out" 2>&1
status=$?
set -e
[[ "$status" -eq 75 ]]
grep -Fq 'restoring Homebrew prefix ownership requires administrator authorization' "$TEST_DIR/defer.out"
grep -Fq 'managed-machine --bootstrap --interactive' "$TEST_DIR/defer.out"
[[ ! -f "$TEST_HOME/.config/managed-machine/migrations.manifest" ]]

set +e
MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive run_managed_machine_migrations \
    >"$TEST_DIR/runner.out" 2>&1
runner_status=$?
set -e
[[ "$runner_status" -eq 75 ]]

echo 'migrate tests passed'

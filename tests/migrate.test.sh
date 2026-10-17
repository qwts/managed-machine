#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

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
[[ "$status" -eq 76 ]]
grep -Fq 'restoring Homebrew prefix ownership needs the administrator dialog' "$TEST_DIR/defer.out"
! grep -Fq 'managed-machine --bootstrap' "$TEST_DIR/defer.out"
[[ ! -f "$TEST_HOME/.config/managed-machine/migrations.manifest" ]]

set +e
MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive run_managed_machine_migrations \
    >"$TEST_DIR/runner.out" 2>&1
runner_status=$?
set -e
[[ "$runner_status" -eq 76 ]]

# When ~/Applications and system_appdir are writable in noninteractive mode
# but a receipted cask bundle needs ownership normalization, migrate_appdir_system_v1
# returns 76 BEFORE moving the bundle.
mkdir -p "$TEST_HOME/Applications/Cursor.app" "$TEST_DIR/sys-apps"
ensure_brew_on_path() { return 0; }
catalog_query() {
    case "${1:-}" in
        cask-tokens) printf 'cursor\n' ;;
        field) printf 'Cursor.app\n' ;;
    esac
}
cask_app_is_running() { return 1; }
cask_has_receipt() { return 0; }
brew_needs_elevation() { return 1; }
adopt_bundle_needs_chown() { return 0; }

set +e
MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive \
MANAGED_MACHINE_USER_APPDIR="$TEST_HOME/Applications" \
MANAGED_MACHINE_SYSTEM_APPDIR="$TEST_DIR/sys-apps" \
    migrate_appdir_system_v1 >"$TEST_DIR/appdir-skip.out" 2>&1
appdir_status=$?
set -e
[[ "$appdir_status" -eq 76 ]]
[[ -d "$TEST_HOME/Applications/Cursor.app" ]]
[[ ! -e "$TEST_DIR/sys-apps/Cursor.app" ]]

# migrate_umask_permissions_v1: repairs 0700 prefix directories/symlinks and
# 0700 .app bundles when owned by the current user, and skips with 76 in
# noninteractive mode when owned by another user.
mkdir -p "$TEST_DIR/prefix/Cellar/uv/0.11.7/bin" "$TEST_DIR/prefix/bin" "$TEST_DIR/prefix/var/mm-home"
: >"$TEST_DIR/prefix/Cellar/uv/0.11.7/bin/uv"
chmod 700 "$TEST_DIR/prefix/Cellar/uv" "$TEST_DIR/prefix/Cellar/uv/0.11.7" "$TEST_DIR/prefix/Cellar/uv/0.11.7/bin/uv"
ln -s ../Cellar/uv/0.11.7/bin/uv "$TEST_DIR/prefix/bin/uv"
chmod -h 700 "$TEST_DIR/prefix/bin/uv"
chmod 700 "$TEST_DIR/prefix/var/mm-home"
prefix_needs_umask_repair "$TEST_DIR/prefix"

resolved_brew_owner() { printf 'otheradmin\n'; }
set +e
MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive migrate_umask_permissions_v1 \
    >"$TEST_DIR/umask-skip.out" 2>&1
umask_skip_rc=$?
set -e
[[ "$umask_skip_rc" -eq 76 ]]
grep -Fq 'repairing shared Homebrew permissions' "$TEST_DIR/umask-skip.out"

resolved_brew_owner() { id -un; }
migrate_umask_permissions_v1 >"$TEST_DIR/umask-repair.out" 2>&1
! prefix_needs_umask_repair "$TEST_DIR/prefix"
[[ "$(stat -f '%Lp' "$TEST_DIR/prefix/Cellar/uv")" == "755" ]]
[[ "$(stat -f '%Lp' "$TEST_DIR/prefix/bin/uv")" == "755" ]]
[[ "$(stat -f '%Lp' "$TEST_DIR/prefix/var/mm-home")" == "700" ]]
grep -qxF 'umask-permissions-v1' "$TEST_HOME/.config/managed-machine/migrations.manifest"

echo 'migrate tests passed'

#!/usr/bin/env bash
# npm catalog rows install verified packages from the public npm registry only.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
NPM_LOG="$TEST_DIR/npm.log"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$CONFIG_REPO" "$TEST_DIR/prefix/bin"
# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

write_catalog() {
    cat >"$CONFIG_REPO/apps.json"
}

write_package_json() {
    local name="${1:-command-code}"
    cat <<EOF
{"name":"$name","version":"0.1.0"}
EOF
}

write_catalog <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "commandcode", "kind": "npm", "package": "command-code", "command": "cmd"}
  ]
}
EOF

cat >"$TEST_BIN/npm" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$NPM_LOG'
printf 'userconfig=%s globalconfig=%s\n' "\${NPM_CONFIG_USERCONFIG-}" "\${NPM_CONFIG_GLOBALCONFIG-}" >>'$NPM_LOG'
case "\$1" in
    view)
        if [[ "\$2" == '--registry=https://registry.npmjs.org/' && "\$3" == 'command-code' ]]; then
            printf '%s\n' '{"name":"command-code","version":"0.1.0"}'
        else
            exit 1
        fi
        ;;
    prefix)
        echo '$TEST_DIR/prefix'
        ;;
    ls)
        if [[ -f '$TEST_DIR/receipt' ]]; then
            printf '%s\n' "$TEST_DIR/prefix/lib/node_modules/command-code"
            exit 0
        fi
        exit 1
        ;;
    install)
        cat >'$TEST_BIN/cmd' <<EOI
#!/usr/bin/env bash
exit 0
EOI
        chmod +x '$TEST_BIN/cmd'
        printf 'installed\n' >'$TEST_DIR/receipt'
        ;;
esac
EOF
chmod +x "$TEST_BIN/npm"

run_install() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    install_catalog_app commandcode
}

write_package_json >"$TEST_DIR/package.json"
run_install >"$TEST_DIR/install.out"
grep -Fq 'Installing command-code from the public npm registry' "$TEST_DIR/install.out"
grep -Fq 'command-code installed' "$TEST_DIR/install.out"
grep -Fxq 'view --registry=https://registry.npmjs.org/ command-code name version --json' "$NPM_LOG"
grep -Fxq 'install --global --no-fund --no-audit --registry=https://registry.npmjs.org/ command-code' "$NPM_LOG"
# Every npm call must run with user/global npm config isolated so a
# machine-scoped or scope-specific registry cannot redirect the lookup or the
# install.
grep -Eq '^userconfig=.*globalconfig=.*$' "$NPM_LOG"
! grep -Eq '^userconfig=$' "$NPM_LOG"
! grep -Eq '^globalconfig=$' "$NPM_LOG"

: >"$NPM_LOG"
run_install >"$TEST_DIR/rerun.out"
grep -Fq 'cmd already installed' "$TEST_DIR/rerun.out"
! grep -q '^install ' "$NPM_LOG"

# A Homebrew-managed node keeps its global prefix under the admin-owned prefix,
# so `npm i -g` needs the prefix owner's rights. The install then routes
# through the npm-public-run helper with an administrator dialog instead of
# running in-process (which would EACCES on /opt/homebrew/lib/node_modules).
rm -f "$TEST_DIR/receipt" "$TEST_BIN/cmd"
ELEVATE_LOG="$TEST_DIR/elevate.log"
: >"$ELEVATE_LOG"
npm_is_system_prefix() { return 0; }
npm_prefix_owner() { printf 'otheradmin\n'; }
elevate_run() {
    printf '%s\n' "$*" >>"$ELEVATE_LOG"
    # Simulate the helper dropping to the owner and running the command.
    "${@:5}"
}
run_install >"$TEST_DIR/elevated.out"
grep -Fq 'npm-public-run' "$ELEVATE_LOG"
grep -Fq 'otheradmin' "$ELEVATE_LOG"
grep -Fq 'run npm install --global --no-fund --no-audit' "$ELEVATE_LOG"
grep -Fxq 'install --global --no-fund --no-audit --registry=https://registry.npmjs.org/ command-code' "$NPM_LOG"
grep -Fq 'command-code installed' "$TEST_DIR/elevated.out"

# Only a system (Homebrew-linked) npm may run as a foreign prefix owner. An
# arbitrary PATH-resolved npm — a test stub, a user-writable shim, a per-user
# node without a system prefix — reports a foreign-owned prefix and must never
# be elevated: it runs in-process instead, with the invoking user's rights.
rm -f "$TEST_DIR/receipt" "$TEST_BIN/cmd"
npm_is_system_prefix() { return 1; }
: >"$NPM_LOG"
: >"$ELEVATE_LOG"
run_install >"$TEST_DIR/non-system.out"
! grep -q 'npm-public-run' "$ELEVATE_LOG"
grep -Fxq 'install --global --no-fund --no-audit --registry=https://registry.npmjs.org/ command-code' "$NPM_LOG"
grep -Fq 'command-code installed' "$TEST_DIR/non-system.out"

# An unresolvable numeric owner (DirectoryService sandbox, stat "(502)") still
# elevates: the helper is passed the raw UID and emits sudo -u "#uid", exactly
# like brew_run; it never silently falls back to an in-process EACCES.
rm -f "$TEST_DIR/receipt" "$TEST_BIN/cmd"
npm_is_system_prefix() { return 0; }
npm_prefix_owner() { printf '424242\n'; }
id() {
    case "$1" in
        -un) echo 'mm-tester' ;;
        -nu) return 1 ;;
        *) command id "$@" ;;
    esac
}
dscl() { return 1; }
: >"$NPM_LOG"
: >"$ELEVATE_LOG"
run_install >"$TEST_DIR/numeric.out"
grep -Fq 'npm-public-run' "$ELEVATE_LOG"
grep -Fq 'as 424242' "$ELEVATE_LOG"
grep -Fxq 'install --global --no-fund --no-audit --registry=https://registry.npmjs.org/ command-code' "$NPM_LOG"
grep -Fq 'command-code installed' "$TEST_DIR/numeric.out"
unset -f id dscl

# A per-user node (nvm and friends) keeps the prefix under the invoking user's
# home, so npm_run stays in-process and never dialogs — covered by the first
# install above, which ran with no elevation.

# A deferred elevated install (noninteractive bootstrap, no dialog) is a
# skipped outcome, not a failure, matching the other elevated sites.
rm -f "$TEST_DIR/receipt" "$TEST_BIN/cmd"
npm_prefix_owner() { printf 'otheradmin\n'; }
elevate_run() {
    printf '%s\n' "$*" >>"$ELEVATE_LOG"
    return 76
}
: >"$NPM_LOG"
: >"$ELEVATE_LOG"
set +e
run_install >"$TEST_DIR/deferred.out" 2>&1
deferred_rc=$?
set -e
[[ "$deferred_rc" -eq 76 ]]
grep -Fq 'npm-public-run' "$ELEVATE_LOG"
! grep -q '^install ' "$NPM_LOG"
grep -Fq 'command-code installed' "$TEST_DIR/deferred.out" || true
# Restore the working dialog stub for the remaining in-process installs.
elevate_run() {
    printf '%s\n' "$*" >>"$ELEVATE_LOG"
    "${@:5}"
}

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"commandcode","kind":"npm"}]}
EOF
rm -f "$TEST_DIR/receipt"
: >"$NPM_LOG"
if run_install >"$TEST_DIR/missing.out" 2>&1; then
    echo 'expected missing package field to fail' >&2
    exit 1
fi
grep -Fq 'missing package; refusing unverified npm install' "$TEST_DIR/missing.out"
! grep -q '^install ' "$NPM_LOG"

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"commandcode","kind":"npm","package":"../../evil"}]}
EOF
: >"$NPM_LOG"
if run_install >"$TEST_DIR/evil.out" 2>&1; then
    echo 'expected invalid package name to fail' >&2
    exit 1
fi
grep -Fq 'invalid npm package name' "$TEST_DIR/evil.out"
! grep -q '^install ' "$NPM_LOG"

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"commandcode","kind":"npm","package":"command-code"}]}
EOF
: >"$NPM_LOG"
if HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO" PATH="/usr/bin:/bin:/usr/sbin" \
    install_catalog_app commandcode >"$TEST_DIR/npm-missing.out" 2>&1; then
    echo 'expected missing npm to fail' >&2
    exit 1
fi
grep -Fq 'npm required' "$TEST_DIR/npm-missing.out"

write_catalog <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "commandcode", "kind": "npm", "package": "command-code", "command": "cmd"}
  ]
}
EOF
mkdir -p "$CONFIG_REPO/config"
cat >"$CONFIG_REPO/config/commandcode" <<'EOF'
#!/usr/bin/env bash
printf 'config-ran\n'
EOF
chmod +x "$CONFIG_REPO/config/commandcode"
PATH="$TEST_BIN:/usr/bin:/bin" run_install >"$TEST_DIR/config.out"
grep -Fq '==> config/commandcode' "$TEST_DIR/config.out"
grep -Fq 'config-ran' "$TEST_DIR/config.out"

echo 'npm tests passed'
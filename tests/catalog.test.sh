#!/usr/bin/env bash
# Catalog auto filter: omitted/true install on bootstrap; false is setup-only.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$CONFIG_REPO_ROOT"
export CONFIG_REPO_ROOT

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

write_catalog() {
    cat >"$CONFIG_REPO_ROOT/apps.json"
}

write_catalog <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "always", "kind": "devin"},
    {"name": "explicit", "kind": "devin", "auto": true},
    {"name": "sometimes", "kind": "devin", "auto": false},
    {"name": "agent-bot", "kind": "devin", "auto": true},
    {"name": "agent-bot-gh", "kind": "devin", "auto": true}
  ]
}
EOF

names="$(catalog_app_names | tr '\n' ' ')"
[[ "$names" == "agent-bot agent-bot-gh always explicit sometimes " ]]
auto_names="$(catalog_auto_app_names | tr '\n' ' ')"
[[ "$auto_names" == "always explicit " ]]
catalog_app_is_auto always
catalog_app_is_auto explicit
# Stale catalog rows cannot reintroduce either retired runtime path.
for retired in agent-bot agent-bot-gh; do
    if install_catalog_app "$retired" >"$TEST_DIR/retired.out" 2>&1; then
        echo "expected $retired catalog install to be rejected" >&2
        exit 1
    fi
    grep -Fq 'catalog setup' "$TEST_DIR/retired.out"
done
if catalog_app_is_auto sometimes; then
    echo 'expected sometimes to be setup-only' >&2
    exit 1
fi
print_catalog_app_names >"$TEST_DIR/help.out"
grep -qxF '  always' "$TEST_DIR/help.out"
grep -qxF '  explicit' "$TEST_DIR/help.out"
grep -qxF '  sometimes (setup only)' "$TEST_DIR/help.out"

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"bad","kind":"devin","auto":"false"}]}
EOF
if catalog_app_names >"$TEST_DIR/string.out" 2>&1; then
    echo 'expected string auto to fail' >&2
    exit 1
fi
grep -Fq 'apps.json auto must be a boolean' "$TEST_DIR/string.out"

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"bad","kind":"devin","auto":0}]}
EOF
if catalog_auto_app_names >"$TEST_DIR/zero.out" 2>&1; then
    echo 'expected numeric auto to fail' >&2
    exit 1
fi
grep -Fq 'apps.json auto must be a boolean' "$TEST_DIR/zero.out"

echo 'catalog tests passed'

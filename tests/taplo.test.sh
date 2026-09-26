#!/usr/bin/env bash
# taplo catalog row: the fleet TOML parser installs from homebrew/core and is auto.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
BREW_LOG="$TEST_DIR/brew.log"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$CONFIG_REPO"
export CONFIG_REPO_ROOT="$CONFIG_REPO"
# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

# Mirrors the managed-machine-config/apps.json row: no "auto" key means
# bootstrap and --update install it.
cat >"$CONFIG_REPO/apps.json" <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "taplo", "kind": "brew-formula", "formula": "taplo"}
  ]
}
EOF

catalog_app_is_auto taplo

cat >"$TEST_DIR/formula.json" <<EOF
{"formulae":[{"name":"taplo","tap":"homebrew/core","full_name":"taplo"}]}
EOF

cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
case "\$1" in
    info)
        cat '$TEST_DIR/formula.json'
        ;;
    install)
        printf 'installed\n' >'$TEST_DIR/receipt'
        ;;
    list)
        if [[ -f '$TEST_DIR/receipt' ]]; then
            echo 'taplo 0.10.0'
            exit 0
        fi
        exit 1
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

run_install() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    install_catalog_app taplo
}

run_install >"$TEST_DIR/install.out"
grep -Fq 'Installing taplo from homebrew/core/taplo' "$TEST_DIR/install.out"
grep -Fq 'taplo installed' "$TEST_DIR/install.out"
grep -Fxq 'install homebrew/core/taplo' "$BREW_LOG"

: >"$BREW_LOG"
run_install >"$TEST_DIR/rerun.out"
grep -Fq 'taplo already installed' "$TEST_DIR/rerun.out"
! grep -q '^install ' "$BREW_LOG"

echo 'taplo tests passed'

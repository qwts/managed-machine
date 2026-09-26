#!/usr/bin/env bash
# Agent utility rows: ripgrep, fd, and ast-grep install from homebrew/core and are auto.
set -euo pipefail

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

# Mirrors the managed-machine-config/apps.json rows: no "auto" key means
# bootstrap and --update install them.
cat >"$CONFIG_REPO/apps.json" <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "ripgrep", "kind": "brew-formula", "formula": "ripgrep", "aliases": ["rg"]},
    {"name": "fd", "kind": "brew-formula", "formula": "fd"},
    {"name": "ast-grep", "kind": "brew-formula", "formula": "ast-grep", "aliases": ["sg"]}
  ]
}
EOF

for name in ripgrep fd ast-grep; do
    catalog_app_is_auto "$name"
done
[[ "$(catalog_resolve_name rg)" == "ripgrep" ]]
[[ "$(catalog_resolve_name sg)" == "ast-grep" ]]

write_formula_json() {
    local formula="$1"
    printf '{"formulae":[{"name":"%s","tap":"homebrew/core","full_name":"%s"}]}' \
        "$formula" "$formula" >"$TEST_DIR/formula.json"
}

cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
case "\$1" in
    info)
        cat '$TEST_DIR/formula.json'
        ;;
    install)
        printf 'installed\n' >'$TEST_DIR/receipt-'"\${2##*/}"
        ;;
    list)
        if [[ -f '$TEST_DIR/receipt-'"\$3" ]]; then
            echo "\$3 0.0.0-test"
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
    install_catalog_app "$1"
}

for formula in ripgrep fd ast-grep; do
    : >"$BREW_LOG"
    write_formula_json "$formula"
    run_install "$formula" >"$TEST_DIR/$formula.out"
    grep -Fq "Installing $formula from homebrew/core/$formula" "$TEST_DIR/$formula.out"
    grep -Fq "$formula installed" "$TEST_DIR/$formula.out"
    grep -Fxq "install homebrew/core/$formula" "$BREW_LOG"

    : >"$BREW_LOG"
    run_install "$formula" >"$TEST_DIR/$formula-rerun.out"
    grep -Fq "$formula already installed" "$TEST_DIR/$formula-rerun.out"
    ! grep -q '^install ' "$BREW_LOG"
done

echo 'agent-tools tests passed'

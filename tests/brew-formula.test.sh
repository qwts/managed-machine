#!/usr/bin/env bash
# brew-formula catalog rows install official homebrew/core formulae only.
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
# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

write_catalog() {
    cat >"$CONFIG_REPO/apps.json"
}

write_formula_json() {
    local tap="${1:-homebrew/core}"
    cat <<EOF
{"formulae":[{"name":"minikube","tap":"$tap","full_name":"minikube"}]}
EOF
}

write_catalog <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "minikube", "kind": "brew-formula", "formula": "minikube"}
  ]
}
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
            echo 'minikube 1.36.0'
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
    install_catalog_app minikube
}

write_formula_json >"$TEST_DIR/formula.json"
run_install >"$TEST_DIR/install.out"
grep -Fq 'Installing minikube from homebrew/core/minikube' "$TEST_DIR/install.out"
grep -Fq 'minikube installed' "$TEST_DIR/install.out"
grep -Fxq 'install homebrew/core/minikube' "$BREW_LOG"

: >"$BREW_LOG"
run_install >"$TEST_DIR/rerun.out"
grep -Fq 'minikube already installed' "$TEST_DIR/rerun.out"
! grep -q '^install ' "$BREW_LOG"

write_formula_json 'example/tap' >"$TEST_DIR/formula.json"
rm -f "$TEST_DIR/receipt"
: >"$BREW_LOG"
if run_install >"$TEST_DIR/tap.out" 2>&1; then
    echo 'expected non-core tap to fail' >&2
    exit 1
fi
grep -Fq 'only homebrew/core is allowed' "$TEST_DIR/tap.out"
! grep -q '^install ' "$BREW_LOG"

write_formula_json >"$TEST_DIR/formula.json"
write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"minikube","kind":"brew-formula"}]}
EOF
if run_install >"$TEST_DIR/missing.out" 2>&1; then
    echo 'expected missing formula field to fail' >&2
    exit 1
fi
grep -Fq 'missing formula; refusing unverified brew-formula' "$TEST_DIR/missing.out"
! grep -q '^install ' "$BREW_LOG"

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"minikube","kind":"brew-formula","formula":"../evil"}]}
EOF
if run_install >"$TEST_DIR/evil.out" 2>&1; then
    echo 'expected invalid formula name to fail' >&2
    exit 1
fi
grep -Fq 'invalid Homebrew formula name' "$TEST_DIR/evil.out"
! grep -q '^install ' "$BREW_LOG"

write_catalog <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "minikube", "kind": "brew-formula", "formula": "minikube"}
  ]
}
EOF
mkdir -p "$CONFIG_REPO/config"
cat >"$CONFIG_REPO/config/minikube" <<'EOF'
#!/usr/bin/env bash
printf 'config-ran\n'
EOF
chmod +x "$CONFIG_REPO/config/minikube"
run_install >"$TEST_DIR/config.out"
grep -Fq '==> config/minikube' "$TEST_DIR/config.out"
grep -Fq 'config-ran' "$TEST_DIR/config.out"

echo 'brew-formula tests passed'

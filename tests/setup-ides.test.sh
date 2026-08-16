#!/usr/bin/env bash
# Signed cask installs: official tap, checksum, host allowlist, Team ID.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-applications"
BREW_LOG="$TEST_DIR/brew.log"
CASK_TAP="homebrew/cask"
CASK_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
CASK_URL_HOST="update.code.visualstudio.com"
CASK_HOME_HOST="code.visualstudio.com"
CASK_TEAM="UBF8T346G9"
CODESIGN_FAIL=0
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$SYSTEM_APPDIR"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
mkdir -p "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

write_cask_json() {
    local token="$1"
    cat <<EOF
{"casks":[{"token":"$token","tap":"$CASK_TAP","sha256":"$CASK_SHA","url":"https://$CASK_URL_HOST/pkg","homepage":"https://$CASK_HOME_HOST/"}]}
EOF
}

cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
case "\$1" in
    info)
        token="\${*: -1}"
        token="\${token##*/}"
        write_cask_json() { :; }
        python3 - <<'PY'
print(open("$TEST_DIR/cask.json").read(), end="")
PY
        ;;
    install)
        for arg in "\$@"; do
            case "\$arg" in
                --no-quarantine)
                    echo "refusing --no-quarantine" >&2
                    exit 2
                    ;;
                --appdir=*)
                    mkdir -p "\${arg#--appdir=}/Visual Studio Code.app"
                    ;;
            esac
        done
        ;;
    list)
        grep -q '^install ' '$BREW_LOG' && echo 'visual-studio-code 1.2.3'
        ;;
esac
EOF
# brew stub calls python to print a file we rewrite per case.
cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
case "\$1" in
    info)
        cat '$TEST_DIR/cask.json'
        ;;
    install)
        for arg in "\$@"; do
            case "\$arg" in
                --no-quarantine)
                    echo "refusing --no-quarantine" >&2
                    exit 2
                    ;;
                --appdir=*)
                    mkdir -p "\${arg#--appdir=}/Visual Studio Code.app"
                    ;;
            esac
        done
        ;;
    list)
        if grep -q '^install ' '$BREW_LOG'; then
            echo 'visual-studio-code 1.2.3'
            exit 0
        fi
        exit 1
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

cat >"$TEST_BIN/codesign" <<EOF
#!/usr/bin/env bash
if [[ "\${CODESIGN_FAIL:-0}" == "1" ]]; then
    echo "failed" >&2
    exit 1
fi
if [[ "\$1" == "--verify" ]]; then
    exit 0
fi
cat <<SIGN
Authority=Developer ID Application: Microsoft Corporation ($CASK_TEAM)
TeamIdentifier=$CASK_TEAM
SIGN
EOF
chmod +x "$TEST_BIN/codesign"

write_cask_json visual-studio-code >"$TEST_DIR/cask.json"

run_setup() {
    HOME="$TEST_HOME" \
    BREW_LOG="$BREW_LOG" \
    CODESIGN_FAIL="${CODESIGN_FAIL:-0}" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-vscode" "$@"
}

# 1. Fresh install uses the qualified homebrew/cask token and --appdir.
: >"$BREW_LOG"
chmod 755 "$SYSTEM_APPDIR"
run_setup >"$TEST_DIR/install.out"
grep -Fq 'homebrew/cask/visual-studio-code' "$BREW_LOG"
grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"
! grep -Fq -- '--no-quarantine' "$BREW_LOG"
grep -Fq "Visual Studio Code.app installed: $SYSTEM_APPDIR/Visual Studio Code.app" "$TEST_DIR/install.out"

# 2. Re-run requires a Homebrew receipt and validated cask metadata, then
# verifies the existing signature and does not install again.
install_count="$(grep -c '^install ' "$BREW_LOG" || true)"
run_setup >"$TEST_DIR/rerun.out"
grep -Fq "already installed: $SYSTEM_APPDIR/Visual Studio Code.app" "$TEST_DIR/rerun.out"
grep -q '^info ' "$BREW_LOG"
[[ "$(grep -c '^install ' "$BREW_LOG")" -eq "$install_count" ]]

# 3. Wrong tap is refused before brew install.
rm -rf "$SYSTEM_APPDIR/Visual Studio Code.app" "$TEST_HOME/Applications"
: >"$BREW_LOG"
CASK_TAP="evil/tap" write_cask_json visual-studio-code >"$TEST_DIR/cask.json"
if run_setup >"$TEST_DIR/bad-tap.out" 2>&1; then
    echo 'expected wrong tap to fail' >&2
    exit 1
fi
grep -Fq 'only homebrew/cask is allowed' "$TEST_DIR/bad-tap.out"
! grep -q '^install ' "$BREW_LOG"
CASK_TAP="homebrew/cask"

# 4. no_check checksum is refused.
CASK_SHA="no_check" write_cask_json visual-studio-code >"$TEST_DIR/cask.json"
if run_setup >"$TEST_DIR/nocheck.out" 2>&1; then
    echo 'expected no_check sha256 to fail' >&2
    exit 1
fi
grep -Fq 'did not publish a sha256 checksum' "$TEST_DIR/nocheck.out"
CASK_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

# 5. Unexpected download host is refused.
CASK_URL_HOST="evil.example" write_cask_json visual-studio-code >"$TEST_DIR/cask.json"
if run_setup >"$TEST_DIR/bad-host.out" 2>&1; then
    echo 'expected unexpected download host to fail' >&2
    exit 1
fi
grep -Fq 'download host' "$TEST_DIR/bad-host.out"
CASK_URL_HOST="update.code.visualstudio.com"
write_cask_json visual-studio-code >"$TEST_DIR/cask.json"

# 6. Wrong Team ID after a planted Homebrew-managed app fails closed.
mkdir -p "$SYSTEM_APPDIR/Visual Studio Code.app"
printf '%s\n' 'install --cask homebrew/cask/visual-studio-code' >"$BREW_LOG"
CASK_TEAM="AAAAAAAAAA" CODESIGN_FAIL=0
# Recreate codesign stub with the wrong team for this case.
cat >"$TEST_BIN/codesign" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == "--verify" ]] && exit 0
echo 'Authority=Developer ID Application: Impostor (AAAAAAAAAA)'
echo 'TeamIdentifier=AAAAAAAAAA'
EOF
chmod +x "$TEST_BIN/codesign"
if run_setup >"$TEST_DIR/bad-team.out" 2>&1; then
    echo 'expected wrong Team ID to fail' >&2
    exit 1
fi
grep -Fq 'expected UBF8T346G9' "$TEST_DIR/bad-team.out"

# Restore a passing codesign stub for later cases.
cat >"$TEST_BIN/codesign" <<EOF
#!/usr/bin/env bash
[[ "\$1" == "--verify" ]] && exit 0
echo 'Authority=Developer ID Application: Microsoft Corporation (UBF8T346G9)'
echo 'TeamIdentifier=UBF8T346G9'
EOF
chmod +x "$TEST_BIN/codesign"

# 7. Unknown allowlist token cannot be installed via the helper.
if HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO" PATH="$TEST_BIN:/usr/bin:/bin" /bin/bash -c '
    source "'"$ROOT"'/lib/install.sh"
    source "'"$ROOT"'/lib/cask-app.sh"
    install_signed_cask_app not-a-real-cask
' >"$TEST_DIR/unknown.out" 2>&1; then
    echo 'expected unknown token to fail' >&2
    exit 1
fi
grep -Fq 'not on the signed-cask allowlist' "$TEST_DIR/unknown.out"

# 8. A vendor-downloaded app without a Homebrew receipt is skipped, not failed.
mkdir -p "$SYSTEM_APPDIR/Visual Studio Code.app"
: >"$BREW_LOG"
set +e
run_setup >"$TEST_DIR/unmanaged.out" 2>&1
unmanaged_status=$?
set -e
[[ "$unmanaged_status" -eq 76 ]]
grep -Fq 'not a Homebrew cask install' "$TEST_DIR/unmanaged.out"
! grep -q '^install ' "$BREW_LOG"

# 9. A Homebrew receipt still requires validated cask metadata.
printf '%s\n' 'install --cask homebrew/cask/visual-studio-code' >"$BREW_LOG"
CASK_TAP="evil/tap" write_cask_json visual-studio-code >"$TEST_DIR/cask.json"
if run_setup >"$TEST_DIR/receipt-bad-tap.out" 2>&1; then
    echo 'expected existing receipt with a shadowed tap to fail' >&2
    exit 1
fi
grep -Fq 'only homebrew/cask is allowed' "$TEST_DIR/receipt-bad-tap.out"
[[ "$(grep -c '^install ' "$BREW_LOG")" -eq 1 ]]

echo 'setup-ides tests passed'

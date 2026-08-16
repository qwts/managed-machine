#!/usr/bin/env bash
# LM Studio is a desktop cask: same tap, checksum, host, and Team ID gates.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-applications"
BREW_LOG="$TEST_DIR/brew.log"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
CASK_TAP="homebrew/cask"
CASK_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
CASK_URL_HOST="installers.lmstudio.ai"
CASK_HOME_HOST="lmstudio.ai"
CASK_TEAM="D65G88RHWN"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$SYSTEM_APPDIR" "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

write_cask_json() {
    cat <<EOF
{"casks":[{"token":"lm-studio","tap":"$CASK_TAP","sha256":"$CASK_SHA","url":"https://$CASK_URL_HOST/pkg","homepage":"https://$CASK_HOME_HOST/"}]}
EOF
}

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
                --appdir=*) mkdir -p "\${arg#--appdir=}/LM Studio.app" ;;
            esac
        done
        ;;
    list)
        if grep -q '^install ' '$BREW_LOG'; then
            echo 'lm-studio 0.4.21'
            exit 0
        fi
        exit 1
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

cat >"$TEST_BIN/codesign" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == "--verify" ]]; then
    exit 0
fi
cat <<SIGN
Authority=Developer ID Application: Element Labs Inc ($CASK_TEAM)
TeamIdentifier=$CASK_TEAM
SIGN
EOF
chmod +x "$TEST_BIN/codesign"

write_cask_json >"$TEST_DIR/cask.json"

run_setup() {
    HOME="$TEST_HOME" \
    BREW_LOG="$BREW_LOG" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-lmstudio" "$@"
}

mkdir -p "$SYSTEM_APPDIR"
chmod 755 "$SYSTEM_APPDIR"
run_setup >"$TEST_DIR/system-install.out"
grep -Fq "LM Studio.app installed: $SYSTEM_APPDIR/LM Studio.app" "$TEST_DIR/system-install.out"
grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"
grep -Fq 'homebrew/cask/lm-studio' "$BREW_LOG"

: >"$BREW_LOG"
printf '%s\n' 'install --cask homebrew/cask/lm-studio' >"$BREW_LOG"
run_setup >"$TEST_DIR/rerun.out"
grep -Fq "already installed" "$TEST_DIR/rerun.out"
[[ "$(grep -c '^install ' "$BREW_LOG")" -eq 1 ]]

CUSTOM="$TEST_DIR/custom-apps"
rm -rf "$SYSTEM_APPDIR/LM Studio.app"
: >"$BREW_LOG"
HOME="$TEST_HOME" BREW_LOG="$BREW_LOG" \
MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
MANAGED_MACHINE_LMSTUDIO_APPDIR="$CUSTOM" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/setup-lmstudio" >"$TEST_DIR/custom-install.out"
grep -Fq "$CUSTOM/LM Studio.app" "$TEST_DIR/custom-install.out"
grep -Fq -- "--appdir=$CUSTOM" "$BREW_LOG"

# kind=cask without Team ID / host allowlists fails closed.
python3 - <<'PY' >"$CONFIG_REPO/apps.json"
import json
print(json.dumps({
    "schema_version": 1,
    "apps": [{
        "name": "lmstudio",
        "kind": "cask",
        "token": "lm-studio",
        "app_name": "LM Studio.app",
    }],
}))
PY
git -C "$CONFIG_REPO" add apps.json
git -C "$CONFIG_REPO" commit --quiet -m 'unsigned row'
rm -rf "$CUSTOM/LM Studio.app"
: >"$BREW_LOG"
if HOME="$TEST_HOME" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-lmstudio" >"$TEST_DIR/unsigned.out" 2>&1; then
    echo 'expected unverified desktop cask to fail' >&2
    exit 1
fi
grep -Fq 'refusing unverified desktop cask' "$TEST_DIR/unsigned.out"
! grep -q '^install ' "$BREW_LOG"

echo 'setup-lmstudio tests passed'

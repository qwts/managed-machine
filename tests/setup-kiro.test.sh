#!/usr/bin/env bash
# Kiro IDE and CLI are desktop casks: same tap, checksum, host, and Team ID gates.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-applications"
BREW_LOG="$TEST_DIR/brew.log"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
CASK_TAP="homebrew/cask"
CASK_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
CASK_TEAM="94KV3E626L"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$SYSTEM_APPDIR" "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email '[EMAIL]'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

write_cask_json() {
    local token url_host
    token="$1"
    case "$token" in
        kiro) url_host="prod.download.desktop.kiro.dev" ;;
        kiro-cli) url_host="desktop-release.q.us-east-1.amazonaws.com" ;;
        *) echo "unknown token: $token" >&2; return 1 ;;
    esac
    cat <<EOF
{"casks":[{"token":"$token","tap":"$CASK_TAP","sha256":"$CASK_SHA","url":"https://$url_host/pkg","homepage":"https://kiro.dev/"}]}
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
        appdir=""
        token=""
        for arg in "\$@"; do
            case "\$arg" in
                --appdir=*) appdir="\${arg#--appdir=}" ;;
                homebrew/cask/kiro) token="kiro" ;;
                homebrew/cask/kiro-cli) token="kiro-cli" ;;
            esac
        done
        case "\$token" in
            kiro) mkdir -p "\$appdir/Kiro.app" ;;
            kiro-cli) mkdir -p "\$appdir/Kiro CLI.app" ;;
        esac
        ;;
    list)
        if grep -q '^install ' '$BREW_LOG'; then
            case "\${*: -1}" in
                kiro) echo 'kiro 1.0.337' ;;
                kiro-cli) echo 'kiro-cli 2.19.1' ;;
            esac
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
Authority=Developer ID Application: AMZN Mobile LLC ($CASK_TEAM)
TeamIdentifier=$CASK_TEAM
SIGN
EOF
chmod +x "$TEST_BIN/codesign"

run_setup() {
    local script="$1"
    shift
    HOME="$TEST_HOME" \
    BREW_LOG="$BREW_LOG" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/$script" "$@"
}

# 1. Fresh install of both casks uses qualified tokens and the system appdir.
: >"$BREW_LOG"
chmod 755 "$SYSTEM_APPDIR"
write_cask_json kiro >"$TEST_DIR/cask.json"
run_setup setup-kiro >"$TEST_DIR/ide-install.out"
grep -Fq "Kiro.app installed: $SYSTEM_APPDIR/Kiro.app" "$TEST_DIR/ide-install.out"
grep -Fq 'homebrew/cask/kiro' "$BREW_LOG"
grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"

: >"$BREW_LOG"
write_cask_json kiro-cli >"$TEST_DIR/cask.json"
run_setup setup-kiro-cli >"$TEST_DIR/cli-install.out"
grep -Fq "Kiro CLI.app installed: $SYSTEM_APPDIR/Kiro CLI.app" "$TEST_DIR/cli-install.out"
grep -Fq 'homebrew/cask/kiro-cli' "$BREW_LOG"

# 2. Re-runs see the Homebrew receipt and do not install again.
: >"$BREW_LOG"
printf '%s\n' 'install --cask homebrew/cask/kiro' 'install --cask homebrew/cask/kiro-cli' >"$BREW_LOG"
write_cask_json kiro >"$TEST_DIR/cask.json"
run_setup setup-kiro >"$TEST_DIR/ide-rerun.out"
grep -Fq "already installed" "$TEST_DIR/ide-rerun.out"
write_cask_json kiro-cli >"$TEST_DIR/cask.json"
run_setup setup-kiro-cli >"$TEST_DIR/cli-rerun.out"
grep -Fq "already installed" "$TEST_DIR/cli-rerun.out"
[[ "$(grep -c '^install ' "$BREW_LOG")" -eq 2 ]]

# 3. An unexpected download host is refused before install.
rm -rf "$SYSTEM_APPDIR/Kiro.app" "$SYSTEM_APPDIR/Kiro CLI.app"
: >"$BREW_LOG"
CASK_JSON="$TEST_DIR/cask.json" write_cask_json kiro >"$TEST_DIR/cask.json"
CASK_JSON="$TEST_DIR/cask.json" python3 -c '
import json, os
path = os.environ["CASK_JSON"]
data = json.load(open(path))
data["casks"][0]["url"] = "https://evil.example/pkg"
json.dump(data, open(path, "w"))
'
if run_setup setup-kiro >"$TEST_DIR/bad-host.out" 2>&1; then
    echo 'expected unexpected download host to fail' >&2
    exit 1
fi
grep -Fq 'download host' "$TEST_DIR/bad-host.out"
! grep -q '^install ' "$BREW_LOG"

echo 'setup-kiro tests passed'

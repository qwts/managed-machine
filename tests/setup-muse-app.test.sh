#!/usr/bin/env bash
# Muse.app is a desktop cask: same tap, host, and Team ID gates as other apps.
# The vendor cask publishes sha256 :no_check, so the catalog row allows the
# rolling URL and integrity rests on Developer ID verification.
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
CASK_TEAM="V9WTTPBFK9"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$SYSTEM_APPDIR" "$CONFIG_REPO"
cat >"$CONFIG_REPO/apps.json" <<'EOF'
{
  "schema_version": 1,
  "apps": [{
    "name": "muse-app",
    "kind": "signed-cask",
    "token": "muse",
    "app_name": "Muse.app",
    "team_id": "V9WTTPBFK9",
    "url_hosts": ["muse.ai"],
    "homepage_hosts": ["muse.ai"],
    "allow_rolling_url": true,
    "aliases": ["muse-desktop"]
  }]
}
EOF
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

cat >"$TEST_DIR/cask.json" <<EOF
{"casks":[{"token":"muse","tap":"$CASK_TAP","sha256":"no_check","url":"https://muse.ai/api/hatch/app-download/mac","homepage":"https://muse.ai/"}]}
EOF

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
                --appdir=*) mkdir -p "\${arg#--appdir=}/Muse.app" ;;
            esac
        done
        ;;
    list)
        if grep -q '^install ' '$BREW_LOG'; then
            echo 'muse 2.0'
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
Authority=Developer ID Application: Meta Platforms, Inc. ($CASK_TEAM)
TeamIdentifier=$CASK_TEAM
SIGN
EOF
chmod +x "$TEST_BIN/codesign"

cat >"$TEST_BIN/spctl" <<EOF
#!/usr/bin/env bash
cat <<ASSESS
\$5: accepted
source=Notarized Developer ID
origin=Developer ID Application: Meta Platforms, Inc. ($CASK_TEAM)
ASSESS
EOF
chmod +x "$TEST_BIN/spctl"

run_setup() {
    HOME="$TEST_HOME" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-muse-app" "$@"
}

mkdir -p "$SYSTEM_APPDIR"
chmod 755 "$SYSTEM_APPDIR"
run_setup >"$TEST_DIR/system-install.out"
grep -Fq "Muse.app installed: $SYSTEM_APPDIR/Muse.app" "$TEST_DIR/system-install.out"
grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"
grep -Fq 'homebrew/cask/muse' "$BREW_LOG"

: >"$BREW_LOG"
printf '%s\n' 'install --cask homebrew/cask/muse' >"$BREW_LOG"
run_setup >"$TEST_DIR/rerun.out"
grep -Fq "already installed" "$TEST_DIR/rerun.out"
[[ "$(grep -c '^install ' "$BREW_LOG")" -eq 1 ]]

CUSTOM="$TEST_DIR/custom-apps"
rm -rf "$SYSTEM_APPDIR/Muse.app"
: >"$BREW_LOG"
HOME="$TEST_HOME" \
MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
MANAGED_MACHINE_MUSE_APPDIR="$CUSTOM" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/setup-muse-app" >"$TEST_DIR/custom-install.out"
grep -Fq "$CUSTOM/Muse.app" "$TEST_DIR/custom-install.out"
grep -Fq -- "--appdir=$CUSTOM" "$BREW_LOG"

echo 'setup-muse-app tests passed'

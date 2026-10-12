#!/usr/bin/env bash
# Muse.app installs from Meta's Sparkle appcast. The Homebrew cask URL
# https://muse.ai/api/hatch/app-download/mac returns 403 not_eligible, so the
# catalog row is a vendor-dmg with sparkle true. Integrity is notarized
# Developer ID (Team ID V9WTTPBFK9); the feed has no stable checksum.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-applications"
CALL_LOG="$TEST_DIR/curl.log"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
CASK_TEAM="V9WTTPBFK9"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$SYSTEM_APPDIR" "$CONFIG_REPO" \
    "$TEST_DIR/payload"
printf 'fake-muse-dmg\n' >"$TEST_DIR/payload/Muse.dmg"
mkdir -p "$TEST_DIR/payload/Muse.app/Contents"
cat >"$TEST_DIR/payload/Muse.app/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleShortVersionString</key><string>4.1</string>
</dict></plist>
EOF

cat >"$CONFIG_REPO/apps.json" <<EOF
{
  "schema_version": 1,
  "apps": [{
    "name": "muse-app",
    "kind": "vendor-dmg",
    "app_name": "Muse.app",
    "team_id": "$CASK_TEAM",
    "url": "https://www.facebook.com/endo/release/appcast.xml?channel=production",
    "url_hosts": ["facebook.com", ".fbcdn.net"],
    "sha256": "no_check",
    "allow_rolling_url": true,
    "sparkle": true,
    "aliases": ["muse-desktop"],
    "retire_cask": "muse"
  }]
}
EOF
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

cat >"$TEST_BIN/curl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$CALL_LOG'
out=""
prev=""
url=""
write=0
for arg in "\$@"; do
    if [[ "\$prev" == "-o" ]]; then
        out="\$arg"
    elif [[ "\$prev" == "-w" ]]; then
        write=1
    fi
    prev="\$arg"
    case "\$arg" in
        http://*|https://*) url="\$arg" ;;
    esac
done
[[ -n "\$out" ]] || exit 1
case "\$url" in
    *appcast.xml*)
        cat >"\$out" <<'XML'
<?xml version="1.0"?>
<rss><channel><item><enclosure url="https://scontent.xx.fbcdn.net/muse.dmg" /></item></channel></rss>
XML
        ;;
    *)
        cp '$TEST_DIR/payload/Muse.dmg' "\$out"
        ;;
esac
if [[ "\$write" == 1 ]]; then
    printf '%s' "\$url"
fi
EOF
chmod +x "$TEST_BIN/curl"

cat >"$TEST_BIN/hdiutil" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == "attach" ]]; then
    mnt=""
    prev=""
    for arg in "\$@"; do
        if [[ "\$prev" == "-mountpoint" ]]; then
            mnt="\$arg"
        fi
        prev="\$arg"
    done
    [[ -n "\$mnt" ]] || exit 1
    mkdir -p "\$mnt"
    cp -R '$TEST_DIR/payload/Muse.app' "\$mnt/"
    exit 0
fi
if [[ "\$1" == "detach" ]]; then
    exit 0
fi
exit 1
EOF
chmod +x "$TEST_BIN/hdiutil"

cat >"$TEST_BIN/codesign" <<EOF
#!/usr/bin/env bash
# CODESIGN_BAD_STAGED fails the bundle inside the mounted DMG only.
if [[ -n "\${CODESIGN_BAD_STAGED:-}" && "\$*" == */mnt/* ]]; then
    exit 1
fi
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

# brew stub: a homebrew/cask/muse record while $TEST_DIR/muse-receipt exists.
# Like the real thing, uninstall also deletes the app bundle it finds.
BREW_LOG="$TEST_DIR/brew.log"
cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
case "\$*" in
    'list --cask --versions muse')
        [[ -f '$TEST_DIR/muse-receipt' ]] && echo 'muse 2.1' && exit 0
        exit 1
        ;;
    'uninstall --cask --force muse')
        rm -rf '$SYSTEM_APPDIR/Muse.app'
        rm -f '$TEST_DIR/muse-receipt'
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

run_setup() {
    HOME="$TEST_HOME" \
    CODESIGN_BAD_STAGED="${CODESIGN_BAD_STAGED:-}" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-muse-app" "$@"
}

mkdir -p "$SYSTEM_APPDIR"
chmod 755 "$SYSTEM_APPDIR"
run_setup >"$TEST_DIR/system-install.out"
grep -Fq "Muse.app installed: $SYSTEM_APPDIR/Muse.app (4.1)" "$TEST_DIR/system-install.out"
grep -Fq 'appcast.xml?channel=production' "$CALL_LOG"
grep -Fq 'https://scontent.xx.fbcdn.net/muse.dmg' "$CALL_LOG"

calls_before="$(wc -l <"$CALL_LOG" | tr -d ' ')"
run_setup >"$TEST_DIR/rerun.out"
grep -Fq "already installed" "$TEST_DIR/rerun.out"
[[ "$(wc -l <"$CALL_LOG" | tr -d ' ')" == "$calls_before" ]]

CUSTOM="$TEST_DIR/custom-apps"
rm -rf "$SYSTEM_APPDIR/Muse.app"
: >"$CALL_LOG"
HOME="$TEST_HOME" \
MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
MANAGED_MACHINE_MUSE_APPDIR="$CUSTOM" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/setup-muse-app" >"$TEST_DIR/custom-install.out"
grep -Fq "Muse.app installed: $CUSTOM/Muse.app (4.1)" "$TEST_DIR/custom-install.out"
[[ -d "$CUSTOM/Muse.app" ]]
[[ ! -e "$SYSTEM_APPDIR/Muse.app" ]]

# --- #180: a Muse.app still recorded by the disabled homebrew/cask/muse ---
# A bundle the cask installed (marked so it can be told apart from a fresh
# copy of the DMG).
put_cask_muse() {
    rm -rf "$SYSTEM_APPDIR/Muse.app"
    cp -R "$TEST_DIR/payload/Muse.app" "$SYSTEM_APPDIR/Muse.app"
    printf 'from-cask\n' >"$SYSTEM_APPDIR/Muse.app/Contents/marker"
    : >"$TEST_DIR/muse-receipt"
}

# 1. A bad vendor DMG stops the migration before Homebrew is touched.
put_cask_muse
: >"$BREW_LOG"
if CODESIGN_BAD_STAGED=1 run_setup >"$TEST_DIR/bad-dmg.out" 2>&1; then
    echo 'expected a bad vendor DMG to fail the migration' >&2
    exit 1
fi
! grep -q '^uninstall' "$BREW_LOG"
[[ -f "$TEST_DIR/muse-receipt" ]]
grep -Fxq 'from-cask' "$SYSTEM_APPDIR/Muse.app/Contents/marker"

# 2. A running Muse is skipped (76) with nothing touched.
cat >"$TEST_BIN/lsof" <<'EOF'
#!/usr/bin/env bash
echo 4242
EOF
chmod +x "$TEST_BIN/lsof"
rc=0
run_setup >"$TEST_DIR/running.out" 2>&1 || rc=$?
rm "$TEST_BIN/lsof"
[[ "$rc" == 76 ]]
grep -Fq 'is running' "$TEST_DIR/running.out"
! grep -q '^uninstall' "$BREW_LOG"
[[ -f "$TEST_DIR/muse-receipt" ]]

# 3. With the DMG verified, the record goes and the app stays as it was.
: >"$CALL_LOG"
run_setup >"$TEST_DIR/retire.out"
grep -Fq 'verifying the vendor DMG before removing that record' "$TEST_DIR/retire.out"
grep -Fq 'https://scontent.xx.fbcdn.net/muse.dmg' "$CALL_LOG"
grep -Fxq 'uninstall --cask --force muse' "$BREW_LOG"
! grep -q -- '--zap' "$BREW_LOG"
[[ ! -f "$TEST_DIR/muse-receipt" ]]
grep -Fxq 'from-cask' "$SYSTEM_APPDIR/Muse.app/Contents/marker"
grep -Fq "Muse.app kept at $SYSTEM_APPDIR/Muse.app" "$TEST_DIR/retire.out"
[[ -z "$(find "$SYSTEM_APPDIR" -name '.Muse.app.mm-retire-*')" ]]

# 4. A rerun is a no-op: no download, no brew uninstall.
: >"$CALL_LOG"
: >"$BREW_LOG"
run_setup >"$TEST_DIR/retire-rerun.out"
grep -Fq 'already installed' "$TEST_DIR/retire-rerun.out"
[[ ! -s "$CALL_LOG" ]]
! grep -q '^uninstall' "$BREW_LOG"

# 5. A record with no app left: the record is retired, then Muse installs
# from the verified DMG.
rm -rf "$SYSTEM_APPDIR/Muse.app"
: >"$TEST_DIR/muse-receipt"
: >"$BREW_LOG"
run_setup >"$TEST_DIR/receipt-only.out"
grep -Fxq 'uninstall --cask --force muse' "$BREW_LOG"
[[ ! -f "$TEST_DIR/muse-receipt" ]]
grep -Fq "Muse.app installed: $SYSTEM_APPDIR/Muse.app (4.1)" "$TEST_DIR/receipt-only.out"

# 6. If brew keeps the record, the step fails and the app is still there.
put_cask_muse
cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
case "\$*" in
    'list --cask --versions muse') echo 'muse 2.1' ;;
    'uninstall --cask --force muse') rm -rf '$SYSTEM_APPDIR/Muse.app'; exit 1 ;;
esac
EOF
if run_setup >"$TEST_DIR/stuck.out" 2>&1; then
    echo 'expected a kept Homebrew record to fail' >&2
    exit 1
fi
grep -Fq 'Homebrew still records the muse cask' "$TEST_DIR/stuck.out"
grep -Fxq 'from-cask' "$SYSTEM_APPDIR/Muse.app/Contents/marker"

echo 'setup-muse-app tests passed'

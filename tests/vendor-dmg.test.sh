#!/usr/bin/env bash
# vendor-dmg engine: pinned-URL downloads with checksum + Team ID gates,
# rolling-URL rows only behind notarization, in-place convergence, and a
# staged bundle that is verified before anything under /Applications moves.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-apps"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
CALL_LOG="$TEST_DIR/calls.log"
TEAM="AAAAAAAAAA"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$SYSTEM_APPDIR" "$CONFIG_REPO_ROOT"
export CONFIG_REPO_ROOT HOME="$TEST_HOME"
export MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR"
export PATH="$TEST_BIN:/usr/bin:/bin"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

# Fixture "DMG payload" and the app the stub mount serves from it.
printf 'fake-disk-image-bytes\n' >"$TEST_DIR/payload.dmg"
PAYLOAD_SHA="$(shasum -a 256 "$TEST_DIR/payload.dmg" | awk '{print $1}')"
mkdir -p "$TEST_DIR/mnt-src/TestApp.app/Contents" "$TEST_DIR/mnt-src/RollingApp.app/Contents"
cat >"$TEST_DIR/mnt-src/TestApp.app/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>example.vendor.testapp</string>
	<key>CFBundleShortVersionString</key>
	<string>1.2.0</string>
</dict>
</plist>
EOF
cat >"$TEST_DIR/mnt-src/RollingApp.app/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>example.vendor.rollingapp</string>
	<key>CFBundleShortVersionString</key>
	<string>9.9.9</string>
</dict>
</plist>
EOF

# codesign: identity always readable; --verify outcome driven by CODESIGN_VERIFY.
cat >"$TEST_BIN/codesign" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == "--verify" ]]; then
    exit "\${CODESIGN_VERIFY:-0}"
fi
cat <<SIGN
Authority=Developer ID Application: Test Vendor, Inc. (\${SIGN_TEAM:-$TEAM})
TeamIdentifier=\${SIGN_TEAM:-$TEAM}
SIGN
EOF
chmod +x "$TEST_BIN/codesign"

# spctl: outcome and printed assessment driven by SPCTL_* env.
cat >"$TEST_BIN/spctl" <<EOF
#!/usr/bin/env bash
if [[ "\${SPCTL_ACCEPT:-1}" != "1" ]]; then
    echo "\$3: rejected" >&2
    exit 3
fi
cat <<ASSESS
\$3: accepted
source=\${SPCTL_SOURCE:-Notarized Developer ID}
origin=Developer ID Application: Test Vendor, Inc. (\${SPCTL_TEAM:-$TEAM})
ASSESS
EOF
chmod +x "$TEST_BIN/spctl"

# curl: serve the fixture payload for -o downloads, log every call, and
# report %{url_effective} as the requested URL unless a later stub overrides it.
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
cp '$TEST_DIR/payload.dmg' "\$out"
if [[ "\$write" == 1 ]]; then
    printf '%s' "\$url"
fi
EOF
chmod +x "$TEST_BIN/curl"

# hdiutil: attach materializes the fixture apps at the mountpoint.
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
    cp -R '$TEST_DIR/mnt-src/.' "\$mnt/"
    exit 0
fi
if [[ "\$1" == "detach" ]]; then
    rm -rf "\$2"
    exit 0
fi
exit 1
EOF
chmod +x "$TEST_BIN/hdiutil"

NATIVE_ARCH="$(uname -m)"
if [[ "$NATIVE_ARCH" == "arm64" ]]; then
    OTHER_ARCH="x86_64"
else
    OTHER_ARCH="arm64"
fi
NATIVE_URL="https://dl.vendor.example/${NATIVE_ARCH}/TestApp.dmg"
OTHER_URL="https://dl.vendor.example/${OTHER_ARCH}/TestApp.dmg"

write_catalog() {
    cat >"$CONFIG_REPO_ROOT/apps.json"
}

write_catalog <<EOF
{
  "schema_version": 1,
  "apps": [
    {"name": "pinned", "kind": "vendor-dmg", "app_name": "TestApp.app",
     "team_id": "$TEAM", "url": "https://dl.vendor.example/TestApp-1.2.0.dmg",
     "url_hosts": ["dl.vendor.example"], "sha256": "$PAYLOAD_SHA", "version": "1.2.0"},
    {"name": "rolling", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://dl.vendor.example/latest/RollingApp.dmg",
     "url_hosts": ["dl.vendor.example"], "sha256": "no_check",
     "allow_rolling_url": true},
    {"name": "badhost", "kind": "vendor-dmg", "app_name": "TestApp.app",
     "team_id": "$TEAM", "url": "https://evil.example/TestApp.dmg",
     "url_hosts": ["dl.vendor.example"], "sha256": "$PAYLOAD_SHA"},
    {"name": "badsha", "kind": "vendor-dmg", "app_name": "TestApp.app",
     "team_id": "$TEAM", "url": "https://dl.vendor.example/TestApp.dmg",
     "url_hosts": ["dl.vendor.example"], "sha256": "abc123"},
    {"name": "nocheck", "kind": "vendor-dmg", "app_name": "TestApp.app",
     "team_id": "$TEAM", "url": "https://dl.vendor.example/TestApp.dmg",
     "url_hosts": ["dl.vendor.example"], "sha256": "no_check"},
    {"name": "plainhttp", "kind": "vendor-dmg", "app_name": "TestApp.app",
     "team_id": "$TEAM", "url": "http://dl.vendor.example/TestApp.dmg",
     "url_hosts": ["dl.vendor.example"], "sha256": "$PAYLOAD_SHA"},
    {"name": "thin", "kind": "vendor-dmg", "app_name": "TestApp.app"},
    {"name": "archsplit", "kind": "vendor-dmg", "app_name": "TestApp.app",
     "team_id": "$TEAM", "url_arm64": "https://dl.vendor.example/arm64/TestApp.dmg",
     "url_x86_64": "https://dl.vendor.example/x86_64/TestApp.dmg",
     "url_hosts": ["dl.vendor.example"], "sha256": "$PAYLOAD_SHA", "version": "1.2.0"},
    {"name": "otherarch", "kind": "vendor-dmg", "app_name": "TestApp.app",
     "team_id": "$TEAM", "url_hosts": ["dl.vendor.example"], "sha256": "$PAYLOAD_SHA",
     "url_${OTHER_ARCH}": "$OTHER_URL"},
    {"name": "sparkle", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://feeds.vendor.example/appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "no_check", "allow_rolling_url": true, "sparkle": true},
    {"name": "sparkle-bad", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://feeds.vendor.example/bad-appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "no_check", "allow_rolling_url": true, "sparkle": true},
    {"name": "sparkle-off", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://evil.example/appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "no_check", "allow_rolling_url": true, "sparkle": true},
    {"name": "sparkle-badsha", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://feeds.vendor.example/appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "abc123", "allow_rolling_url": true, "sparkle": true},
    {"name": "sparkle-nocheck", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://feeds.vendor.example/appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "no_check", "sparkle": true},
    {"name": "sparkle-redir-feed", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://feeds.vendor.example/off-appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "no_check", "allow_rolling_url": true, "sparkle": true},
    {"name": "sparkle-redir-dmg", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://feeds.vendor.example/off-dmg-appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "no_check", "allow_rolling_url": true, "sparkle": true},
    {"name": "sparkle-redir-ok", "kind": "vendor-dmg", "app_name": "RollingApp.app",
     "team_id": "$TEAM", "url": "https://feeds.vendor.example/hop-appcast.xml",
     "url_hosts": ["feeds.vendor.example", ".cdn.vendor.example"],
     "sha256": "no_check", "allow_rolling_url": true, "sparkle": true}
  ]
}
EOF

# --- catalog plumbing -------------------------------------------------------

# 1. dmg-row carries the full policy line.
[[ "$(vendor_dmg_allowlist_row pinned)" == "TestApp.app|$TEAM|https://dl.vendor.example/TestApp-1.2.0.dmg|$PAYLOAD_SHA|dl.vendor.example|1.2.0||" ]]
[[ "$(vendor_dmg_allowlist_row rolling)" == "RollingApp.app|$TEAM|https://dl.vendor.example/latest/RollingApp.dmg|no_check|dl.vendor.example||1|" ]]
[[ "$(vendor_dmg_allowlist_row sparkle)" == "RollingApp.app|$TEAM|https://feeds.vendor.example/appcast.xml|no_check|feeds.vendor.example,.cdn.vendor.example||1|1" ]]
if vendor_dmg_allowlist_row missing >"$TEST_DIR/norow.out" 2>&1; then
    echo 'expected an unknown vendor-dmg name to fail' >&2
    exit 1
fi

# --- source gates (no download happens below) -------------------------------

# 2. Off-allowlist host is refused before curl runs.
: >"$CALL_LOG"
if install_vendor_dmg_from_catalog badhost >"$TEST_DIR/badhost.out" 2>&1; then
    echo 'expected an off-allowlist download host to fail' >&2
    exit 1
fi
grep -Fq 'download host' "$TEST_DIR/badhost.out"
[[ ! -s "$CALL_LOG" ]]

# 3. Plain http is refused.
if install_vendor_dmg_from_catalog plainhttp >"$TEST_DIR/http.out" 2>&1; then
    echo 'expected a non-https URL to fail' >&2
    exit 1
fi
grep -Fq 'must be https' "$TEST_DIR/http.out"

# 3b. A Sparkle appcast host off the allowlist is refused before curl runs.
: >"$CALL_LOG"
if install_vendor_dmg_from_catalog sparkle-off >"$TEST_DIR/sparkle-off.out" 2>&1; then
    echo 'expected an off-allowlist appcast host to fail' >&2
    exit 1
fi
grep -Fq 'download host' "$TEST_DIR/sparkle-off.out"
[[ ! -s "$CALL_LOG" ]]

# 3c. A dotted allowlist entry keeps a www. suffix, and a bare entry still
#     matches that host's www. form.
if vendor_dmg_verify_host sample 'https://evil.example.com/TestApp.dmg' '.www.example.com' \
    >"$TEST_DIR/wwwsuffix.out" 2>&1; then
    echo 'expected .www.example.com to reject evil.example.com' >&2
    exit 1
fi
grep -Fq 'download host' "$TEST_DIR/wwwsuffix.out"
if vendor_dmg_verify_host sample 'https://example.com/TestApp.dmg' '.www.example.com' \
    >"$TEST_DIR/wwwapex.out" 2>&1; then
    echo 'expected .www.example.com to reject the apex' >&2
    exit 1
fi
if vendor_dmg_verify_host sample 'https://notwww.example.com/TestApp.dmg' '.www.example.com' \
    >"$TEST_DIR/wwwlookalike.out" 2>&1; then
    echo 'expected .www.example.com to reject notwww.example.com' >&2
    exit 1
fi
vendor_dmg_verify_host sample 'https://www.example.com/TestApp.dmg' '.www.example.com'
vendor_dmg_verify_host sample 'https://cdn.www.example.com/TestApp.dmg' '.www.example.com'
vendor_dmg_verify_host sample \
    'https://www.facebook.com/endo/release/appcast.xml?channel=production' \
    'facebook.com,.fbcdn.net'
vendor_dmg_verify_host sample \
    'https://scontent-dfw6-1.xx.fbcdn.net/muse.dmg' \
    'facebook.com,.fbcdn.net'
if vendor_dmg_verify_host sample 'https://fbcdn.net.evil/muse.dmg' 'facebook.com,.fbcdn.net' \
    >"$TEST_DIR/fbcdn-lookalike.out" 2>&1; then
    echo 'expected fbcdn.net.evil to be rejected' >&2
    exit 1
fi

# 4. A malformed digest is refused.
if install_vendor_dmg_from_catalog badsha >"$TEST_DIR/sha.out" 2>&1; then
    echo 'expected a malformed sha256 to fail' >&2
    exit 1
fi
grep -Fq 'no usable sha256' "$TEST_DIR/sha.out"

# 5. no_check without the opt-in stays refused.
if install_vendor_dmg_from_catalog nocheck >"$TEST_DIR/nocheck.out" 2>&1; then
    echo 'expected no_check without allow_rolling_url to fail' >&2
    exit 1
fi
grep -Fq 'no usable sha256' "$TEST_DIR/nocheck.out"

# 6. A row missing policy fields is refused.
if install_vendor_dmg_from_catalog thin >"$TEST_DIR/thin.out" 2>&1; then
    echo 'expected a row missing policy fields to fail' >&2
    exit 1
fi
grep -Fq 'missing app_name' "$TEST_DIR/thin.out"

# --- install, idempotency, convergence ---------------------------------------

# 7. Fresh install lands the verified bundle with its version.
install_vendor_dmg_from_catalog pinned >"$TEST_DIR/fresh.out" 2>&1
grep -Fq "TestApp.app installed: $SYSTEM_APPDIR/TestApp.app (1.2.0)" "$TEST_DIR/fresh.out"
[[ -d "$SYSTEM_APPDIR/TestApp.app" ]]
[[ "$(vendor_dmg_bundle_version "$SYSTEM_APPDIR/TestApp.app")" == "1.2.0" ]]
[[ "$(vendor_dmg_status pinned)" == "1.2.0 ($SYSTEM_APPDIR/TestApp.app)" ]]

# 8. Re-run is a no-op and downloads nothing.
calls_before="$(wc -l <"$CALL_LOG")"
install_vendor_dmg_from_catalog pinned >"$TEST_DIR/rerun.out" 2>&1
grep -Fq "TestApp.app already installed: $SYSTEM_APPDIR/TestApp.app (1.2.0)" "$TEST_DIR/rerun.out"
[[ "$(wc -l <"$CALL_LOG")" == "$calls_before" ]]

# 9. Version drift converges to the pinned row.
PLIST="$SYSTEM_APPDIR/TestApp.app/Contents/Info.plist"
python3 -c 'import plistlib; p=plistlib.load(open("'$PLIST'","rb")); p["CFBundleShortVersionString"]="1.1.0"; plistlib.dump(p, open("'$PLIST'","wb"))'
[[ "$(vendor_dmg_status pinned)" == "1.1.0 ($SYSTEM_APPDIR/TestApp.app)" ]]
install_vendor_dmg_from_catalog pinned >"$TEST_DIR/drift.out" 2>&1
grep -Fq 'is 1.1.0; pinned version is 1.2.0' "$TEST_DIR/drift.out"
[[ "$(vendor_dmg_bundle_version "$SYSTEM_APPDIR/TestApp.app")" == "1.2.0" ]]

# 10. An occupier that fails Team ID verification is never touched.
rm -rf "$SYSTEM_APPDIR/TestApp.app"
mkdir -p "$SYSTEM_APPDIR/TestApp.app/Contents"
cp "$TEST_DIR/mnt-src/TestApp.app/Contents/Info.plist" "$SYSTEM_APPDIR/TestApp.app/Contents/Info.plist"
if SIGN_TEAM='ZZZZZZZZZZ' install_vendor_dmg_from_catalog pinned >"$TEST_DIR/impostor.out" 2>&1; then
    echo 'expected a Team ID mismatch on the occupier to fail' >&2
    exit 1
fi
grep -Fq 'remove it manually' "$TEST_DIR/impostor.out"
[[ -d "$SYSTEM_APPDIR/TestApp.app" ]]
[[ "$(vendor_dmg_bundle_version "$SYSTEM_APPDIR/TestApp.app")" == "1.2.0" ]]
rm -rf "$SYSTEM_APPDIR/TestApp.app"

# 11. A staged bundle that fails verification never clobbers the install.
if SIGN_TEAM='ZZZZZZZZZZ' install_vendor_dmg_from_catalog pinned >"$TEST_DIR/staged.out" 2>&1; then
    echo 'expected a staged Team ID mismatch to fail' >&2
    exit 1
fi
[[ ! -e "$SYSTEM_APPDIR/TestApp.app" ]]

# --- rolling rows ------------------------------------------------------------

# 12. A rolling row installs on notarization alone, with no version to pin.
install_vendor_dmg_from_catalog rolling >"$TEST_DIR/rolling.out" 2>&1
grep -Fq 'rolling vendor URL' "$TEST_DIR/rolling.out"
[[ -d "$SYSTEM_APPDIR/RollingApp.app" ]]

# 13. Re-run accepts presence plus a valid signature (no version to compare).
install_vendor_dmg_from_catalog rolling >"$TEST_DIR/rolling-rerun.out" 2>&1
grep -Fq "RollingApp.app already installed: $SYSTEM_APPDIR/RollingApp.app (9.9.9)" "$TEST_DIR/rolling-rerun.out"

# 14. A rolling row without notarization is refused: no checksum stands
#     behind it, so Gatekeeper is the only integrity guarantee left.
rm -rf "$SYSTEM_APPDIR/RollingApp.app"
if SPCTL_SOURCE='Developer ID' install_vendor_dmg_from_catalog rolling >"$TEST_DIR/rolling-unnotarized.out" 2>&1; then
    echo 'expected an un-notarized rolling row to fail' >&2
    exit 1
fi
grep -Fq 'publishes no checksum for this rolling URL, and it is not notarized' "$TEST_DIR/rolling-unnotarized.out"
[[ ! -e "$SYSTEM_APPDIR/RollingApp.app" ]]

# 15. A checksum mismatch refuses the download (corrupt or substituted).
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
printf 'tampered-bytes\n' >"\$out"
if [[ "\$write" == 1 ]]; then
    printf '%s' "\$url"
fi
EOF
chmod +x "$TEST_BIN/curl"
if install_vendor_dmg_from_catalog pinned >"$TEST_DIR/tampered.out" 2>&1; then
    echo 'expected a checksum mismatch to fail' >&2
    exit 1
fi
grep -Fq 'checksum mismatch' "$TEST_DIR/tampered.out"
[[ ! -e "$SYSTEM_APPDIR/TestApp.app" ]]

# Restore the serving curl stub for the remaining cases.
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
cp '$TEST_DIR/payload.dmg' "\$out"
if [[ "\$write" == 1 ]]; then
    printf '%s' "\$url"
fi
EOF
chmod +x "$TEST_BIN/curl"

# 16. dmg-row resolves the per-arch URL for this machine.
[[ "$(vendor_dmg_allowlist_row archsplit)" == "TestApp.app|$TEAM|$NATIVE_URL|$PAYLOAD_SHA|dl.vendor.example|1.2.0||" ]]
install_vendor_dmg_from_catalog archsplit >"$TEST_DIR/archsplit.out" 2>&1
grep -Fq "TestApp.app installed: $SYSTEM_APPDIR/TestApp.app (1.2.0)" "$TEST_DIR/archsplit.out"
rm -rf "$SYSTEM_APPDIR/TestApp.app"

# 17. A row serving only the other architecture skips without downloading.
: >"$CALL_LOG"
rc=0
install_vendor_dmg_from_catalog otherarch >"$TEST_DIR/otherarch.out" 2>&1 || rc=$?
[[ "$rc" -eq 76 ]]
grep -Fq "serves no build for $NATIVE_ARCH" "$TEST_DIR/otherarch.out"
[[ ! -s "$CALL_LOG" ]]
[[ ! -e "$SYSTEM_APPDIR/TestApp.app" ]]

# 18. Sparkle: enclosure host must match the allowlist, including a leading-dot
#     suffix. A lookalike host is refused after the appcast fetch and before
#     the DMG fetch.
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
effective="\$url"
case "\$url" in
    *off-appcast*)
        effective="https://evil.example/appcast.xml"
        cat >"\$out" <<'XML'
<?xml version="1.0"?>
<rss><channel><item><enclosure url="https://bin.cdn.vendor.example/RollingApp.dmg" /></item></channel></rss>
XML
        ;;
    *off-dmg-appcast*)
        cat >"\$out" <<'XML'
<?xml version="1.0"?>
<rss><channel><item><enclosure url="https://bin.cdn.vendor.example/off.dmg" /></item></channel></rss>
XML
        ;;
    *hop-appcast*)
        effective="https://www.feeds.vendor.example/hop-appcast.xml"
        cat >"\$out" <<'XML'
<?xml version="1.0"?>
<rss><channel><item><enclosure url="https://bin.cdn.vendor.example/hop.dmg" /></item></channel></rss>
XML
        ;;
    *bad-appcast*)
        cat >"\$out" <<'XML'
<?xml version="1.0"?>
<rss><channel><item><enclosure url="https://cdn.vendor.example.evil.net/RollingApp.dmg" /></item></channel></rss>
XML
        ;;
    *appcast.xml*)
        cat >"\$out" <<'XML'
<?xml version="1.0"?>
<rss><channel><item><enclosure url="https://bin.cdn.vendor.example/RollingApp.dmg" /></item></channel></rss>
XML
        ;;
    *bin.cdn.vendor.example/off.dmg*)
        effective="https://evil.example/RollingApp.dmg"
        cp '$TEST_DIR/payload.dmg' "\$out"
        ;;
    *bin.cdn.vendor.example/hop.dmg*)
        effective="https://edge.cdn.vendor.example/hop.dmg"
        cp '$TEST_DIR/payload.dmg' "\$out"
        ;;
    *)
        cp '$TEST_DIR/payload.dmg' "\$out"
        ;;
esac
if [[ "\$write" == 1 ]]; then
    printf '%s' "\$effective"
fi
EOF
chmod +x "$TEST_BIN/curl"

: >"$CALL_LOG"
if install_vendor_dmg_from_catalog sparkle-bad >"$TEST_DIR/sparkle-bad.out" 2>&1; then
    echo 'expected a Sparkle enclosure on a lookalike host to fail' >&2
    exit 1
fi
grep -Fq 'download host' "$TEST_DIR/sparkle-bad.out"
[[ "$(grep -c . "$CALL_LOG")" -eq 1 ]]
[[ ! -e "$SYSTEM_APPDIR/RollingApp.app" ]]

# 18b. An appcast redirect off the allowlist is refused before the enclosure
#      is parsed, even when the XML names an allowlisted DMG.
: >"$CALL_LOG"
if install_vendor_dmg_from_catalog sparkle-redir-feed >"$TEST_DIR/redir-feed.out" 2>&1; then
    echo 'expected an appcast redirect off the allowlist to fail' >&2
    exit 1
fi
grep -Fq "download host 'evil.example'" "$TEST_DIR/redir-feed.out"
[[ "$(grep -c . "$CALL_LOG")" -eq 1 ]]
[[ ! -e "$SYSTEM_APPDIR/RollingApp.app" ]]

# 18c. An enclosure download that redirects off the allowlist is refused.
: >"$CALL_LOG"
if install_vendor_dmg_from_catalog sparkle-redir-dmg >"$TEST_DIR/redir-dmg.out" 2>&1; then
    echo 'expected a DMG redirect off the allowlist to fail' >&2
    exit 1
fi
grep -Fq "download host 'evil.example'" "$TEST_DIR/redir-dmg.out"
[[ "$(grep -c . "$CALL_LOG")" -eq 2 ]]
[[ ! -e "$SYSTEM_APPDIR/RollingApp.app" ]]

# 19. Sparkle install follows the enclosure, then a re-run does not fetch again.
: >"$CALL_LOG"
install_vendor_dmg_from_catalog sparkle >"$TEST_DIR/sparkle.out" 2>&1
grep -Fq "RollingApp.app installed: $SYSTEM_APPDIR/RollingApp.app (9.9.9)" "$TEST_DIR/sparkle.out"
grep -Fq 'https://feeds.vendor.example/appcast.xml' "$CALL_LOG"
grep -Fq 'https://bin.cdn.vendor.example/RollingApp.dmg' "$CALL_LOG"
calls_before="$(wc -l <"$CALL_LOG")"
install_vendor_dmg_from_catalog sparkle >"$TEST_DIR/sparkle-rerun.out" 2>&1
grep -Fq "RollingApp.app already installed: $SYSTEM_APPDIR/RollingApp.app (9.9.9)" "$TEST_DIR/sparkle-rerun.out"
[[ "$(wc -l <"$CALL_LOG")" == "$calls_before" ]]

# 19b. Digest policy still applies when a signed bundle is already present.
for bad_row in sparkle-badsha sparkle-nocheck; do
    calls_before="$(wc -l <"$CALL_LOG")"
    if install_vendor_dmg_from_catalog "$bad_row" >"$TEST_DIR/$bad_row.out" 2>&1; then
        echo "expected $bad_row to fail while the bundle is installed" >&2
        exit 1
    fi
    grep -Fq 'no usable sha256' "$TEST_DIR/$bad_row.out"
    if grep -Fq 'already installed' "$TEST_DIR/$bad_row.out"; then
        echo "expected $bad_row to enforce the digest before the short-circuit" >&2
        exit 1
    fi
    [[ "$(wc -l <"$CALL_LOG")" == "$calls_before" ]]
done

# 19c. A redirect whose final host is still allowlisted installs.
rm -rf "$SYSTEM_APPDIR/RollingApp.app"
: >"$CALL_LOG"
install_vendor_dmg_from_catalog sparkle-redir-ok >"$TEST_DIR/redir-ok.out" 2>&1
grep -Fq "RollingApp.app installed: $SYSTEM_APPDIR/RollingApp.app (9.9.9)" "$TEST_DIR/redir-ok.out"
grep -Fq 'https://feeds.vendor.example/hop-appcast.xml' "$CALL_LOG"
grep -Fq 'https://bin.cdn.vendor.example/hop.dmg' "$CALL_LOG"

# 20. sparkle must be a boolean.
printf '%s\n' '{"schema_version":1,"apps":[{"name":"x","kind":"vendor-dmg","sparkle":"yes"}]}' >"$CONFIG_REPO_ROOT/apps.json"
if vendor_dmg_allowlist_row x >"$TEST_DIR/sparkle-type.out" 2>&1; then
    echo 'expected a string sparkle flag to fail' >&2
    exit 1
fi
grep -Fq 'apps.json sparkle must be a boolean' "$TEST_DIR/sparkle-type.out"

echo 'vendor-dmg tests passed'

#!/usr/bin/env bash
# Signature and checksum gates: Gatekeeper is the fallback for bundles whose
# nested helpers carry extraction detritus, and rolling vendor URLs are
# accepted only when the catalog row opts in.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_BIN="$TEST_DIR/bin"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
APP="$TEST_DIR/Rolling Browser.app"
TEAM="KL8N8XSYF4"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_BIN" "$CONFIG_REPO_ROOT" "$APP"
export CONFIG_REPO_ROOT
export PATH="$TEST_BIN:$PATH"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/cask-app.sh
source "$ROOT/lib/cask-app.sh"

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

# 1. codesign --verify passes: accepted without consulting Gatekeeper.
CODESIGN_VERIFY=0 SPCTL_ACCEPT=0 verify_app_signature "$APP" "$TEAM" >"$TEST_DIR/ok.out"
! grep -q 'notarized Developer ID' "$TEST_DIR/ok.out"

# 2. codesign --verify fails (detritus) but Gatekeeper accepts a notarized
#    bundle from the same team: accepted, and the fallback is announced.
CODESIGN_VERIFY=1 verify_app_signature "$APP" "$TEAM" >"$TEST_DIR/fallback.out"
grep -Fq "rejected bundle detritus; accepted on notarized Developer ID ($TEAM)" "$TEST_DIR/fallback.out"

# 3. Both checks fail: refused.
if CODESIGN_VERIFY=1 SPCTL_ACCEPT=0 verify_app_signature "$APP" "$TEAM" >"$TEST_DIR/both.out" 2>&1; then
    echo 'expected a Gatekeeper rejection to fail verification' >&2
    exit 1
fi
grep -Fq 'rejected bundle detritus, and Gatekeeper rejected it' "$TEST_DIR/both.out"

# 4. Gatekeeper accepts, but the app is not notarized: refused.
if CODESIGN_VERIFY=1 SPCTL_SOURCE='Developer ID' \
    verify_app_signature "$APP" "$TEAM" >"$TEST_DIR/unnotarized.out" 2>&1; then
    echo 'expected an un-notarized bundle to fail verification' >&2
    exit 1
fi
grep -Fq 'rejected bundle detritus, and it is not notarized' "$TEST_DIR/unnotarized.out"

# 5. Gatekeeper accepts a notarized bundle signed by a different team: refused.
if CODESIGN_VERIFY=1 SPCTL_TEAM='ZZZZZZZZZZ' \
    verify_app_signature "$APP" "$TEAM" >"$TEST_DIR/otherteam.out" 2>&1; then
    echo 'expected a Team ID mismatch in the Gatekeeper origin to fail' >&2
    exit 1
fi
grep -Fq "does not name Team ID $TEAM" "$TEST_DIR/otherteam.out"

# 5b. A checksumless rolling row demands notarization even when codesign
#     passes: otherwise the build would have neither a checksum nor a
#     notarization behind it.
if CODESIGN_VERIFY=0 SPCTL_SOURCE='Developer ID' \
    verify_app_signature "$APP" "$TEAM" 1 >"$TEST_DIR/rolling-unnotarized.out" 2>&1; then
    echo 'expected a checksumless row to require notarization' >&2
    exit 1
fi
grep -Fq 'publishes no checksum for this rolling URL, and it is not notarized' \
    "$TEST_DIR/rolling-unnotarized.out"

# 5c. Same row, properly notarized: accepted, and the reason names the checksum.
CODESIGN_VERIFY=0 verify_app_signature "$APP" "$TEAM" 1 >"$TEST_DIR/rolling-ok.out"
grep -Fq "publishes no checksum for this rolling URL; accepted on notarized Developer ID ($TEAM)" \
    "$TEST_DIR/rolling-ok.out"

# 5d. A checksummed row is unchanged: codesign alone still suffices, so an
#     un-notarized bundle that passes --deep --strict is not newly refused.
CODESIGN_VERIFY=0 SPCTL_ACCEPT=0 verify_app_signature "$APP" "$TEAM" >/dev/null

# 5e. A checksumless row with a Team ID mismatch in the Gatekeeper origin.
if CODESIGN_VERIFY=0 SPCTL_TEAM='ZZZZZZZZZZ' \
    verify_app_signature "$APP" "$TEAM" 1 >"$TEST_DIR/rolling-team.out" 2>&1; then
    echo 'expected a checksumless row to check the Gatekeeper origin team' >&2
    exit 1
fi
grep -Fq "does not name Team ID $TEAM" "$TEST_DIR/rolling-team.out"

# 6. A wrong Team ID in the signature is refused before any integrity check.
if SIGN_TEAM='ZZZZZZZZZZ' verify_app_signature "$APP" "$TEAM" >"$TEST_DIR/badteam.out" 2>&1; then
    echo 'expected a signature Team ID mismatch to fail' >&2
    exit 1
fi
grep -Fq "Team ID is ZZZZZZZZZZ, expected $TEAM" "$TEST_DIR/badteam.out"

# --- rolling URL checksum policy -------------------------------------------

write_cask_json() {
    cat >"$TEST_DIR/cask.json" <<EOF
{"casks":[{"token":"rolling","tap":"homebrew/cask","sha256":"$1","url":"https://dl.vendor.example/app.dmg","homepage":"https://vendor.example/"}]}
EOF
}

cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
[[ "\$1" == "info" ]] && cat '$TEST_DIR/cask.json'
EOF
chmod +x "$TEST_BIN/brew"

REAL_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

# 7. no_check without the opt-in stays refused (unchanged default).
write_cask_json no_check
if verify_cask_source rolling dl.vendor.example vendor.example >"$TEST_DIR/nocheck.out" 2>&1; then
    echo 'expected no_check without allow_rolling_url to fail' >&2
    exit 1
fi
grep -Fq 'did not publish a sha256 checksum' "$TEST_DIR/nocheck.out"

# 8. no_check with the opt-in is permitted and says so.
verify_cask_source rolling dl.vendor.example vendor.example 1 >"$TEST_DIR/rolling.out" 2>&1
grep -Fq 'rolling vendor URL' "$TEST_DIR/rolling.out"

# 9. The opt-in permits no_check only — a malformed digest is still refused.
write_cask_json abc123
if verify_cask_source rolling dl.vendor.example vendor.example 1 >"$TEST_DIR/short.out" 2>&1; then
    echo 'expected a malformed digest to fail even with allow_rolling_url' >&2
    exit 1
fi
grep -Fq 'did not publish a sha256 checksum' "$TEST_DIR/short.out"

# 10. The opt-in does not loosen the host allowlist.
write_cask_json no_check
if verify_cask_source rolling other.example vendor.example 1 >"$TEST_DIR/host.out" 2>&1; then
    echo 'expected an unexpected download host to fail with allow_rolling_url' >&2
    exit 1
fi
grep -Fq 'download host' "$TEST_DIR/host.out"

# 11. A normal checksum row is unaffected by the new argument.
write_cask_json "$REAL_SHA"
verify_cask_source rolling dl.vendor.example vendor.example >"$TEST_DIR/normal.out" 2>&1
! grep -q 'rolling vendor URL' "$TEST_DIR/normal.out"

# --- catalog plumbing -------------------------------------------------------

cat >"$CONFIG_REPO_ROOT/apps.json" <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "plain", "kind": "signed-cask", "token": "plain", "app_name": "Plain.app",
     "team_id": "AAAAAAAAAA", "url_hosts": ["a.example"], "homepage_hosts": ["a.example"]},
    {"name": "rolling", "kind": "signed-cask", "token": "rolling", "app_name": "Rolling.app",
     "team_id": "BBBBBBBBBB", "url_hosts": ["b.example"], "homepage_hosts": ["b.example"],
     "allow_rolling_url": true}
  ]
}
EOF

[[ "$(cask_allowlist_row plain)" == "Plain.app|AAAAAAAAAA|a.example|a.example|" ]]
[[ "$(cask_allowlist_row rolling)" == "Rolling.app|BBBBBBBBBB|b.example|b.example|1" ]]

# 12. allow_rolling_url must be a boolean, like auto.
cat >"$CONFIG_REPO_ROOT/apps.json" <<'EOF'
{"schema_version":1,"apps":[{"name":"bad","kind":"signed-cask","token":"bad","allow_rolling_url":"true"}]}
EOF
if cask_allowlist_row bad >"$TEST_DIR/badflag.out" 2>&1; then
    echo 'expected a string allow_rolling_url to fail' >&2
    exit 1
fi
grep -Fq 'apps.json allow_rolling_url must be a boolean' "$TEST_DIR/badflag.out"

echo 'cask-verify tests passed'

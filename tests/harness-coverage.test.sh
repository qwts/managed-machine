#!/usr/bin/env bash
# Roster coverage (#112): every active roster harness must resolve to an
# install path — a setup-<harness> or setup-<harness>-cli script, or a
# catalog row (aliases count, e.g. grok -> grok-build). A roster addition
# without an install path fails this test.
#
# tests/fixtures/organization-profile.json mirrors the published
# organization profile (qwts-agent-org/governance/
# organization-profile.json): update the fixture and the ACTIVE count below
# whenever the roster changes.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
mkdir -p "$CONFIG_REPO_ROOT"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO_ROOT/apps.json"
export CONFIG_REPO_ROOT
export MANAGED_MACHINE_ORG_PROFILE="$ROOT/tests/fixtures/organization-profile.json"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/agent-account.sh
source "$ROOT/lib/agent-account.sh"

# catalog_row_installable <name>: the resolved row carries every field its
# install engine reads — name resolution alone is not an install path. Field
# requirements mirror the engine guards in lib/apps.sh, lib/cask-app.sh, and
# lib/vendor-dmg.sh (e.g. npm refuses a row without `package`, signed-cask
# without token/app_name/team_id/url_hosts/homepage_hosts).
catalog_row_installable() {
    local name="$1" kind field
    kind="$(catalog_app_kind "$name")" || return 1
    case "$kind" in
        official-cli)
            for field in command url; do
                catalog_app_field "$name" "$field" >/dev/null 2>&1 || return 1
            done
            ;;
        npm)
            catalog_app_field "$name" package >/dev/null 2>&1 || return 1
            ;;
        brew-formula)
            catalog_app_field "$name" formula >/dev/null 2>&1 || return 1
            ;;
        signed-cask|cask)
            for field in token app_name team_id url_hosts homepage_hosts; do
                catalog_app_field "$name" "$field" >/dev/null 2>&1 || return 1
            done
            ;;
        vendor-dmg)
            for field in app_name team_id url_hosts; do
                catalog_app_field "$name" "$field" >/dev/null 2>&1 || return 1
            done
            { catalog_app_field "$name" url >/dev/null 2>&1 \
                || catalog_app_field "$name" url_arm64 >/dev/null 2>&1 \
                || catalog_app_field "$name" url_x86_64 >/dev/null 2>&1; } || return 1
            { catalog_app_field "$name" sha256 >/dev/null 2>&1 \
                || catalog_app_field "$name" sha256_arm64 >/dev/null 2>&1 \
                || catalog_app_field "$name" sha256_x86_64 >/dev/null 2>&1; } || return 1
            ;;
        opencode|devin)
            # Product-specific engines read no row fields.
            ;;
        *)
            # Unknown kind: install_catalog_app would fail the dispatch.
            return 1
            ;;
    esac
}

ACTIVE=21
failures=()
active=0
retired=0
while IFS=$'\t' read -r slug status; do
    [[ -n "$slug" ]] || continue
    if [[ "$status" != "active" ]]; then
        retired=$((retired + 1))
        continue
    fi
    active=$((active + 1))
    harness="$(agent_roster_query harness "$slug")"
    if [[ -z "$harness" ]]; then
        failures+=("$slug: active roster row names no harness")
        continue
    fi
    setup_name="$(agent_harness_setup_name "$ROOT" "$harness")"
    if [[ -x "$ROOT/setup-$setup_name" ]]; then
        continue
    fi
    resolved="$(catalog_resolve_name "$setup_name" 2>/dev/null || true)"
    [[ -n "$resolved" ]] || resolved="$(catalog_resolve_name "$harness" 2>/dev/null || true)"
    if [[ -z "$resolved" ]]; then
        failures+=("$slug: harness '$harness' (setup name '$setup_name') has no install path")
        continue
    fi
    if ! catalog_row_installable "$resolved"; then
        failures+=("$slug: catalog row '$resolved' for harness '$harness' lacks a field its install engine requires")
    fi
done < <(agent_roster_rows)

[[ "$active" -gt 0 ]] || { echo 'roster fixture names no active identities' >&2; exit 1; }
[[ "$retired" -gt 0 ]] || { echo 'roster fixture names no retired identities to skip' >&2; exit 1; }
[[ "$active" == "$ACTIVE" ]] || {
    echo "expected $ACTIVE active identities, roster fixture has $active — update the fixture and this count when the roster changes" >&2
    exit 1
}
if [[ ${#failures[@]} -gt 0 ]]; then
    printf 'missing install path: %s\n' "${failures[@]}" >&2
    exit 1
fi

# Negative: a resolved row missing a required field is not an install path —
# cline without `package` must be refused like install_catalog_app would.
! catalog_row_installable cline_missing 2>/dev/null
if catalog_row_installable cline; then :; else
    echo 'expected complete cline row to validate' >&2
    exit 1
fi
python3 - "$CONFIG_REPO_ROOT/apps.json" <<'PY'
import json, sys
path = sys.argv[1]
data = json.load(open(path))
for app in data["apps"]:
    if app.get("name") == "cline":
        del app["package"]
json.dump(data, open(path, "w"))
PY
if catalog_row_installable cline; then
    echo 'expected cline row without package to fail validation' >&2
    exit 1
fi

echo "harness coverage tests passed ($active active identities, $retired retired skipped)"

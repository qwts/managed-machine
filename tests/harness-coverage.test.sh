#!/usr/bin/env bash
# Roster coverage (#112): every active roster harness must resolve to an
# install path — a setup-<harness> or setup-<harness>-cli script, or a
# catalog row (aliases count, e.g. grok -> grok-build). A roster addition
# without an install path fails this test.
#
# tests/fixtures/organization-profile.json mirrors the published
# organization profile (playbook-engineering/governance/
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
    if catalog_has_app "$setup_name" || catalog_has_app "$harness"; then
        continue
    fi
    failures+=("$slug: harness '$harness' (setup name '$setup_name') has no install path")
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

echo "harness coverage tests passed ($active active identities, $retired retired skipped)"

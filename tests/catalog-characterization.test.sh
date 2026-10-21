#!/usr/bin/env bash
# F01 characterization goldens for `managed-machine catalog list/resolve` and `setup --list`
# (#243, draft harness for review). The real dispatcher, scripts/catalog and
# lib/ are copied into a throwaway tree; only the host seams are overridden by
# appending stubs to the fixture copy of lib/apps.sh (sourced last by
# scripts/catalog): config checkout location, refresh, install probes,
# declined manifest and provenance. Everything else is the production code
# path, run with an empty HOME and environment against a fixture apps.json.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GOLDEN="$ROOT/tests/fixtures/catalog-characterization"
RECORD=0
[[ "${1:-}" == "--record" ]] && RECORD=1

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT
FIX="$TEST_ROOT/fixture"
# Sibling layout matches the formula install (libexec/managed-machine-config), which
# lib/catalog.sh resolves through REPO_ROOT/../managed-machine-config.
CONFIG="$TEST_ROOT/managed-machine-config"
EMPTY_HOME="$TEST_ROOT/home"
PROBE_LOG="$TEST_ROOT/probe.log"
mkdir -p "$FIX/bin" "$FIX/scripts" "$CONFIG" "$EMPTY_HOME"
cp "$ROOT/bin/managed-machine" "$FIX/bin/"
cp "$ROOT/scripts/catalog" "$ROOT/scripts/status" "$ROOT/scripts/fleet" "$ROOT/scripts/ssh" "$ROOT/scripts/account" "$FIX/scripts/"
cp -R "$ROOT/lib" "$FIX/lib"
printf '0.0.0-fixture\n' >"$FIX/VERSION"
# One wrapper setup script and one standalone script, so both listing kinds appear.
printf '#!/usr/bin/env bash\ninstall_catalog_app grok-build "$@"\n' >"$FIX/setup-grok-build"
printf '#!/usr/bin/env bash\nexit 0\n' >"$FIX/setup-alpha"
chmod +x "$FIX/setup-grok-build" "$FIX/setup-alpha"
cat >"$CONFIG/apps.json" <<'JSON'
{
  "schema_version": 1,
  "apps": [
    {"name": "vscode", "kind": "signed-cask", "token": "visual-studio-code", "app_name": "Visual Studio Code.app", "command": "code", "team_id": "FIXTURETEAM", "aliases": ["code"]},
    {"name": "grok-build", "kind": "official-cli", "command": "grok", "aliases": ["grok"], "url": "https://example.invalid/install.sh"},
    {"name": "optional-app", "kind": "brew-formula", "formula": "minikube", "auto": false}
  ]
}
JSON
printf 'grok-build\t2026-10-15T00:00:00Z\tauthorization declined\n' >"$CONFIG/declined.manifest"
# print_setup_names (setup --list) resolves and refreshes the config checkout
# right after sourcing lib/install.sh, before lib/apps.sh is sourced, so the
# checkout seams must be overridden in the fixture copy of lib/config-repo.sh
# (sourced by install.sh) as well as in lib/apps.sh.
cat >>"$FIX/lib/config-repo.sh" <<'STUB'

# --- characterization stubs (test tree only) --------------------------------
existing_managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
refresh_managed_machine_config_repo() { printf 'refresh %s\n' "$1" >>"$MM_PROBE_LOG"; }
config_repo_provenance_json() { printf '{"fixture":true}\n'; }
STUB
cat >>"$FIX/lib/apps.sh" <<'STUB'

# --- characterization stubs (test tree only) --------------------------------
existing_managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
refresh_managed_machine_config_repo() { printf 'refresh %s\n' "$1" >>"$MM_PROBE_LOG"; }
setup_list_prime() { :; }
declined_apps_manifest_file() { printf '%s\n' "$MM_FIXTURE_CONFIG/declined.manifest"; }
config_repo_provenance_json() { printf '{"fixture":true}\n'; }
catalog_app_installed() { printf 'probe app %s\n' "$1" >>"$MM_PROBE_LOG"; [[ "$1" == "vscode" ]]; }
setup_name_installed() { printf 'probe script %s\n' "$2" >>"$MM_PROBE_LOG"; [[ "$2" == "alpha" ]]; }
STUB

PY_DIR="$(dirname "$(command -v python3)")"
MIN_PATH="$PY_DIR:/usr/bin:/bin"
fail() { echo "FAIL: $1" >&2; exit 1; }
failures=0

# run_case <refresh-expected: yes|no> <name> <args...>
run_case() {
    local refresh="$1" name="$2"; shift 2
    local out="$TEST_ROOT/$name.out" err="$TEST_ROOT/$name.err" code=0
    : >"$PROBE_LOG"
    set +e
    env -i HOME="$EMPTY_HOME" PATH="$MIN_PATH" LANG=C.UTF-8 LC_ALL=C.UTF-8 \
        MM_FIXTURE_CONFIG="$CONFIG" MM_PROBE_LOG="$PROBE_LOG" \
        bash "$FIX/bin/managed-machine" "$@" >"$out" 2>"$err"
    code=$?
    set -e
    if [[ "$refresh" == no ]] && grep -q '^refresh ' "$PROBE_LOG"; then
        fail "$name: --no-refresh still refreshed the config checkout"
    fi
    local actual="$TEST_ROOT/$name.actual"
    {
        printf 'exit %s\n--- stdout\n' "$code"
        sed -e "s|$CONFIG|<CONFIG>|g" -e "s|$FIX|<ROOT>|g" "$out"
        printf -- '--- stderr\n'
        sed -e "s|$CONFIG|<CONFIG>|g" -e "s|$FIX|<ROOT>|g" "$err"
        printf -- '--- probes\n'
        sort "$PROBE_LOG" | sed -e "s|$CONFIG|<CONFIG>|g" -e "s|$FIX|<ROOT>|g"
    } >"$actual"
    if [[ "$RECORD" == 1 ]]; then
        mkdir -p "$GOLDEN"; cp "$actual" "$GOLDEN/$name.golden"; echo "recorded $name"; return
    fi
    if ! diff -u "$GOLDEN/$name.golden" "$actual"; then
        echo "FAIL: $name differs from golden" >&2
        failures=$((failures + 1))
    fi
}

run_case no  list-json catalog list --json --no-refresh
run_case no  list-human catalog list --no-refresh
run_case yes list-json-refresh catalog list --json
run_case no  resolve-alias-json catalog resolve code --json --no-refresh
run_case no  resolve-token-setup-prefix catalog resolve setup-visual-studio-code --json --no-refresh
run_case no  resolve-wrapper catalog resolve grok-build --json --no-refresh
run_case no  resolve-standalone catalog resolve alpha --json --no-refresh
run_case no  resolve-unknown-json catalog resolve nothing-here --json --no-refresh
run_case no  resolve-unknown-plain catalog resolve nothing-here --no-refresh
run_case no  resolve-bogus-flag catalog resolve code --bogus --json --no-refresh
run_case no  list-bogus-flag catalog list --bogus --json
run_case no  usage catalog
run_case yes usage-json catalog --json
run_case yes setup-list setup --list
run_case yes setup-list-json setup --list --json
run_case no  setup-list-bogus setup --list --bogus
run_case no  setup-list-bogus-json setup --list --bogus --json
run_case no  setup-help setup --help
run_case no  status-bad-section-json status --json --section nothing-here
run_case no  status-bad-section status --section nothing-here
run_case no  status-bogus-flag-json status --bogus --json
run_case no  status-help status --help
run_case no  fleet-bogus-json fleet --bogus --json
run_case no  fleet-list-bogus-json fleet list --bogus --json
run_case no  ssh-bogus-json ssh --bogus --json
run_case no  ssh-status-bogus-json ssh status --bogus --json
run_case no  account-bogus-json account --bogus --json
run_case no  account-doctor-noname-json account doctor --json

[[ "$failures" == 0 ]] || fail "$failures golden case(s) differ"
if [[ "$RECORD" == 1 ]]; then echo "catalog characterization: goldens recorded"; else echo "catalog characterization: 28 cases match goldens"; fi

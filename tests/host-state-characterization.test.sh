#!/usr/bin/env bash
# F01 characterization goldens for the host-state routes in a populated state
# (#248): `fleet list` with registry records, `ssh status` with an enrolled key
# and a registered machine, `account doctor` against a seeded roster, every
# `status` section with recorded manifests, and an `adopt` run that reaches
# the skip decision. The real dispatcher, scripts and lib/ are copied into a
# throwaway tree; only host seams are overridden, by appending stub functions
# to the fixture copies (directory-service lookups, config checkout, refresh,
# install probes, plan authorization) and by placing failing stubs for brew,
# rustup, rustc, cargo and a fixed `hostname` first on PATH. Everything else
# is the production code path, run with `env -i` against a fixture HOME.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GOLDEN="$ROOT/tests/fixtures/host-state-characterization"
RECORD=0
[[ "${1:-}" == "--record" ]] && RECORD=1

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT
FIX="$TEST_ROOT/fixture"
# Sibling layout matches the formula install (libexec/managed-machine-config):
# lib/catalog.sh and scripts/status resolve it through REPO_ROOT/../managed-machine-config.
CONFIG="$TEST_ROOT/managed-machine-config"
HOME_DIR="$TEST_ROOT/home"
STATE="$HOME_DIR/.config/managed-machine"
STUB_BIN="$TEST_ROOT/bin"
APPDIR="$TEST_ROOT/Applications"
EMPTY_APPDIR="$TEST_ROOT/no-apps"
SHARED="$TEST_ROOT/shared"
PROBE_LOG="$TEST_ROOT/probe.log"
mkdir -p "$FIX/bin" "$FIX/scripts" "$FIX/Formula" "$CONFIG/fleet/machines" "$STATE" "$HOME_DIR/.ssh" \
    "$STUB_BIN" "$APPDIR/Visual Studio Code.app" "$EMPTY_APPDIR" "$SHARED/markers" "$SHARED/pictures"
chmod 700 "$HOME_DIR/.ssh"
cp "$ROOT/bin/managed-machine" "$FIX/bin/"
cp "$ROOT/scripts/catalog" "$ROOT/scripts/status" "$ROOT/scripts/fleet" "$ROOT/scripts/ssh" "$ROOT/scripts/account" "$ROOT/scripts/adopt" "$FIX/scripts/"
cp -R "$ROOT/lib" "$FIX/lib"
printf '0.0.0-fixture\n' >"$FIX/VERSION"
printf 'class ManagedMachine < Formula\n  version "0.0.0"\nend\n' >"$FIX/Formula/managed-machine.rb"
printf '#!/usr/bin/env bash\ninstall_catalog_app grok-build "$@"\n' >"$FIX/setup-grok-build"
printf '#!/usr/bin/env bash\nexit 0\n' >"$FIX/setup-alpha"
chmod +x "$FIX/setup-grok-build" "$FIX/setup-alpha"

# --- fixture config checkout -------------------------------------------------
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
printf 'v1.2.4\n' >"$CONFIG/local-bin.ref"
printf 'main\n' >"$CONFIG/zsh-functions.ref"
# Two registry records, written in the shape write_machine_record produces.
cat >"$CONFIG/fleet/machines/sha256-fixture_machine_bbbb.toml" <<'TOML'
schema_version = 1
machine_id = "sha256-fixture_machine_bbbb"
hostname = "second-mac"
managed_at = "2026-10-16T02:00:00Z"
managed_machine_ref = "v0.0.0"
local_bin_ref = "v1.2.4"
ssh_key_fingerprint = "SHA256:fixture/machine+bbbb"
public_key = "ssh-ed25519 AAAAfixturebbbb"
TOML
cat >"$CONFIG/fleet/machines/sha256-fixture_machine_aaaa.toml" <<'TOML'
schema_version = 1
machine_id = "sha256-fixture_machine_aaaa"
hostname = "fixture-mac"
managed_at = "2026-10-15T01:00:00Z"
managed_machine_ref = "v0.0.0"
local_bin_ref = "v1.2.3"
ssh_key_fingerprint = "SHA256:fixture/machine+aaaa"
TOML

# --- fixture HOME state --------------------------------------------------------
cat >"$STATE/machine.toml" <<'TOML'
schema_version = 1
machine_id = "sha256-fixture_machine_aaaa"
hostname = "fixture-mac"
TOML
# Shapes follow scripts/bootstrap and scripts/update: key=value metadata plus
# status<TAB>name<TAB>detail rows.
printf 'schema_version=1\nmode=noninteractive\ncomplete\thomebrew\t\ncomplete\tgh\t\ndeferred\tvscode\tauthorization declined\nskipped\toptional-app\tauto false\nfailed\tgrok-build\texit 1\nfinished_at=2026-10-15T01:02:03Z\n' >"$STATE/bootstrap.manifest"
printf 'schema_version=1\nversion=0.0.0-fixture\ncomplete\thomebrew\t\nfailed\tgrok-build\texit 1\nfinished_at=2026-10-16T01:02:03Z\n' >"$STATE/update.manifest"
printf 'schema_version=1\nref=v1.2.3\ncommit=0123456789abcdef0123456789abcdef01234567\ncheckout=%s/.local/share/managed-machine/local-bin/v1.2.3\nrecorded_at=2026-10-15T01:02:03Z\n' "$HOME_DIR" >"$STATE/local-bin.manifest"
printf 'schema_version=1\nref=main\ncommit=89abcdef0123456789abcdef0123456789abcdef\ncheckout=%s/.local/share/managed-machine/zsh-functions/main\nrecorded_at=2026-10-15T01:02:03Z\n' "$HOME_DIR" >"$STATE/zsh-functions.manifest"
# Roster: one provisioned active account, one active account with no OS user,
# one retired slug. Resolved through the config-dir candidate of agent_roster_source.
cat >"$STATE/organization-profile.json" <<'JSON'
{"identities": [
  {"slug": "fixture-agent", "status": "active", "harness": "grok"},
  {"slug": "second-agent", "status": "active", "harness": "grok"},
  {"slug": "retired-agent", "status": "retired", "harness": "grok"}
]}
JSON
# A throwaway key pair stands in for the enrolled GitHub key; its fingerprint
# is normalized out of the goldens. Nothing reads the private key.
ssh-keygen -q -t ed25519 -N '' -C fixture -f "$HOME_DIR/.ssh/id_rsa_github"
FINGERPRINT="$(ssh-keygen -lf "$HOME_DIR/.ssh/id_rsa_github.pub" | awk '{print $2}')"
# Literal form for the extended-regex normalizer (base64 carries + and /).
FINGERPRINT_RE="$(printf '%s' "$FINGERPRINT" | sed -e 's/[][\/.*^$+?(){}|]/\\&/g')"
printf '[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = %s/.ssh/id_rsa_github.pub\n' "$HOME_DIR" >"$HOME_DIR/.gitconfig"
# World-readable markers add-agent would have recorded for the provisioned account.
cat >"$SHARED/markers/fixture-agent.account.json" <<'JSON'
{"schema_version": 1, "command": "account-doctor", "account": "fixture-agent", "ready": true, "status": "ready",
 "checks": [{"id": "account.identity", "status": "ready"}, {"id": "shell.profiles", "status": "ready"},
            {"id": "local_bin.manifest", "status": "ready"}, {"id": "harness.catalog", "status": "ready"}]}
JSON
printf 'ok grok\n' >"$SHARED/markers/fixture-agent.harness"
: >"$SHARED/pictures/fixture-agent.png"

# --- PATH stubs -----------------------------------------------------------------
# Failing brew/rustup/rustc/cargo keep ensure_status_path from discovering the
# host Homebrew prefix (the same device tests/agent-accounts-status.test.sh
# uses); a fixed hostname replaces the machine's; python3 is the real one.
for cmd in brew rustup rustc cargo; do
    printf '#!/usr/bin/env bash\nexit 1\n' >"$STUB_BIN/$cmd"
done
printf '#!/usr/bin/env bash\nprintf "fixture-host\\n"\n' >"$STUB_BIN/hostname"
chmod +x "$STUB_BIN"/*
ln -s "$(command -v python3)" "$STUB_BIN/python3"
MIN_PATH="$STUB_BIN:/usr/bin:/bin"

# --- seams overridden in the fixture tree ------------------------------------------
# Checkout seams (sourced through lib/install.sh before anything else).
cat >>"$FIX/lib/config-repo.sh" <<'STUB'

# --- characterization stubs (test tree only) --------------------------------
existing_managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
refresh_managed_machine_config_repo() { printf 'refresh %s\n' "$1" >>"$MM_PROBE_LOG"; }
config_repo_provenance_json() { printf '{"fixture":true}\n'; }
STUB
# Install probes and the adopt plan authorization (lib/apps.sh is sourced last
# by scripts/catalog and scripts/adopt).
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
adopt_stage_and_authorize_plan() { shift; printf 'plan %s\n' "$*" >>"$MM_PROBE_LOG"; return 0; }
STUB
# Directory-service lookups (dscl, dsmemberutil) answer from the fixture roster;
# the system markers and pictures directories move under the test root (the
# rewrite tests/agent-accounts-status.test.sh uses).
sed -i.bak -e "s|/Library/Application Support/managed-machine/agents|$SHARED/markers|g" \
    -e "s|/Library/User Pictures/agents|$SHARED/pictures|g" "$FIX/lib/agent-account.sh"
rm -f "$FIX/lib/agent-account.sh.bak"
cat >>"$FIX/lib/agent-account.sh" <<'STUB'

# --- characterization stubs (test tree only) --------------------------------
agent_account_exists() { printf 'probe directory exists %s\n' "$1" >>"$MM_PROBE_LOG"; [[ "$1" == "fixture-agent" ]]; }
agent_account_in_group() { printf 'probe directory group %s %s\n' "$1" "$2" >>"$MM_PROBE_LOG"; [[ "$1" == "fixture-agent" && "$2" == "agents" ]]; }
agent_account_home() { printf 'probe directory home %s\n' "$1" >>"$MM_PROBE_LOG"; [[ "$1" == "fixture-agent" ]] && printf '%s\n' "$MM_FIXTURE_HOME"; }
agent_account_picture() { printf 'probe directory picture %s\n' "$1" >>"$MM_PROBE_LOG"; [[ "$1" == "fixture-agent" ]] && printf '%s/pictures/fixture-agent.png\n' "$MM_FIXTURE_SHARED"; }
STUB
# scripts/account defines its own id(1) wrappers after sourcing lib/, so the
# overrides go into the fixture copy just before its entry point. The current
# user is reported as the provisioned account so doctor takes the same-account
# path (the cross-account path needs an attended authorization dialog).
{
    sed '$d' "$ROOT/scripts/account"
    cat <<'STUB'
# --- characterization stubs (test tree only) --------------------------------
account_current_user() { printf 'fixture-agent\n'; }
account_uid() { printf 'probe id uid %s\n' "$1" >>"$MM_PROBE_LOG"; printf '1501\n'; }
account_groups() { printf 'probe id groups %s\n' "$1" >>"$MM_PROBE_LOG"; printf 'staff agents\n'; }
STUB
    tail -n 1 "$ROOT/scripts/account"
} >"$FIX/scripts/account"
chmod +x "$FIX/scripts/account"

CURRENT_USER="$(/usr/bin/id -un)"
fail() { echo "FAIL: $1" >&2; exit 1; }
failures=0

normalize() {
    sed -e "s|$CONFIG|<CONFIG>|g" -e "s|$FIX|<ROOT>|g" -e "s|$HOME_DIR|<HOME>|g" \
        -e "s|$TEST_ROOT|<TMP>|g" -e "s|$FINGERPRINT_RE|<FINGERPRINT>|g" \
        -E -e "s/(^|[^A-Za-z0-9_.-])$CURRENT_USER([^A-Za-z0-9_.-]|\$)/\1<USER>\2/g"
}

# run_case <refresh-expected: yes|no> <name> [VAR=value ...] <args...>
run_case() {
    local refresh="$1" name="$2"; shift 2
    local -a extra=()
    while [[ $# -gt 0 && "$1" == *=* ]]; do extra+=("$1"); shift; done
    local out="$TEST_ROOT/$name.out" err="$TEST_ROOT/$name.err" code=0
    : >"$PROBE_LOG"
    set +e
    env -i HOME="$HOME_DIR" PATH="$MIN_PATH" LANG=C.UTF-8 LC_ALL=C.UTF-8 \
        MM_FIXTURE_CONFIG="$CONFIG" MM_FIXTURE_HOME="$HOME_DIR" MM_FIXTURE_SHARED="$SHARED" MM_PROBE_LOG="$PROBE_LOG" \
        MANAGED_MACHINE_SYSTEM_APPDIR="$APPDIR" MANAGED_MACHINE_ACCOUNT_CONTEXT=1 ${extra[@]+"${extra[@]}"} \
        bash "$FIX/bin/managed-machine" "$@" >"$out" 2>"$err"
    code=$?
    set -e
    if [[ "$refresh" == no ]] && grep -q '^refresh ' "$PROBE_LOG"; then
        fail "$name: route refreshed the config checkout"
    fi
    local actual="$TEST_ROOT/$name.actual"
    {
        printf 'exit %s\n--- stdout\n' "$code"
        normalize <"$out"
        printf -- '--- stderr\n'
        normalize <"$err"
        printf -- '--- probes\n'
        LC_ALL=C sort "$PROBE_LOG" | normalize
    } >"$actual"
    if grep -q -F -e "$TEST_ROOT" -e "$FINGERPRINT" "$actual"; then
        fail "$name: fixture path or fingerprint leaked into the golden"
    fi
    if [[ "$RECORD" == 1 ]]; then
        mkdir -p "$GOLDEN"; cp "$actual" "$GOLDEN/$name.golden"; echo "recorded $name"; return
    fi
    if ! diff -u "$GOLDEN/$name.golden" "$actual"; then
        echo "FAIL: $name differs from golden" >&2
        failures=$((failures + 1))
    fi
}

run_case no fleet-list-json fleet list --json
run_case no fleet-list-human fleet list
run_case no ssh-status-json ssh status --json
run_case no ssh-status-human ssh status
run_case no account-doctor-retired-json account doctor retired-agent --json
run_case no account-doctor-retired-human account doctor retired-agent
run_case no account-doctor-unprovisioned-json account doctor second-agent --json
run_case no account-doctor-provisioned-json account doctor fixture-agent --json
run_case no account-doctor-provisioned-human account doctor fixture-agent
run_case no status-json status --json
run_case no status-human status
run_case no status-pins status --section pins
run_case no status-toolchain-json status --section toolchain --json
run_case no status-machine-ssh-json status --section machine,ssh --json
run_case no status-agent-accounts status --section agent-accounts
run_case no adopt-help adopt --help
run_case no adopt-alias-skipped-missing MANAGED_MACHINE_SYSTEM_APPDIR="$EMPTY_APPDIR" adopt code
run_case no adopt-all-skipped-missing MANAGED_MACHINE_SYSTEM_APPDIR="$EMPTY_APPDIR" adopt

if [[ "$RECORD" == 1 ]]; then
    echo "host-state characterization: goldens recorded in $GOLDEN"
    exit 0
fi
[[ "$failures" -eq 0 ]] || fail "$failures host-state characterization case(s) differ"
echo "host-state characterization: $(ls "$GOLDEN"/*.golden | wc -l | tr -d ' ') cases match goldens"

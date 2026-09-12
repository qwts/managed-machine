#!/usr/bin/env bash
# The setup list marks names whose install is already present. These probes
# are read-only: catalog rows check the same receipts their install engines
# consult (brew receipts, a staged bundle, the CLI command on PATH), setup-*
# wrappers resolve through their install_catalog_app call, and the
# infrastructure scripts check their own markers.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

HOME_DIR="$TEST_DIR/home"
FAKE_ROOT="$TEST_DIR/managed-machine"
STUB_BIN="$TEST_DIR/bin"
export HOME="$HOME_DIR"
export MANAGED_MACHINE_SYSTEM_APPDIR="$TEST_DIR/Applications"
unset NVM_DIR
mkdir -p "$HOME_DIR" "$FAKE_ROOT/lib" "$STUB_BIN" "$MANAGED_MACHINE_SYSTEM_APPDIR"

CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
mkdir -p "$CONFIG_REPO_ROOT"
cat >"$CONFIG_REPO_ROOT/apps.json" <<'EOF'
{
  "apps": [
    {
      "name": "deskapp",
      "kind": "signed-cask",
      "token": "deskapp",
      "app_name": "Desk.app",
      "team_id": "TEAMID1234",
      "url_hosts": ["vendor.example.com"],
      "homepage_hosts": ["vendor.example.com"],
      "sha256": "0000000000000000000000000000000000000000000000000000000000000000"
    },
    {
      "name": "dmgapp",
      "kind": "vendor-dmg",
      "app_name": "Dmg.app",
      "team_id": "TEAMID1234",
      "url": "https://vendor.example.com/dmg.dmg",
      "url_hosts": ["vendor.example.com"],
      "sha256": "0000000000000000000000000000000000000000000000000000000000000000"
    },
    {
      "name": "formulatool",
      "kind": "brew-formula",
      "formula": "formulatool"
    },
    {
      "name": "npmtool",
      "kind": "npm",
      "package": "@fixture/npmtool",
      "command": "npmtool",
      "auto": false
    },
    {
      "name": "clitool",
      "kind": "official-cli",
      "command": "clitool",
      "url": "https://vendor.example.com/install.sh"
    },
    {
      "name": "wrappedcli",
      "kind": "official-cli",
      "command": "wrappedcli",
      "url": "https://vendor.example.com/install.sh"
    },
    {
      "name": "dvx",
      "kind": "devin"
    },
    {
      "name": "ocx",
      "kind": "opencode",
      "command": "ocx"
    }
  ]
}
EOF
export CONFIG_REPO_ROOT
export REPO_ROOT="$FAKE_ROOT"

# setup-<name> wrappers delegate to their install_catalog_app argument.
cat >"$FAKE_ROOT/setup-wraptool" <<'EOF'
#!/usr/bin/env bash
install_catalog_app wrappedcli
EOF
chmod +x "$FAKE_ROOT/setup-wraptool"
cat >"$FAKE_ROOT/setup-notwrapper" <<'EOF'
#!/usr/bin/env bash
echo 'does nothing catalog-shaped'
EOF
chmod +x "$FAKE_ROOT/setup-notwrapper"

# Probes that source per-domain libs resolve them under the passed root.
cp "$ROOT/lib/hostname.sh" "$ROOT/lib/agent-bot-gh.sh" "$ROOT/lib/agent-bot.sh" "$FAKE_ROOT/lib/"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

# Prime once: it snapshots the catalog rows and brew receipts so the checks
# below spawn neither python nor brew per name. The brew receipts it finds on
# the real machine are irrelevant — the fixture names cannot appear there —
# and the tests reassign them for the rows that need control.
setup_list_prime
[[ -n "$MM_CATALOG_ROWS" ]] || { echo 'setup_list_prime produced no catalog snapshot' >&2; exit 1; }

# The snapshot resolves canonical names and alias forms without a lookup.
[[ "$(catalog_row_for clitool)" == official-cli* ]]
[[ "$(catalog_row_for setup-clitool)" == official-cli* ]]
IFS='|' read -r _ _ _ _ _ _ auto _ _ <<<"$(catalog_row_for npmtool)"
[[ "$auto" == "0" ]]
# Without the snapshot, one catalog_query row lookup still resolves.
( unset MM_CATALOG_ROWS; [[ "$(catalog_row_for clitool)" == official-cli* ]] ) \
    || { echo 'catalog_row_for fallback failed' >&2; exit 1; }

# Receipt snapshots: set means "use this list", never spawn brew per row.
MM_BREW_FORMULA_RECEIPTS=""
MM_BREW_CASK_RECEIPTS=""

stub_command() {
    printf '#!/usr/bin/env bash\nexit 0\n' >"$STUB_BIN/$1"
    chmod +x "$STUB_BIN/$1"
    hash -r
}
unstub_command() {
    rm -f "$STUB_BIN/$1"
    hash -r
}
export PATH="$STUB_BIN:$PATH"

expect_installed() {
    if ! catalog_app_installed "$1"; then
        echo "expected catalog app installed: $1" >&2
        exit 1
    fi
}
expect_not_installed() {
    if catalog_app_installed "$1"; then
        echo "expected catalog app not installed: $1" >&2
        exit 1
    fi
}
expect_setup_installed() {
    if ! setup_name_installed "$FAKE_ROOT" "$1"; then
        echo "expected setup name installed: $1" >&2
        exit 1
    fi
}
expect_setup_not_installed() {
    if setup_name_installed "$FAKE_ROOT" "$1"; then
        echo "expected setup name not installed: $1" >&2
        exit 1
    fi
}

# receipt_list_has matches only a full "name <version>" line.
receipt_list_has $'formulatool 1.0\nothertool 2.0' formulatool
receipt_list_has $'formulatool 1.0\nothertool 2.0' othertool
if receipt_list_has $'formulatool-extra 1.0' formulatool; then
    echo 'receipt_list_has matched a prefix name' >&2
    exit 1
fi
if receipt_list_has '' formulatool; then
    echo 'receipt_list_has matched an empty list' >&2
    exit 1
fi

# official-cli / opencode / devin: the command on PATH is the install receipt.
expect_not_installed clitool
expect_not_installed ocx
expect_not_installed dvx
stub_command clitool
stub_command ocx
stub_command dvx
expect_installed clitool
expect_installed ocx
expect_installed dvx

# npm: command on PATH first, global receipt as the fallback.
expect_not_installed npmtool
stub_command npmtool
expect_installed npmtool
unstub_command npmtool
expect_not_installed npmtool

# brew-formula: only the receipt snapshot marks it.
expect_not_installed formulatool
MM_BREW_FORMULA_RECEIPTS=$'othertool 2.0\nformulatool 1.0'
expect_installed formulatool

# signed-cask: only a Homebrew receipt AND the bundle on disk marks it. A
# receiptless occupier is `adopt` territory and a bare receipt leaves no app —
# neither is "already installed".
expect_not_installed deskapp
MM_BREW_CASK_RECEIPTS='deskapp 4.0'
expect_not_installed deskapp
mkdir -p "$MANAGED_MACHINE_SYSTEM_APPDIR/Desk.app"
expect_installed deskapp
MM_BREW_CASK_RECEIPTS=""
expect_not_installed deskapp

# vendor-dmg: the staged bundle must also pass Team ID verification — a bare
# directory is an impostor the installer reports, never an install.
expect_not_installed dmgapp
mkdir -p "$MANAGED_MACHINE_SYSTEM_APPDIR/Dmg.app"
expect_not_installed dmgapp
verify_app_signature() { [[ "$1" == "$MANAGED_MACHINE_SYSTEM_APPDIR/Dmg.app" ]]; }
expect_installed dmgapp
verify_app_signature() { return 1; }
expect_not_installed dmgapp

# A row its engine cannot read is never "installed".
expect_not_installed nonexistent-app

# setup_name_installed: catalog names resolve directly; setup-* wrappers
# resolve through their install_catalog_app argument.
expect_setup_not_installed wraptool
stub_command wrappedcli
expect_setup_installed wraptool
expect_setup_installed clitool
expect_setup_not_installed notwrapper
expect_setup_not_installed missing-entirely

# Infrastructure scripts probe their own markers.
expect_setup_not_installed nvm
mkdir -p "$TEST_DIR/nvm"
NVM_DIR="$TEST_DIR/nvm" setup_name_installed "$FAKE_ROOT" nvm && {
    echo 'nvm marked installed without nvm.sh' >&2
    exit 1
}
printf 'nvm-sh-stub\n' >"$TEST_DIR/nvm/nvm.sh"
NVM_DIR="$TEST_DIR/nvm" setup_name_installed "$FAKE_ROOT" nvm

expect_setup_not_installed zsh
printf '# BEGIN local-bin\nexport PATH="$HOME/.local/bin:$PATH"\n# END local-bin\n' >"$HOME_DIR/.zshrc"
expect_setup_installed zsh

expect_setup_not_installed bin
mkdir -p "$HOME_DIR/.config/managed-machine"
: >"$HOME_DIR/.config/managed-machine/local-bin.manifest"
expect_setup_installed bin

expect_setup_not_installed hostname
printf 'schema_version=1\nname=testmac\n' >"$HOME_DIR/.config/managed-machine/hostname.manifest"
expect_setup_installed hostname

expect_setup_not_installed agent-bot-gh
: >"$HOME_DIR/.config/managed-machine/agent-bot-gh-interposer"
expect_setup_installed agent-bot-gh

# agent-bot: the mark mirrors setup-agent-bot's outcome classes — the
# reviewed runtime installed (a formula receipt), then the doctor gate
# verified or failing only on a specific App's lazily-provisioned
# credentials.
mkdir -p "$HOME_DIR/.local/bin"
cat >"$HOME_DIR/.local/bin/agent-bot" <<EOF
#!/usr/bin/env bash
case "\$(cat "$TEST_DIR/agent-bot-mode" 2>/dev/null || echo fail)" in
    ready) exit 0 ;;
    app) printf '%s\n' '{"first_actionable_failure":{"app_slug":"qwts-vscode-agent","code":"provider-session-required"}}'; exit 1 ;;
    *) printf 'boom\n'; exit 1 ;;
esac
EOF
chmod +x "$HOME_DIR/.local/bin/agent-bot"

# No reviewed runtime: a bare binary is the leftover conflict setup parks.
MM_BREW_FORMULA_RECEIPTS=""
echo ready >"$TEST_DIR/agent-bot-mode"
expect_setup_not_installed agent-bot

# Runtime installed but the doctor gate hard-fails: wiring never verified.
MM_BREW_FORMULA_RECEIPTS='agent-bot 1.0.0'
echo fail >"$TEST_DIR/agent-bot-mode"
expect_setup_not_installed agent-bot

# A failure scoped to one App's credential is lazy provisioning, not unwired.
echo app >"$TEST_DIR/agent-bot-mode"
expect_setup_installed agent-bot

# Fully wired.
echo ready >"$TEST_DIR/agent-bot-mode"
expect_setup_installed agent-bot

expect_setup_not_installed git-hooks
git -C "$FAKE_ROOT" init --quiet
expect_setup_not_installed git-hooks
# A custom local hooksPath is something setup composes with, not a completed
# managed install.
git -C "$FAKE_ROOT" config --local core.hooksPath /custom/hooks
expect_setup_not_installed git-hooks
git -C "$FAKE_ROOT" config --local core.hooksPath git-hooks
expect_setup_not_installed git-hooks
mkdir -p "$FAKE_ROOT/git-hooks"
printf '#!/bin/sh\nexit 0\n' >"$FAKE_ROOT/git-hooks/pre-commit"
chmod +x "$FAKE_ROOT/git-hooks/pre-commit"
# Wiring without gitleaks on PATH is still not the finished step.
(
    PATH="$STUB_BIN:/usr/bin:/bin"
    ensure_brew_on_path() { return 1; }
    ! setup_name_installed "$FAKE_ROOT" git-hooks
) || { echo 'git-hooks marked installed without gitleaks' >&2; exit 1; }
stub_command gitleaks
expect_setup_installed git-hooks
# The generated dispatcher form counts too.
git_dir="$(git -C "$FAKE_ROOT" rev-parse --path-format=absolute --git-common-dir)"
mkdir -p "$git_dir/managed-machine-hooks"
printf '#!/bin/sh\nexit 0\n' >"$git_dir/managed-machine-hooks/pre-commit"
chmod +x "$git_dir/managed-machine-hooks/pre-commit"
git -C "$FAKE_ROOT" config --local core.hooksPath "$git_dir/managed-machine-hooks"
expect_setup_installed git-hooks

# The setup listing refreshes the persistent config checkout pull-only: a
# catalog row merged after the checkout's last pull appears in the list, and
# the checkout fast-forwards — no setup-gh run required.
config_origin="$TEST_DIR/config-origin.git"
config_src="$TEST_DIR/config-src"
config_clone="$TEST_DIR/config-clone"
git init --quiet --bare "$config_origin"
git init --quiet "$config_src"
git -C "$config_src" config user.name 'setup list test'
git -C "$config_src" config user.email 'setup-list-test@example.invalid'
git -C "$config_src" config commit.gpgsign false
cat >"$config_src/apps.json" <<'EOF'
{"schema_version": 1, "apps": [{"name": "seedtool", "kind": "official-cli", "command": "seedtool", "url": "https://example.com/seedtool"}]}
EOF
git -C "$config_src" add . && git -C "$config_src" commit --quiet -m 'catalog v1'
git -C "$config_src" branch -M main
git -C "$config_src" remote add origin "file://$config_origin"
git -C "$config_src" push --quiet -u origin main
git clone --quiet "file://$config_origin" "$config_clone"
python3 - "$config_src/apps.json" <<'PY'
import json, sys
path = sys.argv[1]
data = json.load(open(path))
data["apps"].append({"name": "freshapp", "kind": "official-cli", "command": "freshapp", "url": "https://example.com/freshapp"})
json.dump(data, open(path, "w"), indent=2)
PY
git -C "$config_src" commit --quiet -am 'add freshapp' && git -C "$config_src" push --quiet
# The bare-setup listing prints on stderr.
listing="$(CONFIG_REPO_ROOT="$config_clone" MANAGED_MACHINE_CONFIG_REPO_URL="file://$config_origin" \
    "$ROOT/bin/managed-machine" setup 2>&1 || true)"
grep -q 'freshapp' <<<"$listing" || { echo 'fresh catalog row missing from setup listing' >&2; exit 1; }
[[ "$(git -C "$config_clone" rev-parse HEAD)" == "$(git -C "$config_clone" rev-parse origin/main)" ]] \
    || { echo 'config checkout was not refreshed by the listing' >&2; exit 1; }

echo 'setup list status tests passed'

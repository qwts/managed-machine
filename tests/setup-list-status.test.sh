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
      "command": "npmtool"
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
cp "$ROOT/lib/hostname.sh" "$ROOT/lib/agent-bot-gh.sh" "$FAKE_ROOT/lib/"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

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

# signed-cask: a bundle on disk or a cask receipt marks it.
expect_not_installed deskapp
MM_BREW_CASK_RECEIPTS='deskapp 4.0'
expect_installed deskapp
MM_BREW_CASK_RECEIPTS=""
expect_not_installed deskapp
mkdir -p "$MANAGED_MACHINE_SYSTEM_APPDIR/Desk.app"
expect_installed deskapp

# vendor-dmg: the staged bundle marks it.
expect_not_installed dmgapp
mkdir -p "$MANAGED_MACHINE_SYSTEM_APPDIR/Dmg.app"
expect_installed dmgapp

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

expect_setup_not_installed git-hooks
git -C "$FAKE_ROOT" init --quiet
expect_setup_not_installed git-hooks
git -C "$FAKE_ROOT" config --local core.hooksPath git-hooks
expect_setup_installed git-hooks

echo 'setup list status tests passed'

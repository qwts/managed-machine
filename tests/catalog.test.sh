#!/usr/bin/env bash
# Catalog auto filter: omitted/true install on bootstrap; false is setup-only.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$CONFIG_REPO_ROOT"
export CONFIG_REPO_ROOT

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

write_catalog() {
    cat >"$CONFIG_REPO_ROOT/apps.json"
}

write_catalog <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {"name": "always", "kind": "devin"},
    {"name": "explicit", "kind": "devin", "auto": true},
    {"name": "sometimes", "kind": "devin", "auto": false},
    {"name": "agent-bot", "kind": "devin", "auto": true},
    {"name": "agent-bot-gh", "kind": "devin", "auto": true}
  ]
}
EOF

names="$(catalog_app_names | tr '\n' ' ')"
[[ "$names" == "agent-bot agent-bot-gh always explicit sometimes " ]]
auto_names="$(catalog_auto_app_names | tr '\n' ' ')"
[[ "$auto_names" == "always explicit " ]]
catalog_app_is_auto always
catalog_app_is_auto explicit
# Stale catalog rows cannot reintroduce either retired runtime path.
for retired in agent-bot agent-bot-gh; do
    if install_catalog_app "$retired" >"$TEST_DIR/retired.out" 2>&1; then
        echo "expected $retired catalog install to be rejected" >&2
        exit 1
    fi
    grep -Fq 'catalog setup' "$TEST_DIR/retired.out"
done
if catalog_app_is_auto sometimes; then
    echo 'expected sometimes to be setup-only' >&2
    exit 1
fi
print_catalog_app_names >"$TEST_DIR/help.out"
grep -qxF '  always' "$TEST_DIR/help.out"
grep -qxF '  explicit' "$TEST_DIR/help.out"
grep -qxF '  sometimes (setup only)' "$TEST_DIR/help.out"

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"bad","kind":"devin","auto":"false"}]}
EOF
if catalog_app_names >"$TEST_DIR/string.out" 2>&1; then
    echo 'expected string auto to fail' >&2
    exit 1
fi
grep -Fq 'apps.json auto must be a boolean' "$TEST_DIR/string.out"

write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"bad","kind":"devin","auto":0}]}
EOF
if catalog_auto_app_names >"$TEST_DIR/zero.out" 2>&1; then
    echo 'expected numeric auto to fail' >&2
    exit 1
fi
grep -Fq 'apps.json auto must be a boolean' "$TEST_DIR/zero.out"

# ==========================================================================
# Catalog introspection CLI (managed-machine catalog list / resolve) (#215)
# ==========================================================================
TEST_HOME="$TEST_DIR/home"
FAKE_BIN="$TEST_DIR/bin"
mkdir -p "$TEST_HOME/.config/managed-machine" "$FAKE_BIN"
cat >"$FAKE_BIN/grok" <<'EOF'
#!/usr/bin/env bash
echo 'grok 1.0.0'
EOF
chmod +x "$FAKE_BIN/grok"

write_catalog <<'EOF'
{
  "schema_version": 1,
  "apps": [
    {
      "name": "vscode",
      "kind": "signed-cask",
      "token": "visual-studio-code",
      "app_name": "Visual Studio Code.app",
      "command": "code",
      "team_id": "UBF8T346G9"
    },
    {
      "name": "grok-build",
      "kind": "official-cli",
      "command": "grok",
      "aliases": ["grok"],
      "url": "https://example.invalid/install.sh"
    },
    {
      "name": "codex",
      "kind": "npm",
      "package": "@openai/codex",
      "command": "codex"
    },
    {
      "name": "optional-app",
      "kind": "brew-formula",
      "formula": "minikube",
      "auto": false
    }
  ]
}
EOF

printf 'codex\t2026-10-15T00:00:00Z\tauthorization declined\n' >"$TEST_HOME/.config/managed-machine/declined-apps.manifest"

REMOTE="$TEST_DIR/remote.git"
git init --quiet --bare "$REMOTE"
git init --quiet "$CONFIG_REPO_ROOT"
git -C "$CONFIG_REPO_ROOT" config user.name 'managed-machine test'
git -C "$CONFIG_REPO_ROOT" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO_ROOT" config commit.gpgsign false
git -C "$CONFIG_REPO_ROOT" add apps.json
git -C "$CONFIG_REPO_ROOT" commit --quiet -m 'Seed catalog test repo'
git -C "$CONFIG_REPO_ROOT" branch -M main
git -C "$CONFIG_REPO_ROOT" remote add origin "$REMOTE"
git -C "$CONFIG_REPO_ROOT" push --quiet -u origin main
git --git-dir="$REMOTE" symbolic-ref HEAD refs/heads/main

HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    /bin/bash "$ROOT/scripts/catalog" list --no-refresh >"$TEST_DIR/cat-list.out" 2>"$TEST_DIR/cat-list.err"
[ ! -s "$TEST_DIR/cat-list.err" ]
grep -Fq $'NAME\tKIND\tCOMMAND\tSTATUS\tAUTO\tALIASES' "$TEST_DIR/cat-list.out"
grep -Fq $'grok-build\tofficial-cli\tgrok\tinstalled\tyes\tgrok' "$TEST_DIR/cat-list.out"
grep -Fq $'codex\tnpm\tcodex\tdeclined\tyes\tcodex-cli' "$TEST_DIR/cat-list.out"
grep -Fq $'optional-app\tbrew-formula\tminikube\tnot_installed\tno\t-' "$TEST_DIR/cat-list.out"

HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    /bin/bash "$ROOT/scripts/catalog" list --json --no-refresh >"$TEST_DIR/cat-list.json" 2>"$TEST_DIR/cat-list.err"
[ ! -s "$TEST_DIR/cat-list.err" ]
python3 - "$TEST_DIR/cat-list.json" "$CONFIG_REPO_ROOT" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["schema_version"] == 1
assert data["command"] == "catalog-list"
assert data["ready"] is True
assert data["status"] == "ready"
assert data["config_checkout"]["path"] == sys.argv[2]
assert data["config_checkout"]["refresh_status"] == "not_attempted"
assert "setup" in data["alias_resolution"]
assert "adopt" in data["alias_resolution"]

by_name = {item["name"]: item for item in data["apps"]}
vscode = by_name["vscode"]
assert vscode["kind"] == "signed-cask"
assert vscode["token"] == "visual-studio-code"
assert vscode["command"] == "code"
assert vscode["adoptable"] is True
assert "visual-studio-code" in vscode["aliases"]
assert "setup-vscode" in vscode["aliases"]
assert "visual-studio-code" in vscode["adopt_aliases"]

grok = by_name["grok-build"]
assert grok["installed"] is True
assert grok["status"] == "installed"
assert "grok" in grok["aliases"]

codex = by_name["codex"]
assert codex["declined"] is True
assert codex["status"] == "declined"
assert "codex-cli" in codex["aliases"]
assert "setup-codex-cli" in codex["aliases"]

opt = by_name["optional-app"]
assert opt["auto"] is False
assert opt["command"] == "minikube"

scripts_by_name = {item["name"]: item for item in data["setup_scripts"]}
assert "bin" in scripts_by_name
assert scripts_by_name["bin"]["kind"] == "script"
assert "setup-bin" in scripts_by_name["bin"]["aliases"]
PY

# catalog resolve resolves canonical names, cask tokens, aliases, wrapper scripts, and setup- prefixes.
for pair in \
    'visual-studio-code:vscode:cask_token:true' \
    'setup-vscode:vscode:alias:true' \
    'grok:grok-build:alias:false' \
    'codex-cli:codex:alias:false' \
    'setup-bin:bin:alias:false'; do
    IFS=':' read -r query expected_name expected_via expected_adopt <<<"$pair"
    HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
        /bin/bash "$ROOT/scripts/catalog" resolve "$query" --json --no-refresh >"$TEST_DIR/resolve.json"
    python3 - "$TEST_DIR/resolve.json" "$query" "$expected_name" "$expected_via" "$expected_adopt" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
query, expected_name, expected_via, expected_adopt = sys.argv[2:]
assert data["command"] == "catalog-resolve"
assert data["requested"] == query
assert data["resolved_name"] == expected_name, (data["resolved_name"], expected_name)
assert data["matched_via"] == expected_via, (data["matched_via"], expected_via)
assert data["setup_supported"] is True
assert data["adopt_supported"] is (expected_adopt == "true")
PY
done

# Error envelopes on unknown, invalid, and retired names, including evidence.
for err_case in \
    'missing:catalog-name-unknown' \
    '../bad:catalog-name-invalid' \
    'agent-bot:catalog-name-retired'; do
    IFS=':' read -r query expected_code <<<"$err_case"
    if HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
        /bin/bash "$ROOT/scripts/catalog" resolve "$query" --json --no-refresh >"$TEST_DIR/err.json" 2>"$TEST_DIR/err.stderr"; then
        echo "expected catalog resolve $query --json to fail" >&2
        exit 1
    fi
    [ ! -s "$TEST_DIR/err.stderr" ]
    python3 - "$TEST_DIR/err.json" "$expected_code" "$query" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["status"] == "not_ready"
assert data["error"]["code"] == sys.argv[2]
assert data["error"]["evidence"]["requested"] == sys.argv[3]
assert data["checks"][0]["evidence"]["requested"] == sys.argv[3]
PY
done

# Invalid flags in both orders emit catalog-invalid-arguments.
for flag_order in 'list --bogus --json' 'list --json --bogus' 'resolve --bogus --json' 'resolve --json --bogus'; do
    # shellcheck disable=SC2086
    if HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
        /bin/bash "$ROOT/scripts/catalog" $flag_order >"$TEST_DIR/err.json" 2>"$TEST_DIR/err.stderr"; then
        echo "expected catalog $flag_order to fail" >&2
        exit 1
    fi
    [ ! -s "$TEST_DIR/err.stderr" ]
    python3 - "$TEST_DIR/err.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["error"]["code"] == "catalog-invalid-arguments"
PY
done

# Fresh-HOME and legacy-origin non-materialization tests:
# Even when a bundled seed exists under REPO_ROOT/managed-machine-config,
# catalog list/resolve must never materialize a persistent checkout or rewrite
# a legacy SSH origin on an existing checkout.
ISOLATED_ROOT="$TEST_DIR/isolated-root/libexec"
mkdir -p "$ISOLATED_ROOT"
ln -s "$ROOT/lib" "$ISOLATED_ROOT/lib"
ln -s "$ROOT/scripts" "$ISOLATED_ROOT/scripts"
for s in "$ROOT"/setup-*; do
    [[ -e "$s" ]] && ln -s "$s" "$ISOLATED_ROOT/$(basename "$s")"
done
git clone --quiet "$REMOTE" "$ISOLATED_ROOT/managed-machine-config"

FRESH_HOME="$TEST_DIR/fresh-home"
mkdir -p "$FRESH_HOME"
for subcmd in 'list --no-refresh --json' 'resolve vscode --no-refresh --json'; do
    # shellcheck disable=SC2086
    if env -u CONFIG_REPO_ROOT HOME="$FRESH_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
        MANAGED_MACHINE_CONFIG_REPO_URL="$REMOTE" \
        /bin/bash "$ISOLATED_ROOT/scripts/catalog" $subcmd >"$TEST_DIR/fresh.json" 2>"$TEST_DIR/fresh.err"; then
        echo "expected catalog $subcmd on fresh HOME without checkout to fail" >&2
        exit 1
    fi
    [ ! -s "$TEST_DIR/fresh.err" ]
    [ ! -e "$FRESH_HOME/.local/share/managed-machine" ]
    python3 - "$TEST_DIR/fresh.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is False
assert data["error"]["code"] == "catalog-unavailable"
PY
done

LEGACY_HOME="$TEST_DIR/legacy-home"
LEGACY_CHECKOUT="$LEGACY_HOME/.local/share/managed-machine/managed-machine-config"
mkdir -p "$(dirname "$LEGACY_CHECKOUT")"
git clone --quiet "$REMOTE" "$LEGACY_CHECKOUT"
git -C "$LEGACY_CHECKOUT" remote set-url origin 'git@github.com:qwts/managed-machine-config.git'
env -u CONFIG_REPO_ROOT HOME="$LEGACY_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    MANAGED_MACHINE_CONFIG_REPO_URL='https://github.com/qwts/managed-machine-config.git' \
    /bin/bash "$ISOLATED_ROOT/scripts/catalog" list --no-refresh --json >"$TEST_DIR/legacy.json" 2>"$TEST_DIR/legacy.err"
[ ! -s "$TEST_DIR/legacy.err" ]
[[ "$(git -C "$LEGACY_CHECKOUT" remote get-url origin)" == 'git@github.com:qwts/managed-machine-config.git' ]]
python3 - "$TEST_DIR/legacy.json" "$LEGACY_CHECKOUT" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
assert data["ready"] is True
assert data["config_checkout"]["path"] == sys.argv[2]
assert data["config_checkout"]["source"] == "persistent_checkout"
PY

# SIGPIPE tolerance on truncated pipelines.
HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    /bin/bash "$ROOT/scripts/catalog" list --no-refresh | head -n 1 >/dev/null
HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    /bin/bash "$ROOT/scripts/catalog" list --json --no-refresh | head -n 1 >/dev/null

echo 'catalog tests passed'

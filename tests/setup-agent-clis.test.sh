#!/usr/bin/env bash
# Official curl|bash CLIs: install once, skip PATH edits, no-op when present.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
CURL_LOG="$TEST_ROOT/curl.log"
INSTALL_LOG="$TEST_ROOT/install.log"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"
CONFIG_REPO="$TEST_ROOT/managed-machine-config"
mkdir -p "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

write_curl_stub() {
    local dest_rel="$1"
    local cmd="$2"
    cat >"$TEST_BIN/curl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >>'$CURL_LOG'
cat <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
printf 'url-args=%s\n' "\$*" >>'$INSTALL_LOG'
printf 'CODEX_NON_INTERACTIVE=%s\n' "\${CODEX_NON_INTERACTIVE-}" >>'$INSTALL_LOG'
mkdir -p "\$HOME/$(dirname "$dest_rel")"
cat >"\$HOME/$dest_rel" <<'BIN'
#!/usr/bin/env bash
echo '$cmd 0.1.0-test'
BIN
chmod +x "\$HOME/$dest_rel"
INSTALLER
EOF
    chmod +x "$TEST_BIN/curl"
}

run_setup() {
    local script="$1"
    HOME="$TEST_HOME" PATH="$TEST_BIN:/usr/bin:/bin" \
        CONFIG_REPO_ROOT="$CONFIG_REPO" \
        /bin/bash "$ROOT/$script"
}

assert_install() {
    local script="$1"
    local url="$2"
    local cmd="$3"
    local dest="$4"
    : >"$CURL_LOG"
    : >"$INSTALL_LOG"
    rm -rf "$TEST_HOME/.local" "$TEST_HOME/.opencode" "$TEST_HOME/.zshrc"
    run_setup "$script" >"$TEST_ROOT/out"
    grep -Fq "$url" "$CURL_LOG"
    [[ -x "$dest" ]]
    grep -Fq "$cmd 0.1.0-test" "$TEST_ROOT/out"
    grep -Fq '# BEGIN local-bin' "$TEST_HOME/.zshrc"
    ! grep -Fq '>>> Codex installer >>>' "$TEST_HOME/.zshrc"
    ! grep -Fq '# opencode' "$TEST_HOME/.zshrc"

    : >"$CURL_LOG"
    HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
        CONFIG_REPO_ROOT="$CONFIG_REPO" \
        /bin/bash "$ROOT/$script" >"$TEST_ROOT/rerun.out"
    [[ ! -s "$CURL_LOG" ]]
    grep -Fq 'already installed' "$TEST_ROOT/rerun.out"
}

write_curl_stub '.local/bin/claude' claude
assert_install setup-claude 'https://claude.ai/install.sh' claude "$TEST_HOME/.local/bin/claude"

write_curl_stub '.local/bin/codex' codex
assert_install setup-codex-cli 'https://chatgpt.com/codex/install.sh' codex "$TEST_HOME/.local/bin/codex"
grep -Fxq 'CODEX_NON_INTERACTIVE=1' "$INSTALL_LOG"

write_curl_stub '.local/bin/agy' agy
assert_install setup-antigravity 'https://antigravity.google/cli/install.sh' agy "$TEST_HOME/.local/bin/agy"
! grep -Fq -- '--skip-path' "$INSTALL_LOG"

write_curl_stub '.local/bin/grok' grok
assert_install setup-grok-build 'https://x.ai/cli/install.sh' grok "$TEST_HOME/.local/bin/grok"

write_curl_stub '.opencode/bin/opencode' opencode
assert_install setup-opencode 'https://opencode.ai/install' opencode "$TEST_HOME/.local/bin/opencode"
[[ -L "$TEST_HOME/.local/bin/opencode" ]]
[[ "$(readlink "$TEST_HOME/.local/bin/opencode")" == "$TEST_HOME/.opencode/bin/opencode" ]]
grep -Fq -- '--no-modify-path' "$INSTALL_LOG"

# A user-managed ~/.local/bin/opencode is left alone even if a stale
# ~/.opencode/bin payload is also present. Replace the managed symlink
# first so we do not write through it into the payload.
mkdir -p "$TEST_HOME/.local/bin" "$TEST_HOME/.opencode/bin"
rm -f "$TEST_HOME/.local/bin/opencode"
cat >"$TEST_HOME/.local/bin/opencode" <<'EOF'
#!/usr/bin/env bash
echo 'opencode user-managed'
EOF
cat >"$TEST_HOME/.opencode/bin/opencode" <<'EOF'
#!/usr/bin/env bash
echo 'opencode stale-payload'
EOF
chmod +x "$TEST_HOME/.local/bin/opencode" "$TEST_HOME/.opencode/bin/opencode"
: >"$CURL_LOG"
HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-opencode" >"$TEST_ROOT/user-opencode.out"
[[ ! -s "$CURL_LOG" ]]
[[ ! -L "$TEST_HOME/.local/bin/opencode" ]]
grep -Fq 'already installed' "$TEST_ROOT/user-opencode.out"
grep -Fq 'opencode user-managed' "$TEST_ROOT/user-opencode.out"
! grep -Fq 'opencode stale-payload' "$TEST_ROOT/user-opencode.out"

# Malformed catalog args must fail the install, not be ignored.
python3 -c '
import json, sys
path = sys.argv[1]
with open(path) as fh:
    data = json.load(fh)
for app in data["apps"]:
    if app.get("name") == "antigravity":
        app["args"] = "--skip-path"
        break
with open(path, "w") as fh:
    json.dump(data, fh)
' "$CONFIG_REPO/apps.json"
if HOME="$TEST_HOME" PATH="$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-antigravity" >"$TEST_ROOT/bad-args.out" 2>"$TEST_ROOT/bad-args.err"; then
    echo 'expected malformed antigravity args to fail the install' >&2
    exit 1
fi
grep -Fq 'args must be an array' "$TEST_ROOT/bad-args.err"

ACCOUNT_HOME="$TEST_ROOT/account-home"
mkdir -p "$ACCOUNT_HOME" "$TEST_BIN"
printf '#!/bin/sh\necho shared-claude\n' >"$TEST_BIN/claude"
chmod +x "$TEST_BIN/claude"
write_curl_stub '.local/bin/claude' claude
: >"$CURL_LOG"
HOME="$ACCOUNT_HOME" PATH="$TEST_BIN:/usr/bin:/bin" MANAGED_MACHINE_ACCOUNT_SETUP=1 \
    CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-claude" >"$TEST_ROOT/account.out"
[[ -s "$CURL_LOG" && -x "$ACCOUNT_HOME/.local/bin/claude" ]]
grep -Fq 'claude 0.1.0-test' "$TEST_ROOT/account.out"
: >"$CURL_LOG"
HOME="$ACCOUNT_HOME" PATH="$TEST_BIN:/usr/bin:/bin" MANAGED_MACHINE_ACCOUNT_SETUP=1 \
    CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-claude" >"$TEST_ROOT/account-rerun.out"
[[ ! -s "$CURL_LOG" ]]
grep -Fq 'already installed' "$TEST_ROOT/account-rerun.out"
mkdir -p "$CONFIG_REPO/config"
printf '#!/bin/sh\nprintf ran > "%s"\n' "$TEST_ROOT/human-config-ran" >"$CONFIG_REPO/config/claude"
chmod +x "$CONFIG_REPO/config/claude"
HOME="$ACCOUNT_HOME" PATH="$TEST_BIN:/usr/bin:/bin" MANAGED_MACHINE_ACCOUNT_SETUP=1 \
    CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-claude" >/dev/null
[[ ! -e "$TEST_ROOT/human-config-ran" ]]
HOME="$ACCOUNT_HOME" PATH="$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-claude" >/dev/null
[[ -e "$TEST_ROOT/human-config-ran" ]]
python3 - "$CONFIG_REPO/apps.json" "$TEST_ROOT/foreign-home" <<'PY'
import json, sys
path, foreign = sys.argv[1:]
with open(path) as handle:
    data = json.load(handle)
for app in data['apps']:
    if app['name'] == 'claude':
        app['env'] = {'HOME': foreign}
with open(path, 'w') as handle:
    json.dump(data, handle)
PY
if HOME="$ACCOUNT_HOME" PATH="$TEST_BIN:/usr/bin:/bin" MANAGED_MACHINE_ACCOUNT_SETUP=1 \
    CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-claude" >"$TEST_ROOT/foreign.out" 2>"$TEST_ROOT/foreign.err"; then
    echo 'expected account environment override to fail' >&2
    exit 1
fi
[[ ! -e "$TEST_ROOT/foreign-home" ]]
grep -Fq 'catalog environment cannot override account identity' "$TEST_ROOT/foreign.err"

# Vendor bin_dir: kilocode's installer drops the binary into ~/.kilo/bin,
# which is off the managed PATH. The bin_dir catalog field links it into
# ~/.local/bin automatically without re-downloading on a re-run.
write_curl_stub '.kilo/bin/kilo' kilo
: >"$CURL_LOG"
: >"$TEST_ROOT/kilo.out"
HOME="$TEST_HOME" PATH="$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash -c '
set -euo pipefail
ROOT="'"$ROOT"'"
source "$ROOT/lib/install.sh"
source "$ROOT/lib/apps.sh"
install_catalog_app kilocode
' >"$TEST_ROOT/kilo.out" 2>"$TEST_ROOT/kilo.err"
[[ -L "$TEST_HOME/.local/bin/kilo" ]]
[[ "$(readlink "$TEST_HOME/.local/bin/kilo")" == "$TEST_HOME/.kilo/bin/kilo" ]]
grep -Fq 'kilo 0.1.0-test' "$TEST_ROOT/kilo.out"
grep -Fq '# BEGIN local-bin' "$TEST_HOME/.zshrc"
! grep -Fq 'vendor symlink' "$TEST_ROOT/kilo.err"

# Re-run finds the vendor symlink and skips the download entirely.
: >"$CURL_LOG"
HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash -c '
set -euo pipefail
ROOT="'"$ROOT"'"
source "$ROOT/lib/install.sh"
source "$ROOT/lib/apps.sh"
install_catalog_app kilocode
' >"$TEST_ROOT/kilo-rerun.out"
[[ ! -s "$CURL_LOG" ]]
grep -Fq 'already installed' "$TEST_ROOT/kilo-rerun.out"
grep -Fq 'kilo 0.1.0-test' "$TEST_ROOT/kilo-rerun.out"

# Account setup accepts the linked vendor bin only when it resolves and passes
# the account home/ownership check: the managed ~/.local/bin is prepended, so
# the in-home symlink beats a foreign kilo earlier on the passed PATH.
KILO_ACCOUNT_HOME="$TEST_ROOT/kilo-account-home"
mkdir -p "$KILO_ACCOUNT_HOME" "$TEST_BIN"
printf '#!/bin/sh\necho foreign-kilo\n' >"$TEST_BIN/kilo"
chmod +x "$TEST_BIN/kilo"
write_curl_stub '.kilo/bin/kilo' kilo
: >"$CURL_LOG"
HOME="$KILO_ACCOUNT_HOME" PATH="$TEST_BIN:/usr/bin:/bin" MANAGED_MACHINE_ACCOUNT_SETUP=1 \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash -c '
set -euo pipefail
ROOT="'"$ROOT"'"
source "$ROOT/lib/install.sh"
source "$ROOT/lib/apps.sh"
install_catalog_app kilocode
' >"$TEST_ROOT/kilo-account.out" 2>"$TEST_ROOT/kilo-account.err"
[[ -s "$CURL_LOG" ]]
[[ -L "$KILO_ACCOUNT_HOME/.local/bin/kilo" ]]
[[ "$(readlink "$KILO_ACCOUNT_HOME/.local/bin/kilo")" == "$KILO_ACCOUNT_HOME/.kilo/bin/kilo" ]]
grep -Fq 'kilo 0.1.0-test' "$TEST_ROOT/kilo-account.out"
! grep -Fq 'foreign-kilo' "$TEST_ROOT/kilo-account.out"

# A user-managed binary at the managed link resolves on PATH and passes the
# account check, so account setup reports it as already installed and leaves
# it alone.
rm -rf "$KILO_ACCOUNT_HOME/.local"
mkdir -p "$KILO_ACCOUNT_HOME/.local/bin"
printf '#!/bin/sh\necho user-managed-kilo\n' >"$KILO_ACCOUNT_HOME/.local/bin/kilo"
chmod +x "$KILO_ACCOUNT_HOME/.local/bin/kilo"
: >"$CURL_LOG"
HOME="$KILO_ACCOUNT_HOME" PATH="$TEST_BIN:/usr/bin:/bin" MANAGED_MACHINE_ACCOUNT_SETUP=1 \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash -c '
set -euo pipefail
ROOT="'"$ROOT"'"
source "$ROOT/lib/install.sh"
source "$ROOT/lib/apps.sh"
install_catalog_app kilocode
' >"$TEST_ROOT/kilo-user.out" 2>"$TEST_ROOT/kilo-user.err"
[[ ! -s "$CURL_LOG" ]]
grep -Fq 'already installed' "$TEST_ROOT/kilo-user.out"
grep -Fq 'user-managed-kilo' "$TEST_ROOT/kilo-user.out"
! grep -Fq 'foreign-kilo' "$TEST_ROOT/kilo-user.out"


echo 'setup-agent-clis tests passed'

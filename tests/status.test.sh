#!/usr/bin/env bash
# Read-only status inventory: versions, pins, bootstrap outcomes; never writes.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$CONFIG_REPO"

run_status() {
    HOME="$TEST_HOME" \
    NVM_DIR="$TEST_HOME/.nvm" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$TEST_DIR/system-apps" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/scripts/status" "$@"
}

snapshot_home() {
    # Status must not create or mutate files under HOME or the config checkout.
    find "$TEST_HOME" "$CONFIG_REPO" -print | LC_ALL=C sort
}

# 1. Empty machine: missing rows, checkout formula version, exit 0, no writes.
BEFORE="$(snapshot_home)"
run_status >"$TEST_DIR/empty.out"
AFTER="$(snapshot_home)"
[[ "$BEFORE" == "$AFTER" ]]
grep -qE '^managed-machine +0\.3\.[0-9]+( \(checkout\))?$' "$TEST_DIR/empty.out"
grep -qE '^machine +missing$' "$TEST_DIR/empty.out"
grep -qE '^bootstrap +missing$' "$TEST_DIR/empty.out"
grep -qE '^local-bin +missing$' "$TEST_DIR/empty.out"
grep -qE '^  pin +missing$' "$TEST_DIR/empty.out"
grep -qE '^proton-pass +missing$' "$TEST_DIR/empty.out"
grep -qE '^devin +missing$' "$TEST_DIR/empty.out"
grep -qE '^lm-studio +missing$' "$TEST_DIR/empty.out"
! grep -q 'ssh-rsa' "$TEST_DIR/empty.out"
! grep -q 'SECRET' "$TEST_DIR/empty.out"

# 2. Unknown option fails; --help does not.
if run_status --nope >"$TEST_DIR/bad.out" 2>&1; then
    echo 'expected unknown status option to fail' >&2
    exit 1
fi
grep -Fq 'unknown status option: --nope' "$TEST_DIR/bad.out"
run_status --help >"$TEST_DIR/help.out"
grep -Fq 'managed-machine status' "$TEST_DIR/help.out"

# 3. Populated manifests + stubs: versions, pin, bootstrap, machine; no secrets.
mkdir -p "$TEST_HOME/.config/managed-machine" "$TEST_HOME/Applications/LM Studio.app"
cat >"$TEST_HOME/.config/managed-machine/machine.toml" <<'EOF'
schema_version = 1
machine_id = "sha256-testhostid"
hostname = "macbookairm4"
public_key = "ssh-rsa AAAASECRETKEYMATERIAL"
EOF
cat >"$TEST_HOME/.config/managed-machine/bootstrap.manifest" <<'EOF'
schema_version=1
mode=interactive
started_at=2026-08-13T14:00:00Z
complete	setup-brew	
complete	setup-zsh	
deferred	setup-codex	manually merge fragment
failed	setup-bin	exit status 1
finished_at=2026-08-13T14:32:00Z
EOF
cat >"$TEST_HOME/.config/managed-machine/local-bin.manifest" <<'EOF'
schema_version=1
ref=main
commit=2e637164875f89107f10c0d4b1ef568324e783d8
recorded_at=2026-08-13T14:32:00Z
EOF
printf 'main\n' >"$CONFIG_REPO/local-bin.ref"

cat >"$TEST_BIN/brew" <<'EOF'
#!/usr/bin/env bash
case "$1" in
    --version) echo 'Homebrew 4.4.0' ;;
    list)
        if [[ "${2:-}" == '--cask' && "${3:-}" == '--versions' ]]; then
            echo 'lm-studio 0.3.22'
        elif [[ "${2:-}" == '--versions' && "${3:-}" == 'managed-machine' ]]; then
            echo 'managed-machine 0.3.4'
        else
            exit 1
        fi
        ;;
    *) exit 1 ;;
esac
EOF
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
echo 'gh version 2.74.0 (2026-01-01)'
echo 'https://github.com/cli/cli/releases/tag/v2.74.0'
EOF
cat >"$TEST_BIN/pass-cli" <<'EOF'
#!/usr/bin/env bash
echo 'Proton Pass CLI 2.2.4 (84323b8)'
EOF
cat >"$TEST_BIN/devin" <<'EOF'
#!/usr/bin/env bash
echo 'devin 3000.3.27 (0becb483)'
EOF
cat >"$TEST_BIN/node" <<'EOF'
#!/usr/bin/env bash
echo 'v22.0.0'
EOF
cat >"$TEST_BIN/rustup" <<'EOF'
#!/usr/bin/env bash
echo 'rustup 1.28.2 (0000000 2026-01-01)'
EOF
cat >"$TEST_BIN/rustc" <<'EOF'
#!/usr/bin/env bash
echo 'rustc 1.89.0 (29483883e 2025-08-04)'
EOF
cat >"$TEST_BIN/cargo" <<'EOF'
#!/usr/bin/env bash
echo 'cargo 1.89.0 (c24e10642 2025-06-23)'
EOF
chmod +x "$TEST_BIN"/*

mkdir -p "$TEST_HOME/.nvm"
cat >"$TEST_HOME/.nvm/nvm.sh" <<'EOF'
nvm() {
    if [[ "${1:-}" == '--version' ]]; then
        echo '0.40.4'
    fi
}
node() {
    if [[ "${1:-}" == '--version' ]]; then
        echo 'v22.0.0'
    fi
}
EOF

BEFORE="$(snapshot_home)"
HOME="$TEST_HOME" \
NVM_DIR="$TEST_HOME/.nvm" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
MANAGED_MACHINE_SYSTEM_APPDIR="$TEST_DIR/system-apps" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/scripts/status" >"$TEST_DIR/full.out"
AFTER="$(snapshot_home)"
[[ "$BEFORE" == "$AFTER" ]]

grep -qE '^managed-machine +0\.3\.4$' "$TEST_DIR/full.out"
grep -qE '^machine +sha256-testhostid \(macbookairm4\)$' "$TEST_DIR/full.out"
grep -qE '^bootstrap +interactive  2026-08-13T14:32:00Z$' "$TEST_DIR/full.out"
grep -qE '^  complete +setup-brew, setup-zsh$' "$TEST_DIR/full.out"
grep -qE '^  deferred +setup-codex$' "$TEST_DIR/full.out"
grep -qE '^  failed +setup-bin$' "$TEST_DIR/full.out"
grep -qE '^local-bin +2e637164875f89107f10c0d4b1ef568324e783d8$' "$TEST_DIR/full.out"
grep -qE '^  pin +main \(moving branch\)$' "$TEST_DIR/full.out"
grep -qE '^homebrew +Homebrew 4\.4\.0$' "$TEST_DIR/full.out"
grep -qE '^gh +gh version 2\.74\.0 \(2026-01-01\)$' "$TEST_DIR/full.out"
grep -qE '^nvm +0\.40\.4$' "$TEST_DIR/full.out"
grep -qE '^node +v22\.0\.0$' "$TEST_DIR/full.out"
grep -qE '^proton-pass +Proton Pass CLI 2\.2\.4 \(84323b8\)$' "$TEST_DIR/full.out"
grep -qE '^devin +devin 3000\.3\.27 \(0becb483\)$' "$TEST_DIR/full.out"
grep -qE '^lm-studio +0\.3\.22 \(.*/Applications/LM Studio.app\)$' "$TEST_DIR/full.out"
grep -qE '^rustup +rustup 1\.28\.2' "$TEST_DIR/full.out"
grep -qE '^rustc +rustc 1\.89\.0' "$TEST_DIR/full.out"
grep -qE '^cargo +cargo 1\.89\.0' "$TEST_DIR/full.out"
! grep -q 'AAAASECRETKEYMATERIAL' "$TEST_DIR/full.out"
! grep -q 'public_key' "$TEST_DIR/full.out"
! grep -q 'manually merge fragment' "$TEST_DIR/full.out"

# 4. Immutable installed pin is not annotated as a moving branch.
cat >"$TEST_HOME/.config/managed-machine/local-bin.manifest" <<'EOF'
schema_version=1
ref=v0.2.0
commit=2e637164875f89107f10c0d4b1ef568324e783d8
recorded_at=2026-08-13T14:32:00Z
EOF
printf 'v0.2.0\n' >"$CONFIG_REPO/local-bin.ref"
HOME="$TEST_HOME" \
NVM_DIR="$TEST_HOME/.nvm" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
MANAGED_MACHINE_SYSTEM_APPDIR="$TEST_DIR/system-apps" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/scripts/status" >"$TEST_DIR/pin.out"
grep -qE '^  pin +v0\.2\.0$' "$TEST_DIR/pin.out"
! grep -Fq 'moving branch' "$TEST_DIR/pin.out"
! grep -qE '^  configured ' "$TEST_DIR/pin.out"

# 5. Installed override ref stays with the commit; configured pin is separate.
cat >"$TEST_HOME/.config/managed-machine/local-bin.manifest" <<'EOF'
schema_version=1
ref=v0.2.0
commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
recorded_at=2026-08-13T14:32:00Z
EOF
printf 'v0.1.0\n' >"$CONFIG_REPO/local-bin.ref"
HOME="$TEST_HOME" \
NVM_DIR="$TEST_HOME/.nvm" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
MANAGED_MACHINE_SYSTEM_APPDIR="$TEST_DIR/system-apps" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/scripts/status" >"$TEST_DIR/override.out"
grep -qE '^local-bin +aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa$' "$TEST_DIR/override.out"
grep -qE '^  pin +v0\.2\.0$' "$TEST_DIR/override.out"
grep -qE '^  configured +v0\.1\.0$' "$TEST_DIR/override.out"
! grep -Fq 'moving branch' "$TEST_DIR/override.out"

# 6. CLI dispatches to scripts/status.
mkdir -p "$TEST_DIR/fixture/bin" "$TEST_DIR/fixture/lib" "$TEST_DIR/fixture/scripts"
cp "$ROOT/bin/managed-machine" "$TEST_DIR/fixture/bin/managed-machine"
chmod +x "$TEST_DIR/fixture/bin/managed-machine"
touch "$TEST_DIR/fixture/lib/install.sh"
cat >"$TEST_DIR/fixture/scripts/status" <<EOF
#!/usr/bin/env bash
printf 'status-ran\n'
printf '%s\n' "\$*"
EOF
chmod +x "$TEST_DIR/fixture/scripts/status"
"$TEST_DIR/fixture/bin/managed-machine" status >"$TEST_DIR/cli.out"
[[ "$(sed -n '1p' "$TEST_DIR/cli.out")" == 'status-ran' ]]

echo 'status tests passed'

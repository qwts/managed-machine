#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME"

HOME="$TEST_HOME"
# shellcheck source=../lib/install.sh
source "$ROOT/lib/install.sh"

ZSHRC="$TEST_HOME/.zshrc"

# A pre-existing unguarded local-bin block is upgraded in place, not stacked.
printf '%s\nexport PATH="${HOME}/.local/bin:${PATH}"\n%s\n' \
    '# BEGIN local-bin' '# END local-bin' >"$ZSHRC"
ensure_local_bin_in_zshrc "$ZSHRC" >/dev/null
[[ "$(grep -c '^# BEGIN local-bin$' "$ZSHRC")" == "1" ]]
! grep -qxF 'export PATH="${HOME}/.local/bin:${PATH}"' "$ZSHRC"

ensure_local_bin_in_zshrc "$ZSHRC" >/dev/null
ensure_cargo_bin_in_zshrc "$ZSHRC" >/dev/null
ensure_nvm_in_zshrc "$ZSHRC" >/dev/null

# Each managed block only prepends when the entry is missing from PATH.
grep -qxF 'case ":${PATH}:" in' "$ZSHRC"
grep -qxF '    *":${HOME}/.local/bin:"*) ;;' "$ZSHRC"
grep -qxF '    *) export PATH="${HOME}/.local/bin:${PATH}" ;;' "$ZSHRC"
grep -qxF '    *":${CARGO_HOME:-${HOME}/.cargo}/bin:"*) ;;' "$ZSHRC"
grep -qxF '    *) export PATH="${CARGO_HOME:-${HOME}/.cargo}/bin:${PATH}" ;;' "$ZSHRC"
grep -qxF 'case "$(command -v node 2>/dev/null)" in' "$ZSHRC"
grep -qxF '    "${NVM_DIR}/versions/"*) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" --no-use ;;' "$ZSHRC"
grep -qxF '    *) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" ;;' "$ZSHRC"

# Block counts stay at one across re-runs.
ensure_local_bin_in_zshrc "$ZSHRC" >/dev/null
ensure_cargo_bin_in_zshrc "$ZSHRC" >/dev/null
ensure_nvm_in_zshrc "$ZSHRC" >/dev/null
[[ "$(grep -c '^# BEGIN local-bin$' "$ZSHRC")" == "1" ]]
[[ "$(grep -c '^# BEGIN rustup$' "$ZSHRC")" == "1" ]]
[[ "$(grep -c '^# BEGIN nvm$' "$ZSHRC")" == "1" ]]

if command -v zsh >/dev/null 2>&1; then
    zsh -n "$ZSHRC"

    # Stub nvm.sh that prepends like real nvm unless loaded with --no-use.
    mkdir -p "$TEST_HOME/.nvm/versions/node/v22.0.0/bin"
    cat >"$TEST_HOME/.nvm/nvm.sh" <<'EOF'
nvm() { :; }
if [[ "$1" != "--no-use" ]]; then
    export PATH="${NVM_DIR}/versions/node/v22.0.0/bin:${PATH}"
fi
EOF
    # command -v node must resolve for the guard to take the --no-use branch.
    printf '#!/usr/bin/env bash\n' >"$TEST_HOME/.nvm/versions/node/v22.0.0/bin/node"
    chmod +x "$TEST_HOME/.nvm/versions/node/v22.0.0/bin/node"

    # A nested shell inherits PATH and sources the same file again; every
    # managed entry must still appear exactly once.
    cat >"$TEST_DIR/nested.zsh" <<'EOF'
source "$TEST_HOME/.zshrc"
zsh -c 'source "$TEST_HOME/.zshrc"; print -r -- "$PATH"'
EOF

    env HOME="$TEST_HOME" TEST_HOME="$TEST_HOME" PATH="/usr/bin:/bin" \
        zsh "$TEST_DIR/nested.zsh" >"$TEST_DIR/nested.out"
    FINAL_PATH="$(cat "$TEST_DIR/nested.out")"

    count_entries() {
        tr ':' '\n' <<<"$FINAL_PATH" | grep -xcF "$1" || true
    }
    [[ "$(count_entries "$TEST_HOME/.local/bin")" == "1" ]]
    [[ "$(count_entries "$TEST_HOME/.cargo/bin")" == "1" ]]
    [[ "$(count_entries "$TEST_HOME/.nvm/versions/node/v22.0.0/bin")" == "1" ]]

    # An inherited PATH with a system node ahead of the nvm entry must still
    # activate nvm normally so the configured version wins (PR #48 review).
    SYSBIN="$TEST_DIR/sysbin"
    mkdir -p "$SYSBIN"
    printf '#!/usr/bin/env bash\n' >"$SYSBIN/node"
    chmod +x "$SYSBIN/node"
    env HOME="$TEST_HOME" TEST_HOME="$TEST_HOME" \
        PATH="$SYSBIN:$TEST_HOME/.nvm/versions/node/v22.0.0/bin:/usr/bin:/bin" \
        zsh -c 'source "$TEST_HOME/.zshrc"; command -v node' >"$TEST_DIR/stale.out"
    [[ "$(cat "$TEST_DIR/stale.out")" == "$TEST_HOME/.nvm/versions/node/v22.0.0/bin/node" ]]
fi

echo "zshrc-path-guards tests passed"

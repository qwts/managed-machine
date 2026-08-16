#!/usr/bin/env bash
# setup-zsh writes guarded PATH blocks and backs up stale profiles.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$CONFIG_REPO_ROOT/dotfiles/zsh" "$CONFIG_REPO_ROOT/config"
git init --quiet "$CONFIG_REPO_ROOT"
git -C "$CONFIG_REPO_ROOT" config user.name 'managed-machine test'
git -C "$CONFIG_REPO_ROOT" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO_ROOT" config commit.gpgsign false

cp "$ROOT/tests/fixtures/config-zsh" "$CONFIG_REPO_ROOT/config/zsh"
chmod +x "$CONFIG_REPO_ROOT/config/zsh"

cat >"$CONFIG_REPO_ROOT/dotfiles/zsh/.zshenv" <<'EOF'
# Managed by managed-machine/setup-zsh.
# Env for all zsh invocations. Keep minimal.
EOF
cat >"$CONFIG_REPO_ROOT/dotfiles/zsh/.zprofile" <<'EOF'
# Managed by managed-machine/setup-zsh.
# Login-shell settings go here.
EOF
cat >"$CONFIG_REPO_ROOT/dotfiles/zsh/.zshrc" <<'EOF'
# Managed by managed-machine/setup-zsh.
# Interactive shell settings go here.

# BEGIN local-bin
export PATH="${HOME}/.local/bin:${PATH}"
# END local-bin
EOF

run_setup() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    MANAGED_MACHINE_ROOT="$ROOT" \
    PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-zsh"
}

# 1. Missing files: install, and the template's unguarded local-bin is rewritten.
run_setup >"$TEST_DIR/fresh.out"
grep -Fq 'installed: .zshrc' "$TEST_DIR/fresh.out"
[[ -f "$TEST_HOME/.zshrc" && -f "$TEST_HOME/.zprofile" && -f "$TEST_HOME/.zshenv" ]]
grep -qxF '    *) export PATH="${HOME}/.local/bin:${PATH}" ;;' "$TEST_HOME/.zshrc"
! grep -qxF 'export PATH="${HOME}/.local/bin:${PATH}"' "$TEST_HOME/.zshrc"
[[ "$(echo "$TEST_HOME"/.zshrc.*.bak)" == "$TEST_HOME/.zshrc.*.bak" ]]

# 2. Re-run on guarded files is a no-op (no new backups).
run_setup >"$TEST_DIR/rerun.out"
grep -Fq 'already current: .zshrc' "$TEST_DIR/rerun.out"
[[ "$(echo "$TEST_HOME"/.zshrc.*.bak)" == "$TEST_HOME/.zshrc.*.bak" ]]

# 3. Unguarded PATH + vendor installer lines: backup then rewrite.
cat >"$TEST_HOME/.zshrc" <<'EOF'
# Managed by managed-machine/setup-zsh.

# Added by Antigravity CLI installer
export PATH="/Users/user/.local/bin:$PATH"

# BEGIN nvm
export NVM_DIR="${NVM_DIR:-${HOME}/.nvm}"
[ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
# END nvm

# BEGIN rustup
export PATH="${CARGO_HOME:-${HOME}/.cargo}/bin:${PATH}"
# END rustup

# BEGIN local-bin
export PATH="${HOME}/.local/bin:${PATH}"
# END local-bin
EOF
cat >"$TEST_HOME/.zprofile" <<'EOF'
# Managed by managed-machine/setup-zsh.
eval "$(/opt/homebrew/bin/brew shellenv)"

# Added by Antigravity CLI installer
export PATH="/Users/user/.local/bin:$PATH"
EOF
cat >"$TEST_HOME/.zshenv" <<'EOF'
# Managed by managed-machine/setup-zsh.
. "$HOME/.cargo/env"
export PATH="${HOME}/.local/bin:${PATH}"
EOF

run_setup >"$TEST_DIR/refresh.out"
grep -Fq 'backed up .zshrc' "$TEST_DIR/refresh.out"
grep -Fq 'backed up .zprofile' "$TEST_DIR/refresh.out"
grep -Fq 'backed up .zshenv' "$TEST_DIR/refresh.out"
zshrc_bak="$(echo "$TEST_HOME"/.zshrc.*.bak)"
[[ -f "$zshrc_bak" ]]
grep -Fq 'Added by Antigravity CLI installer' "$zshrc_bak"
! grep -Fq 'Added by Antigravity CLI installer' "$TEST_HOME/.zshrc"
! grep -Fq 'Added by Antigravity CLI installer' "$TEST_HOME/.zprofile"
! grep -qxF 'export PATH="${HOME}/.local/bin:${PATH}"' "$TEST_HOME/.zshrc"
grep -qxF '    *) export PATH="${HOME}/.local/bin:${PATH}" ;;' "$TEST_HOME/.zshrc"
grep -Fq 'brew shellenv' "$TEST_HOME/.zprofile"
grep -Fq '.cargo/env' "$TEST_HOME/.zshenv"

# 4. nvm.sh present: restore the guarded nvm block after a refresh.
mkdir -p "$TEST_HOME/.nvm"
printf '# nvm stub\n' >"$TEST_HOME/.nvm/nvm.sh"
cat >"$TEST_HOME/.zshrc" <<'EOF'
# BEGIN nvm
export NVM_DIR="${NVM_DIR:-${HOME}/.nvm}"
[ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
# END nvm
export PATH="${HOME}/.local/bin:${PATH}"
EOF
run_setup >"$TEST_DIR/nvm.out"
grep -qxF '    "${NVM_DIR}/versions/"*) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" --no-use ;;' "$TEST_HOME/.zshrc"

# 5. A clean custom file without unmanaged PATH lines is left in place.
printf 'echo custom\n' >"$TEST_HOME/.zprofile"
run_setup >"$TEST_DIR/custom.out"
grep -Fq 'already current: .zprofile' "$TEST_DIR/custom.out"
grep -qxF 'echo custom' "$TEST_HOME/.zprofile"

# 6. A vendor installer comment without a PATH mutation is left in place.
zprofile_baks_before="$(echo "$TEST_HOME"/.zprofile.*.bak)"
cat >"$TEST_HOME/.zprofile" <<'EOF'
# Added by Some CLI installer
alias agy-help='agy --help'
EOF
run_setup >"$TEST_DIR/comment-only.out"
grep -Fq 'already current: .zprofile' "$TEST_DIR/comment-only.out"
grep -qxF 'alias agy-help='\''agy --help'\''' "$TEST_HOME/.zprofile"
[[ "$(echo "$TEST_HOME"/.zprofile.*.bak)" == "$zprofile_baks_before" ]]

echo 'setup-zsh tests passed'

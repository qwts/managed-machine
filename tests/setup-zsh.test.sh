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

# 7. Fallback re-runs are byte-identical: blank rot never grows.
printf '# Managed by managed-machine/setup-zsh.\n\n\n\n# BEGIN local-bin\ncase ":${PATH}:" in\n    *":${HOME}/.local/bin:"*) ;;\n    *) export PATH="${HOME}/.local/bin:${PATH}" ;;\nesac\n# END local-bin\n\n\n\nbottom\n\n\n' >"$TEST_HOME/.zshrc"
run_setup >"$TEST_DIR/rot1.out"
cp "$TEST_HOME/.zshrc" "$TEST_DIR/rot.before"
blanks_before="$(grep -c '^$' "$TEST_HOME/.zshrc")"
run_setup >"$TEST_DIR/rot2.out"
cmp -s "$TEST_DIR/rot.before" "$TEST_HOME/.zshrc" || { echo 'FAIL: re-run changed .zshrc' >&2; exit 1; }
[[ "$(grep -c '^$' "$TEST_HOME/.zshrc")" == "$blanks_before" ]] || { echo 'FAIL: blank lines grew' >&2; exit 1; }
grep -qxF '# BEGIN local-bin' "$TEST_HOME/.zshrc"
grep -qxF '# END local-bin' "$TEST_HOME/.zshrc"
grep -qxF '    *) export PATH="${HOME}/.local/bin:${PATH}" ;;' "$TEST_HOME/.zshrc"

# 8. zsh-profile on PATH is delegated to instead of the awk fallback.
mkdir -p "$TEST_DIR/fakebin"
cat >"$TEST_DIR/fakebin/zsh-profile" <<'EOF'
#!/usr/bin/env bash
# Test double: records ensure-block calls, replaces or appends the block.
set -euo pipefail
file=""; name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --file) file="$2"; shift 2 ;;
        --name) name="$2"; shift 2 ;;
        *) shift ;;
    esac
done
printf 'ensure-block --file %s --name %s\n' "$file" "$name" >>"${ZSH_PROFILE_LOG:-/dev/null}"
body_file="$(mktemp)"
cat >"$body_file"
awk -v b="# BEGIN $name" -v e="# END $name" -v bodyfile="$body_file" '
    BEGIN { nbody = 0; while ((getline l < bodyfile) > 0) body[nbody++] = l }
    $0 == b { skip = 1; found = 1; next }
    $0 == e { skip = 0; print b; for (i = 0; i < nbody; i++) print body[i]; print e; next }
    !skip { print }
    END { if (!found) { print ""; print b; for (i = 0; i < nbody; i++) print body[i]; print e } }
' "$file" >"$file.new"
rm -f "$body_file"
mv "$file.new" "$file"
EOF
chmod +x "$TEST_DIR/fakebin/zsh-profile"
export ZSH_PROFILE_LOG="$TEST_DIR/zsh-profile.log"
rm -f "$ZSH_PROFILE_LOG"
printf '# Managed by managed-machine/setup-zsh.\n# Interactive shell settings go here.\n' >"$TEST_HOME/.zshrc"
HOME="$TEST_HOME" \
CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
MANAGED_MACHINE_ROOT="$ROOT" \
PATH="$TEST_DIR/fakebin:/usr/bin:/bin" \
/bin/bash "$ROOT/setup-zsh" >"$TEST_DIR/delegate.out"
grep -Fq 'ensure-block' "$ZSH_PROFILE_LOG" || { echo 'FAIL: zsh-profile not delegated to' >&2; exit 1; }
grep -qxF '# BEGIN local-bin' "$TEST_HOME/.zshrc" || { echo 'FAIL: delegated block missing' >&2; exit 1; }
grep -qxF '# END local-bin' "$TEST_HOME/.zshrc" || { echo 'FAIL: delegated END missing' >&2; exit 1; }
unset ZSH_PROFILE_LOG

# 9. GNU stat ordering: a failing BSD probe must not poison chmod input.
mkdir -p "$TEST_DIR/gnubin"
cat >"$TEST_DIR/gnubin/stat" <<'EOF'
#!/usr/bin/env bash
# Test double mimicking GNU stat: -f prints filesystem blurbage to stdout
# before failing; -c delegates to the real BSD stat.
set -euo pipefail
if [[ "${1:-}" == "-f" ]]; then
    printf '  File: "%s"\n    Size: 0\tBlocks: 0\n' "${2:-}"
    exit 1
fi
if [[ "${1:-}" == "-c" ]]; then
    /usr/bin/stat -f %Lp "${@: -1}"
    exit $?
fi
exec /usr/bin/stat "$@"
EOF
chmod +x "$TEST_DIR/gnubin/stat"
printf 'plain\n' >"$TEST_HOME/.zshrc-gnu"
chmod 640 "$TEST_HOME/.zshrc-gnu"
HOME="$TEST_HOME" \
MANAGED_MACHINE_ROOT="$ROOT" \
PATH="$TEST_DIR/gnubin:/usr/bin:/bin" \
/bin/bash -c 'source "$0/lib/install.sh"; ensure_local_bin_in_zshrc "$1"' \
"$ROOT" "$TEST_HOME/.zshrc-gnu" || { echo 'FAIL: fallback rewrite failed under GNU stat' >&2; exit 1; }
grep -qxF '# BEGIN local-bin' "$TEST_HOME/.zshrc-gnu" || { echo 'FAIL: GNU-stat block missing' >&2; exit 1; }
[[ "$(/usr/bin/stat -f %Lp "$TEST_HOME/.zshrc-gnu")" == "640" ]] || { echo 'FAIL: mode not preserved' >&2; exit 1; }

# 10. agent-bot loose exports in .zshenv are repaired, not reported current.
cat >"$TEST_HOME/.zshenv" <<'EOF'
# Managed by managed-machine/setup-zsh.
export PATH="$HOME/.local/bin:$PATH"  # agent-bot CLI
export PATH="$HOME/.config/agent-bot/bin:$PATH"  # agent-bot gh shim
EOF
run_setup >"$TEST_DIR/agentbot.out"
grep -Fq 'backed up .zshenv' "$TEST_DIR/agentbot.out"
! grep -Fq 'agent-bot' "$TEST_HOME/.zshenv" || { echo 'FAIL: agent-bot exports survived' >&2; exit 1; }
! grep -qE '^[ \t]*export[ \t]+PATH=' "$TEST_HOME/.zshenv" || { echo 'FAIL: unguarded export survived' >&2; exit 1; }

# 11. Managed blocks survive a template refresh exactly once; orphans stay back.
cat >"$CONFIG_REPO_ROOT/dotfiles/zsh/.zshenv" <<'EOF'
# Managed by managed-machine/setup-zsh.
# Env for all zsh invocations. Keep minimal.

# BEGIN zsh-functions
template-body
# END zsh-functions
EOF
cat >"$TEST_HOME/.zshenv" <<'EOF'
# Managed by managed-machine/setup-zsh.
export PATH="$HOME/.local/bin:$PATH"  # agent-bot CLI

# BEGIN zsh-functions
template-body
# END zsh-functions

# BEGIN mystuff
custom-line
# END mystuff

# BEGIN orphan
dangling
EOF
run_setup >"$TEST_DIR/carry.out"
grep -Fq 'backed up .zshenv' "$TEST_DIR/carry.out"
[[ "$(grep -c '^# BEGIN zsh-functions$' "$TEST_HOME/.zshenv")" == "1" ]] \
  || { echo 'FAIL: zsh-functions block duplicated or lost' >&2; exit 1; }
grep -qxF 'custom-line' "$TEST_HOME/.zshenv" || { echo 'FAIL: custom block not carried' >&2; exit 1; }
[[ "$(grep -c '^$' "$TEST_HOME/.zshenv")" == "2" ]] \
  || { echo 'FAIL: carried block separator wrong' >&2; exit 1; }
! grep -Fq 'dangling' "$TEST_HOME/.zshenv" || { echo 'FAIL: orphan block carried' >&2; exit 1; }
grep -Fq 'dangling' "$TEST_HOME"/.zshenv.*.bak || { echo 'FAIL: orphan missing from backup' >&2; exit 1; }
cp "$TEST_HOME/.zshenv" "$TEST_DIR/carry.before"
run_setup >"$TEST_DIR/carry-rerun.out"
cmp -s "$TEST_DIR/carry.before" "$TEST_HOME/.zshenv" || { echo 'FAIL: re-refresh changed .zshenv' >&2; exit 1; }

echo 'setup-zsh tests passed'

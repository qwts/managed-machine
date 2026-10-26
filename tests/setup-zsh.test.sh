#!/usr/bin/env bash
# setup-zsh writes guarded PATH blocks and backs up stale profiles.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/../legacy" && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$CONFIG_REPO_ROOT/dotfiles/zsh" "$CONFIG_REPO_ROOT/config"
git init --quiet "$CONFIG_REPO_ROOT"
git -C "$CONFIG_REPO_ROOT" config user.name 'managed-machine test'
git -C "$CONFIG_REPO_ROOT" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO_ROOT" config commit.gpgsign false

cp "$ROOT/../tests/fixtures/config-zsh" "$CONFIG_REPO_ROOT/config/zsh"
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
export PATH="/Users/someone/.local/bin:$PATH"

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
# Homebrew's installer adds brew shellenv when needed.
eval "$(/opt/homebrew/bin/brew shellenv)"
eval "$(/opt/homebrew/bin/brew shellenv)"

# Added by Antigravity CLI installer
export PATH="/Users/someone/.local/bin:$PATH"
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
# Carried lines land once, inside named blocks, and comments are not
# carried (#169).
[[ "$(grep -c 'brew shellenv' "$TEST_HOME/.zprofile")" == 1 ]]
grep -A1 -xF '# BEGIN brew-shellenv' "$TEST_HOME/.zprofile" | grep -qxF 'eval "$(/opt/homebrew/bin/brew shellenv)"'
grep -qxF '# END brew-shellenv' "$TEST_HOME/.zprofile"
grep -A1 -xF '# BEGIN cargo-env' "$TEST_HOME/.zshenv" | grep -qxF '. "$HOME/.cargo/env"'
grep -qxF '# END cargo-env' "$TEST_HOME/.zshenv"
# A second refresh of the same stale shapes yields byte-identical files.
cp "$TEST_HOME/.zprofile" "$TEST_DIR/zprofile.first"
cp "$TEST_HOME/.zshenv" "$TEST_DIR/zshenv.first"
cat "$TEST_DIR/zprofile.first" >"$TEST_HOME/.zprofile"
printf 'export PATH="/Users/someone/.local/bin:$PATH"\n' >>"$TEST_HOME/.zprofile"
cat "$TEST_DIR/zshenv.first" >"$TEST_HOME/.zshenv"
printf 'export PATH="${HOME}/.local/bin:${PATH}"\n' >>"$TEST_HOME/.zshenv"
run_setup >"$TEST_DIR/refresh2.out"
grep -Fq 'backed up .zprofile' "$TEST_DIR/refresh2.out"
cmp -s "$TEST_DIR/zprofile.first" "$TEST_HOME/.zprofile"
cmp -s "$TEST_DIR/zshenv.first" "$TEST_HOME/.zshenv"

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

# 6b. GeniusBar's marked PATH line in .zprofile is GeniusBar's (#179): alone
# it does not make the file stale, and a refresh for another reason keeps it.
GENIUSBAR_MARK='# geniusbar-cli-tool: added by GeniusBar. "Uninstall command-line tools" removes it.'
printf 'echo custom\n%s\nexport PATH="$HOME/.local/bin:$PATH"\n' "$GENIUSBAR_MARK" >"$TEST_HOME/.zprofile"
cp "$TEST_HOME/.zprofile" "$TEST_DIR/geniusbar.before"
run_setup >"$TEST_DIR/geniusbar-only.out"
grep -Fq 'already current: .zprofile' "$TEST_DIR/geniusbar-only.out"
cmp -s "$TEST_DIR/geniusbar.before" "$TEST_HOME/.zprofile"
printf '%s\nexport PATH="$HOME/.local/bin:$PATH"\nexport PATH="$HOME/.local/bin:$PATH"\n' "$GENIUSBAR_MARK" >"$TEST_HOME/.zprofile"
run_setup >"$TEST_DIR/geniusbar-refresh.out"
grep -Fq 'backed up .zprofile' "$TEST_DIR/geniusbar-refresh.out"
grep -qxF "$GENIUSBAR_MARK" "$TEST_HOME/.zprofile"
[[ "$(grep -A1 -xF "$GENIUSBAR_MARK" "$TEST_HOME/.zprofile" | tail -n 1)" == 'export PATH="$HOME/.local/bin:$PATH"' ]]
[[ "$(grep -cxF 'export PATH="$HOME/.local/bin:$PATH"' "$TEST_HOME/.zprofile")" == 1 ]]
cp "$TEST_HOME/.zprofile" "$TEST_DIR/geniusbar.after"
run_setup >"$TEST_DIR/geniusbar-rerun.out"
grep -Fq 'already current: .zprofile' "$TEST_DIR/geniusbar-rerun.out"
cmp -s "$TEST_DIR/geniusbar.after" "$TEST_HOME/.zprofile"

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
# Test double matching zsh-profile's ensure-block (--absorb-marker), compact, and lint.
set -euo pipefail
cmd="${1:-}"
shift || true
printf '%s %s\n' "$cmd" "$*" >>"${ZSH_PROFILE_LOG:-/dev/null}"
case "$cmd" in
    ensure-block)
        file=""; name=""
        absorb_file="$(mktemp)"
        body_file="$(mktemp)"
        trap 'rm -f "$absorb_file" "$body_file"' EXIT
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --file) file="$2"; shift 2 ;;
                --name) name="$2"; shift 2 ;;
                --absorb-marker) printf '%s\n' "$2" >>"$absorb_file"; shift 2 ;;
                *) shift ;;
            esac
        done
        cat >"$body_file"
        awk -v b="# BEGIN $name" -v e="# END $name" -v bodyfile="$body_file" -v absorbfile="$absorb_file" '
            BEGIN {
                nbody = 0; while ((getline l < bodyfile) > 0) body[nbody++] = l
                nabsorb = 0; while ((getline l < absorbfile) > 0) absorb[nabsorb++] = l
                in_target = 0; in_other = ""; found = 0; emitted = 0; n = 0
            }
            {
                if (in_target) {
                    if ($0 == e) {
                        if (!emitted) {
                            out[n++] = b
                            for (i = 0; i < nbody; i++) out[n++] = body[i]
                            out[n++] = e
                            emitted = 1
                        }
                        in_target = 0
                    }
                    next
                }
                if (in_other != "") {
                    out[n++] = $0
                    if ($0 == in_other) in_other = ""
                    next
                }
                if ($0 == b) { in_target = 1; found = 1; next }
                if ($0 ~ /^# BEGIN /) { in_other = "# END " substr($0, 9); out[n++] = $0; next }
                absorbed = 0
                for (i = 0; i < nabsorb; i++) {
                    if (absorb[i] != "" && index($0, absorb[i]) > 0) { absorbed = 1; break }
                }
                if (absorbed) next
                out[n++] = $0
            }
            END {
                if (in_target) exit 3
                if (!found) {
                    while (n > 0 && out[n-1] ~ /^[ \t]*$/) n--
                    if (n > 0) out[n++] = ""
                    out[n++] = b
                    for (i = 0; i < nbody; i++) out[n++] = body[i]
                    out[n++] = e
                }
                for (i = 0; i < n; i++) print out[i]
            }
        ' "$file" >"$file.new"
        mv "$file.new" "$file"
        ;;
    compact)
        for f in "$@"; do
            awk '
                { buf[n++] = $0 }
                END {
                    m = 0
                    for (i = 0; i < n; i++) {
                        if (buf[i] ~ /^[ \t]*$/ && m > 0 && out[m-1] ~ /^[ \t]*$/) continue
                        out[m++] = buf[i]
                    }
                    while (m > 0 && out[m-1] ~ /^[ \t]*$/) m--
                    s = 0
                    while (s < m && out[s] ~ /^[ \t]*$/) s++
                    for (i = s; i < m; i++) print out[i]
                }
            ' "$f" >"$f.new"
            mv "$f.new" "$f"
        done
        ;;
    lint)
        status=0
        for f in "$@"; do
            awk -v file="$f" '
                BEGIN { open = ""; run = 0; findings = 0 }
                {
                    if ($0 ~ /^# BEGIN /) {
                        if (open != "") { printf "%s:%d: nested BEGIN inside an open block\n", file, NR; findings++ }
                        else { open = $0; openline = NR }
                        if (seen[$0]) { printf "%s:%d: duplicate block: %s\n", file, NR, $0; findings++ }
                        seen[$0] = 1
                    } else if ($0 ~ /^# END /) {
                        if (open == "") { printf "%s:%d: END without BEGIN\n", file, NR; findings++ }
                        else if ($0 != "# END " substr(open, 9)) {
                            printf "%s:%d: %s does not close %s\n", file, NR, $0, open; findings++
                        }
                        open = ""
                    } else if (open == "" && $0 ~ /^[ \t]*export[ \t]+PATH=/) {
                        printf "%s:%d: unguarded PATH export: %s\n", file, NR, $0; findings++
                    } else if (open == "" && $0 ~ /^[ \t]*path=\(/) {
                        printf "%s:%d: unguarded path assignment: %s\n", file, NR, $0; findings++
                    }
                    if ($0 ~ /^[ \t]*$/) {
                        run++
                        if (run == 3) { printf "%s:%d: blank run (3+ consecutive lines)\n", file, NR - 2; findings++ }
                    } else {
                        run = 0
                    }
                }
                END {
                    if (open != "") { printf "%s:%d: unterminated block at EOF: %s\n", file, openline, open; findings++ }
                    if (run > 0) { printf "%s:%d: trailing blank line(s) at EOF\n", file, NR - run + 1; findings++ }
                    exit (findings > 0)
                }
            ' "$f" || status=1
        done
        exit "$status"
        ;;
esac
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
grep -Fq 'ensure-block' "$ZSH_PROFILE_LOG" || { echo 'FAIL: zsh-profile ensure-block not delegated to' >&2; exit 1; }
grep -Fq -- '--absorb-marker added by muse installer' "$ZSH_PROFILE_LOG" || { echo 'FAIL: muse absorb marker not passed' >&2; exit 1; }
grep -Fq -- '--absorb-marker Added by Antigravity CLI installer' "$ZSH_PROFILE_LOG" || { echo 'FAIL: antigravity absorb marker not passed' >&2; exit 1; }
grep -Fq 'compact ' "$ZSH_PROFILE_LOG" || { echo 'FAIL: zsh-profile compact not delegated to' >&2; exit 1; }
grep -Fq 'lint ' "$ZSH_PROFILE_LOG" || { echo 'FAIL: zsh-profile lint not delegated to' >&2; exit 1; }
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
# The template trails two blank lines: the carry must trim them so the
# separator stays exactly one blank, not three.
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

# 12. Issue #169 fixture: literal Muse PATH export with comment, Antigravity
# comment + bare export, multi-entry agent-bot export, path=(...) assignment,
# loose .cargo/env after # END brew, duplicate brew shellenv comments, 90
# consecutive blank lines before # BEGIN nvm, and GeniusBar's marked block.
# Verify both fallback and zsh-profile delegation clean/absorb/compact all
# three files, pass `zsh-profile lint`, preserve GeniusBar's block, and leave
# every file byte-identical on a second run.
for mode in fallback delegated; do
    cat >"$TEST_HOME/.zprofile" <<EOF
# Managed by managed-machine/setup-zsh.
# Homebrew's installer adds brew shellenv when needed.
# Homebrew's installer adds brew shellenv when needed.
eval "\$(/opt/homebrew/bin/brew shellenv)"

export PATH="\$HOME/.config/agent-bot/bin:\$HOME/.local/bin:\$PATH"  # agent-bot installed commands
path=("\$HOME/.local/bin" "\${(@)path:#\$HOME/.local/bin}")  # agent-bot gh shim login priority
$GENIUSBAR_MARK
export PATH="\$HOME/.local/bin:\$PATH"
EOF

    cat >"$TEST_HOME/.zshenv" <<'EOF'
# Managed by managed-machine/setup-zsh.
# Env for all zsh invocations. Keep minimal.

# BEGIN zsh-functions
template-body
# END zsh-functions

# BEGIN brew
typeset -U path PATH
# END brew
. "$HOME/.cargo/env"
EOF

    {
        printf 'export PATH="/Users/someone/.local/bin:$PATH" # added by muse installer\n'
        printf '# Added by Antigravity CLI installer\n'
        printf 'export PATH="/Users/someone/.local/bin:$PATH"\n'
        printf '# BEGIN local-bin\n'
        printf 'case ":${PATH}:" in\n'
        printf '    *":${HOME}/.local/bin:"*) ;;\n'
        printf '    *) export PATH="${HOME}/.local/bin:${PATH}" ;;\n'
        printf 'esac\n'
        printf '# END local-bin\n'
        for _ in $(seq 1 90); do printf '\n'; done
        printf '# BEGIN nvm\n'
        printf 'export NVM_DIR="${NVM_DIR:-${HOME}/.nvm}"\n'
        printf 'case "$(command -v node 2>/dev/null)" in\n'
        printf '    "${NVM_DIR}/versions/"*) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" --no-use ;;\n'
        printf '    *) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" ;;\n'
        printf 'esac\n'
        printf '# END nvm\n'
    } >"$TEST_HOME/.zshrc"

    if [[ "$mode" == "delegated" ]]; then
        run_path="$TEST_DIR/fakebin:/usr/bin:/bin"
    else
        run_path="/usr/bin:/bin"
    fi

    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    MANAGED_MACHINE_ROOT="$ROOT" \
    PATH="$run_path" \
    /bin/bash "$ROOT/setup-zsh" >"$TEST_DIR/169-$mode-1.out"

    grep -Fq 'backed up .zprofile' "$TEST_DIR/169-$mode-1.out"
    grep -Fq 'backed up .zshenv' "$TEST_DIR/169-$mode-1.out"
    grep -Fq 'backed up .zshrc' "$TEST_DIR/169-$mode-1.out"

    ! grep -Fq 'added by muse installer' "$TEST_HOME/.zshrc"
    ! grep -Fq 'Added by Antigravity CLI installer' "$TEST_HOME/.zshrc"
    ! grep -Fq 'agent-bot installed commands' "$TEST_HOME/.zprofile"
    ! grep -Fq 'agent-bot gh shim login priority' "$TEST_HOME/.zprofile"
    grep -qxF "$GENIUSBAR_MARK" "$TEST_HOME/.zprofile"
    grep -A1 -xF '# BEGIN brew-shellenv' "$TEST_HOME/.zprofile" | grep -qxF 'eval "$(/opt/homebrew/bin/brew shellenv)"'
    grep -A1 -xF '# BEGIN cargo-env' "$TEST_HOME/.zshenv" | grep -qxF '. "$HOME/.cargo/env"'

    # zsh-profile lint passes on .zshenv and .zshrc directly, and lint_zsh_startup_file passes on all 3.
    "$TEST_DIR/fakebin/zsh-profile" lint "$TEST_HOME/.zshenv" "$TEST_HOME/.zshrc"
    HOME="$TEST_HOME" PATH="$run_path" /bin/bash -c '
        source "$1/lib/install.sh"
        lint_zsh_startup_file "$2/.zshenv"
        lint_zsh_startup_file "$2/.zprofile"
        lint_zsh_startup_file "$2/.zshrc"
    ' bash "$ROOT" "$TEST_HOME"

    cp "$TEST_HOME/.zshenv" "$TEST_DIR/169-$mode.zshenv"
    cp "$TEST_HOME/.zprofile" "$TEST_DIR/169-$mode.zprofile"
    cp "$TEST_HOME/.zshrc" "$TEST_DIR/169-$mode.zshrc"

    # Second run is a no-op and leaves every file byte-identical.
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    MANAGED_MACHINE_ROOT="$ROOT" \
    PATH="$run_path" \
    /bin/bash "$ROOT/setup-zsh" >"$TEST_DIR/169-$mode-2.out"

    grep -Fq 'already current: .zshenv' "$TEST_DIR/169-$mode-2.out"
    grep -Fq 'already current: .zprofile' "$TEST_DIR/169-$mode-2.out"
    grep -Fq 'already current: .zshrc' "$TEST_DIR/169-$mode-2.out"
    cmp -s "$TEST_DIR/169-$mode.zshenv" "$TEST_HOME/.zshenv"
    cmp -s "$TEST_DIR/169-$mode.zprofile" "$TEST_HOME/.zprofile"
    cmp -s "$TEST_DIR/169-$mode.zshrc" "$TEST_HOME/.zshrc"
done

# 12b. Direct absorption via ensure_local_bin_in_zshrc (without full template
# refresh) removes Muse and Antigravity installer markers and Antigravity's
# following bare export PATH line in both fallback and delegated modes.
for mode in fallback delegated; do
    if [[ "$mode" == "delegated" ]]; then
        run_path="$TEST_DIR/fakebin:/usr/bin:/bin"
    else
        run_path="/usr/bin:/bin"
    fi
    cat >"$TEST_HOME/.zshrc-absorb" <<'EOF'
export PATH="/Users/someone/.local/bin:$PATH" # added by muse installer
# Added by Antigravity CLI installer
export PATH="/Users/someone/.local/bin:$PATH"
alias keep_me=1
EOF
    HOME="$TEST_HOME" PATH="$run_path" /bin/bash -c '
        source "$1/lib/install.sh"
        ensure_local_bin_in_zshrc "$2"
        lint_zsh_startup_file "$2"
    ' bash "$ROOT" "$TEST_HOME/.zshrc-absorb" >/dev/null
    ! grep -Fq 'added by muse installer' "$TEST_HOME/.zshrc-absorb"
    ! grep -Fq 'Added by Antigravity CLI installer' "$TEST_HOME/.zshrc-absorb"
    ! grep -Fq '/Users/someone/.local/bin' "$TEST_HOME/.zshrc-absorb"
    grep -qxF 'alias keep_me=1' "$TEST_HOME/.zshrc-absorb"
    grep -qxF '# BEGIN local-bin' "$TEST_HOME/.zshrc-absorb"
    "$TEST_DIR/fakebin/zsh-profile" lint "$TEST_HOME/.zshrc-absorb"
done

# 13. setup-zsh fails non-zero if a startup file still fails lint at the end
# (for example, a template with an unterminated # BEGIN block).
cat >"$CONFIG_REPO_ROOT/dotfiles/zsh/.zprofile" <<'EOF'
# Managed by managed-machine/setup-zsh.
# BEGIN broken
EOF
rm -f "$TEST_HOME/.zprofile"
if run_setup >"$TEST_DIR/lint-fail.out" 2>&1; then
    echo 'FAIL: expected setup-zsh to fail when lint finds an unterminated block' >&2
    exit 1
fi
grep -Fq 'unterminated block at EOF: # BEGIN broken' "$TEST_DIR/lint-fail.out"

echo 'setup-zsh tests passed'

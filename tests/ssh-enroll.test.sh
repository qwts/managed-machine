#!/usr/bin/env bash
# Issue #120: SSH enrollment is explicit and human-only.
#
# - setup-gh / bootstrap / --update perform zero SSH-enrollment operations:
#   no key generation, no ssh-add, no key upload, no upload-scope refresh, no
#   SSH git_protocol switch, no signing enrollment, no fleet registration.
# - `managed-machine ssh enroll` is a separate human-authorized action that
#   requires at least one explicit purpose flag, refuses agent contexts
#   before any mutation or dialog, and is retry-safe.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
TEST_PREFIX="$TEST_ROOT/prefix"
trap 'rm -rf "$TEST_ROOT"' EXIT

GH_LOG="$TEST_ROOT/gh.log"
OSA_LOG="$TEST_ROOT/osascript.log"
KEYGEN_LOG="$TEST_ROOT/ssh-keygen.log"
SSH_ADD_LOG="$TEST_ROOT/ssh-add.log"
BREW_LOG="$TEST_ROOT/brew.log"
GH_KEYS_STATE="$TEST_ROOT/gh-keys.state"
# A fixed literal, not the real invoking account: agent_current_context (see
# lib/agent-account.sh) now also consults the OS-account-name heuristic in
# managed_machine_agent_session, and on a host whose real account itself
# happens to match the agent-account glob (e.g. it ends in "-agent"), the
# unmocked "happy path" scenarios below would otherwise be misdetected as an
# agent context regardless of intent. The id stub below returns this same
# literal for unmocked `-un` queries so it still shows up in enrollment
# output exactly as a real account name would.
CURRENT_USER="devbox-human"

# The test process's own git calls must never consult the real global
# gitconfig: signing is off and the identity is a fixture.
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=commit.gpgsign GIT_CONFIG_VALUE_0=false
export GIT_AUTHOR_NAME='t' GIT_AUTHOR_EMAIL='t@example.invalid'
export GIT_COMMITTER_NAME='t' GIT_COMMITTER_EMAIL='t@example.invalid'

mkdir -p "$TEST_HOME" "$TEST_BIN" "$TEST_PREFIX/bin"
: >"$GH_KEYS_STATE"

# --- command stubs ---------------------------------------------------------

cat >"$TEST_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$OSA_LOG"
[[ -z "${OSA_STDERR:-}" ]] || printf '%s\n' "$OSA_STDERR" >&2
exit "${OSA_EXIT:-0}"
EOF

cat >"$TEST_BIN/ssh-keygen" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$KEYGEN_LOG"
if [[ "$*" == *'-y -P '* ]]; then
    echo 'ssh-rsa AAAATESTKEY'
    exit 0
fi
if [[ "$*" == *'-lf'* ]]; then
    echo '4096 SHA256:enrolltestkey test@example (RSA)'
    exit 0
fi
target=''
while [[ $# -gt 0 ]]; do
    if [[ "$1" == '-f' ]]; then
        shift
        target="$1"
        break
    fi
    shift
done
[[ -n "$target" ]] || { echo "ASSERT failed (line 66): [[ -n \"$target\" ]]" >&2; exit 1; }
printf 'private test key\n' >"$target"
printf 'ssh-rsa AAAATESTKEY test@example\n' >"$target.pub"
EOF

cat >"$TEST_BIN/ssh-add" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SSH_ADD_LOG"
exit "${SSH_ADD_EXIT:-0}"
EOF

# gh stub: logs every call; the account, scopes, and key listings come from
# the environment/state file so retries see keys the first run "uploaded".
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GH_LOG"
case "$1 ${2:-}" in
    '--version '|'version ') echo 'gh version 2.74.0 (stub)'; exit 0 ;;
    'auth status')
        case "$*" in
            *'.login'*) printf '%s\n' "${MOCK_GH_LOGIN-qwts}" ;;
            *'.scopes'*) printf '%s\n' "${MOCK_GH_SCOPES:-repo, admin:public_key, admin:ssh_signing_key}" ;;
        esac
        exit 0
        ;;
    'auth refresh'|'auth setup-git'|'auth login'|'config set'|'config get') exit 0 ;;
    'api --paginate')
        [[ "${MOCK_LIST_FAILS:-0}" == 1 ]] && { echo 'gh: HTTP 503' >&2; exit 1; }
        case "$3" in
            users/*/keys) /usr/bin/awk -F '\t' '$1=="authentication"{print $2}' "${GH_KEYS_STATE:-/dev/null}" 2>/dev/null ;;
            users/*/ssh_signing_keys) /usr/bin/awk -F '\t' '$1=="signing"{print $2}' "${GH_KEYS_STATE:-/dev/null}" 2>/dev/null ;;
            *) exit 1 ;;
        esac
        exit 0
        ;;
    'api user')
        case "$*" in
            *'.login'*) printf '%s\n' "${MOCK_GH_LOGIN-qwts}" ;;
            *'.id'*) echo '1234' ;;
        esac
        exit 0
        ;;
    'ssh-key add')
        type=''
        pub=''
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --type) type="$2"; shift ;;
                *.pub) pub="$1" ;;
            esac
            shift
        done
        body="$(/usr/bin/awk 'NF >= 2 { print $1 " " $2; exit }' "$pub")"
        printf '%s\t%s\n' "${type:-authentication}" "$body" >>"$GH_KEYS_STATE"
        exit 0
        ;;
esac
exit 1
EOF

# id forwards to the real binary unless the test mocks the account. An
# unmocked `-un` query answers with the fixed CURRENT_USER literal rather
# than the real account, so this suite's account-identity assumptions hold
# regardless of what real OS account happens to run it.
cat >"$TEST_BIN/id" <<EOF
#!/usr/bin/env bash
if [[ -n "\${MOCK_ID_UN:-}" && "\$*" == '-un' ]]; then
    printf '%s\n' "\$MOCK_ID_UN"
elif [[ "\$*" == '-un' ]]; then
    printf '%s\n' "$CURRENT_USER"
elif [[ -n "\${MOCK_ID_UID:-}" && "\$*" == '-u' ]]; then
    printf '%s\n' "\$MOCK_ID_UID"
else
    exec /usr/bin/id "\$@"
fi
EOF

# config_repo_owner (lib/config-repo.sh) reads real filesystem ownership via
# stat to assert the config checkout belongs to the invoking account. Since
# the id stub above answers with the fixed CURRENT_USER literal rather than
# this host's real account, the two would otherwise disagree for the scratch
# config-repo checkout created under TEST_ROOT; scope the same literal to
# just that owner-name query on paths under TEST_ROOT so it stays real
# everywhere else (e.g. brew-prefix and admin-home ownership checks).
cat >"$TEST_BIN/stat" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == '-f' && "\$2" == '%Su' && "\$3" == "$TEST_ROOT"/* ]]; then
    printf '%s\n' "$CURRENT_USER"
else
    exec /usr/bin/stat "\$@"
fi
EOF

# Directory stubs are pinned only in the disposable enrollment fixture;
# membership and home checks never touch the real directory in that fixture.
cat >"$TEST_BIN/dscl" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == *'NFSHomeDirectory'* ]]; then
    [[ "${MOCK_DSCL_FAILS:-0}" == 1 ]] && exit 1
    printf 'NFSHomeDirectory: %s\n' "${MOCK_DSCL_HOME:-$HOME}"
    exit 0
fi
exit 0
EOF

cat >"$TEST_BIN/dsmemberutil" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == *'-G agents'* ]]; then
    if [[ "${MOCK_AGENTS_MEMBER:-0}" == 1 ]]; then
        echo "user is a member of group agents"
        exit 0
    fi
    echo "user is not a member of group agents"
    exit 0
fi
exec /usr/bin/dsmemberutil "$@"
EOF

cat >"$TEST_BIN/scutil" <<'EOF'
#!/usr/bin/env bash
echo testmac
EOF

cat >"$TEST_BIN/brew" <<'EOF'
#!/usr/bin/env bash
printf 'brew %s\n' "$*" >>"${BREW_LOG:-/dev/null}"
case "${1:-}" in
    --prefix) printf '%s\n' "$TEST_PREFIX" ;;
    shellenv) printf 'export PATH="%s/bin:$PATH"\n' "$TEST_PREFIX" ;;
    list) echo 'gh 2.74.0' ;;
    *) exit 0 ;;
esac
EOF

chmod +x "$TEST_BIN"/*
cp "$TEST_BIN/gh" "$TEST_PREFIX/bin/gh"
chmod +x "$TEST_PREFIX/bin/gh"

SSH_ROOT="$TEST_ROOT/runtime/source"
mkdir -p "$SSH_ROOT/scripts"
cp -R "$ROOT/lib" "$SSH_ROOT/lib"
cp "$ROOT/scripts/ssh" "$SSH_ROOT/scripts/ssh"
# The agent-context check (group membership) lives in agent-account.sh, not
# ssh-enroll.sh, since `managed-machine status` shares the same definition of
# "this is an agent, not the human operator" — pin both files identically.
for lib in ssh-enroll agent-account; do
    for pinned in id dscl dsmemberutil osascript; do
        sed "s|/usr/bin/$pinned|$TEST_BIN/$pinned|g" "$SSH_ROOT/lib/$lib.sh" >"$SSH_ROOT/lib/$lib.tmp"
        mv "$SSH_ROOT/lib/$lib.tmp" "$SSH_ROOT/lib/$lib.sh"
    done
done
# agent_current_context forces PATH=/usr/bin:/bin:/usr/sbin:/sbin around the
# bare `id -un` check inside managed_machine_agent_session so a caller's PATH
# can never spoof it — which also means this test's own $TEST_BIN stub is
# invisible to it unless patched in too. Without this, that bare `id -un`
# call hits the real system id, and on a host whose real account name itself
# matches the agent-account glob (e.g. it ends in "-agent"), every scenario
# below would be misdetected as an agent context regardless of MOCK_ID_UN.
sed "s|PATH=/usr/bin:/bin:/usr/sbin:/sbin|PATH=$TEST_BIN:/usr/bin:/bin:/usr/sbin:/sbin|" \
    "$SSH_ROOT/lib/agent-account.sh" >"$SSH_ROOT/lib/agent-account.tmp"
mv "$SSH_ROOT/lib/agent-account.tmp" "$SSH_ROOT/lib/agent-account.sh"

# A private-config checkout fixture backed by a local bare origin.
ORIGIN_REPO="$TEST_ROOT/config-origin.git"
CONFIG_REPO="$TEST_ROOT/managed-machine-config"
git init --bare --quiet "$ORIGIN_REPO"
git -C "$ORIGIN_REPO" symbolic-ref HEAD refs/heads/main
git clone --quiet "$ORIGIN_REPO" "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'managed-machine test'
git -C "$CONFIG_REPO" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" symbolic-ref HEAD refs/heads/main
printf '{"schema_version":1,"apps":[]}\n' >"$CONFIG_REPO/apps.json"
printf 'v0.1.0\n' >"$CONFIG_REPO/local-bin.ref"
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed
git -C "$CONFIG_REPO" push --quiet -u origin main

run_env() {
    env -i \
        HOME="$TEST_HOME" \
        PATH="$TEST_BIN:/usr/bin:/bin" \
        CONFIG_REPO_ROOT="$CONFIG_REPO" \
        TEST_PREFIX="$TEST_PREFIX" \
        GH_LOG="$GH_LOG" OSA_LOG="$OSA_LOG" KEYGEN_LOG="$KEYGEN_LOG" \
        SSH_ADD_LOG="$SSH_ADD_LOG" GH_KEYS_STATE="$GH_KEYS_STATE" \
        BREW_LOG="$BREW_LOG" \
        "$@"
}

run_ssh() {
    run_env /bin/bash "$SSH_ROOT/scripts/ssh" "$@"
}

reset_logs() {
    : >"$GH_LOG"; : >"$OSA_LOG"; : >"$KEYGEN_LOG"; : >"$SSH_ADD_LOG"; : >"$BREW_LOG"; : >"$GH_KEYS_STATE"
}

reset_state() {
    rm -rf "$TEST_HOME/.ssh" "$TEST_HOME/.config/managed-machine"
    run_env git config --global user.name 'managed-machine test'
    run_env git config --global user.email '91036491+qwts@users.noreply.github.com'
    run_env git config --global commit.gpgsign false
    if git -C "$CONFIG_REPO" rev-parse --verify --quiet origin/main >/dev/null; then
        git -C "$CONFIG_REPO" reset --hard -q origin/main
    fi
    git -C "$CONFIG_REPO" clean -fdq
    reset_logs
}

reset_state
if run_env MOCK_ID_UN=spoofed-human MOCK_ID_UID=501 /bin/bash -c '
    source "$1/lib/agent-account.sh"
    source "$1/lib/ssh-enroll.sh"
    managed_machine_agent_session() { return 1; }
    agent_roster_source() { return 1; }
    AGENT_ACCOUNT_GROUP=agents
    ssh_enroll_validate_account
' _ "$ROOT" >"$TEST_ROOT/path-spoof.out" 2>&1; then
    echo 'PATH stubs must not establish a human identity or registered home' >&2
    exit 1
fi

for pinned in id dscl osascript; do
    if ! grep -Fq "command /usr/bin/$pinned " "$ROOT/lib/ssh-enroll.sh"; then
        echo "enrollment must pin $pinned to its system executable" >&2
        exit 1
    fi
done
if ! grep -Fq '/usr/bin/dsmemberutil' "$ROOT/lib/agent-account.sh"; then
    echo "agent-account group membership check must pin dsmemberutil to its system executable" >&2
    exit 1
fi

# ==========================================================================
# 1. Argument contract: purposes are explicit, never silently all-enabled.
# ==========================================================================
reset_state
if run_ssh enroll >"$TEST_ROOT/no-purpose.out" 2>&1; then
    echo 'expected ssh enroll without a purpose to fail' >&2
    exit 1
fi
grep -Fqi 'purpose' "$TEST_ROOT/no-purpose.out"
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 237): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 238): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }

if run_ssh enroll --bogus >"$TEST_ROOT/bogus.out" 2>&1; then
    echo 'expected ssh enroll --bogus to fail' >&2
    exit 1
fi
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 244): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }

if run_ssh >"$TEST_ROOT/bare.out" 2>&1; then
    echo 'expected bare ssh command to fail' >&2
    exit 1
fi
grep -Fq 'managed-machine ssh' "$TEST_ROOT/bare.out"

# ==========================================================================
# 2. Agent/root/foreign contexts are refused before mutation or dialog.
# ==========================================================================

# 2a. Harness environment markers.
reset_state
if run_env CLAUDECODE=1 /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/agent-env.out" 2>&1; then
    echo 'expected agent-session enrollment to be refused' >&2
    exit 1
fi
grep -Fqi 'agent' "$TEST_ROOT/agent-env.out"
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 263): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }
[[ ! -s "$KEYGEN_LOG" ]] || { echo "ASSERT failed (line 264): [[ ! -s \"$KEYGEN_LOG\" ]]" >&2; exit 1; }
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 265): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }
! grep -q 'ssh-key add' "$GH_LOG"

# 2b. OS group membership: the agents group is the directory-level fact.
reset_state
if run_env MOCK_AGENTS_MEMBER=1 /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/agent-group.out" 2>&1; then
    echo 'expected agents-group account enrollment to be refused' >&2
    exit 1
fi
grep -Fqi 'agent' "$TEST_ROOT/agent-group.out"
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 275): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 276): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }

# 2c. Roster identity: the account name is a rostered identity slug.
reset_state
printf '{"identities":[{"slug":"%s","status":"active","harness":"devin"}]}\n' "$CURRENT_USER" >"$TEST_ROOT/profile.json"
if run_env MANAGED_MACHINE_ORG_PROFILE="$TEST_ROOT/profile.json" /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/agent-roster.out" 2>&1; then
    echo 'expected rostered-agent account enrollment to be refused' >&2
    exit 1
fi
grep -Fqi 'agent' "$TEST_ROOT/agent-roster.out"
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 286): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }

# 2d. Root: enrollment never runs privileged.
reset_state
if run_env MOCK_ID_UN=root MOCK_ID_UID=0 /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/root.out" 2>&1; then
    echo 'expected root enrollment to be refused' >&2
    exit 1
fi
grep -Fqi 'root' "$TEST_ROOT/root.out"
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 295): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }

# 2e. HOME must be the account's own registered directory.
reset_state
if run_env MOCK_DSCL_HOME=/var/root /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/home-mismatch.out" 2>&1; then
    echo 'expected HOME-mismatched enrollment to be refused' >&2
    exit 1
fi
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 303): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 304): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }

reset_state
if run_env MOCK_DSCL_FAILS=1 /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/home-unavailable.out" 2>&1; then
    echo 'expected unavailable directory home to refuse enrollment' >&2
    exit 1
fi
[[ ! -s "$OSA_LOG" && ! -e "$TEST_HOME/.ssh" ]]
grep -Fq 'could not verify the registered home' "$TEST_ROOT/home-unavailable.out"

# ==========================================================================
# 3. GitHub identity is required before the ceremony.
# ==========================================================================
reset_state
if run_env MOCK_GH_LOGIN='' /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/no-gh.out" 2>&1; then
    echo 'expected enrollment without gh auth to fail' >&2
    exit 1
fi
grep -Fq 'GitHub' "$TEST_ROOT/no-gh.out"
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 315): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 316): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }

# ==========================================================================
# 4. Authorization outcomes: cancellation and unavailable GUI.
# ==========================================================================
reset_state
if run_env OSA_EXIT=1 OSA_STDERR='execution error: User canceled. (-128)' \
    /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/cancel.out" 2>&1; then
    echo 'expected cancelled authorization to fail enrollment' >&2
    exit 1
fi
grep -Fq 'no enrollment occurred' "$TEST_ROOT/cancel.out"

POISON_BIN="$TEST_ROOT/poison-bin"
mkdir -p "$POISON_BIN"
printf '#!/bin/bash\nprintf "spoofed\\n" >>"$OSA_LOG"\nexit 0\n' >"$POISON_BIN/osascript"
chmod +x "$POISON_BIN/osascript"
reset_logs
if run_env PATH="$POISON_BIN:$TEST_BIN:/usr/bin:/bin" OSA_EXIT=1 \
    /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/consent-spoof.out" 2>&1; then
    echo 'PATH osascript must not override the pinned consent executable' >&2
    exit 1
fi
if grep -q 'spoofed' "$OSA_LOG"; then
    echo 'enrollment invoked the PATH authorization stub' >&2
    exit 1
fi
grep -Fq 'no enrollment occurred' "$TEST_ROOT/consent-spoof.out"
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 328): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }
! grep -q 'ssh-key add' "$GH_LOG"
! grep -q 'auth refresh' "$GH_LOG"

reset_state
if run_env MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive \
    /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/nonint.out" 2>&1; then
    echo 'expected noninteractive enrollment to be skipped' >&2
    exit 1
fi
grep -Fq 'no enrollment occurred' "$TEST_ROOT/nonint.out"
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 339): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 340): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }

# ==========================================================================
# 5. The plan shown before authorization binds account, login, and purposes.
# ==========================================================================
reset_state
run_ssh enroll --authentication >"$TEST_ROOT/auth-enroll.out" 2>&1 || {
    echo 'authentication enrollment failed' >&2
    cat "$TEST_ROOT/auth-enroll.out" >&2
    exit 1
}
grep -Fq "$CURRENT_USER" "$TEST_ROOT/auth-enroll.out"
grep -Fq 'qwts' "$TEST_ROOT/auth-enroll.out"
grep -Fq 'authentication' "$TEST_ROOT/auth-enroll.out"
grep -Fq 'will be created' "$TEST_ROOT/auth-enroll.out"
grep -Fq "$CURRENT_USER" "$OSA_LOG"
grep -Fq 'qwts' "$OSA_LOG"
grep -Fq 'authentication' "$OSA_LOG"
N357="$(grep -c 'with administrator privileges' "$OSA_LOG" || true)"
[[ "$N357" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N357 for: grep -c 'with administrator privileges' "$OSA_LOG"" >&2; exit 1; }
[[ -f "$TEST_HOME/.ssh/id_rsa_github" && -f "$TEST_HOME/.ssh/id_rsa_github.pub" ]] || { echo "ASSERT failed (line 359): [[ -f \"$TEST_HOME/.ssh/id_rsa_github\" && -f \"$TEST_HOME/.ssh/id_rsa_github.pub\" ]]" >&2; exit 1; }
grep -Fq 'Host github.com' "$TEST_HOME/.ssh/config"
[[ -s "$SSH_ADD_LOG" ]] || { echo "ASSERT failed (line 361): [[ -s \"$SSH_ADD_LOG\" ]]" >&2; exit 1; }
N361="$(grep -c 'ssh-key add.*--type authentication' "$GH_LOG" || true)"
[[ "$N361" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N361 for: grep -c 'ssh-key add.*--type authentication' "$GH_LOG"" >&2; exit 1; }
! grep -q 'ssh-key add.*--type signing' "$GH_LOG"
grep -Fq 'config set git_protocol ssh' "$GH_LOG"
[[ ! -f "$TEST_HOME/.config/managed-machine/machine.toml" ]] || { echo "ASSERT failed (line 365): [[ ! -f \"$TEST_HOME/.config/managed-machine/machine.toml\" ]]" >&2; exit 1; }
[[ ! -d "$CONFIG_REPO/fleet" ]] || { echo "ASSERT failed (line 366): [[ ! -d \"$CONFIG_REPO/fleet\" ]]" >&2; exit 1; }
[[ ! -e "$TEST_HOME/.ssh/authorized_keys" ]] || { echo "ASSERT failed (line 367): [[ ! -e \"$TEST_HOME/.ssh/authorized_keys\" ]]" >&2; exit 1; }
if run_env git config --global --get gpg.format >/dev/null 2>&1; then
    echo 'authentication-only enrollment must not configure SSH signing' >&2
    exit 1
fi

# Retry: existing pair and registered key make the second run a no-upload run.
run_ssh enroll --authentication >"$TEST_ROOT/auth-retry.out" 2>&1
N376="$(grep -c 'ssh-key add' "$GH_LOG" || true)"
[[ "$N376" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N376 for: grep -c 'ssh-key add' "$GH_LOG"" >&2; exit 1; }
N377="$(grep -c -- '-t rsa' "$KEYGEN_LOG" || true)"
[[ "$N377" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N377 for: grep -c -- '-t rsa' "$KEYGEN_LOG"" >&2; exit 1; }
grep -Fq 'already registered' "$TEST_ROOT/auth-retry.out"

# A token missing the upload scopes refreshes them once, then uploads.
reset_state
run_env MOCK_GH_SCOPES='repo' /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/scopes.out" 2>&1
grep -Fq 'auth refresh -h github.com -s admin:public_key,admin:ssh_signing_key' "$GH_LOG"
grep -Fq 'ssh-key add' "$GH_LOG"
[[ "$(grep -n 'auth refresh' "$GH_LOG" | cut -d: -f1)" -lt "$(grep -n 'ssh-key add' "$GH_LOG" | cut -d: -f1)" ]] || { echo "ASSERT failed (line 384): [[ \"$(grep -n 'auth refresh' \"$GH_LOG\" | cut -d: -f1)\" -lt \"$(grep -n 'ssh-key add' \"$GH_LOG\" | cut -d: -f1)\" ]]" >&2; exit 1; }

# ==========================================================================
# 6. Signing purpose: git SSH signing + signing-key upload + allowed_signers.
# ==========================================================================
reset_state
run_ssh enroll --signing >"$TEST_ROOT/signing.out" 2>&1 || {
    echo 'signing enrollment failed' >&2
    cat "$TEST_ROOT/signing.out" >&2
    exit 1
}
N396="$(run_env git config --global --get gpg.format)" || { echo "command failed: run_env git config --global --get gpg.format" >&2; exit 1; }
[[ "$N396" == 'ssh' ]] || { echo "ASSERT failed: expected 'ssh', got \$N396 for: run_env git config --global --get gpg.format" >&2; exit 1; }
N397="$(run_env git config --global --get commit.gpgsign)" || { echo "command failed: run_env git config --global --get commit.gpgsign" >&2; exit 1; }
[[ "$N397" == 'true' ]] || { echo "ASSERT failed: expected 'true', got \$N397 for: run_env git config --global --get commit.gpgsign" >&2; exit 1; }
N398="$(run_env git config --global --get user.signingkey)" || { echo "command failed: run_env git config --global --get user.signingkey" >&2; exit 1; }
[[ "$N398" == "$TEST_HOME/.ssh/id_rsa_github.pub" ]] || { echo "ASSERT failed: expected "$TEST_HOME/.ssh/id_rsa_github.pub", got \$N398 for: run_env git config --global --get user.signingkey" >&2; exit 1; }
N399="$(grep -c 'ssh-key add.*--type signing' "$GH_LOG" || true)"
[[ "$N399" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N399 for: grep -c 'ssh-key add.*--type signing' "$GH_LOG"" >&2; exit 1; }
! grep -q 'ssh-key add.*--type authentication' "$GH_LOG"
grep -Fq '# BEGIN managed-machine' "$TEST_HOME/.ssh/allowed_signers"
grep -Fq 'ssh-rsa AAAATESTKEY' "$TEST_HOME/.ssh/allowed_signers"

# ==========================================================================
# 7. Fleet purpose: registration, publish, and local authorized_keys only.
# ==========================================================================
reset_state
run_ssh enroll --fleet >"$TEST_ROOT/fleet.out" 2>&1 || {
    echo 'fleet enrollment failed' >&2
    cat "$TEST_ROOT/fleet.out" >&2
    exit 1
}
[[ -f "$TEST_HOME/.config/managed-machine/machine.toml" ]] || { echo "ASSERT failed (line 412): [[ -f \"$TEST_HOME/.config/managed-machine/machine.toml\" ]]" >&2; exit 1; }
[[ -f "$CONFIG_REPO/fleet/machines/sha256-enrolltestkey.toml" ]] || { echo "ASSERT failed (line 413): [[ -f \"$CONFIG_REPO/fleet/machines/sha256-enrolltestkey.toml\" ]]" >&2; exit 1; }
grep -Fq '# BEGIN managed-machine' "$TEST_HOME/.ssh/authorized_keys"
grep -Fq 'ssh-rsa AAAATESTKEY' "$TEST_HOME/.ssh/authorized_keys"
git -C "$ORIGIN_REPO" cat-file -e "main:fleet/machines/sha256-enrolltestkey.toml"
N418="$(git -C "$CONFIG_REPO" rev-list --count origin/main..HEAD)" || { echo "command failed: git -C "$CONFIG_REPO" rev-list --count origin/main..HEAD" >&2; exit 1; }
[[ "$N418" == "0" ]] || { echo "ASSERT failed: expected "0", got \$N418 for: git -C "$CONFIG_REPO" rev-list --count origin/main..HEAD" >&2; exit 1; }
[[ ! -s "$SSH_ADD_LOG" ]] || { echo "ASSERT failed (line 418): [[ ! -s \"$SSH_ADD_LOG\" ]]" >&2; exit 1; }
! grep -q 'ssh-key add' "$GH_LOG"
! grep -q 'config set git_protocol' "$GH_LOG"

# ==========================================================================
# 8. Combined purposes enroll all three through one authorization.
# ==========================================================================
reset_state
run_ssh enroll --authentication --signing --fleet >"$TEST_ROOT/all.out" 2>&1 || {
    echo 'combined enrollment failed' >&2
    cat "$TEST_ROOT/all.out" >&2
    exit 1
}
N432="$(grep -c 'with administrator privileges' "$OSA_LOG" || true)"
[[ "$N432" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N432 for: grep -c 'with administrator privileges' "$OSA_LOG"" >&2; exit 1; }
N433="$(grep -c 'ssh-key add.*--type authentication' "$GH_LOG" || true)"
[[ "$N433" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N433 for: grep -c 'ssh-key add.*--type authentication' "$GH_LOG"" >&2; exit 1; }
N434="$(grep -c 'ssh-key add.*--type signing' "$GH_LOG" || true)"
[[ "$N434" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N434 for: grep -c 'ssh-key add.*--type signing' "$GH_LOG"" >&2; exit 1; }
[[ -f "$CONFIG_REPO/fleet/machines/sha256-enrolltestkey.toml" ]] || { echo "ASSERT failed (line 434): [[ -f \"$CONFIG_REPO/fleet/machines/sha256-enrolltestkey.toml\" ]]" >&2; exit 1; }
grep -Fq 'ssh-rsa AAAATESTKEY' "$TEST_HOME/.ssh/allowed_signers"

# ==========================================================================
# 9. Failure after authorization stops that run; retry stays upload-safe.
# ==========================================================================
reset_state
if run_env MOCK_LIST_FAILS=1 /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/list-fail.out" 2>&1; then
    echo 'expected a listing failure to fail enrollment' >&2
    exit 1
fi
[[ -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 445): [[ -s \"$OSA_LOG\" ]]" >&2; exit 1; }
! grep -q 'ssh-key add' "$GH_LOG"
run_ssh enroll --authentication >/dev/null 2>&1
N449="$(grep -c 'ssh-key add.*--type authentication' "$GH_LOG" || true)"
[[ "$N449" == 1 ]] || { echo "ASSERT failed: expected 1, got \$N449 for: grep -c 'ssh-key add.*--type authentication' "$GH_LOG"" >&2; exit 1; }

reset_state
if run_env SSH_ADD_EXIT=1 /bin/bash "$SSH_ROOT/scripts/ssh" enroll --authentication >"$TEST_ROOT/agent-fail.out" 2>&1; then
    echo 'expected ssh-agent loading failure to fail authentication enrollment' >&2
    exit 1
fi
grep -Fq 'failed purposes: authentication' "$TEST_ROOT/agent-fail.out"
if grep -q 'SSH key loaded into agent' "$TEST_ROOT/agent-fail.out" \
    || grep -Eq 'ssh-key add|config set git_protocol' "$GH_LOG"; then
    echo 'failed key loading must not report success, upload, or switch protocols' >&2
    exit 1
fi

reset_state
run_ssh enroll --signing >"$TEST_ROOT/signing-agent.out" 2>&1
[[ -s "$SSH_ADD_LOG" ]] || { echo 'signing must load the private key into ssh-agent' >&2; exit 1; }
if grep -q 'config set git_protocol' "$GH_LOG"; then
    echo 'signing-only enrollment must not switch protocols' >&2
    exit 1
fi
[[ ! -e "$TEST_HOME/.ssh/config" ]]

reset_state
if run_env SSH_ADD_EXIT=1 /bin/bash "$SSH_ROOT/scripts/ssh" enroll --signing >"$TEST_ROOT/signing-agent-fail.out" 2>&1; then
    echo 'expected ssh-agent loading failure to fail signing enrollment' >&2
    exit 1
fi
[[ "$(run_env git config --global --get commit.gpgsign)" == false ]]
if grep -q 'ssh-key add' "$GH_LOG"; then
    echo 'failed key loading must not upload a signing key' >&2
    exit 1
fi

reset_state
run_env MANAGED_MACHINE_CONFIG_REPO_URL="$ORIGIN_REPO" /bin/bash -c '
    unset CONFIG_REPO_ROOT
    exec /bin/bash "$1/scripts/ssh" enroll --signing
' _ "$SSH_ROOT" >"$TEST_ROOT/signing-discovery.out" 2>&1
if grep -q 'unbound variable' "$TEST_ROOT/signing-discovery.out"; then
    echo 'signing must resolve the config checkout before reading the fleet registry' >&2
    exit 1
fi
[[ -d "$TEST_HOME/.local/share/managed-machine/managed-machine-config/.git" ]]

# ==========================================================================
# 10. ssh status is read-only and distinguishes not-enrolled from enrolled.
# ==========================================================================
reset_state
run_ssh status >"$TEST_ROOT/status-none.out" 2>&1
grep -Fqi 'not enrolled' "$TEST_ROOT/status-none.out"
grep -Fq 'ssh enroll' "$TEST_ROOT/status-none.out"

run_ssh enroll --authentication --fleet >/dev/null 2>&1
run_ssh status >"$TEST_ROOT/status-enrolled.out" 2>&1
grep -Fq 'SHA256:enrolltestkey' "$TEST_ROOT/status-enrolled.out"
grep -Fqi 'fleet' "$TEST_ROOT/status-enrolled.out"

# An agent context gets the refusal note plus read-only findings.
run_env MOCK_AGENTS_MEMBER=1 /bin/bash "$SSH_ROOT/scripts/ssh" status >"$TEST_ROOT/status-agent.out" 2>&1 || true
grep -Fqi 'agent' "$TEST_ROOT/status-agent.out"

# No secret material ever reaches output.
for out in "$TEST_ROOT"/*.out; do
    if grep -q 'private test key' "$out"; then
        echo "private key material leaked into $out" >&2
        exit 1
    fi
done

# ==========================================================================
# 11. setup-gh performs zero SSH-enrollment operations.
# ==========================================================================
reset_state
rm -f "$TEST_HOME/.gitconfig"

# Ahead/behind fixture: origin gets one new commit so the refresh can be
# observed; the checkout also gets a local-only commit which must NOT be
# pushed by the default path.
SEED_CLONE="$TEST_ROOT/seed-clone"
git clone --quiet "$ORIGIN_REPO" "$SEED_CLONE"
git -C "$SEED_CLONE" symbolic-ref HEAD refs/heads/main
git -C "$SEED_CLONE" config user.name 'seed'
git -C "$SEED_CLONE" config user.email 'seed@example.invalid'
printf 'remote-change\n' >"$SEED_CLONE/apps.json"
git -C "$SEED_CLONE" commit -aqm 'remote change'
git -C "$SEED_CLONE" push --quiet origin HEAD:main
printf 'local\n' >"$CONFIG_REPO/local-only.txt"
git -C "$CONFIG_REPO" add local-only.txt
git -C "$CONFIG_REPO" commit --quiet -m 'local-only commit'
LOCAL_ONLY_SHA="$(git -C "$CONFIG_REPO" rev-parse HEAD)"

if ! run_env /bin/bash "$ROOT/setup-gh" >"$TEST_ROOT/setup-gh.out" 2>&1; then
    echo 'setup-gh failed under stubs' >&2
    cat "$TEST_ROOT/setup-gh.out" >&2
    exit 1
fi

# Zero SSH operations.
[[ ! -s "$KEYGEN_LOG" ]] || { echo "ASSERT failed (line 504): [[ ! -s \"$KEYGEN_LOG\" ]]" >&2; exit 1; }
[[ ! -s "$SSH_ADD_LOG" ]] || { echo "ASSERT failed (line 505): [[ ! -s \"$SSH_ADD_LOG\" ]]" >&2; exit 1; }
[[ ! -s "$OSA_LOG" ]] || { echo "ASSERT failed (line 506): [[ ! -s \"$OSA_LOG\" ]]" >&2; exit 1; }
! grep -q 'ssh-key add' "$GH_LOG"
! grep -q 'auth refresh' "$GH_LOG"
! grep -q ' -s admin:' "$GH_LOG"
! grep -q 'config set git_protocol' "$GH_LOG"
! grep -q 'auth login' "$GH_LOG"
[[ ! -e "$TEST_HOME/.ssh" ]] || { echo "ASSERT failed (line 512): [[ ! -e \"$TEST_HOME/.ssh\" ]]" >&2; exit 1; }
[[ ! -f "$TEST_HOME/.config/managed-machine/machine.toml" ]] || { echo "ASSERT failed (line 513): [[ ! -f \"$TEST_HOME/.config/managed-machine/machine.toml\" ]]" >&2; exit 1; }
N528="$(git -C "$CONFIG_REPO" log --format=%s origin/main..HEAD)" || { echo "git log failed" >&2; exit 1; }
[[ "$N528" == 'local-only commit' ]] || { echo "ASSERT failed: setup-gh created config-repo commits: $N528" >&2; exit 1; }
if run_env git config --global --get gpg.format >/dev/null 2>&1; then
    echo 'setup-gh must not configure SSH signing' >&2
    exit 1
fi

# HTTPS path intact: credential helper wired, git identity configured.
grep -Fq 'auth setup-git' "$GH_LOG"
N523="$(run_env git config --global --get user.name)" || { echo "command failed: run_env git config --global --get user.name" >&2; exit 1; }
[[ "$N523" == 'qwts' ]] || { echo "ASSERT failed: expected 'qwts', got \$N523 for: run_env git config --global --get user.name" >&2; exit 1; }
N524="$(run_env git config --global --get user.email)" || { echo "command failed: run_env git config --global --get user.email" >&2; exit 1; }
[[ "$N524" == '1234+qwts@users.noreply.github.com' ]] || { echo "ASSERT failed: expected '1234+qwts@users.noreply.github.com', got \$N524 for: run_env git config --global --get user.email" >&2; exit 1; }

# Config checkout was refreshed from origin; local commits were not pushed.
N527="$(git -C "$CONFIG_REPO" merge-base HEAD origin/main)" || { echo "command failed: git -C "$CONFIG_REPO" merge-base HEAD origin/main" >&2; exit 1; }
[[ "$N527" == "$(git -C "$CONFIG_REPO" rev-parse origin/main)" ]] || { echo "ASSERT failed: expected "$(git -C "$CONFIG_REPO" rev-parse origin/main)", got \$N527 for: git -C "$CONFIG_REPO" merge-base HEAD origin/main" >&2; exit 1; }
if git -C "$ORIGIN_REPO" cat-file -e "$LOCAL_ONLY_SHA" 2>/dev/null; then
    echo 'setup-gh pushed local-only commits to origin' >&2
    exit 1
fi

# setup-gh advertises the explicit enrollment path instead of enrolling.
grep -Fq 'ssh enroll' "$TEST_ROOT/setup-gh.out"

# Source-level guard: the default path must not reference the enrollment
# machinery at all.
if grep -nE 'ssh-keygen|ssh-add|ssh-key add|git_protocol|gpgsign|gpg\.format|allowedSigners|allowed_signers|authorized_keys|ensure_github_ssh_key|ensure_ssh|add_key_to_agent|upload_ssh|register_current_machine|import_legacy_fleet|sync_local_|generate_fleet|admin:public_key|admin:ssh_signing_key' "$ROOT/setup-gh"; then
    echo 'setup-gh still contains SSH-enrollment operations' >&2
    exit 1
fi

# The account, update, and bootstrap paths carry no SSH enrollment either.
if grep -nE 'ssh-keygen|ssh-add|ssh-key add|register_current_machine|upload_ssh|ensure_github_ssh_key' \
    "$ROOT/scripts/account" "$ROOT/lib/account-setup.sh" "$ROOT/scripts/update" "$ROOT/scripts/bootstrap"; then
    echo 'account/update/bootstrap still contain SSH-enrollment operations' >&2
    exit 1
fi

# ==========================================================================
# 12. Config refresh: clean fast-forward, dirty-skip, never push.
# ==========================================================================
reset_state

# Behind origin: clean checkout fast-forwards.
git clone --quiet "$ORIGIN_REPO" "$TEST_ROOT/seed-clone2"
git -C "$TEST_ROOT/seed-clone2" symbolic-ref HEAD refs/heads/main
git -C "$TEST_ROOT/seed-clone2" config user.name s
git -C "$TEST_ROOT/seed-clone2" config user.email s@e.i
printf 'r2\n' >>"$TEST_ROOT/seed-clone2/local-bin.ref"
git -C "$TEST_ROOT/seed-clone2" commit -aqm 'remote2'
git -C "$TEST_ROOT/seed-clone2" push --quiet origin HEAD:main
run_env /bin/bash -c '
    source "$1/lib/install.sh"
    refresh_managed_machine_config_repo "$2"
' _ "$ROOT" "$CONFIG_REPO" >"$TEST_ROOT/refresh.out" 2>&1
N567="$(git -C "$CONFIG_REPO" rev-parse HEAD)" || { echo "command failed: git -C "$CONFIG_REPO" rev-parse HEAD" >&2; exit 1; }
[[ "$N567" == "$(git -C "$ORIGIN_REPO" rev-parse main)" ]] || { echo "ASSERT failed: expected "$(git -C "$ORIGIN_REPO" rev-parse main)", got \$N567 for: git -C "$CONFIG_REPO" rev-parse HEAD" >&2; exit 1; }

# Dirty checkout: refresh warns and leaves the tree untouched.
printf 'dirty\n' >"$CONFIG_REPO/dirty.txt"
run_env /bin/bash -c '
    source "$1/lib/install.sh"
    refresh_managed_machine_config_repo "$2"
' _ "$ROOT" "$CONFIG_REPO" >"$TEST_ROOT/refresh-dirty.out" 2>&1
grep -Fqi 'local changes' "$TEST_ROOT/refresh-dirty.out"
[[ -f "$CONFIG_REPO/dirty.txt" ]] || { echo "ASSERT failed (line 575): [[ -f \"$CONFIG_REPO/dirty.txt\" ]]" >&2; exit 1; }
rm -f "$CONFIG_REPO/dirty.txt"

# A local commit is never pushed by refresh.
printf 'local2\n' >"$CONFIG_REPO/local2.txt"
git -C "$CONFIG_REPO" add local2.txt
git -C "$CONFIG_REPO" commit --quiet -m 'local only'
LOCAL_SHA2="$(git -C "$CONFIG_REPO" rev-parse HEAD)"
run_env /bin/bash -c '
    source "$1/lib/install.sh"
    refresh_managed_machine_config_repo "$2"
' _ "$ROOT" "$CONFIG_REPO" >/dev/null 2>&1
if git -C "$ORIGIN_REPO" cat-file -e "$LOCAL_SHA2" 2>/dev/null; then
    echo 'refresh pushed a local commit to origin' >&2
    exit 1
fi

echo 'SSH enrollment tests passed'

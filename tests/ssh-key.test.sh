#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
SSH_KEYGEN_LOG="$TEST_ROOT/ssh-keygen.log"
export HOME="$TEST_HOME" SSH_KEYGEN_LOG
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"

cat >"$TEST_BIN/ssh-keygen" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$SSH_KEYGEN_LOG"

if [[ "$*" == *'-y -P '* ]]; then
    if [[ "${MOCK_KEY_ENCRYPTED:-0}" == "1" ]]; then
        exit 1
    fi
    echo 'ssh-rsa AAAAtest'
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
[[ -n "$target" ]]
printf 'private test key\n' >"$target"
printf 'ssh-rsa AAAAtest test@example\n' >"$target.pub"
EOF
chmod +x "$TEST_BIN/ssh-keygen"
PATH="$TEST_BIN:$PATH"
export PATH

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/ssh-key.sh
source "$ROOT/lib/ssh-key.sh"

reset_case() {
    rm -rf "$TEST_HOME/.ssh" "$TEST_HOME/.config/managed-machine"
    : >"$SSH_KEYGEN_LOG"
    unset MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE MOCK_KEY_ENCRYPTED
    unset -f ssh_key_interactive_input 2>/dev/null || true
    source "$ROOT/lib/ssh-key.sh"
}

file_mode() {
    if stat -c '%a' "$1" >/dev/null 2>&1; then
        stat -c '%a' "$1"
    else
        stat -f '%Lp' "$1"
    fi
}

PRIVATE_KEY="$TEST_HOME/.ssh/id_rsa_github"
PUBLIC_KEY="$PRIVATE_KEY.pub"
POLICY_FILE="$TEST_HOME/.config/managed-machine/ssh-key-policy.toml"

# No TTY fails before ssh-keygen and gives both safe and explicit remediation.
reset_case
ssh_key_interactive_input() { return 1; }
if ensure_github_ssh_key "$PRIVATE_KEY" "$PUBLIC_KEY" >"$TEST_ROOT/no-tty.out" 2>&1; then
    echo 'expected no-TTY key creation to fail closed' >&2
    exit 1
fi
[[ ! -e "$PRIVATE_KEY" && ! -e "$PUBLIC_KEY" ]]
[[ ! -s "$SSH_KEYGEN_LOG" ]]
grep -Fq 'managed-machine setup gh' "$TEST_ROOT/no-tty.out"
grep -Fq 'MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE=1' "$TEST_ROOT/no-tty.out"

# Explicit noninteractive opt-in creates and records an empty-passphrase key.
reset_case
MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE=1
export MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE
ensure_github_ssh_key "$PRIVATE_KEY" "$PUBLIC_KEY"
grep -Fq -- "-N  -C" "$SSH_KEYGEN_LOG"
grep -qxF 'passphrase_policy = "empty-explicit-opt-in"' "$POLICY_FILE"
grep -qxF 'opt_in_variable = "MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE"' "$POLICY_FILE"
[[ "$(file_mode "$PRIVATE_KEY")" == "600" ]]
[[ "$(file_mode "$POLICY_FILE")" == "600" ]]

# Invalid opt-in values fail without creating a key.
reset_case
MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE=true
export MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE
if ensure_github_ssh_key "$PRIVATE_KEY" "$PUBLIC_KEY" >"$TEST_ROOT/invalid-opt-in.out" 2>&1; then
    echo 'expected invalid empty-passphrase opt-in to fail' >&2
    exit 1
fi
grep -Fq 'must be exactly 1' "$TEST_ROOT/invalid-opt-in.out"
[[ ! -e "$PRIVATE_KEY" && ! -e "$PUBLIC_KEY" ]]

# Interactive empty passphrases are detected and the new pair is removed.
reset_case
ssh_key_interactive_input() { printf '%s\n' /dev/null; }
if ensure_github_ssh_key "$PRIVATE_KEY" "$PUBLIC_KEY" >"$TEST_ROOT/empty-interactive.out" 2>&1; then
    echo 'expected interactive empty passphrase to be rejected' >&2
    exit 1
fi
[[ ! -e "$PRIVATE_KEY" && ! -e "$PUBLIC_KEY" ]]
grep -Fq 'empty SSH key passphrase rejected' "$TEST_ROOT/empty-interactive.out"

# Interactive encrypted keys are retained and recorded.
reset_case
ssh_key_interactive_input() { printf '%s\n' /dev/null; }
MOCK_KEY_ENCRYPTED=1
export MOCK_KEY_ENCRYPTED
ensure_github_ssh_key "$PRIVATE_KEY" "$PUBLIC_KEY"
grep -qxF 'passphrase_policy = "encrypted"' "$POLICY_FILE"

# Existing complete pairs are reused without invoking ssh-keygen.
: >"$SSH_KEYGEN_LOG"
ensure_github_ssh_key "$PRIVATE_KEY" "$PUBLIC_KEY"
[[ ! -s "$SSH_KEYGEN_LOG" ]]

# Partial key pairs still fail safely.
reset_case
mkdir -p "$(dirname "$PRIVATE_KEY")"
touch "$PRIVATE_KEY"
if ensure_github_ssh_key "$PRIVATE_KEY" "$PUBLIC_KEY" >"$TEST_ROOT/incomplete.out" 2>&1; then
    echo 'expected incomplete key pair to fail' >&2
    exit 1
fi
grep -Fq 'incomplete key pair' "$TEST_ROOT/incomplete.out"

echo 'SSH key tests passed'

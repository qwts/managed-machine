#!/usr/bin/env bash
# setup-gh's key registration (#86): a registered key never opens a scope
# flow, an upload asks for the missing scopes once and only then, and a
# listing failure refuses to upload blind.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_BIN="$TEST_ROOT/bin"
GH_LOG="$TEST_ROOT/gh.log"
export GH_LOG
trap 'rm -rf "$TEST_ROOT"' EXIT
mkdir -p "$TEST_BIN"

# gh stub: the active account, its scopes, and the public key listings come
# from the environment; every call is logged so the test can see what setup
# would have done.
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GH_LOG"
case "$1 $2" in
    'auth status')
        if [[ "$*" == *'.login'* ]]; then
            printf '%s\n' "${MOCK_GH_LOGIN:-qwts}"
        else
            printf '%s\n' "${MOCK_GH_SCOPES:-repo, read:org}"
        fi
        ;;
    'auth refresh')
        [[ "${MOCK_REFRESH_FAILS:-0}" == 1 ]] && exit 1
        exit 0
        ;;
    'api --paginate')
        [[ "${MOCK_LIST_FAILS:-0}" == 1 ]] && { echo 'gh: HTTP 503' >&2; exit 1; }
        case "$3" in
            users/*/keys) printf '%s\n' "${MOCK_AUTH_KEYS:-}" ;;
            users/*/ssh_signing_keys) printf '%s\n' "${MOCK_SIGNING_KEYS:-}" ;;
            *) exit 1 ;;
        esac
        ;;
    'ssh-key add') exit 0 ;;
    *) exit 1 ;;
esac
EOF
cat >"$TEST_BIN/ssh-keygen" <<'EOF'
#!/usr/bin/env bash
echo '256 SHA256:testfingerprint test@example (ED25519)'
EOF
chmod +x "$TEST_BIN"/*
export PATH="$TEST_BIN:$PATH"

PUB="$TEST_ROOT/id_rsa_github.pub"
printf 'ssh-ed25519 AAAAmachinekey machine@example\n' >"$PUB"

# shellcheck source=lib/github-ssh-keys.sh
source "$ROOT/lib/github-ssh-keys.sh"

run_upload() {
    : >"$GH_LOG"
    upload_ssh_key_as "$1" "$2" "$PUB" >"$TEST_ROOT/out" 2>&1
}

# 1. Registered key, token without the upload scopes: no refresh, no upload.
# This is the --update that used to sit at a device-code prompt.
MOCK_AUTH_KEYS=$'ssh-rsa AAAAother\nssh-ed25519 AAAAmachinekey' run_upload authentication MacStudioM2-20260901
grep -Fq 'already registered on GitHub (authentication, SHA256:testfingerprint)' "$TEST_ROOT/out"
! grep -q '^auth refresh' "$GH_LOG"
! grep -q '^ssh-key add' "$GH_LOG"
grep -Fxq 'api --paginate users/qwts/keys --jq .[].key' "$GH_LOG"

# 1b. The presence check is an exact key-body match, not a substring: a key
# whose body merely contains ours is a different key.
MOCK_AUTH_KEYS='ssh-ed25519 AAAAmachinekeyXYZ' MOCK_GH_SCOPES='admin:public_key' run_upload authentication t
grep -q '^ssh-key add' "$GH_LOG"

# 2. Key absent, token without scopes: the report names the key and why, the
# missing scopes are refreshed once, then the key is added.
MOCK_AUTH_KEYS='ssh-rsa AAAAother' MOCK_SIGNING_KEYS='' run_upload authentication MacStudioM2-20260901
grep -Fq "Public key SHA256:testfingerprint is not among qwts's 1 registered authentication key(s) on GitHub" "$TEST_ROOT/out"
grep -Fq 'Token missing scopes: admin:public_key,admin:ssh_signing_key' "$TEST_ROOT/out"
[[ "$(grep -c '^auth refresh' "$GH_LOG")" == 1 ]]
grep -Fxq 'auth refresh -h github.com -s admin:public_key,admin:ssh_signing_key' "$GH_LOG"
grep -Fxq "ssh-key add $PUB --title MacStudioM2-20260901 --type authentication" "$GH_LOG"
# The refresh happens before the add, never after.
[[ "$(grep -n '^auth refresh' "$GH_LOG" | cut -d: -f1)" -lt "$(grep -n '^ssh-key add' "$GH_LOG" | cut -d: -f1)" ]]

# 3. Key absent, token already scoped: straight to the upload.
MOCK_SIGNING_KEYS='' MOCK_GH_SCOPES='repo, admin:public_key, admin:ssh_signing_key' run_upload signing t-signing
! grep -q '^auth refresh' "$GH_LOG"
grep -Fxq "ssh-key add $PUB --title t-signing --type signing" "$GH_LOG"
grep -Fq "not among qwts's 0 registered signing key(s)" "$TEST_ROOT/out"

# 4. Listing fails: refuse to upload blind (a duplicate is what the old
# substring-of-an-empty-listing check produced), and say so.
if MOCK_LIST_FAILS=1 run_upload authentication t; then
    echo 'expected a listing failure to fail the upload' >&2
    exit 1
fi
grep -Fq "could not list qwts's registered authentication keys on GitHub; not uploading blind" "$TEST_ROOT/out"
! grep -q '^ssh-key add' "$GH_LOG"
! grep -q '^auth refresh' "$GH_LOG"

# 5. A failed refresh stops before the add.
if MOCK_AUTH_KEYS='' MOCK_REFRESH_FAILS=1 run_upload authentication t; then
    echo 'expected a failed scope refresh to fail the upload' >&2
    exit 1
fi
! grep -q '^ssh-key add' "$GH_LOG"

# 6. Only the missing scopes are requested.
MOCK_AUTH_KEYS='' MOCK_GH_SCOPES='repo, admin:ssh_signing_key' run_upload authentication t
grep -Fxq 'auth refresh -h github.com -s admin:public_key' "$GH_LOG"

echo 'github-ssh-keys tests passed'

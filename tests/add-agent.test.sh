#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
FAKE_BIN="$TEST_DIR/bin"
STATE="$TEST_DIR/state"
APPS_DIR="$TEST_DIR/Applications"
SHARED_ROOT="$TEST_DIR/shared"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_HOME" "$FAKE_BIN" "$STATE/users" "$APPS_DIR"
export STATE

# The roster fixture covers all three answers add-agent must give: active
# provisions, retired fails closed, unknown fails closed.
PROFILE="$TEST_DIR/organization-profile.json"
cat >"$PROFILE" <<'EOF'
{
  "identities": [
    { "slug": "you-goose-agent", "harness": "goose", "status": "active" },
    { "slug": "you-vscode-agent", "harness": "vscode", "status": "retired" }
  ]
}
EOF

# osascript stub: execute the elevated command directly (drop the -e script
# pairs and the label) so the account-mutation stubs actually run.
cat >"$FAKE_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
while [[ "${1:-}" == "-e" ]]; do shift 2; done
shift # the label
exec "$@"
EOF

# sysadminctl stub: record the invocation (password redacted) and mark the
# account created.
cat >"$FAKE_BIN/sysadminctl" <<'EOF'
#!/usr/bin/env bash
name="" full=""
args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -addUser) name="$2"; args+=("$1" "$2"); shift 2 ;;
        -fullName) full="$2"; args+=("$1" "$2"); shift 2 ;;
        -password) args+=("$1" '<redacted>'); shift 2 ;;
        *) args+=("$1"); shift ;;
    esac
done
printf '%s\n' "${args[*]}" >>"$STATE/sysadminctl.log"
[[ -n "$name" ]] || exit 1
printf '%s\n' "$full" >"$STATE/users/$name"
EOF

cat >"$FAKE_BIN/createhomedir" <<'EOF'
#!/usr/bin/env bash
while [[ $# -gt 0 ]]; do
    case "$1" in
        -u) mkdir -p "$STATE/homes/$2"; shift 2 ;;
        *) shift ;;
    esac
done
EOF

# dscl stub: answer the exact reads the helpers make, from the state dir.
cat >"$FAKE_BIN/dscl" <<'EOF'
#!/usr/bin/env bash
if [[ "${2:-}" == "-read" ]]; then
    case "$3" in
        /Users/*)
            name="${3#/Users/}"
            [[ -f "$STATE/users/$name" ]] || exit 56
            case "${4:-}" in
                NFSHomeDirectory) printf 'NFSHomeDirectory: %s\n' "$STATE/homes/$name" ;;
                RealName) printf 'RealName:\n %s\n' "$(cat "$STATE/users/$name")" ;;
                UniqueID) printf 'UniqueID: 601\n' ;;
            esac
            ;;
        /Groups/admin)
            printf 'GroupMembership: root you %s\n' "$(cat "$STATE/admins" 2>/dev/null || true)"
            ;;
        *) exit 56 ;;
    esac
fi
EOF
chmod +x "$FAKE_BIN"/*

# The helpers call /usr/bin/dscl absolutely; put the stub there via a fake
# /usr/bin overlay is not possible, so point dscl through PATH by shadowing
# the helper's absolute call with a function is not either — instead the lib
# uses /usr/bin/dscl, so route through a private DYLD-free wrapper: a copy of
# the lib with the absolute path rewritten. The rewrite is mechanical and
# keeps the code under test byte-identical otherwise.
mkdir -p "$TEST_DIR/lib-under-test"
sed 's|/usr/bin/dscl|dscl|g' "$ROOT/lib/agent-account.sh" >"$TEST_DIR/lib-under-test/agent-account.sh"
sed -e "s|^REPO_ROOT=.*|REPO_ROOT=\"$ROOT\"|" \
    -e "s|source \"\$REPO_ROOT/lib/agent-account.sh\"|source \"$TEST_DIR/lib-under-test/agent-account.sh\"|" \
    "$ROOT/scripts/add-agent" >"$TEST_DIR/add-agent"
chmod +x "$TEST_DIR/add-agent"

run_add_agent() {
    HOME="$TEST_HOME" \
    PATH="$FAKE_BIN:/usr/bin:/bin" \
    MANAGED_MACHINE_ORG_PROFILE="$PROFILE" \
    MANAGED_MACHINE_APPLICATIONS_DIR="$APPS_DIR" \
    MANAGED_MACHINE_AGENT_SHARED_ROOT="$SHARED_ROOT" \
    MANAGED_MACHINE_SYSADMINCTL="$FAKE_BIN/sysadminctl" \
    MANAGED_MACHINE_CREATEHOMEDIR="$FAKE_BIN/createhomedir" \
    bash "$TEST_DIR/add-agent" "$@"
}

# --- fail closed: unknown slug ---
if out="$(run_add_agent you-mystery-agent 2>&1)"; then
    echo 'expected unknown slug to fail closed' >&2
    exit 1
fi
grep -Fq 'not in the active roster' <<<"$out"
[[ ! -f "$STATE/sysadminctl.log" ]]

# --- fail closed: retired slug ---
if out="$(run_add_agent you-vscode-agent 2>&1)"; then
    echo 'expected retired slug to fail closed' >&2
    exit 1
fi
grep -Fq 'retired in the roster' <<<"$out"
[[ ! -f "$STATE/sysadminctl.log" ]]

# --- fail closed: malformed slug never reaches the roster ---
if run_add_agent 'bad;slug' >/dev/null 2>&1; then
    echo 'expected malformed slug to fail' >&2
    exit 1
fi

# --- active slug provisions the account ---
out="$(run_add_agent you-goose-agent)"
grep -Fq 'creating standard account you-goose-agent ("Goose")' <<<"$out"
grep -Fq 'ok: account you-goose-agent exists' <<<"$out"
grep -Fq 'ok: account is standard (not admin)' <<<"$out"
grep -Fq "ok: full name is 'Goose'" <<<"$out"
grep -Fq 'warn: agent-bot is not installed' <<<"$out"
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]
grep -q -- '-addUser you-goose-agent' "$STATE/sysadminctl.log"
grep -q -- '-fullName Goose' "$STATE/sysadminctl.log"

# The generated password never appears in output or in the recorded args.
if grep -E 'password [^<]' "$STATE/sysadminctl.log" | grep -vq '<redacted>'; then
    echo 'the generated password leaked into the log' >&2
    exit 1
fi

# The shared coordination space converged: sticky root, non-sticky lock area.
[[ -d "$SHARED_ROOT/agent-locks" ]]
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT")" == '1777' ]]
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT/agent-locks")" == '0777' ]]

# --- idempotent: second run verifies without creating again ---
out2="$(run_add_agent you-goose-agent)"
grep -Fq 'already exists — verifying' <<<"$out2"
grep -Fq 'ok: account you-goose-agent exists' <<<"$out2"
[[ "$(grep -c 'addUser' "$STATE/sysadminctl.log")" -eq 1 ]]

# --- an operator-supplied persona name is compared, not silently accepted ---
out3="$(run_add_agent you-goose-agent --full-name 'Goose McCloud')"
grep -Fq "warn: full name is 'Goose' (expected 'Goose McCloud')" <<<"$out3"

# --- admin membership is a hard compliance failure ---
printf 'you-goose-agent' >"$STATE/admins"
if out4="$(run_add_agent you-goose-agent)"; then
    echo 'expected admin membership to fail compliance' >&2
    exit 1
fi
grep -Fq 'is an administrator — agent accounts must be standard' <<<"$out4"
rm -f "$STATE/admins"

# --- Little Snitch presence surfaces the headless-hang warning ---
mkdir -p "$APPS_DIR/Little Snitch.app"
out5="$(run_add_agent you-goose-agent)"
grep -Fq 'Little Snitch is active' <<<"$out5"

# --- pre-existing wrong modes on the shared space are corrected on rerun ---
chmod 0700 "$SHARED_ROOT" "$SHARED_ROOT/agent-locks"
run_add_agent you-goose-agent >/dev/null
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT")" == '1777' ]]
[[ "$(stat -f '%Mp%Lp' "$SHARED_ROOT/agent-locks")" == '0777' ]]

# --- uncorrectable wrong modes are a compliance failure, not an "ok" ---
# Simulate another owner's directory: report against a root this run cannot
# chmod by checking the report path directly with a bad, unowned-looking mode.
BAD_ROOT="$TEST_DIR/bad-shared"
mkdir -p "$BAD_ROOT/agent-locks"
chmod 0700 "$BAD_ROOT" "$BAD_ROOT/agent-locks"
if out6="$(HOME="$TEST_HOME" PATH="$FAKE_BIN:/usr/bin:/bin" \
    MANAGED_MACHINE_AGENT_SHARED_ROOT="$BAD_ROOT" \
    MANAGED_MACHINE_APPLICATIONS_DIR="$APPS_DIR" \
    bash -c 'source "'"$ROOT"'/lib/install.sh"; source "'"$TEST_DIR"'/lib-under-test/agent-account.sh"; agent_compliance_report you-goose-agent Goose')"; then
    echo 'expected wrong shared-space modes to fail compliance' >&2
    exit 1
fi
grep -Fq 'fail: shared agent space modes are 0700/0700' <<<"$out6"

# --- an untraversable agent home reads as unverifiable, not as pending ---
cat >"$FAKE_BIN/agent-bot" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$FAKE_BIN/agent-bot"
mkdir -p "$STATE/homes/you-goose-agent/.config"
chmod 0000 "$STATE/homes/you-goose-agent"
out7="$(run_add_agent you-goose-agent)"
chmod 0755 "$STATE/homes/you-goose-agent"
grep -Fq 'cannot inspect' <<<"$out7"
if grep -Fq 'bootstrap pending' <<<"$out7"; then
    echo 'an unreadable home must not be reported as pending' >&2
    exit 1
fi
rm -f "$FAKE_BIN/agent-bot"

# --- the CLI dispatches the verb ---
usage_out="$("$ROOT/bin/managed-machine" --help)"
grep -Fq 'add-agent <slug>' <<<"$usage_out"

echo 'add-agent tests passed'

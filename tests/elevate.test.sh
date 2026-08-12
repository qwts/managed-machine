#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_BIN="$TEST_DIR/bin"
OSA_LOG="$TEST_DIR/osascript.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_BIN"
cat >"$TEST_BIN/osascript" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$OSA_LOG"
exit "${OSA_EXIT:-0}"
EOF
cat >"$TEST_BIN/uname" <<'EOF'
#!/usr/bin/env bash
echo Darwin
EOF
chmod +x "$TEST_BIN"/*

export PATH="$TEST_BIN:$PATH" OSA_LOG
# shellcheck source=lib/elevate.sh
source "$ROOT/lib/elevate.sh"

# Success: the label and every argument reach osascript, quoted per-argument
# through AppleScript argv (no shell string is built by the caller).
unset MANAGED_MACHINE_BOOTSTRAP_MODE || true
elevate_run 'install the test payload' /usr/bin/true "arg with spaces" >"$TEST_DIR/ok.out"
grep -Fq 'install the test payload' "$OSA_LOG"
grep -Fq 'arg with spaces' "$OSA_LOG"
grep -Fq 'with administrator privileges' "$OSA_LOG"
grep -Fq 'Requesting administrator authorization to install the test payload' "$TEST_DIR/ok.out"

# Cancelled dialog: clean failure, clear message, nonzero exit.
if OSA_EXIT=1 elevate_run 'do the cancelled thing' /usr/bin/true >"$TEST_DIR/cancel.out" 2>&1; then
    echo 'expected cancelled authorization to fail' >&2
    exit 1
fi
grep -Fq 'cancelled or failed — did not do the cancelled thing' "$TEST_DIR/cancel.out"

# Noninteractive bootstrap never pops a dialog.
: >"$OSA_LOG"
if MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive elevate_run 'noninteractive thing' /usr/bin/true >"$TEST_DIR/nonint.out" 2>&1; then
    echo 'expected noninteractive elevation to fail' >&2
    exit 1
fi
grep -Fq 'authorization dialog unavailable' "$TEST_DIR/nonint.out"
[[ ! -s "$OSA_LOG" ]]

# A missing command is a usage error.
if elevate_run 'no command' >"$TEST_DIR/usage.out" 2>&1; then
    echo 'expected missing command to fail' >&2
    exit 1
fi
grep -Fq 'requires a command' "$TEST_DIR/usage.out"

echo 'elevate tests passed'

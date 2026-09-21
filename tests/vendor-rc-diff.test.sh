#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
trap 'rm -rf "$TEST_DIR"' EXIT

export TEST_HOME
export HOME="$TEST_HOME"
mkdir -p "$TEST_HOME"

# shellcheck source=../lib/install.sh
source "$ROOT/lib/install.sh"

# Helpers --------------------------------------------------------------
reset_home() {
    rm -rf "$TEST_HOME"
    mkdir -p "$TEST_HOME"
    printf '%s\n' 'alias a=1' >"$TEST_HOME/.zshrc"
}

snapshot_before() {
    snapshot_startup_files >"$TEST_DIR/before"
}

run_report() {
    snapshot_startup_files >"$TEST_DIR/after"
    report_vendor_startup_edits "$TEST_DIR/before" "$TEST_DIR/after" "$1" 2>&1 || true
}

# --- no change -> silent -------------------------------------------------
reset_home
snapshot_before
REPORT="$(run_report demo)"
[[ -z "$REPORT" ]]

# --- guarded-only change -> ok note, never a warn --------------------------
reset_home
snapshot_before
printf '%s\n' 'alias a=1' '# BEGIN vendor' 'export PATH="${HOME}/.local/bin:${PATH}"' '# END vendor' \
    >"$TEST_HOME/.zshrc"
REPORT="$(run_report demo)"
[[ "$REPORT" == *"ok: demo touched a startup file but added no unguarded PATH line"* ]]
! grep -q '^warn:' <<<"$REPORT"

# --- non-PATH line added -> ok note, not a warn -----------------------------
reset_home
snapshot_before
printf '%s\n' 'alias a=1' 'alias dev="cd ~/code"' >"$TEST_HOME/.zshrc"
REPORT="$(run_report demo)"
[[ "$REPORT" == *"ok: demo touched a startup file but added no unguarded PATH line"* ]]
! grep -q '^warn:' <<<"$REPORT"

# --- unterminated BEGIN does not suppress the export after it ----------------
reset_home
snapshot_before
printf '%s\n' 'alias a=1' '# BEGIN orphan' 'export PATH="$HOME/.local/bin:$PATH"' >"$TEST_HOME/.zshrc"
REPORT="$(run_report demo)"
grep -qF "warn: demo added unguarded line to $TEST_HOME/.zshrc: export PATH=\"\$HOME/.local/bin:\$PATH\"" <<<"$REPORT"

# --- vendor moves an export out of a closed block -> relocated leak reported -
reset_home
printf '%s\n' 'alias a=1' '# BEGIN vendor' 'export PATH="${HOME}/.local/bin:${PATH}"' '# END vendor' >"$TEST_HOME/.zshrc"
snapshot_before
printf '%s\n' 'alias a=1' 'export PATH="${HOME}/.local/bin:${PATH}"' >"$TEST_HOME/.zshrc"
REPORT="$(run_report demo)"
grep -qF "warn: demo added unguarded line to $TEST_HOME/.zshrc: export PATH=\"\${HOME}/.local/bin:\${PATH}\"" <<<"$REPORT"
grep -qF "warn: demo edited a startup file outside managed-machine's guards — run 'managed-machine setup zsh' to re-own PATH" <<<"$REPORT"

# --- a pre-existing leak on re-run is quiet (idempotent) ----------------------
REPORT="$(run_report demo)"
! grep -q '^warn:' <<<"$REPORT"

# --- but appending a second copy of an existing unguarded export warns again --
printf '%s\n' 'export PATH="${HOME}/.local/bin:${PATH}"' >>"$TEST_HOME/.zshrc"
REPORT="$(run_report demo)"
grep -qF "warn: demo added unguarded line to $TEST_HOME/.zshrc: export PATH=\"\${HOME}/.local/bin:\${PATH}\"" <<<"$REPORT"

# --- unguarded line added to .zprofile -> warn names file + line ------------
reset_home
snapshot_before
printf '%s\n' 'export PATH="$HOME/.config/agent-bot/bin:$PATH"  # leak' >>"$TEST_HOME/.zprofile"
REPORT="$(run_report demo)"
grep -qF "warn: demo added unguarded line to $TEST_HOME/.zprofile: export PATH=\"\$HOME/.config/agent-bot/bin:\$PATH\"  # leak" <<<"$REPORT"
grep -qF "warn: demo edited a startup file outside managed-machine's guards — run 'managed-machine setup zsh' to re-own PATH" <<<"$REPORT"

# --- foreign vendor dir (.uv/bin) -> warn with a manual-review remedy, the
# --- managed-dir remediation would be wrong because refresh won't move it ----
reset_home
snapshot_before
printf '%s\n' 'export PATH="$HOME/.uv/bin:$PATH"' >>"$TEST_HOME/.zshrc"
REPORT="$(run_report demo)"
grep -qF "warn: demo added unguarded line to $TEST_HOME/.zshrc: export PATH=\"\$HOME/.uv/bin:\$PATH\"" <<<"$REPORT"
grep -qF "warn: demo added an unmanaged PATH line to a startup file — review it and remove it manually if unwanted" <<<"$REPORT"
! grep -qF 'managed-machine setup zsh' <<<"$REPORT"

# --- brew shellenv carry-over never reported ---------------------------------
reset_home
snapshot_before
printf '%s\n' 'eval "$(/opt/homebrew/bin/brew shellenv)"' >>"$TEST_HOME/.zprofile"
REPORT="$(run_report demo)"
! grep -q '^warn:' <<<"$REPORT"

# --- cargo env carry-over never reported --------------------------------------
reset_home
snapshot_before
printf '%s\n' '[[ -f "$HOME/.cargo/env" ]] && source "$HOME/.cargo/env"' >>"$TEST_HOME/.zshrc"
REPORT="$(run_report demo)"
! grep -q '^warn:' <<<"$REPORT"

# --- unguarded line added to .zshenv is reported with its file ----------------
reset_home
snapshot_before
printf '%s\n' 'export PATH="$HOME/.config/agent-bot/bin:$PATH"' >>"$TEST_HOME/.zshenv"
REPORT="$(run_report demo)"
grep -qF "warn: demo added unguarded line to $TEST_HOME/.zshenv: export PATH=\"\$HOME/.config/agent-bot/bin:\$PATH\"" <<<"$REPORT"
grep -qF "warn: demo edited a startup file outside managed-machine's guards — run 'managed-machine setup zsh' to re-own PATH" <<<"$REPORT"

# --- Integration: install_official_cli reports an unguarded add -------------
reset_home
stub_installer() {
    printf 'echo '\''export PATH="$HOME/.local/bin:$PATH"'\'' >>"$TEST_HOME/.zshrc"\n'
}
curl() { stub_installer; }
export -f curl stub_installer

ERR_OUT="$TEST_DIR/install.err"
set +e
install_official_cli "demo vendor" demo-tool "https://vendor.example/demo.sh" \
    >/dev/null 2>"$ERR_OUT"
set -e
grep -qF "warn: demo vendor added unguarded line to $TEST_HOME/.zshrc: export PATH=\"\$HOME/.local/bin:\$PATH\"" "$ERR_OUT"
grep -qF "warn: demo vendor edited a startup file outside managed-machine's guards — run 'managed-machine setup zsh' to re-own PATH" "$ERR_OUT"

# --- Integration: re-running after the leak is quiet (idempotent) -----------
set +e
install_official_cli "demo vendor" demo-tool "https://vendor.example/demo.sh" \
    >/dev/null 2>"$ERR_OUT"
set -e
[[ "$(grep -c '^warn:' "$ERR_OUT" || true)" == "0" ]]
! grep -qF "added unguarded line" "$ERR_OUT"
! grep -qF "edited a startup file outside managed-machine's guards" "$ERR_OUT"

echo "vendor-rc-diff tests passed"
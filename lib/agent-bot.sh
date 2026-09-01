#!/usr/bin/env bash
# Install and wire the agent-bot identity runtime.
#
# agent-bot ships from the reviewed self-tap qwts/agent-bot-identity — a
# deliberate, documented exception to the homebrew/core-only catalog rule in
# AGENTS.md. A catalog row would weaken that rule for every tap; this script
# keeps the exception explicit, reviewable, and pinned to one known tap. The
# tap's formula pins a tagged release tarball by sha256, and `brew pin` holds
# the installed version so upgrading agent-bot is a deliberate act rather
# than drift on the next `brew upgrade`.
#
# The machine phase (`agent-bot bootstrap --machine-only`) is headless-safe —
# no TTY, no macOS Keychain — but it live-mints an installation token for
# every App in the roster, so it needs network and, on a fresh machine, an
# unlocked pass-cli session. A locked or absent provider is the normal
# first-run outcome on an unattended machine (preflight on #75): the wiring
# reads agent-bot's typed readiness codes and defers on provider states
# instead of failing the run, and fails closed on everything else.

AGENT_BOT_TAP="qwts/agent-bot-identity"
AGENT_BOT_TAP_URL="https://github.com/qwts/agent-bot-identity.git"
AGENT_BOT_FORMULA="agent-bot"
AGENT_BOT_QUALIFIED_FORMULA="qwts/agent-bot-identity/agent-bot"
AGENT_BOT_PROFILE_URL="https://raw.githubusercontent.com/qwts/playbook-engineering/main/governance/organization-profile.json"
AGENT_BOT_DOCTOR_SCHEMA_VERSION=1
# Readiness codes that mean "retry once a human logs in / unlocks pass-cli",
# not "the wiring was rejected".
AGENT_BOT_PROVIDER_CODES='provider-(session-required|unavailable|locked)'

# The runtime's own machine gate. Trusted over the bootstrap exit code: a
# wired machine is one doctor vouches for, nothing less.
agent_bot_doctor_machine_gate() {
    local cli="$1"
    "$cli" doctor --machine-only --json \
        --require-schema-version "$AGENT_BOT_DOCTOR_SCHEMA_VERSION"
}

agent_bot_machine_is_wired() {
    local cli
    cli="$(agent_bot_cli_path 2>/dev/null)" || return 1
    agent_bot_doctor_machine_gate "$cli" >/dev/null 2>&1
}

# Print the git checkout directory behind ~/.local/bin/agent-bot, if any.
# agent-bot's installer refuses to replace a developer checkout link, so the
# conflict is detected here instead of surfacing as an opaque installer error.
agent_bot_local_bin_checkout() {
    local link="$HOME/.local/bin/agent-bot" target dir
    [[ -L "$link" ]] || return 1
    target="$(readlink "$link")" || return 1
    case "$target" in
        /*) ;;
        *) target="$HOME/.local/bin/$target" ;;
    esac
    dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd)" || return 1
    while [[ "$dir" != "/" ]]; do
        if [[ -e "$dir/.git" ]]; then
            printf '%s\n' "$dir"
            return 0
        fi
        dir="$(dirname "$dir")"
    done
    return 1
}

agent_bot_brew_supports_trust() {
    brew commands 2>/dev/null | tr -s ' \t' '\n' | grep -qx trust
}

agent_bot_tap_present() {
    brew tap 2>/dev/null | grep -qxF "$AGENT_BOT_TAP"
}

agent_bot_formula_installed() {
    local out
    out="$(brew list --versions "$AGENT_BOT_FORMULA" 2>/dev/null)" || return 1
    [[ "$out" == "$AGENT_BOT_FORMULA "* || "$out" == "$AGENT_BOT_FORMULA" ]]
}

agent_bot_formula_pinned() {
    brew list --pinned 2>/dev/null | grep -qxF "$AGENT_BOT_FORMULA"
}

# Tap and install through brew_run: the prefix is admin-owned, so this
# elevates like every other formula install, and in a noninteractive run the
# elevation layer defers instead of popping a dialog — propagate its status.
install_agent_bot_runtime() {
    if ! ensure_brew_on_path; then
        echo "Error: brew required — run setup-brew first" >&2
        return 1
    fi
    if agent_bot_formula_installed; then
        echo "agent-bot already installed"
        brew list --versions "$AGENT_BOT_FORMULA" || true
        # A prior run interrupted between install and pin leaves the runtime
        # unpinned; re-apply the pin instead of reporting converged state.
        if ! agent_bot_formula_pinned; then
            brew_run pin "$AGENT_BOT_FORMULA" || return $?
            echo "agent-bot pinned"
        fi
        return 0
    fi
    if ! agent_bot_tap_present; then
        echo "Tapping $AGENT_BOT_TAP..."
        brew_run tap "$AGENT_BOT_TAP" "$AGENT_BOT_TAP_URL" || return $?
    fi
    # Newer Homebrew refuses formulae from untrusted third-party taps, and
    # the failure reads like a network error. Detect the subcommand rather
    # than assume every Homebrew has it.
    if agent_bot_brew_supports_trust; then
        brew_run trust "$AGENT_BOT_TAP" || return $?
    fi
    echo "Installing $AGENT_BOT_QUALIFIED_FORMULA (tagged release, not head)..."
    brew_run install "$AGENT_BOT_QUALIFIED_FORMULA" || return $?
    if ! agent_bot_formula_installed; then
        echo "Install finished but $AGENT_BOT_FORMULA was not found." >&2
        return 1
    fi
    brew_run pin "$AGENT_BOT_FORMULA" || return $?
    echo "agent-bot installed and pinned"
    brew list --versions "$AGENT_BOT_FORMULA" || true
}

# Wire machine state from the published governance projection. The profile is
# fetched into memory and piped on stdin (`--profile -`) so governance data
# never lands in a temp file and there is no cleanup path that can fail.
wire_agent_bot_machine() {
    local cli profile out result=0
    ensure_local_bin_in_zshrc "${HOME}/.zshrc"
    export_local_bin_to_path
    cli="$(agent_bot_cli_path)" || return 1
    echo "Fetching the published organization profile..."
    if ! profile="$(curl -fsSL "$AGENT_BOT_PROFILE_URL")" || [[ -z "$profile" ]]; then
        echo "Error: could not fetch the organization profile from $AGENT_BOT_PROFILE_URL" >&2
        echo "Refusing to wire agent-bot without the governance profile — agents would keep committing as the human identity." >&2
        return 1
    fi
    echo "Wiring machine state (agent-bot bootstrap --machine-only)..."
    out="$(printf '%s\n' "$profile" \
        | "$cli" bootstrap --profile - --with-gh-shim --machine-only --json 2>&1)" \
        || result=$?
    if [[ "$result" -ne 0 ]]; then
        [[ -z "$out" ]] || printf '%s\n' "$out" >&2
        if grep -qE "$AGENT_BOT_PROVIDER_CODES" <<<"$out"; then
            defer_setup "agent-bot machine wiring is waiting on the secret provider (pass-cli login or session unlock), then re-run setup agent-bot" \
                || return $?
        fi
        echo "Error: agent-bot bootstrap refused the machine wiring; the machine is not wired" >&2
        return 1
    fi
    verify_agent_bot_machine "$cli"
}

verify_agent_bot_machine() {
    local cli="${1:-}" out
    if [[ -z "$cli" ]]; then
        cli="$(agent_bot_cli_path)" || return 1
    fi
    if ! out="$(agent_bot_doctor_machine_gate "$cli" 2>&1)"; then
        [[ -z "$out" ]] || printf '%s\n' "$out" >&2
        echo "Error: agent-bot doctor did not pass the machine gate; refusing to report this machine as wired" >&2
        return 1
    fi
    echo "agent-bot machine wiring verified: doctor --machine-only passed (schema version $AGENT_BOT_DOCTOR_SCHEMA_VERSION)."
}

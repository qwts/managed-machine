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
AGENT_BOT_PROFILE_URL="https://raw.githubusercontent.com/qwts/qwts-agent-org/main/governance/organization-profile.json"
AGENT_BOT_FORMULA_URL="https://raw.githubusercontent.com/qwts/agent-bot-identity/main/Formula/agent-bot.rb"
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

# True when a resolved path lives under the Homebrew prefix. The prefix is
# itself a git checkout on ARM Macs (/opt/homebrew/.git), so a brew-owned
# path must not read as a developer checkout.
agent_bot_dir_in_brew_prefix() {
    local dir="$1" prefix
    for prefix in "${HOMEBREW_PREFIX:-}" \
        "$(brew_prefix_path 2>/dev/null || true)" /opt/homebrew /usr/local; do
        [[ -n "$prefix" ]] || continue
        case "$dir" in
            "$prefix"/*) return 0 ;;
        esac
    done
    return 1
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
    # The wired state is itself a link at the brew stable entrypoint; the
    # deferred-provider retry must re-wire, not hard-stop on the prefix.
    if agent_bot_dir_in_brew_prefix "$dir"; then
        return 1
    fi
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
agent_bot_installed_version() {
    brew list --versions "$AGENT_BOT_FORMULA" 2>/dev/null | awk 'NR==1{print $2}'
}

# The tagged version a formula file points at. The formula is parsed, never
# loaded through brew: Homebrew's tap trust is per user, so loading it as
# anyone but the account that ran `brew trust` is refused, and a version
# check must never need an authorization prompt.
agent_bot_formula_file_version() {
    sed -n 's|^  url ".*/refs/tags/v\([0-9][0-9.]*\)\.tar\.gz"$|\1|p' "$1" | head -1
}

# The version the tap checkout on disk currently carries. It only advances
# with `brew update`, which runs as the prefix owner (one prompt).
agent_bot_tap_version() {
    local tap
    tap="$(brew --repository "$AGENT_BOT_TAP" 2>/dev/null)" || return 1
    [[ -r "$tap/Formula/$AGENT_BOT_FORMULA.rb" ]] || return 1
    agent_bot_formula_file_version "$tap/Formula/$AGENT_BOT_FORMULA.rb"
}

# The version the tap publishes right now: the newer of the formula fetched
# from the tap's main branch and the tap checkout on disk. The fetch lets a
# machine whose checkout is stale see the new tag without a prompt; the
# checkout covers the fetch being served from a cache that lags a release by
# minutes (raw.githubusercontent.com caches for five), which otherwise reads
# as "current" in the --update that follows a release. Offline, the on-disk
# tap is the best available answer.
agent_bot_published_version() {
    local fetched tap
    fetched="$(curl -fsSL --max-time 15 "$AGENT_BOT_FORMULA_URL" 2>/dev/null | agent_bot_formula_file_version /dev/stdin)" || fetched=""
    tap="$(agent_bot_tap_version 2>/dev/null)" || tap=""
    [[ -n "$fetched" || -n "$tap" ]] || return 1
    printf '%s\n' "$fetched" "$tap" | grep -v '^$' | sort -V | tail -1
}

# True when the pinned install is behind the published tag. The pin stops
# `brew upgrade` from moving agent-bot as a side effect of an unrelated
# update; this is the deliberate path that moves it and re-wires the machine.
agent_bot_formula_outdated() {
    local installed published
    installed="$(agent_bot_installed_version)"
    published="$(agent_bot_published_version)" || return 1
    [[ -n "$installed" && -n "$published" && "$installed" != "$published" ]]
}

# Move the pinned runtime to the published tagged release in one
# authorization: refresh the tap when its checkout is behind, then unpin,
# upgrade, pin. The caller re-runs the machine wiring afterwards so the
# identity daemon restarts on the new runtime and the hooks are re-read.
upgrade_agent_bot_runtime() {
    local target script installed
    target="$(agent_bot_published_version)" || return 1
    script="brew unpin '$AGENT_BOT_FORMULA' && brew upgrade '$AGENT_BOT_QUALIFIED_FORMULA' && brew pin '$AGENT_BOT_FORMULA'"
    if [[ "$(agent_bot_tap_version 2>/dev/null)" != "$target" ]]; then
        script="brew update && $script"
    fi
    echo "Upgrading agent-bot $(agent_bot_installed_version) -> $target (tagged release, one authorization)..."
    brew_run_script "$script" || return $?
    if ! agent_bot_formula_pinned; then
        echo "Error: agent-bot is not pinned after the upgrade; re-run setup agent-bot" >&2
        return 1
    fi
    installed="$(agent_bot_installed_version)"
    if [[ "$installed" != "$target" ]]; then
        echo "Error: agent-bot is $installed after the upgrade, expected $target; re-run setup agent-bot" >&2
        return 1
    fi
    echo "agent-bot upgraded and pinned"
    brew list --versions "$AGENT_BOT_FORMULA" || true
}

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

# park_agent_bot_dev_link: move a developer-checkout link at
# ~/.local/bin/agent-bot aside so the install can proceed (#91). The link is
# renamed next to itself with a timestamp, never deleted, and the checkout it
# points at is untouched: `mv` it back to undo. Prints the parked path.
park_agent_bot_dev_link() {
    local link="$HOME/.local/bin/agent-bot" parked
    parked="${link}.devlink-$(date +%Y%m%d-%H%M%S)"
    [[ ! -e "$parked" ]] || parked="${parked}-$$"
    mv "$link" "$parked" || return 1
    printf '%s\n' "$parked"
}

#!/usr/bin/env bash
# Explicit, restorable convergence for agent-bot's Codex desktop gh interposer.

agent_bot_gh_marker_path() {
    printf '%s/.config/managed-machine/agent-bot-gh-interposer\n' "$HOME"
}

agent_bot_gh_pending_restore_path() {
    printf '%s.restore-pending\n' "$(agent_bot_gh_marker_path)"
}

agent_bot_cli_path() {
    if [[ -x "$HOME/.local/bin/agent-bot" ]]; then
        printf '%s/.local/bin/agent-bot\n' "$HOME"
        return 0
    fi
    if command -v agent-bot >/dev/null 2>&1; then
        command -v agent-bot
        return 0
    fi
    echo "Error: agent-bot is not installed; install the reviewed runtime first" >&2
    return 1
}

agent_bot_homebrew_prefix() {
    local prefix="${HOMEBREW_PREFIX:-}"
    if [[ -z "$prefix" ]]; then
        if ! ensure_brew_on_path; then
            echo "Error: brew required — run setup-brew first" >&2
            return 1
        fi
        prefix="$(brew --prefix 2>/dev/null)" || {
            echo "Error: could not resolve the Homebrew prefix" >&2
            return 1
        }
    fi
    case "$prefix" in
        /*) printf '%s\n' "$prefix" ;;
        *)
            echo "Error: Homebrew returned a non-absolute prefix" >&2
            return 1
            ;;
    esac
}

agent_bot_homebrew_gh_path() {
    local prefix
    prefix="$(agent_bot_homebrew_prefix)" || return 1
    printf '%s/bin/gh\n' "$prefix"
}

agent_bot_gh_is_configured() {
    local marker pending
    marker="$(agent_bot_gh_marker_path)"
    pending="$(agent_bot_gh_pending_restore_path)"
    [[ -e "$marker" || -L "$marker" || -e "$pending" || -L "$pending" ]]
}

read_agent_bot_gh_marker() {
    local marker pending path line_count
    marker="$(agent_bot_gh_marker_path)"
    pending="$(agent_bot_gh_pending_restore_path)"
    if [[ -e "$pending" || -L "$pending" ]]; then
        echo "Error: agent-bot gh restore is incomplete; inspect $pending before retrying" >&2
        return 1
    fi
    if [[ ! -f "$marker" || -L "$marker" ]]; then
        echo "Error: agent-bot gh interposition is not explicitly configured" >&2
        return 1
    fi
    IFS= read -r path <"$marker" || true
    line_count="$(wc -l <"$marker" | tr -d ' ')"
    if [[ "$line_count" != "1" ]]; then
        echo "Error: agent-bot gh interposition marker is malformed" >&2
        return 1
    fi
    case "$path" in
        /*/gh) printf '%s\n' "$path" ;;
        *)
            echo "Error: agent-bot gh interposition marker is malformed" >&2
            return 1
            ;;
    esac
}

record_agent_bot_gh_marker() {
    local path="$1" marker temporary
    marker="$(agent_bot_gh_marker_path)"
    mkdir -p "$(dirname "$marker")"
    umask 077
    temporary="$(mktemp "${marker}.tmp.XXXXXX")"
    printf '%s\n' "$path" >"$temporary"
    chmod 600 "$temporary"
    mv -f "$temporary" "$marker"
}

install_agent_bot_gh_interposer() {
    local cli gh_path configured_path marker recorded=0
    cli="$(agent_bot_cli_path)" || return 1
    gh_path="$(agent_bot_homebrew_gh_path)" || return 1
    if agent_bot_gh_is_configured; then
        configured_path="$(read_agent_bot_gh_marker)" || return 1
        if [[ "$configured_path" != "$gh_path" ]]; then
            echo "Error: configured agent-bot gh path does not match the current Homebrew prefix" >&2
            return 1
        fi
    fi
    if [[ ! -x "$gh_path" ]]; then
        echo "Error: stock Homebrew gh is missing at $gh_path; run setup-gh first" >&2
        return 1
    fi
    marker="$(agent_bot_gh_marker_path)"
    if [[ ! -e "$marker" && ! -L "$marker" ]]; then
        # Record explicit consent before runtime mutation. If the process dies
        # after interposition, update repair still sees configured state.
        record_agent_bot_gh_marker "$gh_path"
        recorded=1
    fi
    if ! "$cli" install-gh-shim --codex-desktop-gh "$gh_path"; then
        [[ "$recorded" == "0" ]] || rm -f "$marker"
        return 1
    fi
    echo "Configured explicit Codex desktop gh interposition: $gh_path"
}

repair_agent_bot_gh_if_configured() {
    local cli configured_path current_path
    agent_bot_gh_is_configured || return 0
    configured_path="$(read_agent_bot_gh_marker)" || return 1
    current_path="$(agent_bot_homebrew_gh_path)" || return 1
    if [[ "$configured_path" != "$current_path" ]]; then
        echo "Error: configured agent-bot gh path does not match the current Homebrew prefix" >&2
        return 1
    fi
    if [[ ! -x "$configured_path" ]]; then
        echo "Error: configured Homebrew gh path is missing: $configured_path" >&2
        return 1
    fi
    cli="$(agent_bot_cli_path)" || return 1
    "$cli" install-gh-shim --codex-desktop-gh "$configured_path"
    echo "Reconciled explicit Codex desktop gh interposition: $configured_path"
}

restore_agent_bot_gh_interposer() {
    local cli gh_path marker pending
    gh_path="$(read_agent_bot_gh_marker)" || return 1
    cli="$(agent_bot_cli_path)" || return 1
    marker="$(agent_bot_gh_marker_path)"
    pending="$(agent_bot_gh_pending_restore_path)"
    mv "$marker" "$pending"
    if ! "$cli" install-gh-shim --restore-codex-desktop-gh "$gh_path"; then
        mv "$pending" "$marker"
        return 1
    fi
    rm -f "$pending"
    echo "Restored stock Homebrew gh: $gh_path"
}

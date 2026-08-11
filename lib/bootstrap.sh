#!/usr/bin/env bash
# Shared bootstrap mode and deferral helpers.

# Setup scripts may return this code to report a pending interactive action
# without turning the full bootstrap into a failure.
MANAGED_MACHINE_DEFERRED_EXIT=75

bootstrap_status_file() {
    printf '%s/bootstrap.manifest\n' "$(managed_machine_config_dir)"
}

bootstrap_interactive_input() {
    if [[ -t 0 && -t 1 ]]; then
        printf '/dev/stdin\n'
        return 0
    fi
    if ( : </dev/tty && : >/dev/tty ) 2>/dev/null; then
        printf '/dev/tty\n'
        return 0
    fi
    return 1
}

bootstrap_has_interactive_terminal() {
    bootstrap_interactive_input >/dev/null
}

bootstrap_resolve_mode() {
    local requested="${1:-auto}"
    case "$requested" in
        auto)
            if bootstrap_has_interactive_terminal; then
                printf 'interactive\n'
            else
                printf 'noninteractive\n'
            fi
            ;;
        interactive)
            if ! bootstrap_has_interactive_terminal; then
                echo "Error: interactive bootstrap requested, but no usable terminal is attached" >&2
                return 1
            fi
            printf 'interactive\n'
            ;;
        noninteractive)
            printf 'noninteractive\n'
            ;;
        *)
            echo "Error: unknown bootstrap mode: $requested" >&2
            return 1
            ;;
    esac
}

bootstrap_brew_available() {
    command -v brew >/dev/null 2>&1 \
        || [[ -x /opt/homebrew/bin/brew ]] \
        || [[ -x /usr/local/bin/brew ]]
}

bootstrap_lmstudio_available() {
    local brew_command
    [[ -d "/Applications/LM Studio.app" ]] && return 0
    if command -v brew >/dev/null 2>&1; then
        brew_command="$(command -v brew)"
    elif [[ -x /opt/homebrew/bin/brew ]]; then
        brew_command="/opt/homebrew/bin/brew"
    elif [[ -x /usr/local/bin/brew ]]; then
        brew_command="/usr/local/bin/brew"
    else
        return 1
    fi
    "$brew_command" list --cask lm-studio >/dev/null 2>&1
}

# Print a reason when a setup step must be deferred before a noninteractive
# run. Returning nonzero means the step is safe to attempt without prompts.
bootstrap_noninteractive_deferral_reason() {
    local name="$1"
    case "$name" in
        setup-brew)
            bootstrap_brew_available && return 1
            echo "Homebrew installation may require administrator approval"
            ;;
        setup-gh)
            echo "SSH passphrase, agent/Keychain, or browser authorization may be required"
            ;;
        setup-bin)
            echo "private repository access may require SSH authentication"
            ;;
        setup-lmstudio)
            bootstrap_lmstudio_available && return 1
            echo "the LM Studio cask may require administrator approval"
            ;;
        *)
            return 1
            ;;
    esac
}

defer_setup() {
    local reason="$1"
    local command="${2:-}"
    echo "Deferred: $reason" >&2
    if [[ -n "$command" ]]; then
        echo "Complete later with: $command" >&2
    fi
    return "$MANAGED_MACHINE_DEFERRED_EXIT"
}

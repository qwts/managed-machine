#!/usr/bin/env bash
# Shared bootstrap mode and deferral helpers.

# Setup scripts may return this code to report a pending interactive action
# without turning the full bootstrap into a failure.
MANAGED_MACHINE_DEFERRED_EXIT=75
# Setup scripts may return this code when the step does not apply in this
# install layout (for example Homebrew libexec is not a git clone).
MANAGED_MACHINE_SKIPPED_EXIT=76

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

# Print a reason when a setup step is not part of a noninteractive install.
# Returning nonzero means the step is safe to attempt without a dialog.
bootstrap_noninteractive_skip_reason() {
    local name="$1"
    case "$name" in
        setup-brew)
            bootstrap_brew_available && return 1
            echo "Homebrew installation needs the administrator dialog"
            ;;
        setup-hostname)
            hostname_needs_prompt || return 1
            echo "setting the Mac hostname needs the administrator dialog"
            ;;
        *)
            return 1
            ;;
    esac
}

# Backward-compatible name used by older callers/tests.
bootstrap_noninteractive_deferral_reason() {
    bootstrap_noninteractive_skip_reason "$@"
}

defer_setup() {
    local reason="$1"
    echo "Skipped: $reason" >&2
    return "$MANAGED_MACHINE_SKIPPED_EXIT"
}

skip_setup() {
    local reason="$1"
    echo "Skipped: $reason" >&2
    return "$MANAGED_MACHINE_SKIPPED_EXIT"
}

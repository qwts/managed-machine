#!/usr/bin/env bash
# Privileged escalation through the macOS Authorization Services dialog.
#
# Terminal sudo/su is fragile when setup runs as a non-admin user and a poor
# fit for agent-driven installs (decision recorded on #7). Privileged steps
# escalate through osascript's "with administrator privileges", Apple's
# supported user-facing wrapper over Authorization Services: it presents the
# standard Touch ID / password dialog and works when the invoking user is not
# an admin.
#
# Batch the commands of one logical phase into a single elevate_run call
# (e.g. via `/bin/sh -c 'first && second'`) so the owner is prompted once per
# phase, not once per command.

# Return 0 when the GUI authorization dialog can be presented at all.
elevation_available() {
    [[ "$(uname -s)" == "Darwin" ]] || return 1
    command -v osascript >/dev/null 2>&1 || return 1
    # Noninteractive bootstrap must never pop a dialog; defer instead.
    [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" != "noninteractive" ]] || return 1
}

# elevate_run <label> <command> [args...]
#
# Run one command elevated via the system authorization dialog. The label
# names the phase in the prompt and in failure messages. Arguments are passed
# through AppleScript's `quoted form of`, so no caller-side quoting is needed
# and no shell interpolation of arguments can occur. Fails with a clear
# message and nonzero status when the dialog is cancelled or unavailable.
elevate_run() {
    local label="$1"
    shift

    if [[ $# -eq 0 ]]; then
        echo "Error: elevate_run requires a command" >&2
        return 1
    fi
    if ! elevation_available; then
        echo "Error: administrator authorization dialog unavailable (noninteractive or non-macOS) — cannot $label" >&2
        return 1
    fi

    echo "Requesting administrator authorization to $label (system dialog)..."
    if ! osascript \
        -e 'on run argv' \
        -e 'set lbl to item 1 of argv' \
        -e 'set cmd to ""' \
        -e 'repeat with i from 2 to count of argv' \
        -e 'set cmd to cmd & quoted form of (item i of argv as text) & " "' \
        -e 'end repeat' \
        -e 'do shell script cmd with prompt ("managed-machine needs administrator access to " & lbl & ".") with administrator privileges' \
        -e 'end run' \
        "$label" "$@" >/dev/null; then
        echo "Error: administrator authorization was cancelled or failed — did not $label" >&2
        return 1
    fi
}

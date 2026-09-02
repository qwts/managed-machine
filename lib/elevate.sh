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

# Home directory for a dropped-privilege user. A brew-owner account such as
# `admin` often has a stub /Users/admin (no zsh profile). If the directory
# from dscl is missing, use a prefix-owned fallback rather than sudo -H.
elevate_user_home() {
    local user="$1"
    local home
    home="$(/usr/bin/dscl . -read "/Users/$user" NFSHomeDirectory 2>/dev/null | /usr/bin/awk '{print $2}')"
    if [[ -n "$home" && -d "$home" ]]; then
        printf '%s\n' "$home"
        return 0
    fi
    if [[ -x /opt/homebrew/bin/brew ]]; then
        printf '%s\n' /opt/homebrew/var/mm-home
    elif [[ -x /usr/local/bin/brew ]]; then
        printf '%s\n' /usr/local/var/mm-home
    else
        printf '%s\n' "/tmp/mm-home-$user"
    fi
}

# elevate_as_user <label> <user> <command> [args...]
#
# Run a command as another user. When the current user already is that user,
# the command runs in-process. Otherwise it escalates to root through
# elevate_run and drops to the target with `sudo -u` plus an explicit PATH
# and HOME. Homebrew must not run as root; this is how mutating brew
# commands run as `admin`. Do not use `sudo -H`: a stub home is an empty shell.
# Supports numeric UIDs: sudo -u "#502" when DirectoryService is sandboxed.
elevate_as_user() {
    local label="$1"
    local user="$2"
    local home sudo_user
    shift 2

    if [[ $# -eq 0 ]]; then
        echo "Error: elevate_as_user requires a command" >&2
        return 1
    fi
    if [[ -z "$user" ]]; then
        echo "Error: elevate_as_user requires a user" >&2
        return 1
    fi
    # Resolve numeric UID handling: if user is "502" or "(502)", use sudo -u "#502"
    local clean="${user//[()]/}"
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        # Try to resolve to name first (id may work, or check /Users/admin)
        local admin_uid resolved
        admin_uid="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid" && "$clean" == "$admin_uid" ]]; then
            user="admin"
        else
            resolved="$(id -nu "$clean" 2>/dev/null || true)"
            if [[ -n "$resolved" && "$resolved" != "$clean" ]]; then
                user="$resolved"
            else
                resolved="$(/usr/bin/dscl . -search /Users UniqueID "$clean" 2>/dev/null | /usr/bin/awk 'NR==1{print $1}' || true)"
                if [[ -n "$resolved" && "$resolved" != "$clean" ]]; then
                    user="$resolved"
                else
                    # Fall back to numeric sudo syntax
                    sudo_user="#$clean"
                fi
            fi
        fi
    fi
    if [[ -z "${sudo_user:-}" ]]; then
        # If numeric and not resolved, use #uid syntax
        if [[ "$clean" =~ ^[0-9]+$ && "$user" == "$clean" ]]; then
            sudo_user="#$clean"
        else
            sudo_user="$user"
        fi
    fi
    if [[ "$user" == "$(id -un)" || "$sudo_user" == "$(id -un)" ]]; then
        "$@"
        return
    fi
    home="$(elevate_user_home "$user")"
    elevate_run "$label" /usr/bin/sudo -u "$sudo_user" /usr/bin/env \
        HOME="$home" \
        PATH=/opt/homebrew/bin:/usr/local/bin:/usr/sbin:/usr/bin:/bin \
        "$@"
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
        echo "Skipped: administrator authorization dialog unavailable — cannot $label" >&2
        return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
    fi

    echo "Requesting administrator authorization to $label (system dialog)..."
    # osascript reports both outcomes as a nonzero exit: a dismissed dialog
    # ("User canceled. (-128)") and the elevated command itself failing
    # ("execution error: <its stderr> (<status>)"). The operator approved the
    # dialog in the second case, so say what the command said instead of
    # blaming the authorization.
    local detail
    if ! detail="$(osascript \
        -e 'on run argv' \
        -e 'set lbl to item 1 of argv' \
        -e 'set cmd to ""' \
        -e 'repeat with i from 2 to count of argv' \
        -e 'set cmd to cmd & quoted form of (item i of argv as text) & " "' \
        -e 'end repeat' \
        -e 'do shell script cmd with prompt ("managed-machine needs administrator access to " & lbl & ".") with administrator privileges' \
        -e 'end run' \
        "$label" "$@" 2>&1 >/dev/null)"; then
        if [[ -z "$detail" || "$detail" == *"(-128)"* || "$detail" == *"User canceled"* ]]; then
            echo "Error: administrator authorization was cancelled or failed — did not $label" >&2
        else
            echo "Error: the elevated step to $label failed after authorization: ${detail#*execution error: }" >&2
        fi
        return 1
    fi
}

#!/usr/bin/env bash
# Interactive hostname via a macOS dialog, then scutil. Fleet registration
# reads LocalHostName so a popup typo like AlexsMacbookPro can be corrected
# by re-running `managed-machine setup hostname`.

hostname_manifest_file() {
    printf '%s/hostname.manifest\n' "$(managed_machine_config_dir)"
}

hostname_manifest_name() {
    local file
    file="$(hostname_manifest_file)"
    [[ -f "$file" ]] || return 1
    sed -n 's/^name=//p' "$file" | head -1
}

# RFC 1034 label: letter, then letters/digits/hyphens, max 63.
hostname_is_valid() {
    [[ "$1" =~ ^[A-Za-z][A-Za-z0-9-]{0,62}$ ]]
}

current_local_hostname() {
    local value
    if command -v scutil >/dev/null 2>&1; then
        value="$(scutil --get LocalHostName 2>/dev/null || true)"
        if [[ -n "$value" ]]; then
            printf '%s\n' "$value"
            return 0
        fi
    fi
    hostname -s 2>/dev/null || hostname
}

# Prompt for a hostname. MANAGED_MACHINE_HOSTNAME skips the dialog (tests and
# explicit overrides). Cancelled dialogs fail closed.
prompt_hostname() {
    local default="$1"
    local answer
    if [[ -n "${MANAGED_MACHINE_HOSTNAME:-}" ]]; then
        printf '%s\n' "$MANAGED_MACHINE_HOSTNAME"
        return 0
    fi
    if [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == "noninteractive" ]]; then
        return 1
    fi
    if [[ "$(uname -s)" != "Darwin" ]] || ! command -v osascript >/dev/null 2>&1; then
        echo "Error: hostname prompt requires macOS osascript" >&2
        return 1
    fi
    answer="$(osascript \
        -e 'on run argv' \
        -e 'set defaultName to item 1 of argv' \
        -e 'set dlg to display dialog "Hostname for this Mac (LocalHostName, ComputerName, and HostName as name.lan):" default answer defaultName with title "managed-machine hostname" buttons {"Cancel", "Set"} default button "Set"' \
        -e 'if button returned of dlg is "Cancel" then error number -128' \
        -e 'return text returned of dlg' \
        -e 'end run' \
        "$default" 2>/dev/null)" || return 1
    printf '%s\n' "$answer"
}

write_hostname_manifest() {
    local name="$1"
    local file tmp
    file="$(hostname_manifest_file)"
    mkdir -p "$(dirname "$file")"
    chmod 700 "$(dirname "$file")" 2>/dev/null || true
    tmp="$(mktemp "${file}.XXXXXX")"
    {
        echo 'schema_version=1'
        printf 'name=%s\n' "$name"
        printf 'set_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    } >"$tmp"
    chmod 600 "$tmp"
    mv "$tmp" "$file"
}

scutil_hostnames_match() {
    local name="$1"
    [[ "$(scutil --get LocalHostName 2>/dev/null || true)" == "$name" ]] \
        && [[ "$(scutil --get ComputerName 2>/dev/null || true)" == "$name" ]] \
        && [[ "$(scutil --get HostName 2>/dev/null || true)" == "${name}.lan" ]]
}

apply_scutil_hostname() {
    local name="$1"
    if scutil_hostnames_match "$name"; then
        echo "Hostname already set: $name (${name}.lan)"
        return 0
    fi
    elevate_run "set this Mac hostname to $name" /bin/sh -c \
        'scutil --set LocalHostName "$1" && scutil --set ComputerName "$1" && scutil --set HostName "$1.lan"' \
        hostname "$name"
}

# Bootstrap: skip the dialog when a previous managed hostname is already live.
hostname_needs_prompt() {
    local recorded
    recorded="$(hostname_manifest_name 2>/dev/null || true)"
    [[ -n "$recorded" ]] && hostname_is_valid "$recorded" && scutil_hostnames_match "$recorded" && return 1
    return 0
}

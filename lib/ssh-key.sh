#!/usr/bin/env bash
# Safe GitHub SSH key creation helpers.

EMPTY_SSH_PASSPHRASE_OPT_IN="MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE"

ssh_key_policy_file() {
    printf '%s/ssh-key-policy.toml\n' "$(managed_machine_config_dir)"
}

ssh_key_interactive_input() {
    if [[ -t 0 ]]; then
        printf '/dev/stdin\n'
        return 0
    fi
    if ( : </dev/tty ) 2>/dev/null; then
        printf '/dev/tty\n'
        return 0
    fi
    return 1
}

empty_ssh_passphrase_opted_in() {
    local value="${MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE:-}"
    case "$value" in
        1) return 0 ;;
        ""|0) return 1 ;;
        *)
            echo "Error: $EMPTY_SSH_PASSPHRASE_OPT_IN must be exactly 1 to opt in" >&2
            return 2
            ;;
    esac
}

record_ssh_key_policy() {
    local policy="$1"
    local file tmp
    file="$(ssh_key_policy_file)"
    mkdir -p "$(dirname "$file")" || return 1
    chmod 700 "$(dirname "$file")" || return 1
    if ! (
        umask 077
        tmp="$(mktemp "${file}.XXXXXX")" || exit 1
        trap 'rm -f "$tmp"' EXIT
        {
            echo 'schema_version = 1'
            printf 'passphrase_policy = "%s"\n' "$policy"
            if [[ "$policy" == "empty-explicit-opt-in" ]]; then
                printf 'opt_in_variable = "%s"\n' "$EMPTY_SSH_PASSPHRASE_OPT_IN"
            fi
            printf 'recorded_at = "%s"\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        } >"$tmp" || exit 1
        mv "$tmp" "$file" || exit 1
        trap - EXIT
    ); then
        echo "Error: could not record SSH key passphrase policy" >&2
        return 1
    fi
    echo "Recorded SSH key passphrase policy: $file"
}

cleanup_new_ssh_key_pair() {
    local private_key="$1"
    local public_key="$2"
    rm -f "$private_key" "$public_key"
}

private_key_has_empty_passphrase() {
    local private_key="$1"
    ssh-keygen -y -P '' -f "$private_key" >/dev/null 2>&1
}

ensure_github_ssh_key() {
    local private_key="$1"
    local public_key="$2"
    local comment input policy opt_in_status

    mkdir -p "$(dirname "$private_key")" || return 1
    chmod 700 "$(dirname "$private_key")" || return 1

    if [[ -f "$private_key" && -f "$public_key" ]]; then
        echo "SSH key already present: $private_key"
        return 0
    fi
    if [[ -e "$private_key" || -e "$public_key" ]]; then
        echo "Error: incomplete key pair at $private_key — resolve manually" >&2
        return 1
    fi

    if empty_ssh_passphrase_opted_in; then
        opt_in_status=0
    else
        opt_in_status=$?
    fi
    if [[ "$opt_in_status" == "2" ]]; then
        return 1
    fi

    comment="$(git config --global user.email 2>/dev/null || true)"
    if [[ -z "$comment" ]]; then
        comment="${USER:-user}@$(hostname -s)"
    fi

    echo "Generating RSA 4096 SSH key for this machine..."
    if [[ "$opt_in_status" == "0" ]]; then
        echo "Warning: creating an empty-passphrase key by explicit $EMPTY_SSH_PASSPHRASE_OPT_IN=1 opt-in." >&2
        if ! ssh-keygen -t rsa -b 4096 -N '' -C "$comment" -f "$private_key"; then
            cleanup_new_ssh_key_pair "$private_key" "$public_key"
            return 1
        fi
        if ! private_key_has_empty_passphrase "$private_key"; then
            cleanup_new_ssh_key_pair "$private_key" "$public_key"
            echo "Error: could not verify explicitly unencrypted SSH key" >&2
            return 1
        fi
        policy="empty-explicit-opt-in"
    else
        if ! input="$(ssh_key_interactive_input)"; then
            cat >&2 <<EOF
Error: refusing to create an SSH key without an interactive terminal.
Run this from a terminal to create a passphrase-protected, Keychain-backed key:

  managed-machine setup gh

To explicitly accept an unencrypted key instead:

  $EMPTY_SSH_PASSPHRASE_OPT_IN=1 managed-machine setup gh
EOF
            return 1
        fi
        echo "Enter a passphrase; macOS Keychain will remember it after setup."
        if ! ssh-keygen -t rsa -b 4096 -C "$comment" -f "$private_key" <"$input"; then
            cleanup_new_ssh_key_pair "$private_key" "$public_key"
            return 1
        fi
        if private_key_has_empty_passphrase "$private_key"; then
            cleanup_new_ssh_key_pair "$private_key" "$public_key"
            cat >&2 <<EOF
Error: empty SSH key passphrase rejected; the new key was removed.
Re-run and enter a passphrase, or explicitly opt in to an unencrypted key:

  $EMPTY_SSH_PASSPHRASE_OPT_IN=1 managed-machine setup gh
EOF
            return 1
        fi
        policy="encrypted"
    fi

    if [[ ! -f "$private_key" || ! -f "$public_key" ]]; then
        cleanup_new_ssh_key_pair "$private_key" "$public_key"
        echo "Error: ssh-keygen did not create a complete key pair" >&2
        return 1
    fi
    if ! chmod 600 "$private_key" || ! chmod 644 "$public_key"; then
        cleanup_new_ssh_key_pair "$private_key" "$public_key"
        echo "Error: could not secure SSH key permissions" >&2
        return 1
    fi
    if ! record_ssh_key_policy "$policy"; then
        cleanup_new_ssh_key_pair "$private_key" "$public_key"
        return 1
    fi
    echo "Created: $private_key"
}

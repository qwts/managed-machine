#!/usr/bin/env bash
# Explicit, human-authorized GitHub SSH enrollment (#120).
#
# Nothing in the default bootstrap/update path creates or changes SSH
# identity; this library implements `managed-machine ssh enroll` and is only
# reached through that command. The ceremony is deliberately split:
#
#   1. validate the invoking account is a real local human (never root,
#      never an agent account, HOME owned by and registered to that account);
#   2. display the exact plan — local account, GitHub login, key fingerprint
#      or creation intent, and the selected purposes;
#   3. authorize through the OS dialog with operation-specific wording;
#   4. mutate unprivileged, as the invoking account.
#
# The osascript "with administrator privileges" call is a consent gate, not
# an execution context: it runs a non-mutating no-op, so an administrator
# approving for a standard human user cannot land keys under the admin home.
#
# Owner-approved limitation (#120): the dialog may be satisfied by Touch ID
# or a recently cached Authorization Services grant — it is an OS-native
# human confirmation, not a guaranteed fresh-password challenge. Passwords
# never leave the OS authentication UI: no custom dialog, shell argument,
# environment variable, stdin protocol, or log ever carries one.

ssh_enroll_key_path() {
    printf '%s/.ssh/id_rsa_github\n' "$HOME"
}

# PATH-resolved directory queries keep the check testable; they are read-only
# and sit behind the other layers, so a spoofed lookup can only refuse, never
# authorize. dsmemberutil answers nested/UUID membership; the dscl group
# listing is the fallback when it cannot.
ssh_enroll_in_agents_group() {
    local account="$1" verdict
    if command -v dsmemberutil >/dev/null 2>&1; then
        if verdict="$(dsmemberutil checkmembership -U "$account" -G "$AGENT_ACCOUNT_GROUP" 2>/dev/null)"; then
            case "$verdict" in
                *'is a member'*) return 0 ;;
                *'not a member'*) return 1 ;;
            esac
        fi
    fi
    dscl . -read "/Groups/$AGENT_ACCOUNT_GROUP" GroupMembership 2>/dev/null \
        | tr ' ' '\n' | grep -Fxq "$account"
}

# True when the invocation is an agent context: a harness session marker, an
# account in the OS-level agents group every roster account joins (add-agent
# guarantees membership — this is a directory fact, not a name glob), or an
# account name that IS a rostered identity slug (ENG-0339: the name is the
# mapping). An absent or unreadable roster source does not by itself prove
# the account is human, but the group check remains authoritative for
# provisioned agent accounts.
ssh_enroll_agent_context() {
    local account="$1" status
    if managed_machine_agent_session; then
        echo 'agent session markers are present in this environment' >&2
        return 0
    fi
    if ssh_enroll_in_agents_group "$account"; then
        echo "account $account is a member of the $AGENT_ACCOUNT_GROUP group" >&2
        return 0
    fi
    if agent_roster_source >/dev/null 2>&1; then
        status="$(agent_roster_query status "$account" 2>/dev/null || true)"
        if [[ -n "$status" && "$status" != "unknown" ]]; then
            echo "account $account is a rostered agent identity (status: $status)" >&2
            return 0
        fi
    fi
    return 1
}

# Validate the invoking account is the local human the enrollment binds to.
# Prints the account name. Any refusal happens before mutation or dialog.
ssh_enroll_validate_account() {
    local account home registered_home actual canonical
    account="$(id -un 2>/dev/null || true)"
    if [[ -z "$account" || "$(id -u 2>/dev/null || echo 0)" == "0" || "$account" == "root" ]]; then
        echo 'Error: SSH enrollment must run as the human account it binds to, never as root' >&2
        return 1
    fi
    if ssh_enroll_agent_context "$account"; then
        echo 'Error: SSH enrollment is human-only — refusing to enroll an agent account' >&2
        return 1
    fi
    home="$(cd "$HOME" 2>/dev/null && pwd -P || true)"
    if [[ -z "$home" || -L "$HOME" || ! -O "$HOME" ]]; then
        echo "Error: HOME ($HOME) is not a directory owned by $account — refusing enrollment" >&2
        return 1
    fi
    # Cross-check the account's registered home when the directory answers.
    # A HOME that is merely owned by the caller is not enough: enrollment
    # writes under the account's real home, never an admin's or a scratch dir.
    registered_home="$(dscl . -read "/Users/$account" NFSHomeDirectory 2>/dev/null \
        | sed -n 's/^NFSHomeDirectory: //p' || true)"
    if [[ -n "$registered_home" ]]; then
        canonical="$(cd "$registered_home" 2>/dev/null && pwd -P || printf '%s\n' "$registered_home")"
        if [[ "$canonical" != "$home" ]]; then
            echo "Error: HOME ($home) is not the registered home of $account ($canonical) — refusing enrollment" >&2
            return 1
        fi
    fi
    printf '%s\n' "$account"
}

# The active github.com login. Enrollment never opens a login or device flow:
# gh must already be authenticated, which `managed-machine setup gh` provides
# over HTTPS.
ssh_enroll_github_login() {
    local login
    if ! command -v gh >/dev/null 2>&1; then
        echo "Error: gh is required — run 'managed-machine setup gh' first" >&2
        return 1
    fi
    login="$(gh_active_account)"
    if [[ -z "$login" ]]; then
        echo "Error: no active GitHub account — run 'managed-machine setup gh' (or 'gh auth login -h github.com -p https -w') before enrolling SSH" >&2
        return 1
    fi
    printf '%s\n' "$login"
}

# Signing and fleet purposes need the git identity setup-gh configures.
ssh_enroll_require_git_identity() {
    if [[ -z "$(git config --global --get user.email 2>/dev/null || true)" \
        || -z "$(git config --global --get user.name 2>/dev/null || true)" ]]; then
        echo "Error: git identity is not configured — run 'managed-machine setup gh' first" >&2
        return 1
    fi
}

# One OS-native authorization per invocation, bound to the operation: the
# system prompt names the local account, the GitHub login, and the selected
# purposes. The elevated side runs /usr/bin/true — a consent proof only.
# Cancellation, a dismissed dialog, or an unavailable GUI leave all key and
# enrollment state unchanged and report that no enrollment occurred.
ssh_enroll_authorize() {
    local prompt="$1" detail
    if ! elevation_available; then
        echo 'Skipped: the system authorization dialog is unavailable (noninteractive or headless session) — no enrollment occurred' >&2
        return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
    fi
    echo 'Requesting human authorization through the system dialog...'
    if ! detail="$(osascript \
        -e 'on run argv' \
        -e 'do shell script "/usr/bin/true" with prompt (item 1 of argv as text) with administrator privileges' \
        -e 'end run' \
        "$prompt" 2>&1 >/dev/null)"; then
        if [[ -z "$detail" || "$detail" == *"(-128)"* || "$detail" == *"User canceled"* ]]; then
            echo 'Error: authorization was cancelled — no enrollment occurred' >&2
        else
            echo "Error: authorization failed (${detail#*execution error: }) — no enrollment occurred" >&2
        fi
        return 1
    fi
    echo 'Authorization granted.'
}

# --- per-purpose steps -------------------------------------------------------

ssh_enroll_ensure_key() {
    local key_path pub_path
    key_path="$(ssh_enroll_key_path)"
    pub_path="${key_path}.pub"
    ensure_github_ssh_key "$key_path" "$pub_path"
}

# The github.com ssh config block (identity file, keychain, agent loading).
# An existing Host github.com block is left untouched.
ssh_enroll_ssh_config() {
    local key_path ssh_config
    key_path="$(ssh_enroll_key_path)"
    ssh_config="${HOME}/.ssh/config"

    mkdir -p "${HOME}/.ssh" || return 1
    chmod 700 "${HOME}/.ssh" || return 1

    if [[ -f "$ssh_config" ]] && grep -Eq '^[[:space:]]*Host[[:space:]]+github\.com([[:space:]]|$)' "$ssh_config"; then
        echo "SSH config already has Host github.com — leaving unchanged"
        return 0
    fi

    if [[ ! -f "$ssh_config" ]]; then
        touch "$ssh_config" || return 1
        chmod 600 "$ssh_config" || return 1
    fi

    cat >>"$ssh_config" <<EOF

# Added by managed-machine ssh enroll
Host github.com
  HostName github.com
  User git
  IdentityFile ${key_path}
  IdentitiesOnly yes
  AddKeysToAgent yes
  UseKeychain yes
EOF
    echo "Configured SSH Host github.com → $key_path"
}

ssh_enroll_add_key_to_agent() {
    local key_path
    key_path="$(ssh_enroll_key_path)"
    if [[ "$(uname -s)" == "Darwin" ]]; then
        ssh-add --apple-use-keychain "$key_path" 2>/dev/null || ssh-add "$key_path" || return 1
    else
        ssh-add "$key_path" || return 1
    fi
    echo "SSH key loaded into agent"
}

# Authentication purpose: key, ssh config, agent/keychain import, GitHub
# upload as an authentication key, and the gh SSH remote protocol.
ssh_enroll_authentication() {
    local pub_path title
    pub_path="$(ssh_enroll_key_path).pub"
    ssh_enroll_ssh_config || return 1
    ssh_enroll_add_key_to_agent || return 1
    title="$(hostname -s)-$(date +%Y%m%d)"
    upload_ssh_key_as authentication "$title" "$pub_path" || return 1
    gh config set git_protocol ssh -h github.com || return 1
    echo "Configured: git_protocol=ssh (github.com)"
}

# Signing purpose: upload the same public key as a signing key, configure
# global SSH commit/tag signing, and refresh allowed_signers. The managed
# allowed-signers block tracks the fleet registry; the local key is added
# outside the block when absent so this machine's own commits verify even
# without fleet enrollment.
ssh_enroll_signing() {
    local pub_path title principal
    CONFIG_REPO_ROOT="$(managed_machine_config_repo_dir)" || return 1
    pub_path="$(ssh_enroll_key_path).pub"
    title="$(hostname -s)-$(date +%Y%m%d)"
    ssh_enroll_add_key_to_agent || return 1
    upload_ssh_key_as signing "${title}-signing" "$pub_path" || return 1

    git config --global gpg.format ssh || return 1
    git config --global user.signingkey "$pub_path" || return 1
    git config --global commit.gpgsign true || return 1
    git config --global tag.gpgSign true || return 1
    git config --global gpg.ssh.allowedSignersFile "${HOME}/.ssh/allowed_signers" || return 1
    echo "Configured Git SSH commit/tag signing:"
    echo "  gpg.format=$(git config --global --get gpg.format)"
    echo "  user.signingkey=$(git config --global --get user.signingkey)"
    echo "  commit.gpgsign=$(git config --global --get commit.gpgsign)"
    echo "  tag.gpgSign=$(git config --global --get tag.gpgSign)"
    echo "  gpg.ssh.allowedSignersFile=$(git config --global --get gpg.ssh.allowedSignersFile)"

    principal="$(git config --global --get user.email)"
    sync_local_allowed_signers "$principal" || return 1
    ssh_enroll_allow_self_signer "$pub_path" "$principal" || return 1
}

# Ensure the local public key verifies commits locally: append it to
# allowed_signers outside the managed block when it is not already listed.
ssh_enroll_allow_self_signer() {
    local pub_path="$1" principal="$2"
    local allowed_signers key_body
    allowed_signers="${HOME}/.ssh/allowed_signers"
    key_body="$(awk 'NF >= 2 { print $1 " " $2; exit }' "$pub_path")"
    [[ -n "$key_body" ]] || return 1
    mkdir -p "${HOME}/.ssh" || return 1
    touch "$allowed_signers" || return 1
    chmod 600 "$allowed_signers" || return 1
    if grep -qF "$key_body" "$allowed_signers"; then
        return 0
    fi
    printf '%s %s\n' "$principal" "$key_body" >>"$allowed_signers" || return 1
    echo "Added this machine's key to $allowed_signers for local signature verification"
}

# Fleet purpose: register this machine in the private fleet registry,
# publish managed fleet state, and sync local authorized_keys.
ssh_enroll_fleet() {
    local pub_path
    pub_path="$(ssh_enroll_key_path).pub"

    import_legacy_fleet_entries || return 1
    register_current_machine "$pub_path" || return 1
    generate_fleet_authorized_keys || return 1
    sync_managed_machine_config_repo \
        "$CONFIG_REPO_ROOT" \
        "Register managed machine $(scutil --get LocalHostName 2>/dev/null || hostname -s)" \
        generate_fleet_authorized_keys || return 1
    generate_fleet_authorized_keys || return 1
    sync_local_authorized_keys || return 1
    report_fleet_changes || return 1
}

# --- read-only status --------------------------------------------------------

ssh_enroll_status() {
    local account="" key_path pub_path fingerprint login body
    key_path="$(ssh_enroll_key_path)"
    pub_path="${key_path}.pub"

    account="$(id -un 2>/dev/null || true)"
    if [[ -n "$account" ]] && ssh_enroll_agent_context "$account" 2>/dev/null; then
        echo "account: $account is an agent account — SSH enrollment is refused by policy"
        if [[ -e "$key_path" || -e "$pub_path" ]]; then
            echo "warn: managed SSH state exists in this account — removal is a separate owner-confirmed action; nothing is changed automatically"
        fi
    else
        echo "account: ${account:-unknown} (human enrollment eligible)"
    fi

    if [[ -f "$key_path" && -f "$pub_path" ]]; then
        fingerprint="$(ssh_public_key_fingerprint "$pub_path")"
        echo "key: present ($key_path${fingerprint:+, $fingerprint})"
    else
        echo "key: absent ($key_path)"
    fi

    login="$(gh_active_account 2>/dev/null || true)"
    if [[ -z "$login" ]]; then
        echo "github: no active gh account — registration state unknown"
    elif [[ -f "$pub_path" ]]; then
        body="$(awk 'NF >= 2 { print $2; exit }' "$pub_path")"
        local type listed
        for type in authentication signing; do
            if listed="$(github_registered_key_bodies "$type" 2>/dev/null)" \
                && grep -qxF "$body" <<<"$listed"; then
                echo "github: registered as $type key on $login"
            else
                echo "github: not registered as $type key on $login (or listing unavailable)"
            fi
        done
    else
        echo "github: active account $login; no local key to compare"
    fi

    if [[ "$(git config --global --get gpg.format 2>/dev/null || true)" == "ssh" \
        && -n "$(git config --global --get user.signingkey 2>/dev/null || true)" ]]; then
        echo "signing: SSH commit/tag signing configured"
    else
        echo "signing: not configured"
    fi

    local state_file machine_id
    state_file="$(local_machine_state_file)"
    machine_id=""
    [[ -f "$state_file" ]] && machine_id="$(toml_value "$state_file" machine_id)"
    if [[ -n "$machine_id" ]]; then
        echo "fleet: registered ($machine_id)"
    else
        echo "fleet: not registered"
    fi

    if [[ ! -f "$key_path" ]]; then
        echo "SSH is not enrolled — default setup never enrolls it. To enroll explicitly: managed-machine ssh enroll --authentication|--signing|--fleet"
    fi
}

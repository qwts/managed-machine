#!/usr/bin/env bash
# Machine identity and private fleet registry helpers.

FLEET_SCHEMA_VERSION=1
AUTHORIZED_KEYS_BEGIN="# BEGIN managed-machine"
AUTHORIZED_KEYS_END="# END managed-machine"
LEGACY_AUTHORIZED_KEYS_BEGIN="# BEGIN home-bin new-machine"
LEGACY_AUTHORIZED_KEYS_END="# END home-bin new-machine"
ALLOWED_SIGNERS_BEGIN="# BEGIN managed-machine"
ALLOWED_SIGNERS_END="# END managed-machine"

fleet_registry_dir() {
    printf '%s/fleet/machines\n' "$CONFIG_REPO_ROOT"
}

fleet_authorized_keys_file() {
    printf '%s/ssh/authorized_keys\n' "$CONFIG_REPO_ROOT"
}

local_machine_state_file() {
    printf '%s/machine.toml\n' "$(managed_machine_config_dir)"
}

pending_github_revocation_dir() {
    printf '%s/pending-github-key-revocations\n' "$(managed_machine_config_dir)"
}

validate_machine_id() {
    if [[ ! "$1" =~ ^sha256-[A-Za-z0-9_-]+$ ]]; then
        echo "Error: invalid machine ID: $1" >&2
        return 1
    fi
}

pending_github_revocation_file() {
    local machine_id="$1"
    validate_machine_id "$machine_id" || return 1
    printf '%s/%s.pub\n' "$(pending_github_revocation_dir)" "$machine_id"
}

save_pending_github_revocation() {
    local machine_id="$1"
    local public_key="$2"
    local file key_body
    file="$(pending_github_revocation_file "$machine_id")" || return 1
    case "$public_key" in
        ssh-*|'ecdsa-'*|'sk-'*) ;;
        *)
            echo "Error: refusing invalid pending GitHub public key" >&2
            return 1
            ;;
    esac
    if [[ "$public_key" == *$'\n'* || "$public_key" == *$'\r'* ]]; then
        echo "Error: refusing multiline pending GitHub public key" >&2
        return 1
    fi
    key_body="$(printf '%s\n' "$public_key" | awk 'NF >= 2 { print $2; exit }')"
    if [[ -z "$key_body" ]]; then
        echo "Error: refusing incomplete pending GitHub public key" >&2
        return 1
    fi
    mkdir -p "$(dirname "$file")" || return 1
    chmod 700 "$(dirname "$file")" || return 1
    if ! (
        umask 077
        tmp="$(mktemp "${file}.XXXXXX")" || exit 1
        trap 'rm -f "$tmp"' EXIT
        printf '%s\n' "$public_key" >"$tmp" || exit 1
        mv "$tmp" "$file" || exit 1
        trap - EXIT
    ); then
        echo "Error: could not save pending GitHub key revocation" >&2
        return 1
    fi
    echo "Saved pending GitHub key revocation: $file"
}

load_pending_github_revocation() {
    local machine_id="$1"
    local file
    file="$(pending_github_revocation_file "$machine_id")" || return 1
    [[ -f "$file" ]] || return 1
    cat "$file" || return 1
}

clear_pending_github_revocation() {
    local machine_id="$1"
    local file
    file="$(pending_github_revocation_file "$machine_id")" || return 1
    rm -f "$file" || return 1
}

toml_clean_value() {
    printf '%s' "$1" | tr -d '\r\n' | tr '"\\' '__'
}

toml_value() {
    local file="$1"
    local key="$2"
    sed -n "s/^${key} = \"\(.*\)\"$/\1/p" "$file" | head -1
}

ssh_public_key() {
    awk 'NF >= 2 { print $1 " " $2; exit }' "$1"
}

ssh_fingerprint() {
    ssh-keygen -lf "$1" -E sha256 | awk 'NR == 1 { print $2 }'
}

machine_id_from_fingerprint() {
    local digest="${1#SHA256:}"
    digest="${digest//+/-}"
    digest="${digest//\//_}"
    digest="${digest//=}"
    printf 'sha256-%s\n' "$digest"
}

managed_machine_ref() {
    local version
    if git -C "$REPO_ROOT" rev-parse --verify HEAD >/dev/null 2>&1; then
        git -C "$REPO_ROOT" rev-parse HEAD
        return
    fi
    if command -v brew >/dev/null 2>&1; then
        version="$(brew list --versions managed-machine 2>/dev/null | awk 'NR == 1 { print $2 }')"
        if [[ -n "$version" ]]; then
            printf 'v%s\n' "$version"
            return
        fi
    fi
    printf 'unknown\n'
}

configured_local_bin_ref() {
    local ref_file="$CONFIG_REPO_ROOT/local-bin.ref"
    if [[ ! -f "$ref_file" ]]; then
        printf 'unknown\n'
        return
    fi
    grep -vE '^[[:space:]]*(#|$)' "$ref_file" | head -1 | tr -d '[:space:]'
}

write_machine_record() {
    local destination="$1"
    local machine_id="$2"
    local hostname_value="$3"
    local managed_at="$4"
    local managed_ref="$5"
    local local_bin_ref="$6"
    local fingerprint="$7"
    local public_key="${8:-}"
    local tmp

    tmp="$(mktemp)"
    {
        printf 'schema_version = %s\n' "$FLEET_SCHEMA_VERSION"
        printf 'machine_id = "%s"\n' "$(toml_clean_value "$machine_id")"
        printf 'hostname = "%s"\n' "$(toml_clean_value "$hostname_value")"
        printf 'managed_at = "%s"\n' "$(toml_clean_value "$managed_at")"
        printf 'managed_machine_ref = "%s"\n' "$(toml_clean_value "$managed_ref")"
        printf 'local_bin_ref = "%s"\n' "$(toml_clean_value "$local_bin_ref")"
        printf 'ssh_key_fingerprint = "%s"\n' "$(toml_clean_value "$fingerprint")"
        if [[ -n "$public_key" ]]; then
            printf 'public_key = "%s"\n' "$(toml_clean_value "$public_key")"
        fi
    } >"$tmp"

    mkdir -p "$(dirname "$destination")"
    if [[ -f "$destination" ]] && cmp -s "$tmp" "$destination"; then
        rm -f "$tmp"
        return 1
    fi
    mv "$tmp" "$destination"
    return 0
}

import_legacy_fleet_entries() {
    local authorized_keys registry_dir comment host managed_date line key_file
    local public_key fingerprint machine_id destination

    authorized_keys="$(fleet_authorized_keys_file)"
    registry_dir="$(fleet_registry_dir)"
    [[ -f "$authorized_keys" ]] || return 0

    mkdir -p "$registry_dir"
    comment=""
    key_file="$(mktemp)"
    while IFS= read -r line || [[ -n "$line" ]]; do
        case "$line" in
            '# '*)
                comment="${line#\# }"
                ;;
            ssh-*|'ecdsa-'*|'sk-'*)
                printf '%s\n' "$line" >"$key_file"
                public_key="$(ssh_public_key "$key_file")"
                fingerprint="$(ssh_fingerprint "$key_file")"
                machine_id="$(machine_id_from_fingerprint "$fingerprint")"
                destination="$registry_dir/$machine_id.toml"
                if [[ ! -f "$destination" ]]; then
                    host="$(printf '%s\n' "$comment" | awk '{print $1}')"
                    managed_date="$(printf '%s\n' "$comment" | awk '{print $2}')"
                    [[ -n "$host" ]] || host="legacy-machine"
                    if [[ "$managed_date" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
                        managed_date="${managed_date}T00:00:00Z"
                    else
                        managed_date="unknown"
                    fi
                    write_machine_record "$destination" "$machine_id" "$host" \
                        "$managed_date" "legacy" "legacy" "$fingerprint" "$public_key" || true
                    echo "Imported legacy fleet entry: $machine_id ($host)"
                    FLEET_CHANGED=1
                fi
                comment=""
                ;;
        esac
    done <"$authorized_keys"
    rm -f "$key_file"
}

register_current_machine() {
    local public_key_path="$1"
    local registry_dir state_file public_key fingerprint machine_id destination
    local hostname_value managed_at managed_ref local_bin_ref existing_ref local_state_matches=0

    registry_dir="$(fleet_registry_dir)"
    state_file="$(local_machine_state_file)"
    public_key="$(ssh_public_key "$public_key_path")"
    fingerprint="$(ssh_fingerprint "$public_key_path")"
    machine_id="$(machine_id_from_fingerprint "$fingerprint")"
    destination="$registry_dir/$machine_id.toml"

    managed_at=""
    if [[ -f "$state_file" ]] && [[ "$(toml_value "$state_file" machine_id)" == "$machine_id" ]]; then
        managed_at="$(toml_value "$state_file" managed_at)"
        local_state_matches=1
    elif [[ -f "$destination" ]]; then
        managed_at="$(toml_value "$destination" managed_at)"
    fi
    [[ -n "$managed_at" && "$managed_at" != "unknown" ]] || managed_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

    hostname_value="$(scutil --get LocalHostName 2>/dev/null || true)"
    [[ -n "$hostname_value" ]] || hostname_value="$(hostname -s)"
    managed_ref="$(managed_machine_ref)"
    local_bin_ref="$(configured_local_bin_ref)"
    if [[ "$local_state_matches" == "1" ]]; then
        managed_ref="$(toml_value "$state_file" managed_machine_ref)"
        local_bin_ref="$(toml_value "$state_file" local_bin_ref)"
    fi
    existing_ref=""
    if [[ -f "$destination" ]]; then
        existing_ref="$(toml_value "$destination" managed_machine_ref)"
    fi
    if [[ -n "$existing_ref" && "$existing_ref" != "legacy" ]]; then
        managed_ref="$existing_ref"
        local_bin_ref="$(toml_value "$destination" local_bin_ref)"
    fi

    if write_machine_record "$state_file" "$machine_id" "$hostname_value" "$managed_at" \
        "$managed_ref" "$local_bin_ref" "$fingerprint"; then
        echo "Wrote local machine state: $state_file"
    else
        echo "Local machine state already current: $state_file"
    fi

    if write_machine_record "$destination" "$machine_id" "$hostname_value" "$managed_at" \
        "$managed_ref" "$local_bin_ref" "$fingerprint" "$public_key"; then
        echo "Registered fleet machine: $machine_id ($hostname_value)"
        FLEET_CHANGED=1
    else
        echo "Fleet machine already registered: $machine_id ($hostname_value)"
    fi

    CURRENT_MACHINE_ID="$machine_id"
}

generate_fleet_authorized_keys() {
    local registry_dir authorized_keys tmp file hostname_value managed_at machine_id public_key
    registry_dir="$(fleet_registry_dir)"
    authorized_keys="$(fleet_authorized_keys_file)"
    tmp="$(mktemp)"

    {
        echo '# Public keys for host-to-host SSH among your machines.'
        echo '# Generated from managed-machine-config/fleet/machines by managed-machine.'
        if [[ -d "$registry_dir" ]]; then
            while IFS= read -r file; do
                [[ -n "$file" ]] || continue
                hostname_value="$(toml_value "$file" hostname)"
                managed_at="$(toml_value "$file" managed_at)"
                machine_id="$(toml_value "$file" machine_id)"
                public_key="$(toml_value "$file" public_key)"
                if [[ -z "$machine_id" || -z "$public_key" ]]; then
                    echo "Error: incomplete fleet entry: $file" >&2
                    rm -f "$tmp"
                    return 1
                fi
                printf '# %s %s %s\n' "$hostname_value" "${managed_at%%T*}" "$machine_id"
                printf '%s\n' "$public_key"
            done < <(find "$registry_dir" -type f -name '*.toml' | sort)
        fi
    } >"$tmp"

    mkdir -p "$(dirname "$authorized_keys")"
    if [[ -f "$authorized_keys" ]] && cmp -s "$tmp" "$authorized_keys"; then
        rm -f "$tmp"
        return 0
    fi
    mv "$tmp" "$authorized_keys"
    FLEET_CHANGED=1
    echo "Generated fleet authorized keys: $authorized_keys"
}

sync_local_authorized_keys() {
    local repo_authorized_keys local_authorized_keys outside tmp
    repo_authorized_keys="$(fleet_authorized_keys_file)"
    local_authorized_keys="${HOME}/.ssh/authorized_keys"
    mkdir -p "${HOME}/.ssh"
    chmod 700 "${HOME}/.ssh"

    if [[ ! -f "$repo_authorized_keys" ]]; then
        echo "Error: missing $repo_authorized_keys" >&2
        return 1
    fi

    outside="$(mktemp)"
    tmp="$(mktemp)"
    if [[ -f "$local_authorized_keys" ]]; then
        # Trailing blank lines are trimmed so the separator added before the
        # managed block does not accumulate one blank line per rerun.
        awk -v b1="$LEGACY_AUTHORIZED_KEYS_BEGIN" -v e1="$LEGACY_AUTHORIZED_KEYS_END" \
            -v b2="$AUTHORIZED_KEYS_BEGIN" -v e2="$AUTHORIZED_KEYS_END" '
            $0 == b1 { skip = 1; next }
            $0 == e1 { skip = 0; next }
            $0 == b2 { skip = 1; next }
            $0 == e2 { skip = 0; next }
            !skip { lines[++n] = $0 }
            END {
                while (n > 0 && lines[n] == "") n--
                for (i = 1; i <= n; i++) print lines[i]
            }
        ' "$local_authorized_keys" >"$outside"
    else
        : >"$outside"
    fi

    {
        if [[ -s "$outside" ]]; then
            cat "$outside"
            printf '\n'
        fi
        printf '%s\n' "$AUTHORIZED_KEYS_BEGIN"
        cat "$repo_authorized_keys"
        printf '%s\n' "$AUTHORIZED_KEYS_END"
    } >"$tmp"

    if [[ -f "$local_authorized_keys" ]] && cmp -s "$tmp" "$local_authorized_keys"; then
        rm -f "$outside" "$tmp"
        echo "Local authorized keys already current: $local_authorized_keys"
        return 0
    fi
    mv "$tmp" "$local_authorized_keys"
    rm -f "$outside"
    chmod 600 "$local_authorized_keys"
    echo "Updated $local_authorized_keys from fleet registry"
}

# Rewrite the managed block of ~/.ssh/allowed_signers from the fleet registry
# so `git log --show-signature` verifies commits signed by any fleet machine.
# Every fleet key signs as the same GitHub account, so each entry uses the
# configured git email as its principal. Lines outside the managed block are
# never touched.
sync_local_allowed_signers() {
    local principal="$1"
    local registry_dir allowed_signers outside tmp file public_key
    registry_dir="$(fleet_registry_dir)"
    allowed_signers="${HOME}/.ssh/allowed_signers"

    if [[ -z "$principal" ]]; then
        echo "Error: allowed-signers principal (git email) is empty" >&2
        return 1
    fi

    mkdir -p "${HOME}/.ssh"
    chmod 700 "${HOME}/.ssh"

    outside="$(mktemp)"
    tmp="$(mktemp)"
    if [[ -f "$allowed_signers" ]]; then
        # Trailing blank lines are trimmed so the separator added before the
        # managed block does not accumulate one blank line per rerun.
        awk -v b="$ALLOWED_SIGNERS_BEGIN" -v e="$ALLOWED_SIGNERS_END" '
            $0 == b { skip = 1; next }
            $0 == e { skip = 0; next }
            !skip { lines[++n] = $0 }
            END {
                while (n > 0 && lines[n] == "") n--
                for (i = 1; i <= n; i++) print lines[i]
            }
        ' "$allowed_signers" >"$outside"
    else
        : >"$outside"
    fi

    {
        if [[ -s "$outside" ]]; then
            cat "$outside"
            printf '\n'
        fi
        printf '%s\n' "$ALLOWED_SIGNERS_BEGIN"
        if [[ -d "$registry_dir" ]]; then
            while IFS= read -r file; do
                [[ -n "$file" ]] || continue
                # keytype + key only: ssh-keygen -Y parses fixed fields and a
                # trailing key comment would corrupt the entry.
                public_key="$(toml_value "$file" public_key | awk 'NF >= 2 { print $1 " " $2; exit }')"
                if [[ -z "$public_key" ]]; then
                    echo "Error: incomplete fleet entry: $file" >&2
                    rm -f "$outside" "$tmp"
                    return 1
                fi
                printf '%s %s\n' "$principal" "$public_key"
            done < <(find "$registry_dir" -type f -name '*.toml' | sort)
        fi
        printf '%s\n' "$ALLOWED_SIGNERS_END"
    } >"$tmp"

    if [[ -f "$allowed_signers" ]] && cmp -s "$tmp" "$allowed_signers"; then
        rm -f "$outside" "$tmp"
        echo "Local allowed signers already current: $allowed_signers"
        return 0
    fi
    mv "$tmp" "$allowed_signers"
    rm -f "$outside"
    chmod 600 "$allowed_signers"
    echo "Updated $allowed_signers from fleet registry"
}

list_fleet_machines() {
    local registry_dir file
    registry_dir="$(fleet_registry_dir)"
    printf 'MACHINE_ID\tHOSTNAME\tMANAGED_AT\tMANAGED_MACHINE_REF\tLOCAL_BIN_REF\n'
    [[ -d "$registry_dir" ]] || return 0
    while IFS= read -r file; do
        [[ -n "$file" ]] || continue
        printf '%s\t%s\t%s\t%s\t%s\n' \
            "$(toml_value "$file" machine_id)" \
            "$(toml_value "$file" hostname)" \
            "$(toml_value "$file" managed_at)" \
            "$(toml_value "$file" managed_machine_ref)" \
            "$(toml_value "$file" local_bin_ref)"
    done < <(find "$registry_dir" -type f -name '*.toml' | sort)
}

remove_fleet_machine() {
    local machine_id="$1"
    local registry_file state_file
    registry_file="$(fleet_machine_record_file "$machine_id")" || return 1

    REMOVED_PUBLIC_KEY="$(toml_value "$registry_file" public_key)"
    rm -f "$registry_file"
    FLEET_CHANGED=1
    echo "Removed fleet machine: $machine_id"

    state_file="$(local_machine_state_file)"
    if [[ -f "$state_file" ]] && [[ "$(toml_value "$state_file" machine_id)" == "$machine_id" ]]; then
        rm -f "$state_file"
        echo "Removed local machine state: $state_file"
    fi

    generate_fleet_authorized_keys
    sync_local_authorized_keys
}

fleet_machine_record_file() {
    local machine_id="$1"
    local registry_file
    validate_machine_id "$machine_id" || return 1
    registry_file="$(fleet_registry_dir)/$machine_id.toml"
    if [[ ! -f "$registry_file" ]]; then
        echo "Error: fleet machine not found: $machine_id" >&2
        return 1
    fi
    printf '%s\n' "$registry_file"
}

revoke_github_public_key() {
    local public_key="$1"
    local key_body endpoint id key keys found=0
    key_body="$(printf '%s\n' "$public_key" | awk '{print $2}')"
    for endpoint in user/keys user/ssh_signing_keys; do
        if ! keys="$(gh api "$endpoint" --paginate --jq '.[] | [.id, .key] | @tsv')"; then
            echo "Error: could not list GitHub keys from $endpoint" >&2
            return 1
        fi
        while IFS=$'\t' read -r id key; do
            [[ -n "$id" ]] || continue
            if [[ "$(printf '%s\n' "$key" | awk '{print $2}')" == "$key_body" ]]; then
                if ! gh api -X DELETE "$endpoint/$id"; then
                    echo "Error: could not revoke GitHub key $endpoint/$id" >&2
                    return 1
                fi
                echo "Revoked GitHub key: $endpoint/$id"
                found=1
            fi
        done <<<"$keys"
    done
    if [[ "$found" == "0" ]]; then
        echo "No matching GitHub authentication or signing key found."
    fi
}

report_fleet_changes() {
    if [[ "${FLEET_CHANGED:-0}" == "1" ]]; then
        echo "Fleet registry synchronized through $CONFIG_REPO_ROOT"
    fi
}

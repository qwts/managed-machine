#!/usr/bin/env bash
# One-shot layout migrations from older managed-machine versions.
# Applied IDs live in ~/.config/managed-machine/migrations.manifest.

migrations_manifest_file() {
    printf '%s/migrations.manifest\n' "$(managed_machine_config_dir)"
}

migration_already_applied() {
    local id="$1"
    local file
    file="$(migrations_manifest_file)"
    [[ -f "$file" ]] && grep -qxF "$id" "$file"
}

record_migration() {
    local id="$1"
    local file
    file="$(migrations_manifest_file)"
    mkdir -p "$(dirname "$file")"
    chmod 700 "$(dirname "$file")" 2>/dev/null || true
    touch "$file"
    chmod 600 "$file"
    if ! grep -qxF "$id" "$file"; then
        printf '%s\n' "$id" >>"$file"
    fi
}

# Restore a prefix stolen by the old installer (chown to the non-admin user).
migrate_brew_owner_v1() {
    local prefix owner preferred
    prefix="$(brew_prefix_path)" || return 0
    owner="$(brew_prefix_owner)" || return 0
    preferred="$(preferred_brew_owner)"
    # Normalize numeric owner to name when it matches admin — only verified admin UID, not tautological prefix UID
    if [[ "$owner" =~ ^[0-9]+$ && "$preferred" == "admin" ]]; then
        local admin_uid
        admin_uid="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid" && "$owner" == "$admin_uid" ]]; then
            record_migration brew-owner-v1
            return 0
        fi
    fi
    if [[ "$owner" == "$preferred" ]]; then
        record_migration brew-owner-v1
        return 0
    fi
    if user_in_admin_group "$owner"; then
        echo "Homebrew prefix ($prefix) is owned by admin-group user '$owner' — leaving ownership unchanged"
        record_migration brew-owner-v1
        return 0
    fi
    if [[ "$owner" != "$(id -un)" ]]; then
        echo "Error: Homebrew prefix ($prefix) is owned by '$owner'; expected '$preferred'" >&2
        return 1
    fi
    echo "Migrating Homebrew prefix ownership from '$owner' to '$preferred'..."
    if [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == "noninteractive" ]]; then
        echo "Skipped: restoring Homebrew prefix ownership needs the administrator dialog" >&2
        return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
    fi
    elevate_run "restore Homebrew ownership to $preferred" /usr/sbin/chown -R "$preferred" "$prefix" || return $?
    echo "Homebrew prefix now owned by $preferred"
    record_migration brew-owner-v1
}

# Move managed casks out of ~/Applications into /Applications.
migrate_appdir_system_v1() {
    local user_appdir system_appdir token app_name installed dest parent
    user_appdir="$(managed_machine_user_appdir)"
    system_appdir="$(managed_machine_system_appdir)"
    [[ -d "$user_appdir" ]] || {
        record_migration appdir-system-v1
        return 0
    }
    ensure_brew_on_path || {
        record_migration appdir-system-v1
        return 0
    }
    while IFS= read -r token; do
        [[ -n "$token" ]] || continue
        app_name="$(catalog_query field "$token" app_name 2>/dev/null || true)"
        [[ -n "$app_name" ]] || continue
        installed="$user_appdir/$app_name"
        [[ -d "$installed" ]] || continue
        dest="$system_appdir/$app_name"
        if [[ -e "$dest" ]]; then
            echo "skip: $app_name already exists at $dest"
            continue
        fi
        if cask_app_is_running "$installed"; then
            echo "skip: $app_name is running at $installed — quit, then re-run managed-machine --update"
            continue
        fi
        echo "Migrating $app_name from $user_appdir to $system_appdir..."
        parent="$(dirname "$installed")"
        if [[ -w "$system_appdir" && -w "$parent" ]]; then
            /bin/mv "$installed" "$dest" || return 1
        else
            if [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == "noninteractive" ]]; then
                echo "Skipped: moving apps into $system_appdir needs the administrator dialog" >&2
                return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
            fi
            elevate_run "move $app_name to $system_appdir" /bin/mv "$installed" "$dest" || return $?
        fi
        if cask_has_receipt "$token"; then
            brew_run install --cask --adopt "$(cask_qualified_token "$token")" --appdir="$system_appdir" || return 1
        fi
    done < <(catalog_query cask-tokens 2>/dev/null || true)
    record_migration appdir-system-v1
}

run_managed_machine_migrations() {
    migrate_brew_owner_v1 || return $?
    migrate_appdir_system_v1 || return $?
}

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
    if declare -F elevate_plan_step_has_recorded_status >/dev/null \
        && elevate_plan_step_has_recorded_status "migrations" "chown-prefix"; then
        local plan_rc=0
        elevate_plan_step_status "migrations" "chown-prefix" || plan_rc=$?
        [[ "$plan_rc" -eq 0 ]] || return "$plan_rc"
        preferred="$(preferred_brew_owner)"
        echo "Homebrew prefix now owned by $preferred"
        record_migration brew-owner-v1
        return 0
    fi
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
    if declare -F elevate_plan_step_has_recorded_status >/dev/null \
        && elevate_plan_step_has_recorded_status "migrations" "migrate-app-mv"; then
        local plan_rc=0
        elevate_plan_step_status "migrations" "migrate-app-mv" || plan_rc=$?
        [[ "$plan_rc" -eq 0 ]] || return "$plan_rc"
        if elevate_plan_step_has_recorded_status "migrations" "migrate-cask-adopt"; then
            elevate_plan_step_status "migrations" "migrate-cask-adopt" || plan_rc=$?
            [[ "$plan_rc" -eq 0 ]] || return "$plan_rc"
        fi
    elif declare -F elevate_plan_step_in_plan >/dev/null \
        && ( elevate_plan_step_in_plan "migrations" "migrate-app-mv" || elevate_plan_step_in_plan "migrations" "migrate-cask-adopt" ); then
        if [[ -n "${MANAGED_MACHINE_ELEVATION_DECLINED_FILE:-}" && -e "$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" ]]; then
            echo "Skipped: administrator authorization was declined earlier in this run — not asking again to move apps into $(managed_machine_system_appdir)" >&2
            return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
        fi
        echo "Error: administrator authorization was cancelled or failed — did not migrate apps into $(managed_machine_system_appdir)" >&2
        return 1
    fi
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
        local has_receipt=0 need_adopt_elevate=0 brew_owner=""
        if cask_has_receipt "$token"; then
            has_receipt=1
            brew_owner="$(resolved_brew_owner 2>/dev/null || id -un)"
            if brew_needs_elevation || ( declare -F adopt_bundle_needs_chown >/dev/null && adopt_bundle_needs_chown "$installed" "$brew_owner" ); then
                need_adopt_elevate=1
            fi
        fi
        if [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == "noninteractive" ]] \
            && [[ ! -w "$system_appdir" || ! -w "$parent" || "$need_adopt_elevate" -eq 1 ]]; then
            echo "Skipped: moving apps into $system_appdir needs the administrator dialog" >&2
            return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
        fi
        if [[ -w "$system_appdir" && -w "$parent" ]]; then
            /bin/mv "$installed" "$dest" || return 1
        else
            elevate_run "move $app_name to $system_appdir" /bin/mv "$installed" "$dest" || return $?
        fi
        if [[ "$has_receipt" -eq 1 ]]; then
            if declare -F brew_adopt_cask_in_appdir >/dev/null; then
                brew_adopt_cask_in_appdir "$(cask_qualified_token "$token")" "$system_appdir" "$dest" "migrations" || return $?
            else
                brew_run install --cask --adopt "$(cask_qualified_token "$token")" --appdir="$system_appdir" || return $?
            fi
        fi
    done < <(catalog_query cask-tokens 2>/dev/null || true)
    record_migration appdir-system-v1
}

# True when shared Homebrew directories or links under <prefix> lack world
# read/traverse permissions (for example after an elevated brew or git run
# inherited umask 077). Never inspects <prefix>/var/mm-home.
prefix_needs_umask_repair() {
    local prefix="${1:-}"
    [[ -n "$prefix" && -d "$prefix" ]] || return 1
    case "$prefix" in
        /opt/homebrew|/usr/local)
            local brew_bin=""
            brew_bin="$(command -v brew 2>/dev/null || true)"
            [[ "$brew_bin" == "$prefix/bin/brew" && "$HOME" == /Users/* ]] || return 1
            ;;
    esac
    local sub d
    for sub in Cellar Caskroom opt bin sbin lib include share etc var/homebrew Library; do
        d="$prefix/$sub"
        if [[ -d "$d" || -L "$d" ]]; then
            if [[ ! -r "$d" || ( -d "$d" && ! -x "$d" ) ]]; then
                return 0
            fi
            if [[ -n "$(/usr/bin/find "$d" \( \( -type l ! -perm -o+rx \) -o \( ! -type l ! -perm -o+r \) -o \( -type d ! -perm -o+x \) \) -print -quit 2>/dev/null || true)" ]]; then
                return 0
            fi
        fi
    done
    return 1
}

repair_prefix_shared_permissions() {
    local prefix="$1" sub d
    for sub in Cellar Caskroom opt bin sbin lib include share etc var/homebrew Library; do
        d="$prefix/$sub"
        if [[ -d "$d" || -L "$d" ]]; then
            /usr/bin/find "$d" ! -type l \( ! -perm -o+r -o \( -type d ! -perm -o+x \) \) \
                -exec /bin/chmod a+rX {} + || return 1
            /usr/bin/find "$d" -type l ! -perm -o+rx \
                -exec /bin/chmod -h a+rx {} + || return 1
        fi
    done
}

# True when a managed .app bundle under the system appdir lacks world
# read/traverse permissions (for example after an elevated vendor-dmg copy
# inherited umask 077).
system_app_needs_umask_repair() {
    local bundle="${1:-}"
    [[ -n "$bundle" && -d "$bundle" && ! -L "$bundle" ]] || return 1
    if [[ ! -r "$bundle" || ! -x "$bundle" ]]; then
        return 0
    fi
    if [[ -n "$(/usr/bin/find "$bundle" \( \( -type l ! -perm -o+rx \) -o \( ! -type l ! -perm -o+r \) -o \( -type d ! -perm -o+x \) \) -print -quit 2>/dev/null || true)" ]]; then
        return 0
    fi
    return 1
}

managed_system_app_bundles() {
    local system_appdir
    system_appdir="$(managed_machine_system_appdir)"
    [[ -d "$system_appdir" ]] || return 0
    if [[ "$system_appdir" == "/Applications" && "$HOME" != /Users/* ]]; then
        return 0
    fi
    declare -F catalog_query >/dev/null || return 0
    local kind app_name seen="|"
    while IFS='|' read -r _ kind _ app_name _; do
        case "$kind" in
            signed-cask|cask|vendor-dmg)
                [[ -n "$app_name" ]] || continue
                case "$seen" in
                    *"|$app_name|"*) continue ;;
                esac
                seen="${seen}${app_name}|"
                if [[ -d "$system_appdir/$app_name" && ! -L "$system_appdir/$app_name" ]]; then
                    printf '%s\n' "$system_appdir/$app_name"
                fi
                ;;
        esac
    done < <(catalog_query rows 2>/dev/null || true)
}

# Repair 0700 Homebrew prefix entries and /Applications/*.app bundles left by
# older elevated brew/git/vendor-dmg runs that inherited umask 077.
migrate_umask_permissions_v1() {
    local prefix="" owner="" bundle
    local had_plan=0 plan_rc=0
    if declare -F elevate_plan_step_has_recorded_status >/dev/null; then
        if elevate_plan_step_has_recorded_status "migrations" "chmod-prefix-shared"; then
            had_plan=1
            elevate_plan_step_status "migrations" "chmod-prefix-shared" || plan_rc=$?
            [[ "$plan_rc" -eq 0 ]] || return "$plan_rc"
        fi
        if elevate_plan_step_has_recorded_status "migrations" "chmod-app-bundle"; then
            had_plan=1
            elevate_plan_step_status "migrations" "chmod-app-bundle" || plan_rc=$?
            [[ "$plan_rc" -eq 0 ]] || return "$plan_rc"
        fi
        if [[ "$had_plan" -eq 1 ]]; then
            record_migration umask-permissions-v1
            return 0
        fi
    fi
    if declare -F elevate_plan_step_in_plan >/dev/null \
        && ( elevate_plan_step_in_plan "migrations" "chmod-prefix-shared" || elevate_plan_step_in_plan "migrations" "chmod-app-bundle" ); then
        if [[ -n "${MANAGED_MACHINE_ELEVATION_DECLINED_FILE:-}" && -e "$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" ]]; then
            echo "Skipped: administrator authorization was declined earlier in this run — not asking again to repair shared permissions" >&2
            return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
        fi
        echo "Error: administrator authorization was cancelled or failed — did not repair shared permissions" >&2
        return 1
    fi

    prefix="$(brew_prefix_path 2>/dev/null || true)"
    if [[ -n "$prefix" ]] && prefix_needs_umask_repair "$prefix"; then
        owner="$(resolved_brew_owner 2>/dev/null || brew_prefix_owner 2>/dev/null || id -un)"
        if declare -F resolve_brew_owner_name >/dev/null; then
            owner="$(resolve_brew_owner_name "$owner")"
        fi
        if [[ -z "$owner" || "$owner" == "$(id -un)" ]]; then
            echo "Repairing shared Homebrew permissions in $prefix..."
            repair_prefix_shared_permissions "$prefix" || return 1
        else
            if [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == "noninteractive" ]]; then
                echo "Skipped: repairing shared Homebrew permissions in $prefix needs the administrator dialog" >&2
                return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
            fi
            echo "Repairing shared Homebrew permissions in $prefix (as $owner)..."
            elevate_as_user "repair shared Homebrew permissions in $prefix" "$owner" \
                /bin/sh -c 'for sub in Cellar Caskroom opt bin sbin lib include share etc var/homebrew Library; do d="$1/$sub"; if [ -d "$d" ] || [ -L "$d" ]; then /usr/bin/find "$d" ! -type l \( ! -perm -o+r -o \( -type d ! -perm -o+x \) \) -exec /bin/chmod a+rX {} + || exit 1; /usr/bin/find "$d" -type l ! -perm -o+rx -exec /bin/chmod -h a+rx {} + || exit 1; fi; done' \
                _ "$prefix" || return $?
        fi
    fi

    while IFS= read -r bundle; do
        [[ -n "$bundle" ]] || continue
        system_app_needs_umask_repair "$bundle" || continue
        if [[ -O "$bundle" ]]; then
            echo "Repairing shared app bundle permissions on $bundle..."
            /bin/chmod -R a+rX,go-w "$bundle" || return 1
        else
            if [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == "noninteractive" ]]; then
                echo "Skipped: repairing app bundle permissions on $bundle needs the administrator dialog" >&2
                return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
            fi
            echo "Repairing shared app bundle permissions on $bundle..."
            elevate_run "repair permissions on $(basename "$bundle")" /bin/chmod -R a+rX,go-w "$bundle" || return $?
        fi
    done < <(managed_system_app_bundles)

    record_migration umask-permissions-v1
}

run_managed_machine_migrations() {
    migrate_brew_owner_v1 || return $?
    migrate_appdir_system_v1 || return $?
    migrate_umask_permissions_v1 || return $?
}

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
    if [[ -n "${MANAGED_MACHINE_PLAN_CONFIRM:-}" ]]; then
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

# ---------------------------------------------------------------------------
# Interactive plan construction, confirmation, and single-approval execution
# (#174).
#
# Plan files live in a 700 session directory:
#   $session_dir/steps.tsv   — <step_name>\t<privileged:0|1>\t<description>
#   $session_dir/actions.tsv — <action_id>\t<step_name>\t<action_kind>\t<owner>\t<target>\t<dest_path>\t<extra>\t<summary>
# ---------------------------------------------------------------------------

bootstrap_new_plan_id() {
    local phase="${1:-bootstrap}"
    local ts rand
    ts="$(date -u +%Y%m%dT%H%M%SZ)"
    rand="$(LC_ALL=C tr -dc 'a-f0-9' </dev/urandom 2>/dev/null | head -c 8 || printf '%04x%04x' "$$" "$RANDOM")"
    printf '%s-%s-%s\n' "$phase" "$ts" "$rand"
}

bootstrap_plan_add_step() {
    local session_dir="$1"
    local step_name="$2"
    local privileged="$3"
    local description="$4"
    printf '%s\t%s\t%s\n' "$step_name" "$privileged" "$description" >>"$session_dir/steps.tsv"
}

bootstrap_plan_add_action() {
    local session_dir="$1"
    local step_name="$2"
    local action_kind="$3"
    local owner="$4"
    local target="$5"
    local dest_path="$6"
    local extra="$7"
    local summary="$8"
    local idx=1
    if [[ -f "$session_dir/actions.tsv" ]]; then
        idx=$(( $(wc -l <"$session_dir/actions.tsv" | tr -d '[:space:]') + 1 ))
    fi
    local action_id
    action_id="$(printf 'act-%03d' "$idx")"
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$action_id" "$step_name" "$action_kind" "$owner" "$target" "$dest_path" "$extra" "$summary" \
        >>"$session_dir/actions.tsv"
}

# Inspect one step read-only and append its plan row(s) to $session_dir.
bootstrap_inspect_step_for_plan() {
    local session_dir="$1"
    local step_name="$2"
    local brew_will_elevate=0 brew_owner=""

    if declare -F ensure_brew_on_path >/dev/null; then
        ensure_brew_on_path >/dev/null 2>&1 || true
    fi
    if declare -F resolved_brew_owner >/dev/null; then
        brew_owner="$(resolved_brew_owner 2>/dev/null || true)"
    elif declare -F preferred_brew_owner >/dev/null; then
        brew_owner="$(preferred_brew_owner 2>/dev/null || true)"
    fi
    [[ -n "$brew_owner" ]] || brew_owner="$(id -un)"

    if declare -F brew_needs_elevation >/dev/null && brew_needs_elevation; then
        brew_will_elevate=1
    elif ! bootstrap_brew_available && [[ "$brew_owner" != "$(id -un)" ]]; then
        brew_will_elevate=1
    fi

    case "$step_name" in
        migrations)
            local mig_priv=0 mig_notes=()
            local prefix current_owner target_owner
            if declare -F brew_prefix_path >/dev/null && prefix="$(brew_prefix_path 2>/dev/null || true)" && [[ -n "$prefix" ]]; then
                current_owner="$(brew_prefix_owner 2>/dev/null || true)"
                target_owner="$(preferred_brew_owner 2>/dev/null || true)"
                if [[ -n "$current_owner" && -n "$target_owner" && "$current_owner" == "$(id -un)" && "$current_owner" != "$target_owner" ]] \
                    && ! user_in_admin_group "$current_owner"; then
                    mig_priv=1
                    local summary="restore Homebrew prefix ownership to $target_owner:admin on $prefix"
                    mig_notes+=("$summary")
                    bootstrap_plan_add_action "$session_dir" "migrations" "chown-prefix" "$target_owner" "$prefix" "$prefix" "" "$summary"
                fi
            fi
            if declare -F cask_allowlist_tokens >/dev/null; then
                local system_appdir="${MANAGED_MACHINE_SYSTEM_APPDIR:-/Applications}"
                local -a homes=()
                if [[ "${MANAGED_MACHINE_MIGRATE_SCAN_HOMES:-0}" == "1" && -d /Users ]]; then
                    local entry
                    for entry in /Users/*; do
                        [[ -d "$entry" ]] || continue
                        case "$(basename "$entry")" in
                            Shared|Guest|.*) continue ;;
                        esac
                        homes+=("$entry")
                    done
                else
                    homes=("${HOME}")
                fi
                local home_dir token row app_name src dest
                for home_dir in "${homes[@]}"; do
                    [[ -d "$home_dir/Applications" ]] || continue
                    while IFS= read -r token; do
                        [[ -n "$token" ]] || continue
                        row="$(cask_allowlist_row "$token" 2>/dev/null)" || continue
                        IFS='|' read -r app_name _ <<<"$row"
                        src="$home_dir/Applications/$app_name"
                        dest="$system_appdir/$app_name"
                        [[ -d "$src" && ! -e "$dest" ]] || continue
                        if declare -F cask_app_is_running >/dev/null && cask_app_is_running "$src"; then
                            continue
                        fi
                        if [[ ! -w "$home_dir/Applications" || ! -w "$system_appdir" ]]; then
                            mig_priv=1
                            local mv_summary="move $app_name from $src to $dest (as root)"
                            mig_notes+=("$mv_summary")
                            bootstrap_plan_add_action "$session_dir" "migrations" "migrate-app-mv" "root" "$src" "$dest" "$system_appdir" "$mv_summary"
                        fi
                        if cask_has_receipt "$token" 2>/dev/null && [[ "$brew_will_elevate" -eq 1 ]]; then
                            mig_priv=1
                            local adopt_summary="adopt $app_name (homebrew/cask/$token) in $system_appdir (brew as $brew_owner)"
                            mig_notes+=("$adopt_summary")
                            bootstrap_plan_add_action "$session_dir" "migrations" "migrate-cask-adopt" "$brew_owner" "homebrew/cask/$token" "$dest" "$system_appdir" "$adopt_summary"
                        fi
                    done < <(cask_allowlist_tokens 2>/dev/null || true)
                done
            fi
            if [[ "$mig_priv" -eq 1 ]]; then
                local IFS='; '
                bootstrap_plan_add_step "$session_dir" "migrations" 1 "${mig_notes[*]}"
            else
                bootstrap_plan_add_step "$session_dir" "migrations" 0 "check Homebrew prefix ownership and user app bundles (no administrator access)"
            fi
            ;;
        setup-hostname)
            local target_host="${MANAGED_MACHINE_HOSTNAME:-}"
            if [[ -z "$target_host" ]] && declare -F hostname_needs_prompt >/dev/null && hostname_needs_prompt; then
                if [[ -f "${REPO_ROOT:-}/setup-hostname" ]] && grep -Fq 'apply_scutil_hostname' "${REPO_ROOT:-}/setup-hostname" 2>/dev/null && declare -F prompt_hostname >/dev/null; then
                    local default_host chosen_host=""
                    default_host="$(hostname_manifest_name 2>/dev/null || true)"
                    [[ -n "$default_host" ]] || default_host="$(current_local_hostname 2>/dev/null || scutil --get LocalHostName 2>/dev/null || hostname -s 2>/dev/null || echo unknown)"
                    if chosen_host="$(prompt_hostname "$default_host")"; then
                        chosen_host="${chosen_host// /}"
                        export MANAGED_MACHINE_HOSTNAME="$chosen_host"
                        if hostname_is_valid "$chosen_host"; then
                            target_host="$chosen_host"
                        fi
                    else
                        export MANAGED_MACHINE_HOSTNAME_DECLINED=1
                    fi
                fi
            fi
            if [[ -n "$target_host" ]]; then
                local summary="set Mac hostname (LocalHostName, HostName, ComputerName) to $target_host (scutil as root)"
                bootstrap_plan_add_action "$session_dir" "setup-hostname" "scutil-hostname" "root" "$target_host" "LocalHostName,HostName,ComputerName" "" "$summary"
                bootstrap_plan_add_step "$session_dir" "setup-hostname" 1 "$summary"
            elif [[ "${MANAGED_MACHINE_HOSTNAME_DECLINED:-0}" == "1" ]]; then
                bootstrap_plan_add_step "$session_dir" "setup-hostname" 0 "hostname dialog cancelled or unavailable (will skip)"
            elif declare -F hostname_needs_prompt >/dev/null && hostname_needs_prompt; then
                local cur_host
                cur_host="$(scutil --get LocalHostName 2>/dev/null || hostname -s 2>/dev/null || echo unknown)"
                bootstrap_plan_add_step "$session_dir" "setup-hostname" 0 "hostname is default ($cur_host); prompt or skip if blank"
            else
                local cur_host
                cur_host="$(scutil --get LocalHostName 2>/dev/null || hostname -s 2>/dev/null || echo configured)"
                bootstrap_plan_add_step "$session_dir" "setup-hostname" 0 "hostname already set ($cur_host); no administrator access"
            fi
            ;;
        setup-brew)
            if bootstrap_brew_available; then
                local pfx
                pfx="$(brew_prefix_path 2>/dev/null || echo /opt/homebrew)"
                bootstrap_plan_add_step "$session_dir" "setup-brew" 0 "Homebrew already installed at $pfx (owner $brew_owner); ensure shell PATH block"
            else
                local pfx="/opt/homebrew"
                [[ "$(uname -m 2>/dev/null || true)" == "x86_64" ]] && pfx="/usr/local"
                local summary="install Homebrew into $pfx owned by $brew_owner:admin (installer runs as $brew_owner, never root)"
                bootstrap_plan_add_action "$session_dir" "setup-brew" "install-homebrew" "$brew_owner" "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh" "$pfx" "" "$summary"
                bootstrap_plan_add_step "$session_dir" "setup-brew" 1 "$summary"
            fi
            ;;
        setup-zsh)
            bootstrap_plan_add_step "$session_dir" "setup-zsh" 0 "configure ~/.zshenv and ~/.zshrc (owner $(id -un), no administrator access)"
            ;;
        setup-nvm)
            bootstrap_plan_add_step "$session_dir" "setup-nvm" 0 "install/verify nvm and Node LTS under ~/.nvm (owner $(id -un), no administrator access)"
            ;;
        setup-git-hooks)
            bootstrap_plan_add_step "$session_dir" "setup-git-hooks" 0 "wire gitleaks git hooks (no administrator access)"
            ;;
        setup-gh)
            local gh_pfx=""
            if declare -F brew_prefix_path >/dev/null; then
                gh_pfx="$(brew_prefix_path 2>/dev/null || true)"
            fi
            [[ -n "$gh_pfx" ]] || gh_pfx="/opt/homebrew"
            if [[ -f "${REPO_ROOT:-}/setup-gh" ]] && grep -Fq 'brew_run install gh' "${REPO_ROOT:-}/setup-gh" 2>/dev/null \
                && [[ ! -x "$gh_pfx/bin/gh" && "$brew_will_elevate" -eq 1 ]]; then
                local gh_summary="install formula gh into $gh_pfx/Cellar/gh (brew as $brew_owner) and verify GitHub CLI auth"
                bootstrap_plan_add_action "$session_dir" "setup-gh" "brew-formula" "$brew_owner" "gh" "$gh_pfx/Cellar/gh" "$gh_pfx" "$gh_summary"
                bootstrap_plan_add_step "$session_dir" "setup-gh" 1 "$gh_summary"
            else
                bootstrap_plan_add_step "$session_dir" "setup-gh" 0 "verify GitHub CLI auth and refresh ~/.config/managed-machine/config checkout (owner $(id -un), no administrator access)"
            fi
            ;;
        setup-bin)
            local lb_dir="${LOCAL_BIN_DIR:-${HOME_BIN_DIR:-}}"
            if [[ -z "$lb_dir" ]]; then
                if [[ -d "${REPO_ROOT:-}/managed-machine-config" ]]; then
                    lb_dir="${REPO_ROOT}/local-bin"
                elif [[ -n "${REPO_ROOT:-}" ]]; then
                    lb_dir="${REPO_ROOT}/../local-bin"
                fi
            fi
            local lb_priv=0 lb_owner="" lb_ref="" lb_mode="update"
            local lb_pin_file="${CONFIG_REPO_ROOT:-}/local-bin.ref"
            if [[ -n "${CONFIG_REPO_ROOT:-}" && -f "$lb_pin_file" ]]; then
                lb_ref="${LOCAL_BIN_REF:-${HOME_BIN_REF:-$(grep -vE '^[[:space:]]*(#|$)' "$lb_pin_file" | head -1 | tr -d '[:space:]')}}"
            fi
            local prefix=""
            if declare -F brew_prefix_path >/dev/null; then
                prefix="$(brew_prefix_path 2>/dev/null || true)"
            fi
            if [[ -n "$lb_dir" && -n "$prefix" && "$lb_dir" == "$prefix"/* && "$brew_owner" != "$(id -un)" ]]; then
                lb_owner="$brew_owner"
                if [[ ! -d "$lb_dir" ]]; then
                    lb_priv=1
                    lb_mode="clone"
                elif [[ -e "$lb_dir/.git" && -n "$lb_ref" ]]; then
                    local head_sha tag_sha
                    head_sha="$(git -c "safe.directory=$lb_dir" -C "$lb_dir" rev-parse HEAD 2>/dev/null || true)"
                    tag_sha="$(git -c "safe.directory=$lb_dir" -C "$lb_dir" rev-parse --verify --quiet "refs/tags/${lb_ref}^{commit}" 2>/dev/null || true)"
                    if [[ -z "$head_sha" || ( "$tag_sha" != "$head_sha" && "$head_sha" != "$lb_ref"* ) ]]; then
                        lb_priv=1
                        lb_mode="update"
                    fi
                fi
            fi
            if [[ "$lb_priv" -eq 1 ]]; then
                local lb_repo="${LOCAL_BIN_REPO:-${HOME_BIN_REPO:-https://github.com/qwts/local-bin.git}}"
                local lb_extra="${lb_ref}|${MANAGED_MACHINE_ALLOW_BRANCH_PIN:-0}|${lb_pin_file}|${lb_repo}"
                local summary="checkout local-bin@${lb_ref:-pinned} in $lb_dir (git as $lb_owner) and link ~/.local/bin"
                bootstrap_plan_add_action "$session_dir" "setup-bin" "git-checkout-owner" "$lb_owner" "$lb_mode" "$lb_dir" "$lb_extra" "$summary"
                bootstrap_plan_add_step "$session_dir" "setup-bin" 1 "$summary"
            else
                bootstrap_plan_add_step "$session_dir" "setup-bin" 0 "sync local-bin${lb_ref:+ ($lb_ref)} and link tools into ~/.local/bin (no administrator access)"
            fi
            ;;
        setup-zsh-functions)
            local zf_dir="${ZSH_FUNCTIONS_DIR:-}"
            if [[ -z "$zf_dir" ]]; then
                if [[ -d "${REPO_ROOT:-}/managed-machine-config" ]]; then
                    zf_dir="${REPO_ROOT}/zsh-functions"
                elif [[ -n "${REPO_ROOT:-}" ]]; then
                    zf_dir="${REPO_ROOT}/../zsh-functions"
                fi
            fi
            local zf_priv=0 zf_owner="" zf_ref="" zf_mode="update"
            local zf_pin_file="${CONFIG_REPO_ROOT:-}/zsh-functions.ref"
            if [[ -n "${CONFIG_REPO_ROOT:-}" && -f "$zf_pin_file" ]]; then
                zf_ref="${ZSH_FUNCTIONS_REF:-$(grep -vE '^[[:space:]]*(#|$)' "$zf_pin_file" | head -1 | tr -d '[:space:]')}"
            fi
            local zf_prefix=""
            if declare -F brew_prefix_path >/dev/null; then
                zf_prefix="$(brew_prefix_path 2>/dev/null || true)"
            fi
            if [[ -n "$zf_dir" && -n "$zf_prefix" && "$zf_dir" == "$zf_prefix"/* && "$brew_owner" != "$(id -un)" ]]; then
                zf_owner="$brew_owner"
                if [[ ! -d "$zf_dir" ]]; then
                    zf_priv=1
                    zf_mode="clone"
                elif [[ -e "$zf_dir/.git" && -n "$zf_ref" ]]; then
                    local zf_head zf_tag
                    zf_head="$(git -c "safe.directory=$zf_dir" -C "$zf_dir" rev-parse HEAD 2>/dev/null || true)"
                    zf_tag="$(git -c "safe.directory=$zf_dir" -C "$zf_dir" rev-parse --verify --quiet "refs/tags/${zf_ref}^{commit}" 2>/dev/null || true)"
                    if [[ -z "$zf_head" || ( "$zf_tag" != "$zf_head" && "$zf_head" != "$zf_ref"* ) ]]; then
                        zf_priv=1
                        zf_mode="update"
                    fi
                fi
            fi
            if [[ "$zf_priv" -eq 1 ]]; then
                local zf_repo="${ZSH_FUNCTIONS_REPO:-https://github.com/qwts/zsh-functions.git}"
                local zf_extra="${zf_ref}|${MANAGED_MACHINE_ALLOW_BRANCH_PIN:-0}|${zf_pin_file}|${zf_repo}"
                local zf_summary="checkout zsh-functions@${zf_ref:-pinned} in $zf_dir (git as $zf_owner) and install ~/.config/zsh/functions"
                bootstrap_plan_add_action "$session_dir" "setup-zsh-functions" "git-checkout-owner" "$zf_owner" "$zf_mode" "$zf_dir" "$zf_extra" "$zf_summary"
                bootstrap_plan_add_step "$session_dir" "setup-zsh-functions" 1 "$zf_summary"
            else
                bootstrap_plan_add_step "$session_dir" "setup-zsh-functions" 0 "install zsh functions into ~/.config/zsh/functions (owner $(id -un), no administrator access)"
            fi
            ;;
        setup-rust)
            bootstrap_plan_add_step "$session_dir" "setup-rust" 0 "install/verify rustup and stable toolchain under ~/.cargo and ~/.rustup (owner $(id -un), no administrator access)"
            ;;
        setup-*)
            local app_name="${step_name#setup-}"
            if declare -F declined_app_is_recorded >/dev/null && declined_app_is_recorded "$app_name"; then
                bootstrap_plan_add_step "$session_dir" "$step_name" 0 "not installed (declined) — run 'managed-machine setup $app_name' to install"
                return 0
            fi
            if ! declare -F catalog_app_kind >/dev/null || ! catalog_has_app "$app_name" 2>/dev/null; then
                bootstrap_plan_add_step "$session_dir" "$step_name" 0 "run $step_name (no administrator access)"
                return 0
            fi
            local kind
            kind="$(catalog_app_kind "$app_name" 2>/dev/null || true)"
            case "$kind" in
                signed-cask|cask)
                    local token row bundle_name team_id url_hosts homepage_hosts allow_rolling override appdir dest installed
                    token="$(catalog_app_field "$app_name" token 2>/dev/null || true)"
                    row="$(cask_allowlist_row "$token" 2>/dev/null || true)"
                    IFS='|' read -r bundle_name team_id url_hosts homepage_hosts allow_rolling <<<"$row"
                    override="$(cask_appdir_override_for_token "$token")"
                    appdir="$(resolve_cask_appdir "$override")"
                    dest="$appdir/$bundle_name"
                    if installed="$(find_cask_app "$bundle_name" "$override" 2>/dev/null)"; then
                        if cask_has_receipt "$token" 2>/dev/null; then
                            bootstrap_plan_add_step "$session_dir" "$step_name" 0 "$bundle_name already installed at $installed (no administrator access)"
                        else
                            bootstrap_plan_add_step "$session_dir" "$step_name" 0 "$bundle_name present at $installed without Homebrew cask receipt (will skip)"
                        fi
                    elif [[ "$brew_will_elevate" -eq 1 || ( -d "$appdir" && ! -w "$appdir" ) || ( ! -d "$appdir" && ! -w "$(dirname "$appdir")" ) ]]; then
                        local summary="install $bundle_name (homebrew/cask/$token) into $dest (brew as $brew_owner)"
                        bootstrap_plan_add_action "$session_dir" "$step_name" "signed-cask" "$brew_owner" "homebrew/cask/$token" "$dest" "$appdir|$team_id|$allow_rolling" "$summary"
                        bootstrap_plan_add_step "$session_dir" "$step_name" 1 "$summary"
                    else
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "install $bundle_name (homebrew/cask/$token) into $dest (owner $(id -un), no administrator access)"
                    fi
                    ;;
                vendor-dmg)
                    local row bundle_name team_id url sha256 url_hosts version allow_rolling sparkle override installed destdir dest retire_cask has_retire=0
                    row="$(vendor_dmg_allowlist_row "$app_name" 2>/dev/null || true)"
                    IFS='|' read -r bundle_name team_id url sha256 url_hosts version allow_rolling sparkle <<<"$row"
                    if [[ -z "$url" ]]; then
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "$app_name has no build for $(uname -m) (will skip)"
                        return 0
                    fi
                    override="$(vendor_dmg_appdir_override "$app_name")"
                    retire_cask="$(catalog_app_field "$app_name" retire_cask 2>/dev/null || true)"
                    if [[ -n "$retire_cask" ]] && command -v brew >/dev/null 2>&1 && cask_has_receipt "$retire_cask" 2>/dev/null; then
                        has_retire=1
                    fi
                    if installed="$(vendor_dmg_find_app "$bundle_name" "$override" 2>/dev/null)"; then
                        local current
                        current="$(vendor_dmg_bundle_version "$installed")"
                        if [[ "$has_retire" -eq 0 && ( -z "$version" || "$current" == "$version" ) ]]; then
                            bootstrap_plan_add_step "$session_dir" "$step_name" 0 "$bundle_name already installed at $installed${current:+ ($current)} (no administrator access)"
                            return 0
                        fi
                        destdir="$(dirname "$installed")"
                        dest="$installed"
                    else
                        destdir="$(resolve_cask_appdir "$override")"
                        dest="$destdir/$bundle_name"
                    fi
                    local dmg_priv=0 dmg_notes=()
                    if [[ "$has_retire" -eq 1 && "$brew_will_elevate" -eq 1 ]]; then
                        dmg_priv=1
                        local retire_summary="retire Homebrew cask $retire_cask for $bundle_name at ${installed:-$dest} (brew as $brew_owner)"
                        dmg_notes+=("$retire_summary")
                        bootstrap_plan_add_action "$session_dir" "$step_name" "vendor-dmg-retire-cask" "$brew_owner" "$retire_cask" "${installed:-}" "$app_name" "$retire_summary"
                    fi
                    local need_copy=1
                    if [[ -n "$installed" && "$has_retire" -eq 1 ]]; then
                        local current_ver
                        current_ver="$(vendor_dmg_bundle_version "$installed")"
                        if [[ -z "$version" || "$current_ver" == "$version" ]]; then
                            need_copy=0
                        fi
                    fi
                    if [[ "$need_copy" -eq 1 ]]; then
                        if [[ ( -d "$destdir" && ! -w "$destdir" ) || ( ! -d "$destdir" && ! -w "$(dirname "$destdir")" ) ]]; then
                            dmg_priv=1
                            local dmg_summary="install $bundle_name (vendor DMG $app_name${version:+ $version}) into $dest (as root)"
                            dmg_notes+=("$dmg_summary")
                            bootstrap_plan_add_action "$session_dir" "$step_name" "vendor-dmg" "root" "$app_name" "$dest" "$destdir|$team_id|$allow_rolling" "$dmg_summary"
                        fi
                    fi
                    if [[ "$dmg_priv" -eq 1 ]]; then
                        local IFS='; '
                        bootstrap_plan_add_step "$session_dir" "$step_name" 1 "${dmg_notes[*]}"
                    else
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "install $bundle_name (vendor DMG $app_name${version:+ $version}) into $dest (no administrator access)"
                    fi
                    ;;
                brew-formula)
                    local formula pfx
                    formula="$(catalog_app_field "$app_name" formula 2>/dev/null || true)"
                    pfx="$(brew_prefix_path 2>/dev/null || echo /opt/homebrew)"
                    if brew_formula_has_receipt "$formula" 2>/dev/null; then
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "formula homebrew/core/$formula already installed in $pfx (no administrator access)"
                    elif [[ "$brew_will_elevate" -eq 1 ]]; then
                        local summary="install formula homebrew/core/$formula into $pfx/Cellar/$formula (brew as $brew_owner)"
                        bootstrap_plan_add_action "$session_dir" "$step_name" "brew-formula" "$brew_owner" "homebrew/core/$formula" "$pfx/Cellar/$formula" "$pfx" "$summary"
                        bootstrap_plan_add_step "$session_dir" "$step_name" 1 "$summary"
                    else
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "install formula homebrew/core/$formula into $pfx (owner $(id -un), no administrator access)"
                    fi
                    ;;
                npm)
                    local package version command spec npm_owner npm_pfx already=0
                    package="$(catalog_app_field "$app_name" package 2>/dev/null || true)"
                    version="$(catalog_app_field "$app_name" version 2>/dev/null || true)"
                    command="$(catalog_app_field "$app_name" command 2>/dev/null || printf '%s\n' "$package")"
                    spec="$package${version:+@$version}"
                    if command -v npm >/dev/null 2>&1; then
                        if [[ -n "$version" ]]; then
                            local cur_npm
                            cur_npm="$(npm_global_package_version "$package" 2>/dev/null || true)"
                            if [[ "$cur_npm" == "$version" ]] || ( [[ -z "$cur_npm" ]] && command -v "$command" >/dev/null 2>&1 ); then
                                already=1
                            fi
                        elif command -v "$command" >/dev/null 2>&1 || npm_package_has_receipt "$package" 2>/dev/null; then
                            already=1
                        fi
                    fi
                    if [[ "$already" -eq 1 ]]; then
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "npm package $spec already installed (no administrator access)"
                    elif declare -F npm_needs_elevation >/dev/null && npm_needs_elevation; then
                        npm_owner="$(resolved_npm_owner)"
                        npm_pfx="$(npm_public prefix -g 2>/dev/null || echo /opt/homebrew)"
                        local summary="install npm package $spec into $npm_pfx/lib/node_modules/$package (npm as $npm_owner)"
                        bootstrap_plan_add_action "$session_dir" "$step_name" "npm-global" "$npm_owner" "$spec" "$npm_pfx/lib/node_modules/$package" "$(command -v npm)" "$summary"
                        bootstrap_plan_add_step "$session_dir" "$step_name" 1 "$summary"
                    else
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "install npm package $spec into user global prefix (owner $(id -un), no administrator access)"
                    fi
                    ;;
                official-cli|uv-tool|opencode|devin)
                    local cmd_name
                    cmd_name="$(catalog_app_field "$app_name" command 2>/dev/null || printf '%s\n' "$app_name")"
                    if command -v "$cmd_name" >/dev/null 2>&1; then
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "$app_name ($kind) already installed at $(command -v "$cmd_name") (no administrator access)"
                    else
                        bootstrap_plan_add_step "$session_dir" "$step_name" 0 "install $app_name ($kind) into ~/.local/bin (owner $(id -un), no administrator access)"
                    fi
                    ;;
                *)
                    bootstrap_plan_add_step "$session_dir" "$step_name" 0 "install $app_name ($kind) (no administrator access)"
                    ;;
            esac
            ;;
        *)
            bootstrap_plan_add_step "$session_dir" "$step_name" 0 "run $step_name (no administrator access)"
            ;;
    esac
}

# Build the execution plan for the given step names in $session_dir.
bootstrap_build_plan() {
    local session_dir="$1"
    shift
    mkdir -p "$session_dir"
    chmod 700 "$session_dir"
    : >"$session_dir/steps.tsv"
    : >"$session_dir/actions.tsv"
    chmod 600 "$session_dir/steps.tsv" "$session_dir/actions.tsv"
    local step_name
    for step_name in "$@"; do
        bootstrap_inspect_step_for_plan "$session_dir" "$step_name"
    done
    MANAGED_MACHINE_APPROVED_PLAN_SHA256="$(shasum -a 256 "$session_dir/actions.tsv" | awk '{print $1}')"
    export MANAGED_MACHINE_APPROVED_PLAN_SHA256
}

bootstrap_plan_has_privileged_actions() {
    local session_dir="$1"
    [[ -f "$session_dir/actions.tsv" && -s "$session_dir/actions.tsv" ]]
}

# Print the human-readable execution plan to stdout.
bootstrap_print_plan() {
    local phase_title="$1"
    local plan_id="$2"
    local session_dir="$3"
    local step_name privileged desc priv_count=0

    echo
    echo "Managed-machine $phase_title plan ($plan_id):"
    echo "  Steps to run:"
    if [[ -f "$session_dir/steps.tsv" ]]; then
        while IFS=$'\t' read -r step_name privileged desc; do
            [[ -n "$step_name" ]] || continue
            if [[ "$privileged" == "1" ]]; then
                priv_count=$((priv_count + 1))
                printf '    [admin] %-24s %s\n' "$step_name" "$desc"
            else
                printf '    [user]  %-24s %s\n' "$step_name" "$desc"
            fi
        done <"$session_dir/steps.tsv"
    fi

    if [[ -f "$session_dir/actions.tsv" && -s "$session_dir/actions.tsv" ]]; then
        echo "  Privileged changes requiring administrator authorization (one approval for all):"
        local action_id a_step a_kind a_owner a_target a_dest a_extra a_summary
        while IFS=$'\t' read -r action_id a_step a_kind a_owner a_target a_dest a_extra a_summary; do
            [[ -n "$action_id" ]] || continue
            printf '    - %s: %s (target: %s, path: %s, owner: %s)\n' \
                "$a_step" "$a_summary" "$a_target" "$a_dest" "$a_owner"
        done <"$session_dir/actions.tsv"
    else
        echo "  Privileged changes: none (no administrator dialog needed)"
    fi
    echo
}

# Ask the owner to confirm the printed plan once in the terminal.
# Returns 0 when confirmed, 1 when declined.
bootstrap_confirm_plan() {
    local plan_id="$1"
    local session_dir="$2"
    local reply=""

    if ! bootstrap_plan_has_privileged_actions "$session_dir" && [[ -z "${MANAGED_MACHINE_PLAN_CONFIRM:-}" ]]; then
        return 0
    fi

    if [[ -n "${MANAGED_MACHINE_PLAN_CONFIRM:-}" ]]; then
        reply="$MANAGED_MACHINE_PLAN_CONFIRM"
    elif [[ -t 0 && -t 1 ]]; then
        printf 'Approve plan %s and continue? [Y/n] ' "$plan_id"
        if ! IFS= read -r reply; then
            reply="n"
        fi
    elif [[ -p /dev/stdin || -f /dev/stdin ]]; then
        if ! IFS= read -r reply; then
            reply="n"
        fi
    elif [[ -c /dev/stdin && ! -t 0 ]]; then
        reply="n"
    elif ( : </dev/tty && : >/dev/tty ) 2>/dev/null; then
        printf 'Approve plan %s and continue? [Y/n] ' "$plan_id" >/dev/tty
        if ! IFS= read -r reply </dev/tty; then
            reply="n"
        fi
    else
        reply="n"
    fi

    case "$reply" in
        ""|[Yy]|[Yy][Ee][Ss])
            return 0
            ;;
        *)
            echo "Plan $plan_id declined — no changes made." >&2
            return 1
            ;;
    esac
}

# Record any privileged catalog apps from a declined plan into
# declined-apps.manifest so future --update runs do not offer them again (#174).
bootstrap_record_declined_plan_apps() {
    local session_dir="$1"
    local reason="${2:-plan declined}"
    [[ -f "$session_dir/actions.tsv" ]] || return 0
    declare -F record_declined_app >/dev/null || return 0

    local action_id step_name action_kind rest
    while IFS=$'\t' read -r action_id step_name action_kind rest; do
        [[ -n "$action_id" ]] || continue
        case "$step_name" in
            setup-hostname|setup-zsh|setup-brew|setup-nvm|setup-git-hooks|setup-gh|setup-bin|setup-zsh-functions|setup-rust|migrations)
                continue
                ;;
            setup-*)
                record_declined_app "${step_name#setup-}" "$reason"
                ;;
        esac
    done <"$session_dir/actions.tsv"
}

# Pre-verify formula/cask/npm sources and pre-stage any vendor-dmg downloads in
# user space, then run all privileged actions in $session_dir/actions.tsv under
# a single osascript dialog (#174).
bootstrap_stage_and_authorize_plan() {
    local plan_id="$1"
    local session_dir="$2"
    local phase_label="${3:-apply approved plan $plan_id}"
    local plan_file="$session_dir/actions.tsv"

    if [[ ! -f "$plan_file" || ! -s "$plan_file" ]]; then
        return 0
    fi

    if [[ -n "${MANAGED_MACHINE_APPROVED_PLAN_SHA256:-}" ]]; then
        local current_sha
        current_sha="$(shasum -a 256 "$plan_file" | awk '{print $1}')"
        if [[ "$current_sha" != "$MANAGED_MACHINE_APPROVED_PLAN_SHA256" ]]; then
            echo "Error: refusing to execute plan $plan_id: plan checksum mismatch before authorization" >&2
            return 1
        fi
    fi

    mkdir -p "$session_dir/staged" "$session_dir/status" "$session_dir/detail"

    local action_id step_name action_kind owner target dest_path extra summary
    while IFS=$'\t' read -r action_id step_name action_kind owner target dest_path extra summary; do
        [[ -n "$action_id" ]] || continue
        case "$action_kind" in
            brew-formula)
                if command -v brew >/dev/null 2>&1 && declare -F verify_brew_formula_source >/dev/null; then
                    local formula="${target#homebrew/core/}"
                    if ! verify_brew_formula_source "$formula"; then
                        printf '1\n' >"$session_dir/status/$action_id"
                        printf 'formula source verification failed for %s\n' "$formula" >"$session_dir/detail/$action_id"
                        return 1
                    fi
                fi
                ;;
            signed-cask|migrate-cask-adopt)
                if command -v brew >/dev/null 2>&1 && declare -F verify_cask_source >/dev/null; then
                    local token="${target#homebrew/cask/}" row url_hosts homepage_hosts allow_rolling
                    row="$(cask_allowlist_row "$token" 2>/dev/null || true)"
                    IFS='|' read -r _ _ url_hosts homepage_hosts allow_rolling <<<"$row"
                    if ! verify_cask_source "$token" "$url_hosts" "$homepage_hosts" "$allow_rolling"; then
                        printf '1\n' >"$session_dir/status/$action_id"
                        printf 'cask source verification failed for %s\n' "$token" >"$session_dir/detail/$action_id"
                        return 1
                    fi
                fi
                ;;
            vendor-dmg)
                if ! vendor_dmg_stage_for_plan "$target" "$session_dir/staged/$action_id"; then
                    printf '1\n' >"$session_dir/status/$action_id"
                    printf 'vendor DMG staging failed for %s\n' "$target" >"$session_dir/detail/$action_id"
                    return 1
                fi
                ;;
            npm-global)
                if command -v npm >/dev/null 2>&1 && declare -F verify_npm_package_source >/dev/null; then
                    local pkg="$target" ver=""
                    if [[ "$target" == ?*@* ]]; then
                        pkg="${target%@*}"
                        ver="${target##*@}"
                    fi
                    if ! verify_npm_package_source "$pkg" "$ver"; then
                        printf '1\n' >"$session_dir/status/$action_id"
                        printf 'npm package verification failed for %s\n' "$target" >"$session_dir/detail/$action_id"
                        return 1
                    fi
                fi
                ;;
        esac
    done <"$plan_file"

    elevate_run_plan "$plan_id" "$phase_label" "$plan_file" "$session_dir"
}

# Re-resolve CONFIG_REPO_ROOT after setup-gh refreshes the persistent config
# checkout, and discover any newly added auto catalog apps (or newly updated
# local-bin / zsh-functions refs) so they are not skipped (#174).
bootstrap_sync_post_gh_catalog_apps() {
    local phase_title="$1"
    local plan_id="$2"
    local session_dir="$3"
    local manifest_tmp="$4"

    if declare -F managed_machine_config_repo_dir >/dev/null; then
        local refreshed_root=""
        refreshed_root="$(managed_machine_config_repo_dir 2>/dev/null || true)"
        if [[ -n "$refreshed_root" && -d "$refreshed_root" ]]; then
            CONFIG_REPO_ROOT="$refreshed_root"
            export CONFIG_REPO_ROOT
        fi
    fi

    local -a newly_discovered_steps=()
    local candidate existing found
    while IFS= read -r candidate; do
        [[ -n "$candidate" ]] || continue
        found=0
        if [[ ${#auto_app_names[@]} -gt 0 ]]; then
            for existing in "${auto_app_names[@]}"; do
                if [[ "$existing" == "$candidate" ]]; then
                    found=1
                    break
                fi
            done
        fi
        if [[ "$found" -eq 0 ]]; then
            auto_app_names+=("$candidate")
            newly_discovered_steps+=("setup-$candidate")
        fi
    done < <(catalog_auto_app_names 2>/dev/null || true)

    [[ -n "$session_dir" && -d "$session_dir" ]] || return 0

    # Also check whether setup-bin or setup-zsh-functions acquired a new pin
    # during setup-gh that was not yet in $session_dir/actions.tsv.
    local pin_step
    for pin_step in setup-bin setup-zsh-functions; do
        if ! awk -F'\t' -v s="$pin_step" '$2 == s { found=1; exit } END { exit (found ? 0 : 1) }' "$session_dir/actions.tsv" 2>/dev/null; then
            local probe_dir="$session_dir/probe-$pin_step"
            mkdir -p "$probe_dir"
            : >"$probe_dir/steps.tsv"
            : >"$probe_dir/actions.tsv"
            bootstrap_inspect_step_for_plan "$probe_dir" "$pin_step"
            if [[ -s "$probe_dir/actions.tsv" ]]; then
                newly_discovered_steps+=("$pin_step")
            fi
            rm -rf "$probe_dir"
        fi
    done

    [[ ${#newly_discovered_steps[@]} -gt 0 ]] || return 0

    local delta_dir="$session_dir/post-gh"
    local saved_sha="${MANAGED_MACHINE_APPROVED_PLAN_SHA256:-}"
    bootstrap_build_plan "$delta_dir" "${newly_discovered_steps[@]}"
    cat "$delta_dir/steps.tsv" >>"$session_dir/steps.tsv"

    if ! bootstrap_plan_has_privileged_actions "$delta_dir"; then
        MANAGED_MACHINE_APPROVED_PLAN_SHA256="$saved_sha"
        export MANAGED_MACHINE_APPROVED_PLAN_SHA256
        rm -rf "$delta_dir"
        return 0
    fi

    # Renumber delta action IDs so they do not collide with $session_dir/actions.tsv.
    local base_idx=0
    if [[ -f "$session_dir/actions.tsv" ]]; then
        base_idx="$(wc -l <"$session_dir/actions.tsv" | tr -d '[:space:]')"
    fi
    local renumbered="$delta_dir/actions.renumbered.tsv"
    : >"$renumbered"
    local _old_id a_step a_kind a_owner a_target a_dest a_extra a_summary new_id
    while IFS=$'\t' read -r _old_id a_step a_kind a_owner a_target a_dest a_extra a_summary; do
        [[ -n "$_old_id" ]] || continue
        base_idx=$((base_idx + 1))
        new_id="$(printf 'act-%03d' "$base_idx")"
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$new_id" "$a_step" "$a_kind" "$a_owner" "$a_target" "$a_dest" "$a_extra" "$a_summary" \
            >>"$renumbered"
    done <"$delta_dir/actions.tsv"
    mv "$renumbered" "$delta_dir/actions.tsv"
    MANAGED_MACHINE_APPROVED_PLAN_SHA256="$(shasum -a 256 "$delta_dir/actions.tsv" | awk '{print $1}')"
    export MANAGED_MACHINE_APPROVED_PLAN_SHA256

    bootstrap_print_plan "$phase_title (post-gh catalog refresh)" "$plan_id" "$delta_dir"
    if [[ -z "${MANAGED_MACHINE_ELEVATION_DECLINED_FILE:-}" || ! -e "$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" ]]; then
        if ! bootstrap_confirm_plan "$plan_id" "$delta_dir"; then
            [[ -z "${MANAGED_MACHINE_ELEVATION_DECLINED_FILE:-}" ]] || : >"$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" 2>/dev/null || true
            bootstrap_record_declined_plan_apps "$delta_dir" "plan declined"
        elif ! bootstrap_stage_and_authorize_plan "$plan_id" "$delta_dir" "apply approved $phase_title plan $plan_id"; then
            if [[ -n "${MANAGED_MACHINE_ELEVATION_DECLINED_FILE:-}" && -e "$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" ]]; then
                bootstrap_record_declined_plan_apps "$delta_dir" "authorization declined"
            fi
        fi
    else
        bootstrap_record_declined_plan_apps "$delta_dir" "authorization declined"
    fi

    cat "$delta_dir/actions.tsv" >>"$session_dir/actions.tsv"
    mkdir -p "$session_dir/status" "$session_dir/detail" "$session_dir/out"
    cp -R "$delta_dir/status/." "$session_dir/status/" 2>/dev/null || true
    cp -R "$delta_dir/detail/." "$session_dir/detail/" 2>/dev/null || true
    cp -R "$delta_dir/out/." "$session_dir/out/" 2>/dev/null || true
    if [[ -n "$manifest_tmp" && -f "$manifest_tmp" ]]; then
        while IFS=$'\t' read -r new_id a_step a_kind a_owner a_target a_dest a_extra a_summary; do
            [[ -n "$new_id" ]] || continue
            printf 'plan\t%s\t%s\t%s\t%s\t%s\t%s\n' \
                "$plan_id" "$a_step" "$a_kind" "$a_owner" "$a_target" "$a_dest" >>"$manifest_tmp"
        done <"$delta_dir/actions.tsv"
    fi
    MANAGED_MACHINE_APPROVED_PLAN_SHA256="$(shasum -a 256 "$session_dir/actions.tsv" | awk '{print $1}')"
    export MANAGED_MACHINE_APPROVED_PLAN_SHA256
    rm -rf "$delta_dir"
}

# Print the plan summary for a step if it had privileged actions in $session_dir.
bootstrap_plan_summary_for_step() {
    local plan_id="$1"
    local session_dir="$2"
    local want_step="$3"
    local plan_file="$session_dir/actions.tsv"
    [[ -f "$plan_file" ]] || return 1

    local parts=()
    local action_id step_name action_kind owner target dest_path extra summary
    while IFS=$'\t' read -r action_id step_name action_kind owner target dest_path extra summary; do
        [[ -n "$action_id" && "$step_name" == "$want_step" ]] || continue
        parts+=("$summary")
    done <"$plan_file"
    [[ ${#parts[@]} -gt 0 ]] || return 1
    local IFS='; '
    printf 'plan=%s: %s\n' "$plan_id" "${parts[*]}"
}

# Write plan metadata lines to a manifest file (bootstrap.manifest / update.manifest).
bootstrap_record_plan_manifest_lines() {
    local plan_id="$1"
    local session_dir="$2"
    local manifest_tmp="$3"
    printf 'plan_id=%s\n' "$plan_id" >>"$manifest_tmp"
    if [[ -f "$session_dir/actions.tsv" ]]; then
        local action_id step_name action_kind owner target dest_path extra summary
        while IFS=$'\t' read -r action_id step_name action_kind owner target dest_path extra summary; do
            [[ -n "$action_id" ]] || continue
            printf 'plan\t%s\t%s\t%s\t%s\t%s\t%s\n' \
                "$plan_id" "$step_name" "$action_kind" "$owner" "$target" "$dest_path" >>"$manifest_tmp"
        done <"$session_dir/actions.tsv"
    fi
}

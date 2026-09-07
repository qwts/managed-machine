#!/usr/bin/env bash

account_setup_error() {
    printf 'Error: %s\n' "$1" >&2
    return 1
}

account_setup_path() {
    local path="$1" part cursor="$HOME" rest
    [[ "$HOME" == /* && "$HOME" != / && -d "$HOME" && ! -L "$HOME" && -O "$HOME" ]] || {
        account_setup_error 'Run account setup with the validated target account UID and HOME.'; return 1;
    }
    case "$HOME/" in */../*|*/./*|*//*) return 1 ;; esac
    case "$path" in "$HOME"|"$HOME"/*) ;; *) account_setup_error 'Setup paths must remain inside the target account HOME; remove inherited path overrides.'; return 1 ;; esac
    rest="${path#"$HOME"}"
    while [[ -n "$rest" ]]; do
        rest="${rest#/}"; part="${rest%%/*}"
        [[ -n "$part" && "$part" != . && "$part" != .. ]] || return 1
        cursor="$cursor/$part"
        [[ ! -L "$cursor" && ( ! -e "$cursor" || -O "$cursor" ) ]] || {
            account_setup_error 'Refusing symlinked or foreign-owned target paths; repair account-local ownership and paths.'; return 1;
        }
        [[ "$rest" == */* ]] || break
        [[ ! -e "$cursor" || -d "$cursor" ]] || return 1
        rest="${rest#*/}"
    done
}

account_setup_tree() {
    local path="$1"
    account_setup_path "$path" || return 1
    [[ -e "$path" ]] || return 0
    if [[ -n "$(find "$path" \( -type l -o ! -user "$(id -un)" \) -print)" ]]; then
        account_setup_error 'Refusing symlinked or foreign-owned account checkout/state; repair target account paths.'
        return 1
    fi
}

account_setup_clean() {
    env -i HOME="$HOME" USER="$(id -un)" LOGNAME="$(id -un)" \
        PATH=/usr/bin:/bin:/usr/sbin:/sbin \
        GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_OPTIONAL_LOCKS=0 \
        GIT_ALLOW_PROTOCOL=file GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0=/dev/null \
        "$@"
}

account_config_source() {
    local root="$1" candidate
    if [[ -n "${CONFIG_REPO_ROOT:-}" ]]; then
        account_setup_path "$CONFIG_REPO_ROOT" || return 1
        [[ -d "$CONFIG_REPO_ROOT" && -e "$CONFIG_REPO_ROOT/.git" ]] || {
            account_setup_error 'The target CONFIG_REPO_ROOT must be an existing config checkout.'; return 1;
        }
        printf '%s\n' "$CONFIG_REPO_ROOT"
        return
    fi
    candidate="${XDG_DATA_HOME:-$HOME/.local/share}/managed-machine/managed-machine-config"
    account_setup_path "$candidate" || return 1
    if [[ -d "$candidate" && -e "$candidate/.git" ]]; then
        printf '%s\n' "$candidate"; return
    fi
    for candidate in "$root/managed-machine-config" "$root/../managed-machine-config"; do
        if [[ -d "$candidate" && ! -L "$candidate" && -e "$candidate/.git" ]]; then
            (cd "$candidate" && pwd -P)
            return
        fi
    done
    account_setup_error 'Config seed unavailable; update managed-machine bundled private sources from the owning administrator account, then retry account setup.'
    return 75
}

account_prepare_config() {
    local root="$1" target seed candidate status
    target="${CONFIG_REPO_ROOT:-${XDG_DATA_HOME:-$HOME/.local/share}/managed-machine/managed-machine-config}"
    account_setup_tree "$target" || return 1
    seed=""
    if [[ -z "${CONFIG_REPO_ROOT:-}" || ! -e "$target" ]]; then
        for candidate in "$root/managed-machine-config" "$root/../managed-machine-config"; do
            if [[ -e "$candidate" || -L "$candidate" ]]; then
                seed="$candidate"
                break
            fi
        done
        if [[ -z "$seed" && ! -e "$target" ]]; then
            account_setup_error 'Config seed unavailable; update managed-machine bundled private sources from the owning administrator account, then retry account setup.'; return 75
        fi
    fi
    if ! account_setup_clean /bin/bash -c '
        source "$1/lib/install.sh"
        materialize_managed_machine_config_repo "$2" "$3" "$MANAGED_MACHINE_CONFIG_DEFAULT_REPO"
    ' bash "$root" "$seed" "$target" >/dev/null 2>&1; then
        account_setup_error 'Could not prepare account config; check target checkout ownership and origin, or update the clean bundled config seed.'; return 1
    fi
    if [[ -n "${CONFIG_REPO_ROOT:-}" ]]; then
        printf 'Using explicit target-owned CONFIG_REPO_ROOT; bundled config refresh is disabled.\n' >&2
    elif [[ -z "$seed" ]]; then
        printf 'Config seed unavailable; retaining existing valid account config without refresh.\n' >&2
    else
        status=0
        account_setup_clean /bin/bash -c '
            source "$1/lib/install.sh"
            seed="$2"; target="$3"
            assert_bundled_config_seed "$seed" || exit 75
            clean="$(config_repo_git "$seed" status --porcelain --untracked-files=all)" || exit 75
            [[ -z "$clean" ]] || { printf "Config seed is dirty; retaining existing account config.\n" >&2; exit 75; }
            head="$(config_repo_git "$seed" rev-parse --verify HEAD)" || exit 75
            if git -C "$target" merge-base --is-ancestor "$head" HEAD 2>/dev/null; then exit 0; fi
            clean="$(git -C "$target" status --porcelain --untracked-files=all)" || exit 75
            [[ -z "$clean" ]] || { printf "Account config is dirty; cannot fast-forward to bundled config.\n" >&2; exit 75; }
            seed="$(cd "$seed" && pwd -P)" || exit 75
            git -c "safe.directory=$seed" -c "safe.directory=$seed/.git" -C "$target" fetch --quiet --no-tags --no-write-fetch-head --recurse-submodules=no "file://$seed" "$head" || exit 75
            if git -C "$target" merge-base --is-ancestor "$head" HEAD; then exit 0; fi
            git -C "$target" merge-base --is-ancestor HEAD "$head" || {
                printf "Account config has local/divergent commits; cannot fast-forward to bundled config.\n" >&2; exit 75;
            }
            git -C "$target" -c core.fsmonitor=false merge --ff-only --no-edit --no-overwrite-ignore "$head" || exit 75
        ' bash "$root" "$seed" "$target" >/dev/null || status=$?
        if [[ "$status" != 0 ]]; then
            account_setup_error 'Account config was not refreshed; repair the clean bundled seed or reconcile target changes, then retry.'
            return "$status"
        fi
        account_setup_tree "$target" || return 1
    fi
    export CONFIG_REPO_ROOT="$target"
    printf '%s\n' "$target"
}

account_prepare_shell() {
    local root="$1" zdotdir="${ZDOTDIR:-$HOME}" name manifest
    [[ -n "${CONFIG_REPO_ROOT:-}" ]] || { account_setup_error 'Prepare target account config before shell setup.'; return 1; }
    account_setup_tree "$CONFIG_REPO_ROOT" || return 1
    account_setup_path "$zdotdir" || return 1
    manifest="$HOME/.config/managed-machine/zsh.manifest"
    account_setup_path "$manifest" || return 1
    for name in .zshenv .zprofile .zshrc; do
        account_setup_path "$zdotdir/$name" || return 1
        [[ -f "$CONFIG_REPO_ROOT/dotfiles/zsh/$name" && ! -L "$CONFIG_REPO_ROOT/dotfiles/zsh/$name" ]] || {
            account_setup_error 'Shell templates unavailable; update the target account config checkout.'; return 1;
        }
    done
    if ! account_setup_clean /bin/bash -c '
        set -e
        source "$1/lib/install.sh"
        for name in .zshenv .zprofile .zshrc; do
            install_home_file "$2/dotfiles/zsh/$name" "$3/$name" "$4" "$name"
            ensure_local_bin_in_zshrc "$3/$name"
        done
    ' bash "$root" "$CONFIG_REPO_ROOT" "$zdotdir" "$manifest" >/dev/null 2>&1; then
        account_setup_error 'Could not install account shell profiles; check account-local paths and permissions.'; return 1
    fi
}

account_local_bin_destinations() {
    local target="$1" category path name link owner manifest="$HOME/.config/local-bin/linked-commands"
    if ! grep -qxF 'CATEGORIES=(images rename files media utils dns)' "$target/install" \
        || ! grep -qxF 'SKIP_NAMES=(trash_util.py)' "$target/install"; then
        account_setup_error 'Unsupported local-bin installer command manifest; update bundled local-bin sources.'; return 75
    fi
    if [[ -f "$manifest" ]]; then
        while IFS= read -r name || [[ -n "$name" ]]; do
            [[ -z "$name" || "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || {
                account_setup_error 'Invalid local-bin linked-commands manifest; repair the target account manifest.'; return 1;
            }
            path="$HOME/.local/bin/$name"
            if [[ -n "$name" && -L "$path" ]]; then
                link="$(readlink "$path")"
                case "$link" in "$target/"*)
                    owner="$(stat -c '%u' "$path" 2>/dev/null || stat -f '%u' "$path" 2>/dev/null)"
                    [[ "$owner" == "$(id -u)" ]] || {
                        account_setup_error "Local-bin command collision: $name; repair ownership before retrying."; return 1;
                    }
                    ;;
                esac
            fi
        done < "$manifest"
    fi
    for category in images rename files media utils dns; do
        [[ -d "$target/$category" ]] || {
            account_setup_error 'Unsupported local-bin installer layout; update bundled local-bin sources.'; return 75;
        }
        for path in "$target/$category/"*; do
            [[ -f "$path" && -x "$path" ]] || continue
            name="${path##*/}"
            [[ "$name" != trash_util.py ]] || continue
            [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || return 1
            path="$HOME/.local/bin/$name"
            if [[ -L "$path" ]]; then
                link="$(readlink "$path")"
                owner="$(stat -c '%u' "$path" 2>/dev/null || stat -f '%u' "$path" 2>/dev/null)"
                if [[ "$owner" == "$(id -u)" && -f "$manifest" ]] && grep -qxF "$name" "$manifest"; then
                    case "$link" in
                        "$target/"*|"${XDG_DATA_HOME:-$HOME/.local/share}/managed-machine/local-bin/"*)
                            account_setup_path "$link" || return 1
                            continue ;;
                    esac
                fi
            elif [[ ! -e "$path" ]]; then
                continue
            fi
            account_setup_error "Local-bin command collision: $name; preserve or relocate the existing command before retrying."
            return 1
        done
    done
    path="$HOME/.local/bin/local-bin-run"
    account_setup_path "$path" || return 1
    if [[ -e "$path" ]] && { [[ ! -f "$path" ]] || ! grep -qxF '# local-bin-run <command> [args...] — exec a local-bin command, but first' "$path"; }; then
        account_setup_error 'Local-bin command collision: local-bin-run; preserve or relocate the existing command before retrying.'
        return 1
    fi
}

account_prepare_local_bin() {
    local root="$1" ref source resolved="" target path branch=""
    [[ -n "${CONFIG_REPO_ROOT:-}" ]] || { account_setup_error 'Prepare target account config before local-bin setup.'; return 1; }
    account_setup_path "$CONFIG_REPO_ROOT/local-bin.ref" || return 1
    [[ -f "$CONFIG_REPO_ROOT/local-bin.ref" ]] || { account_setup_error 'Missing local-bin.ref; update target account config.'; return 75; }
    ref="$(awk '!/^[[:space:]]*(#|$)/ {gsub(/[[:space:]]/, ""); print; exit}' "$CONFIG_REPO_ROOT/local-bin.ref")"
    [[ "$ref" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ && "$ref" != *..* ]] || {
        account_setup_error 'Invalid local-bin pin; pin an immutable tag or commit in target account config.'; return 1;
    }
    for source in "$root/local-bin" "$root/../local-bin"; do
        [[ -d "$source" && ! -L "$source" && -e "$source/.git" ]] || continue
        source="$(cd "$source" && pwd -P)"
        resolved="$(account_setup_clean git -c "safe.directory=$source" -C "$source" rev-parse --verify --quiet "refs/tags/${ref}^{commit}" 2>/dev/null)" || resolved=""
        if [[ -z "$resolved" && "$ref" =~ ^[0-9a-f]{7,40}$ ]]; then
            resolved="$(account_setup_clean git -c "safe.directory=$source" -C "$source" rev-parse --verify --quiet "${ref}^{commit}" 2>/dev/null)" || resolved=""
        fi
        [[ -z "$resolved" ]] || break
        if account_setup_clean git -c "safe.directory=$source" -C "$source" show-ref --verify --quiet "refs/heads/$ref" \
            || account_setup_clean git -c "safe.directory=$source" -C "$source" show-ref --verify --quiet "refs/remotes/origin/$ref"; then
            branch=1
        fi
    done
    if [[ -z "$resolved" && -n "$branch" ]]; then
        account_setup_error 'Invalid local-bin branch pin; pin an immutable tag or commit in target account config.'; return 1
    fi
    [[ -n "$resolved" ]] || {
        account_setup_error 'Pinned local-bin unavailable offline; update managed-machine bundled local-bin to include the immutable local-bin.ref from the owning administrator account, then retry account setup.'; return 75;
    }
    target="${LOCAL_BIN_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/managed-machine/local-bin/$resolved}"
    for path in "$target" "$HOME/.local/bin" "$HOME/.bin" "$HOME/.zshrc" "${ZDOTDIR:-$HOME}/.zshrc" "$HOME/.config/managed-machine/local-bin.manifest"; do
        account_setup_path "$path" || return 1
    done
    account_setup_tree "$HOME/.config/local-bin" || return 1
    if ! account_setup_clean ZDOTDIR="${ZDOTDIR:-$HOME}" /bin/bash -c '
        set -e
        source "$1/lib/install.sh"
        target="$3"
        if [[ ! -e "$target" ]]; then
            mkdir -p "$(dirname "$target")"
            git -c "safe.directory=$2" clone --quiet --no-hardlinks "$2" "$target"
        fi
        assert_managed_machine_config_repo "$target"
        [[ -z "$(find "$target" -type l -print)" ]]
        [[ -z "$(find "$target" ! -user "$(id -un)" -print)" ]]
        [[ -z "$(git -C "$target" status --porcelain)" ]]
        git -C "$target" checkout --quiet --detach "$4"
        [[ -z "$(find "$target" -type l -print)" ]]
        git -C "$target" remote set-url origin https://github.com/qwts/local-bin.git
    ' bash "$root" "$source" "$target" "$resolved" >/dev/null 2>&1; then
        account_setup_error 'Could not prepare account-local tools; check the clean account-owned local-bin checkout and target permissions.'; return 1
    fi
    account_local_bin_destinations "$target" || return $?
    if ! account_setup_clean ZDOTDIR="${ZDOTDIR:-$HOME}" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
        LOCAL_BIN_DIR="$target" LOCAL_BIN_REF="$ref" /bin/bash -c '
        set -e
        /bin/bash "$1/setup-bin"
        source "$1/lib/install.sh"
        for profile in "$HOME/.zshrc" "${ZDOTDIR:-$HOME}/.zshrc"; do
            if [[ -f "$profile" ]]; then ensure_local_bin_in_zshrc "$profile"; fi
        done
    ' bash "$root" >/dev/null 2>&1; then
        account_setup_error 'Could not install account-local tools; check target account paths and permissions.'; return 1
    fi
}

account_prepare_environment() {
    account_prepare_config "$1" >/dev/null || return $?
    account_prepare_shell "$1" || return $?
    account_prepare_local_bin "$1"
}

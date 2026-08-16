#!/usr/bin/env bash
# Persistent managed-machine-config checkout and synchronization helpers.

MANAGED_MACHINE_CONFIG_DEFAULT_REPO="https://github.com/qwts/managed-machine-config.git"
MANAGED_MACHINE_CONFIG_DEFAULT_BRANCH="main"

managed_machine_data_dir() {
    printf '%s/managed-machine\n' "${XDG_DATA_HOME:-${HOME}/.local/share}"
}

managed_machine_config_checkout_dir() {
    printf '%s/managed-machine-config\n' "$(managed_machine_data_dir)"
}

managed_machine_config_repo_url() {
    printf '%s\n' "${MANAGED_MACHINE_CONFIG_REPO_URL:-$MANAGED_MACHINE_CONFIG_DEFAULT_REPO}"
}

managed_machine_config_branch() {
    printf '%s\n' "${MANAGED_MACHINE_CONFIG_BRANCH:-$MANAGED_MACHINE_CONFIG_DEFAULT_BRANCH}"
}

config_repo_owner() {
    if stat -c '%U' "$1" >/dev/null 2>&1; then
        stat -c '%U' "$1"
    else
        stat -f '%Su' "$1" 2>/dev/null || true
    fi
}

# Git 2.35+ refuses a repo owned by another user. The Homebrew-bundled seed
# is prefix-owned by design. Trust that exact path for one command; do not
# write safe.directory into the user's gitconfig.
config_repo_git() {
    local repo="$1"
    shift
    git -c "safe.directory=$repo" -C "$repo" "$@"
}

assert_config_repo_git_root() {
    local repo="$1"
    local label="$2"
    local repo_root repo_path
    local -a git_c=(git -C "$repo")

    if [[ "${3:-}" == "--trust-foreign-owner" ]]; then
        git_c=(git -c "safe.directory=$repo" -C "$repo")
    fi
    if [[ -L "$repo" ]]; then
        echo "Error: refusing symlinked ${label}: $repo" >&2
        return 1
    fi
    if [[ "$("${git_c[@]}" rev-parse --is-inside-work-tree 2>/dev/null || true)" != "true" ]]; then
        echo "Error: ${label} is not a git checkout: $repo" >&2
        return 1
    fi
    repo_root="$("${git_c[@]}" rev-parse --show-toplevel 2>/dev/null || true)"
    repo_path="$(cd "$repo" 2>/dev/null && pwd -P)"
    if [[ -z "$repo_path" || "$repo_path" != "$repo_root" ]]; then
        echo "Error: ${label} path must be the repository root: $repo" >&2
        return 1
    fi
}

# Homebrew-bundled seed is a read-only trusted input. It is owned by the
# prefix owner (often `admin`), not the invoking user, and must not use the
# writable-checkout owner assertion.
assert_bundled_config_seed() {
    assert_config_repo_git_root "$1" "bundled managed-machine-config seed" --trust-foreign-owner
}

assert_managed_machine_config_repo() {
    local repo="$1"
    local owner

    assert_config_repo_git_root "$repo" "managed-machine-config" || return 1
    owner="$(config_repo_owner "$repo")"
    if [[ -z "$owner" || "$owner" != "$(id -un)" ]]; then
        echo "Error: managed-machine-config must be owned by $(id -un): $repo" >&2
        return 1
    fi
}

assert_config_repo_remote_is_safe() {
    local repo="$1"
    local expected_url="$2"
    local actual_url
    actual_url="$(git -C "$repo" remote get-url origin 2>/dev/null || true)"

    if [[ -z "$actual_url" ]]; then
        echo "Error: managed-machine-config has no origin remote: $repo" >&2
        return 1
    fi
    if ! config_repo_remote_has_no_credentials "$actual_url"; then
        return 1
    fi
    if [[ "$actual_url" != "$expected_url" ]]; then
        echo "Error: managed-machine-config origin does not match the configured repository" >&2
        echo "  expected: $expected_url" >&2
        return 1
    fi
}

config_repo_remote_has_no_credentials() {
    local remote_url="$1"
    case "$remote_url" in
        http://*@*|https://*@*)
            echo "Error: refusing managed-machine-config remote with embedded credentials" >&2
            return 1
            ;;
    esac
}

# Normalize a github.com remote to owner/repo, or fail for other hosts.
config_repo_github_path() {
    local url="$1"
    case "$url" in
        git@github.com:*) url="${url#git@github.com:}" ;;
        ssh://git@github.com/*) url="${url#ssh://git@github.com/}" ;;
        https://github.com/*) url="${url#https://github.com/}" ;;
        *) return 1 ;;
    esac
    printf '%s\n' "${url%.git}"
}

# Machines bootstrapped before the HTTPS-first change carry an SSH origin for
# the same repository. Rewrite it to the configured URL once, out loud, so the
# strict remote assertion keeps rejecting genuinely foreign remotes.
migrate_config_repo_legacy_remote() {
    local repo="$1"
    local expected_url="$2"
    local actual_url actual_path expected_path
    actual_url="$(git -C "$repo" remote get-url origin 2>/dev/null || true)"
    [[ -n "$actual_url" && "$actual_url" != "$expected_url" ]] || return 0
    actual_path="$(config_repo_github_path "$actual_url")" || return 0
    expected_path="$(config_repo_github_path "$expected_url")" || return 0
    [[ "$actual_path" == "$expected_path" ]] || return 0
    echo "Migrating managed-machine-config origin to $expected_url (was $actual_url)" >&2
    git -C "$repo" remote set-url origin "$expected_url"
}

materialize_managed_machine_config_repo() {
    local seed="$1"
    local target="$2"
    local repo_url="$3"
    local parent tmp seed_status

    config_repo_remote_has_no_credentials "$repo_url" || return 1
    if [[ -e "$target" ]]; then
        assert_managed_machine_config_repo "$target" || return 1
        migrate_config_repo_legacy_remote "$target" "$repo_url" || return 1
        assert_config_repo_remote_is_safe "$target" "$repo_url" || return 1
        return
    fi

    parent="$(dirname "$target")"
    mkdir -p "$parent" || return 1
    chmod 700 "$parent" || return 1
    tmp="$(mktemp -d "$parent/.managed-machine-config.XXXXXX")" || return 1
    # shellcheck disable=SC2064
    trap 'rm -rf "$tmp"; trap - RETURN' RETURN

    if [[ -n "$seed" ]]; then
        assert_bundled_config_seed "$seed" || return 1
        if ! seed_status="$(config_repo_git "$seed" status --porcelain)"; then
            echo "Error: could not inspect bundled managed-machine-config seed: $seed" >&2
            return 1
        fi
        if [[ -n "$seed_status" ]]; then
            echo "Error: bundled managed-machine-config seed is unexpectedly dirty: $seed" >&2
            return 1
        fi
        echo "Creating persistent managed-machine-config checkout from bundled seed..." >&2
        if ! git -c "safe.directory=$seed" clone --quiet --no-hardlinks "$seed" "$tmp/repo"; then
            echo "Error: could not copy bundled managed-machine-config seed" >&2
            return 1
        fi
        git -C "$tmp/repo" remote set-url origin "$repo_url" || return 1
    else
        echo "Cloning managed-machine-config into persistent storage..." >&2
        if ! git clone --quiet "$repo_url" "$tmp/repo"; then
            echo "Error: could not clone managed-machine-config" >&2
            return 1
        fi
    fi

    assert_managed_machine_config_repo "$tmp/repo" || return 1
    assert_config_repo_remote_is_safe "$tmp/repo" "$repo_url" || return 1
    mv "$tmp/repo" "$target" || return 1
    chmod 700 "$target" || return 1
    rmdir "$tmp" || return 1
    trap - RETURN
    echo "Persistent managed-machine-config checkout: $target" >&2
}

# Resolve the private config checkout. Explicit overrides and sibling development
# repos are used in place; a Homebrew-bundled checkout is copied once into
# persistent user storage and is never mutated.
managed_machine_config_repo_dir() {
    local sibling bundled target repo_url

    if [[ -n "${CONFIG_REPO_ROOT:-}" ]]; then
        assert_managed_machine_config_repo "$CONFIG_REPO_ROOT" || return 1
        printf '%s\n' "$CONFIG_REPO_ROOT"
        return
    fi

    sibling="$REPO_ROOT/../managed-machine-config"
    if [[ -e "$sibling/.git" ]]; then
        assert_managed_machine_config_repo "$sibling" || return 1
        printf '%s\n' "$sibling"
        return
    fi

    bundled="$REPO_ROOT/managed-machine-config"
    target="$(managed_machine_config_checkout_dir)"
    repo_url="$(managed_machine_config_repo_url)"
    if [[ -e "$bundled/.git" ]]; then
        materialize_managed_machine_config_repo "$bundled" "$target" "$repo_url" || return 1
    else
        materialize_managed_machine_config_repo "" "$target" "$repo_url" || return 1
    fi
    printf '%s\n' "$target"
}

config_repo_changed_paths() {
    local repo="$1"
    {
        git -C "$repo" diff --name-only
        git -C "$repo" diff --cached --name-only
        git -C "$repo" ls-files --others --exclude-standard
    } | LC_ALL=C sort -u
}

is_managed_fleet_path() {
    [[ "$1" =~ ^fleet/machines/sha256-[A-Za-z0-9_-]+\.toml$ || "$1" == "ssh/authorized_keys" ]]
}

assert_only_managed_fleet_changes() {
    local repo="$1"
    local path
    while IFS= read -r path; do
        [[ -n "$path" ]] || continue
        if ! is_managed_fleet_path "$path"; then
            echo "Error: refusing to synchronize managed-machine-config with unrelated changes: $path" >&2
            echo "  commit, stash, or revert that change and retry" >&2
            return 1
        fi
    done < <(config_repo_changed_paths "$repo")
}

assert_only_managed_fleet_commits() {
    local repo="$1"
    local upstream="$2"
    local path paths

    if ! paths="$(git -C "$repo" log -m --format= --name-only --no-renames "$upstream..HEAD" | LC_ALL=C sort -u)"; then
        echo "Error: could not inspect local managed-machine-config commits" >&2
        return 1
    fi
    while IFS= read -r path; do
        [[ -n "$path" ]] || continue
        if ! is_managed_fleet_path "$path"; then
            echo "Error: refusing to push an unrelated committed config path: $path" >&2
            echo "  publish or move that commit separately, then retry" >&2
            return 1
        fi
    done <<<"$paths"
}

commit_managed_fleet_changes() {
    local repo="$1"
    local message="$2"
    local path

    assert_only_managed_fleet_changes "$repo" || return 1
    while IFS= read -r path; do
        [[ -n "$path" ]] || continue
        if ! git -C "$repo" add -A -- "$path"; then
            echo "Error: could not stage managed fleet path: $path" >&2
            return 1
        fi
    done < <(config_repo_changed_paths "$repo")
    if git -C "$repo" diff --cached --quiet; then
        return 0
    fi
    if ! git -C "$repo" commit --quiet -m "$message"; then
        echo "Error: could not commit managed fleet state in $repo" >&2
        return 1
    fi
    echo "Committed managed-machine-config fleet state." >&2
}

rebase_managed_machine_config_repo() {
    local repo="$1"
    local upstream="$2"
    local resolver="${3:-}"
    local conflicts path

    if git -C "$repo" rebase "$upstream"; then
        return 0
    fi

    while :; do
        conflicts="$(git -C "$repo" diff --name-only --diff-filter=U)"
        if [[ -z "$conflicts" || -z "$resolver" ]] || ! declare -F "$resolver" >/dev/null; then
            git -C "$repo" rebase --abort >/dev/null 2>&1 || true
            echo "Error: managed-machine-config could not rebase cleanly" >&2
            return 1
        fi
        while IFS= read -r path; do
            if [[ "$path" != "ssh/authorized_keys" ]]; then
                git -C "$repo" rebase --abort >/dev/null 2>&1 || true
                echo "Error: refusing to auto-resolve config conflict in $path" >&2
                return 1
            fi
        done <<<"$conflicts"

        if ! "$resolver"; then
            git -C "$repo" rebase --abort >/dev/null 2>&1 || true
            echo "Error: could not regenerate authorized_keys during config rebase" >&2
            return 1
        fi
        if ! git -C "$repo" add -- ssh/authorized_keys; then
            git -C "$repo" rebase --abort >/dev/null 2>&1 || true
            echo "Error: could not stage regenerated authorized_keys" >&2
            return 1
        fi
        if GIT_EDITOR=true git -C "$repo" rebase --continue; then
            return 0
        fi
    done
}

# Synchronize and publish only managed fleet state. A non-fast-forward push is
# retried after rebasing. Only the deterministic generated authorized_keys file
# may be resolved automatically; every other conflict fails closed.
sync_managed_machine_config_repo() {
    local repo="$1"
    local message="$2"
    local resolver="${3:-}"
    local branch upstream attempt ahead remote_url

    assert_managed_machine_config_repo "$repo" || return 1
    remote_url="$(git -C "$repo" remote get-url origin 2>/dev/null || true)"
    if [[ -z "$remote_url" ]]; then
        echo "Error: managed-machine-config has no origin remote: $repo" >&2
        return 1
    fi
    config_repo_remote_has_no_credentials "$remote_url" || return 1
    branch="$(managed_machine_config_branch)"
    if [[ "$(git -C "$repo" symbolic-ref --quiet --short HEAD || true)" != "$branch" ]]; then
        echo "Error: managed-machine-config must be on branch $branch" >&2
        return 1
    fi

    for attempt in 1 2 3; do
        commit_managed_fleet_changes "$repo" "$message" || return 1
        if ! git -C "$repo" fetch --quiet origin; then
            echo "Error: could not fetch managed-machine-config origin" >&2
            return 1
        fi
        upstream="origin/$branch"
        if ! git -C "$repo" rev-parse --verify --quiet "$upstream" >/dev/null; then
            echo "Error: managed-machine-config origin has no $branch branch" >&2
            return 1
        fi
        rebase_managed_machine_config_repo "$repo" "$upstream" "$resolver" || return 1
        assert_only_managed_fleet_commits "$repo" "$upstream" || return 1

        if [[ -n "$resolver" ]]; then
            if ! "$resolver"; then
                echo "Error: could not regenerate managed fleet authorized_keys" >&2
                return 1
            fi
            commit_managed_fleet_changes "$repo" "$message" || return 1
        fi
        assert_only_managed_fleet_commits "$repo" "$upstream" || return 1

        ahead="$(git -C "$repo" rev-list --count "$upstream..HEAD")"
        if [[ "$ahead" == "0" ]]; then
            echo "managed-machine-config already synchronized." >&2
            return 0
        fi
        if git -C "$repo" push --quiet origin "HEAD:refs/heads/$branch"; then
            echo "Pushed managed-machine-config fleet state." >&2
            return 0
        fi
        echo "managed-machine-config changed remotely; retrying safely ($attempt/3)..." >&2
    done

    echo "Error: could not push managed-machine-config after 3 attempts; local commits were preserved" >&2
    return 1
}

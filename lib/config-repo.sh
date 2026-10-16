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
    git -c "safe.directory=$repo" -c "safe.directory=$repo/.git" -C "$repo" "$@"
}

assert_config_repo_git_root() {
    local repo="$1"
    local label="$2"
    local repo_root repo_path
    local -a git_c=(git -C "$repo")

    if [[ "${3:-}" == "--trust-foreign-owner" ]]; then
        git_c=(git -c "safe.directory=$repo" -c "safe.directory=$repo/.git" -C "$repo")
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
        if ! git -c "safe.directory=$seed" -c "safe.directory=$seed/.git" clone --quiet --no-hardlinks "$seed" "$tmp/repo"; then
            echo "Error: could not copy bundled managed-machine-config seed" >&2
            return 1
        fi
        git -C "$tmp/repo" remote set-url origin "$repo_url" || return 1
        # A tag-pinned seed clones out on a detached HEAD; land the checkout
        # on the configured branch at the pinned commit so the pull-only
        # refresh can rebase onto origin — its fetch advances to the tip.
        git -C "$tmp/repo" checkout --quiet -B "$(managed_machine_config_branch)" || return 1
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

# Resolve an already-materialized config checkout without creating directories,
# cloning, or rewriting legacy remotes. Used by read-only inspection and
# dry-run commands so they never mutate the filesystem or git configuration.
existing_managed_machine_config_repo_dir() {
    local sibling target repo_url actual_url actual_path expected_path

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

    target="$(managed_machine_config_checkout_dir)"
    repo_url="$(managed_machine_config_repo_url)"
    config_repo_remote_has_no_credentials "$repo_url" || return 1
    if [[ ! -e "$target" ]]; then
        echo "Error: managed-machine-config checkout does not exist: $target" >&2
        return 1
    fi
    assert_managed_machine_config_repo "$target" || return 1
    actual_url="$(git -C "$target" remote get-url origin 2>/dev/null || true)"
    if [[ -z "$actual_url" ]]; then
        echo "Error: managed-machine-config has no origin remote: $target" >&2
        return 1
    fi
    config_repo_remote_has_no_credentials "$actual_url" || return 1
    if [[ "$actual_url" != "$repo_url" ]]; then
        actual_path="$(config_repo_github_path "$actual_url" 2>/dev/null || true)"
        expected_path="$(config_repo_github_path "$repo_url" 2>/dev/null || true)"
        if [[ -z "$actual_path" || -z "$expected_path" || "$actual_path" != "$expected_path" ]]; then
            echo "Error: managed-machine-config origin does not match the configured repository" >&2
            echo "  expected: $repo_url" >&2
            return 1
        fi
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

# Pull-side refresh of the persistent config checkout: fetch origin and
# rebase the managed branch so later steps read current templates and pins.
# This is deliberately not the publish path: it never commits, stages, or
# pushes. Publishing managed fleet state is reserved for the explicit fleet
# and SSH-enrollment actions (sync_managed_machine_config_repo). A checkout
# with uncommitted changes is left in place with a note rather than mutated;
# a rebase conflict is aborted and reported instead of resolved blindly.
MANAGED_MACHINE_CONFIG_REFRESH_STATUS="${MANAGED_MACHINE_CONFIG_REFRESH_STATUS:-not_attempted}"

refresh_managed_machine_config_repo() {
    local repo="$1"
    local branch remote_url before_sha after_sha

    if ! assert_managed_machine_config_repo "$repo"; then
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="unverified"
        return 1
    fi
    remote_url="$(git -C "$repo" remote get-url origin 2>/dev/null || true)"
    if [[ -z "$remote_url" ]]; then
        echo "Error: managed-machine-config has no origin remote: $repo" >&2
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="fetch_failed"
        return 1
    fi
    if ! config_repo_remote_has_no_credentials "$remote_url"; then
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="fetch_failed"
        return 1
    fi
    branch="$(managed_machine_config_branch)"
    if [[ "$(git -C "$repo" symbolic-ref --quiet --short HEAD || true)" != "$branch" ]]; then
        echo "Error: managed-machine-config must be on branch $branch" >&2
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="fetch_failed"
        return 1
    fi
    before_sha="$(git -C "$repo" rev-parse HEAD 2>/dev/null || true)"
    if ! git -C "$repo" fetch --quiet origin; then
        echo "Error: could not fetch managed-machine-config origin" >&2
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="fetch_failed"
        return 1
    fi
    if ! git -C "$repo" rev-parse --verify --quiet "origin/$branch" >/dev/null; then
        echo "Error: managed-machine-config origin has no $branch branch" >&2
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="fetch_failed"
        return 1
    fi
    if [[ -n "$(config_repo_changed_paths "$repo")" ]]; then
        echo "note: managed-machine-config has local changes; leaving $repo in place — reconcile them before refreshing" >&2
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="stale_dirty"
        return 0
    fi
    if ! git -C "$repo" rebase "origin/$branch"; then
        git -C "$repo" rebase --abort >/dev/null 2>&1 || true
        echo "Error: managed-machine-config could not rebase onto origin/$branch; resolve it manually" >&2
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="rebase_failed"
        return 1
    fi
    after_sha="$(git -C "$repo" rev-parse HEAD 2>/dev/null || true)"
    if [[ -n "$before_sha" && "$before_sha" != "$after_sha" ]]; then
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="refreshed"
    else
        MANAGED_MACHINE_CONFIG_REFRESH_STATUS="current"
    fi
    echo "Refreshed managed-machine-config from origin/$branch." >&2
}

config_repo_provenance_json() {
    local repo="${1:-${CONFIG_REPO_ROOT:-}}"
    local refresh_status="${2:-${MANAGED_MACHINE_CONFIG_REFRESH_STATUS:-not_attempted}}"
    local source="unknown" branch="" commit="" upstream_commit="" dirty="0" sibling="" target=""
    if [[ -z "$repo" ]]; then
        if declare -F existing_managed_machine_config_repo_dir >/dev/null; then
            repo="$(existing_managed_machine_config_repo_dir 2>/dev/null || true)"
        elif declare -F managed_machine_config_repo_dir >/dev/null; then
            repo="$(managed_machine_config_repo_dir 2>/dev/null || true)"
        fi
    fi
    if [[ -n "${REPO_ROOT:-}" ]]; then
        sibling="$REPO_ROOT/../managed-machine-config"
    fi
    target="$(managed_machine_config_checkout_dir 2>/dev/null || true)"
    if [[ -n "$repo" ]]; then
        if [[ -n "$target" && "$repo" == "$target" ]]; then
            source="persistent_checkout"
        elif [[ -n "$sibling" && "$repo" == "$sibling" ]]; then
            source="sibling_checkout"
        elif [[ -n "${CONFIG_REPO_ROOT:-}" && "$repo" == "$CONFIG_REPO_ROOT" ]]; then
            source="env_override"
        fi
        if [[ -d "$repo" && "$(git -C "$repo" rev-parse --is-inside-work-tree 2>/dev/null || true)" == "true" ]]; then
            branch="$(git -C "$repo" symbolic-ref --quiet --short HEAD 2>/dev/null || printf 'detached')"
            commit="$(git -C "$repo" rev-parse HEAD 2>/dev/null || true)"
            if [[ -n "$branch" && "$branch" != "detached" ]]; then
                upstream_commit="$(git -C "$repo" rev-parse --verify --quiet "origin/$branch" 2>/dev/null || true)"
            fi
            if [[ -n "$(config_repo_changed_paths "$repo" 2>/dev/null || true)" ]]; then
                dirty="1"
            fi
        fi
    fi
    python3 - "$repo" "$source" "$branch" "$commit" "$upstream_commit" "$dirty" "$refresh_status" <<'PY'
import json, sys
repo, source, branch, commit, upstream_commit, dirty_flag, refresh_status = sys.argv[1:]
dirty = dirty_flag == "1"
stale = bool(
    dirty
    or refresh_status in ("stale_dirty", "fetch_failed", "rebase_failed", "unverified")
    or (commit and upstream_commit and commit != upstream_commit)
)
print(json.dumps({
    "path": repo or None,
    "source": source,
    "branch": branch or None,
    "commit": commit or None,
    "upstream_commit": upstream_commit or None,
    "dirty": dirty,
    "refresh_status": refresh_status,
    "stale": stale,
}))
PY
}

MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE=""
MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE=""
MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION=""

# Non-mutating local preflight shared by sync_managed_machine_config_repo and
# fleet_remove_dry_run. Checks repo ownership/git-root, origin remote presence
# and credential hygiene, checked-out branch, unrelated working-tree changes,
# and any already-fetched ahead commits outside managed fleet paths.
preflight_managed_machine_config_sync() {
    local repo="$1"
    local branch remote_url current_branch upstream

    MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE=""
    MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE=""
    MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION=""

    if ! assert_managed_machine_config_repo "$repo"; then
        MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE="fleet-config-repo-invalid"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE="Error: managed-machine-config is not a valid user-owned git checkout: $repo"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION="Verify the managed-machine-config checkout at $repo."
        return 1
    fi
    remote_url="$(git -C "$repo" remote get-url origin 2>/dev/null || true)"
    if [[ -z "$remote_url" ]]; then
        MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE="fleet-config-repo-no-origin"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE="Error: managed-machine-config has no origin remote: $repo"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION="Configure the origin remote on $repo and retry."
        echo "$MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE" >&2
        return 1
    fi
    if ! config_repo_remote_has_no_credentials "$remote_url"; then
        MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE="fleet-config-repo-unsafe-origin"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE="Error: refusing managed-machine-config remote with embedded credentials"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION="Remove embedded credentials from the origin remote on $repo and retry."
        return 1
    fi
    branch="$(managed_machine_config_branch)"
    current_branch="$(git -C "$repo" symbolic-ref --quiet --short HEAD || true)"
    if [[ "$current_branch" != "$branch" ]]; then
        MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE="fleet-config-repo-wrong-branch"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE="Error: managed-machine-config must be on branch $branch"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION="Check out branch $branch in $repo and retry."
        echo "$MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE" >&2
        return 1
    fi
    if ! assert_only_managed_fleet_changes "$repo"; then
        MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE="fleet-config-repo-dirty"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE="Error: refusing to synchronize managed-machine-config with unrelated changes"
        MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION="Commit, stash, or revert unrelated changes in $repo and retry."
        return 1
    fi
    upstream="origin/$branch"
    if git -C "$repo" rev-parse --verify --quiet "$upstream" >/dev/null; then
        if ! assert_only_managed_fleet_commits "$repo" "$upstream"; then
            MANAGED_MACHINE_CONFIG_PREFLIGHT_CODE="fleet-config-repo-unrelated-commits"
            MANAGED_MACHINE_CONFIG_PREFLIGHT_MESSAGE="Error: refusing to push an unrelated committed config path"
            MANAGED_MACHINE_CONFIG_PREFLIGHT_ACTION="Publish or move unrelated commits in $repo separately, then retry."
            return 1
        fi
    fi
}

# Synchronize and publish only managed fleet state. A non-fast-forward push is
# retried after rebasing. Only the deterministic generated authorized_keys file
# may be resolved automatically; every other conflict fails closed.
sync_managed_machine_config_repo() {
    local repo="$1"
    local message="$2"
    local resolver="${3:-}"
    local branch upstream attempt ahead

    preflight_managed_machine_config_sync "$repo" || return 1
    branch="$(managed_machine_config_branch)"

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

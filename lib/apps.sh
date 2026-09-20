#!/usr/bin/env bash
# Install engines driven by the config-repo catalog. New kinds require a
# managed-machine upgrade; new apps are catalog rows only.

# shellcheck source=cask-app.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cask-app.sh"
# shellcheck source=vendor-dmg.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/vendor-dmg.sh"
# shellcheck source=devin.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/devin.sh"

install_opencode_cli() {
    local opencode_bin opencode_link
    opencode_bin="${HOME}/.opencode/bin/opencode"
    opencode_link="${HOME}/.local/bin/opencode"

    ensure_local_bin_in_zshrc "${HOME}/.zshrc"
    export_local_bin_to_path

    link_opencode() {
        [[ -x "$opencode_bin" ]] || return 1
        mkdir -p "${HOME}/.local/bin"
        if [[ -e "$opencode_link" || -L "$opencode_link" ]]; then
            if [[ -L "$opencode_link" && "$(readlink "$opencode_link")" == "$opencode_bin" ]]; then
                export_local_bin_to_path
                return 0
            fi
            echo "Error: $opencode_link exists and is not the managed OpenCode symlink" >&2
            return 1
        fi
        ln -s "$opencode_bin" "$opencode_link"
        export_local_bin_to_path
    }

    if managed_cli_available opencode; then
        echo "OpenCode already installed: $(command -v opencode)"
        opencode --version
        return 0
    fi
    if link_opencode && managed_cli_available opencode; then
        echo "OpenCode already installed: $(command -v opencode)"
        opencode --version
        return 0
    fi
    if ! command -v curl >/dev/null 2>&1; then
        echo "Error: curl is required to install OpenCode" >&2
        return 1
    fi
    echo "Installing OpenCode..."
    curl -fsSL https://opencode.ai/install | bash -s -- --no-modify-path
    link_opencode || true
    if ! managed_cli_available opencode; then
        echo "Install finished but opencode not found on PATH." >&2
        echo "Open a new shell or: export PATH=\"${HOME}/.local/bin:\$PATH\"" >&2
        return 1
    fi
    echo "OpenCode installed: $(command -v opencode)"
    opencode --version
}

install_devin_app() {
    ensure_local_bin_in_zshrc "${HOME}/.zshrc"
    export_local_bin_to_path
    if managed_cli_available devin; then
        echo "Devin CLI already installed: $(command -v devin)"
    else
        if ! command -v curl >/dev/null 2>&1; then
            echo "Error: curl is required to install Devin CLI" >&2
            return 1
        fi
        echo "Installing Devin CLI without launching interactive setup..."
        install_devin_cli
    fi
    if ! managed_cli_available devin; then
        echo "Install finished but devin not found on PATH." >&2
        echo "Open a new shell or: export PATH=\"${HOME}/.local/bin:\$PATH\"" >&2
        return 1
    fi
    echo "Devin CLI installed: $(command -v devin)"
    devin --version 2>/dev/null || true
    ensure_devin_authentication
}

install_official_cli_from_catalog() {
    local name="$1"
    local display command url bin_dir env_json key value
    display="$(catalog_app_field "$name" display 2>/dev/null || catalog_app_field "$name" name)"
    command="$(catalog_app_field "$name" command)" || return 1
    url="$(catalog_app_field "$name" url)" || return 1
    bin_dir="$(catalog_app_field "$name" bin_dir 2>/dev/null || true)"
    env_json="$(catalog_app_field "$name" env 2>/dev/null || true)"
    if [[ -n "$env_json" && "$env_json" != "{}" ]]; then
        while IFS=$'\t' read -r key value; do
            [[ -n "$key" ]] || continue
            if [[ "${MANAGED_MACHINE_ACCOUNT_SETUP:-}" == 1 ]]; then
                case "$key" in
                    HOME|USER|LOGNAME|SHELL|PATH|BASH_ENV|ENV|ZDOTDIR|XDG_*|CLAUDE_CONFIG_DIR|GH_*|GITHUB_*|SSH_*|GIT_*|AGENT_BOT_*|MANAGED_MACHINE_*|HOMEBREW_*|LD_*|DYLD_*)
                        echo 'Error: catalog environment cannot override account identity, credentials, or execution boundaries.' >&2
                        return 1 ;;
                esac
            fi
            export "$key=$value"
        done < <(MANAGED_MACHINE_ENV_JSON="$env_json" python3 -c '
import json, os
for key, value in json.loads(os.environ["MANAGED_MACHINE_ENV_JSON"]).items():
    print("%s\t%s" % (key, value))
')
    fi
    local args_out
    local -a extra_args=()
    if ! args_out="$(catalog_app_args "$name")"; then
        return 1
    fi
    while IFS= read -r arg; do
        [[ -n "$arg" ]] || continue
        extra_args+=("$arg")
    done <<< "$args_out"

    if ((${#extra_args[@]})); then
        install_official_cli "$display" "$command" "$url" "$bin_dir" "${extra_args[@]}"
    else
        install_official_cli "$display" "$command" "$url" "$bin_dir"
    fi
}

install_cask_from_catalog() {
    local name="$1"
    local token override
    token="$(catalog_app_field "$name" token)" || return 1
    override="$(cask_appdir_override_for_token "$token")"
    install_signed_cask_app "$token" "$override"
}

brew_formula_qualified() {
    printf 'homebrew/core/%s\n' "$1"
}

# Verify brew will install the official homebrew/core formula: exact name and tap.
verify_brew_formula_source() {
    local formula="$1"
    local json
    if [[ ! "$formula" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
        echo "Error: invalid Homebrew formula name: $formula" >&2
        return 1
    fi
    json="$(brew info --json=v2 --formula "$(brew_formula_qualified "$formula")")" || {
        echo "Error: could not read Homebrew formula metadata for $formula" >&2
        return 1
    }
    EXPECT_FORMULA="$formula" python3 -c '
import json, os, sys
data = json.load(sys.stdin)
formulae = data.get("formulae") or []
if len(formulae) != 1:
    sys.stderr.write("Error: expected exactly one formula record\n")
    sys.exit(1)
formula = formulae[0]
expect = os.environ["EXPECT_FORMULA"]
if formula.get("name") != expect:
    sys.stderr.write("Error: formula name %r does not match %r\n" % (formula.get("name"), expect))
    sys.exit(1)
if formula.get("tap") != "homebrew/core":
    sys.stderr.write("Error: refusing formula %s from tap %r; only homebrew/core is allowed\n" % (expect, formula.get("tap")))
    sys.exit(1)
' <<<"$json"
}

brew_formula_has_receipt() {
    local formula="$1"
    local out
    out="$(brew list --versions "$formula" 2>/dev/null)" || return 1
    [[ "$out" == "$formula "* || "$out" == "$formula" ]]
}

install_brew_formula_from_catalog() {
    local name="$1"
    local formula qualified
    formula="$(catalog_app_field "$name" formula 2>/dev/null || true)"
    if [[ -z "$formula" ]]; then
        echo "Error: $name is missing formula; refusing unverified brew-formula" >&2
        return 1
    fi
    if ! ensure_brew_on_path; then
        echo "Error: brew required — run setup-brew first" >&2
        return 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        echo "Error: python3 is required to verify Homebrew formula metadata" >&2
        return 1
    fi

    verify_brew_formula_source "$formula" || return 1
    if brew_formula_has_receipt "$formula"; then
        echo "$formula already installed"
        brew list --versions "$formula" || true
        return 0
    fi

    qualified="$(brew_formula_qualified "$formula")"
    echo "Installing $formula from $qualified..."
    brew_run install "$qualified"
    if ! brew_formula_has_receipt "$formula"; then
        echo "Install finished but $formula was not found." >&2
        return 1
    fi
    echo "$formula installed"
    brew list --versions "$formula" || true
}

npm_package_qualified() {
    printf '%s\n' "$1"
}

# Resolve the directory npm installs global binaries into, so setup paths that
# run without a login shell can still find tools installed with --global. The
# same isolation as npm_public keeps the prefix consistent with installs.
npm_global_bin_dir() {
    local prefix
    prefix="$(npm_public prefix -g 2>/dev/null)" || return 1
    printf '%s/bin\n' "$prefix"
}

npm_registry_url() {
    printf '%s\n' 'https://registry.npmjs.org/'
}

# Run an npm subcommand with user, global, and project npm configuration
# isolated. A machine can point `registry` or a `@scope:registry` entry at a
# private server through user, global, or project .npmrc (or NPM_CONFIG_*
# environment), so a same-named private package could otherwise pass the
# metadata check and have its lifecycle scripts executed globally. The lookup,
# install, and receipt queries all run with empty user and global config files
# from a throwaway working directory so no configured registry and no project
# .npmrc is in effect; callers still pass --registry to pin the public registry.
npm_public() {
    local isolated
    isolated="$(mktemp -d)"
    : >"$isolated/userconfig"
    : >"$isolated/globalconfig"
    (
        cd "$isolated"
        NPM_CONFIG_USERCONFIG="$isolated/userconfig" \
        NPM_CONFIG_GLOBALCONFIG="$isolated/globalconfig" \
        npm "$@"
    )
    local rc=$?
    rm -rf "$isolated"
    return "$rc"
}

# Owner of the npm global prefix. A Homebrew-managed node keeps its prefix
# under the admin-owned /opt/homebrew, so global installs fail with EACCES for
# a non-admin user; npm_run escalates to this owner. A per-user node (nvm and
# friends) keeps the prefix under the invoking user's home, so no elevation is
# needed. Returns the owner name, or empty when it cannot be resolved.
npm_prefix_owner() {
    local prefix owner
    prefix="$(npm_public prefix -g 2>/dev/null)" || return 1
    if stat -c '%U' "$prefix" >/dev/null 2>&1; then
        owner="$(stat -c '%U' "$prefix" 2>/dev/null || true)"
    else
        owner="$(stat -f '%Su' "$prefix" 2>/dev/null || true)"
    fi
    owner="${owner//[()]/}"
    printf '%s\n' "$owner"
}

# True when this npm binary is the system (Homebrew-linked) npm, not a test
# stub or a per-user node. Only a system npm is trusted to run as the prefix
# owner: elevating an arbitrary PATH-resolved npm would hand the prefix
# owner's rights to whatever binary shadowed it.
npm_is_system_prefix() {
    local npm_bin="${1:-}"
    case "$npm_bin" in
        /opt/homebrew/bin/npm|/usr/local/bin/npm) return 0 ;;
        *) return 1 ;;
    esac
}

# Run an npm subcommand as the npm global-prefix owner when the current user
# does not own it. Mirrors brew_run: a prefix this user owns (or an
# unresolvable owner) runs in-process through npm_public; a foreign owner
# (Homebrew-node under /opt/homebrew, owned by admin) and only a system npm
# binary runs through the npm-public-run helper with one administrator dialog.
# The helper preserves npm_public's isolation (empty user/global config,
# throwaway workdir), so a machine-scoped or project .npmrc can never redirect
# the elevated install.
npm_run() {
    local owner helper npm_bin
    if ! npm_bin="$(command -v npm 2>/dev/null)"; then
        echo "Error: npm required — run setup-nvm first" >&2
        return 1
    fi
    if ! npm_is_system_prefix "$npm_bin"; then
        npm_public "$@"
        return
    fi
    owner="$(npm_prefix_owner 2>/dev/null || true)"
    # Resolve numeric UID to name, exactly as brew_run does; keep the numeric
    # owner when DirectoryService is sandboxed so the helper can use sudo -u
    # "#uid" rather than silently falling back to an in-process EACCES.
    if [[ "$owner" =~ ^[0-9]+$ ]]; then
        owner="$(resolve_brew_owner_name "$owner")"
    fi
    if [[ -z "$owner" || "$owner" == "$(id -un)" ]]; then
        npm_public "$@"
        return
    fi
    helper="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/npm-public-run"
    if [[ ! -f "$helper" ]]; then
        echo "Error: missing $helper" >&2
        return 1
    fi
    elevate_run "run npm $* as $owner" /bin/sh "$helper" "$owner" "$npm_bin" "$@"
}

# Verify the catalog package is a public npm-registry package. Scoped packages
# and names with a registered scope are allowed; anything that would make npm
# read a registry URL, a tarball, or a git remote is refused. Lookup and install
# both go through npm_public so machine-scoped user/global/project npm registry
# configuration can never be used as a supply channel.
verify_npm_package_source() {
    local package="$1"
    local json
    if [[ ! "$package" =~ ^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$ ]]; then
        echo "Error: invalid npm package name: $package" >&2
        return 1
    fi
    json="$(npm_public view --registry="$(npm_registry_url)" "$package" name version --json 2>/dev/null)" || {
        echo "Error: could not read public npm registry metadata for $package" >&2
        return 1
    }
    EXPECT_PACKAGE="$package" python3 -c '
import json, os, sys
data = json.load(sys.stdin)
expect = os.environ["EXPECT_PACKAGE"]
if not isinstance(data, dict) or data.get("name") != expect:
    sys.stderr.write("Error: refusing npm package %s from a non-public registry\n" % expect)
    sys.exit(1)
' <<<"$json"
}

npm_package_has_receipt() {
    local package="$1"
    npm_public ls --global --parseable --depth=0 "$package" 2>/dev/null | grep -qF "node_modules/$package"
}

install_npm_package_from_catalog() {
    local name="$1"
    local package command bin_dir install_status
    package="$(catalog_app_field "$name" package 2>/dev/null || true)"
    if [[ -z "$package" ]]; then
        echo "Error: $name is missing package; refusing unverified npm install" >&2
        return 1
    fi
    if ! command -v npm >/dev/null 2>&1; then
        echo "Error: npm required — run setup-nvm first" >&2
        return 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        echo "Error: python3 is required to verify npm package metadata" >&2
        return 1
    fi

    verify_npm_package_source "$package" || return 1

    command="$(catalog_app_field "$name" command 2>/dev/null || printf '%s\n' "$package")"
    export_local_bin_to_path
    if bin_dir="$(npm_global_bin_dir)"; then
        case ":${PATH}:" in
            *":$bin_dir:"*) ;;
            *) export PATH="${bin_dir}:${PATH}" ;;
        esac
    fi
    if command -v "$command" >/dev/null 2>&1; then
        echo "$command already installed: $(command -v "$command")"
        "$command" --version 2>/dev/null || true
        return 0
    fi
    if npm_package_has_receipt "$package"; then
        echo "$package already installed (npm global)"
        npm_public ls --global --parseable --depth=0 "$package" || true
        return 0
    fi

    echo "Installing $package from the public npm registry..."
    npm_run install --global --no-fund --no-audit --registry="$(npm_registry_url)" "$package"
    install_status=$?
    # A deferred elevated install (no dialog in noninteractive bootstrap) must
    # stay a skipped outcome, like the other elevated sites, not a failure.
    if [[ "$install_status" -eq "${MANAGED_MACHINE_SKIPPED_EXIT:-76}" ]]; then
        return "$install_status"
    fi
    [[ "$install_status" -eq 0 ]] || return 1
    if ! npm_package_has_receipt "$package"; then
        echo "Install finished but $package was not found." >&2
        return 1
    fi
    if command -v "$command" >/dev/null 2>&1; then
        echo "$package installed: $(command -v "$command")"
        "$command" --version 2>/dev/null || true
    else
        echo "$package installed (npm global)"
        npm_public ls --global --parseable --depth=0 "$package" || true
    fi
}

# Install one catalog app, then run config/<name> when that script exists.
install_catalog_app() {
    local requested="$1"
    local name kind
    if ! name="$(catalog_resolve_name "$requested")"; then
        echo "Error: unknown catalog app: $requested" >&2
        return 1
    fi
    kind="$(catalog_app_kind "$name")" || return 1
    case "$kind" in
        signed-cask|cask)
            install_cask_from_catalog "$name" || return $?
            ;;
        vendor-dmg)
            install_vendor_dmg_from_catalog "$name" || return $?
            ;;
        brew-formula)
            install_brew_formula_from_catalog "$name" || return $?
            ;;
        official-cli)
            install_official_cli_from_catalog "$name" || return $?
            ;;
        npm)
            install_npm_package_from_catalog "$name" || return $?
            ;;
        opencode)
            install_opencode_cli || return $?
            ;;
        devin)
            install_devin_app || return $?
            ;;
        *)
            echo "Error: unknown app kind '$kind' for $name — upgrade managed-machine to install this app" >&2
            return 1
            ;;
    esac
    if [[ "${MANAGED_MACHINE_ACCOUNT_SETUP:-}" != 1 ]]; then
        apply_config_script "$name"
    fi
}

print_catalog_app_names() {
    local name
    echo
    echo "Available catalog apps (bare and setup- prefixed forms are accepted):"
    while IFS= read -r name; do
        [[ -n "$name" ]] || continue
        if catalog_app_is_auto "$name"; then
            printf '  %s\n' "$name"
        else
            printf '  %s (setup only)\n' "$name"
        fi
    done < <(catalog_app_names 2>/dev/null || true)
}

# True when "name <version>" appears as a line in a `brew list --versions`
# style dump. Lets a listing pass match one snapshot instead of spawning brew
# per row.
receipt_list_has() {
    [[ -n "$1" && $'\n'"$1"$'\n' == *$'\n'"$2 "* ]]
}

# Print the pipe-delimited catalog summary row for a name or alias
# (kind|token|app_name|formula|package|command|auto|team_id|allow_rolling).
# When MM_CATALOG_ROWS holds the `catalog_query rows` snapshot (see
# setup_list_prime) the match is pure bash; otherwise one python lookup.
catalog_row_for() {
    local name="$1" key rest
    if [[ "${MM_CATALOG_ROWS+x}" == x ]]; then
        while IFS='|' read -r key rest; do
            if [[ "$key" == "$name" ]]; then
                printf '%s\n' "$rest"
                return 0
            fi
        done <<<"$MM_CATALOG_ROWS"
        return 1
    fi
    catalog_query row "$name" 2>/dev/null
}

# Read-only "is this catalog app already installed?" for the setup list.
# Mirrors what each install engine treats as done — a Homebrew receipt plus
# the bundle on disk for signed casks (a receiptless occupier is adopt
# territory, not an install), a Team ID-verified bundle for vendor DMGs, a
# formula receipt, or the CLI command on PATH — without installing or
# elevating anything. MM_BREW_FORMULA_RECEIPTS and MM_BREW_CASK_RECEIPTS may
# each hold one `brew list --versions` snapshot (see setup_list_prime) so a
# full list does not spawn brew per row; unset, each row queries brew itself.
catalog_app_installed() {
    local name="$1"
    local row kind token app_name formula package command auto team_id allow_rolling
    local installed_app
    row="$(catalog_row_for "$name")" || return 1
    IFS='|' read -r kind token app_name formula package command auto team_id allow_rolling <<<"$row"
    case "$kind" in
        signed-cask|cask)
            [[ -n "$token" && -n "$app_name" ]] || return 1
            # install_signed_cask_app reports "already installed" only for a
            # bundle backed by a Homebrew receipt; an occupier without one is
            # skipped for `adopt`, so it never earns the mark.
            if [[ "${MM_BREW_CASK_RECEIPTS+x}" == x ]]; then
                receipt_list_has "$MM_BREW_CASK_RECEIPTS" "$token" || return 1
            else
                cask_has_receipt "$token" 2>/dev/null || return 1
            fi
            find_cask_app "$app_name" "$(cask_appdir_override_for_token "$token")" >/dev/null 2>&1
            ;;
        vendor-dmg)
            # The engine converges an existing bundle in place only after its
            # Team ID verifies; an unverified occupier is reported, never
            # treated as installed — so the mark requires the same check.
            [[ -n "$app_name" && -n "$team_id" ]] || return 1
            installed_app="$(vendor_dmg_find_app "$app_name" 2>/dev/null)" || return 1
            verify_app_signature "$installed_app" "$team_id" "$allow_rolling" >/dev/null 2>&1
            ;;
        brew-formula)
            [[ -n "$formula" ]] || return 1
            if [[ "${MM_BREW_FORMULA_RECEIPTS+x}" == x ]]; then
                receipt_list_has "$MM_BREW_FORMULA_RECEIPTS" "$formula"
            else
                brew_formula_has_receipt "$formula" 2>/dev/null
            fi
            ;;
        npm)
            [[ -n "$command" ]] || command="${package:-$name}"
            if [[ -n "$command" ]] && command -v "$command" >/dev/null 2>&1; then
                return 0
            fi
            [[ -n "$package" ]] || return 1
            if [[ "${MM_NPM_RECEIPTS+x}" == x ]]; then
                grep -qF "node_modules/$package" <<<"$MM_NPM_RECEIPTS"
            else
                command -v npm >/dev/null 2>&1 \
                    && npm_package_has_receipt "$package" 2>/dev/null
            fi
            ;;
        official-cli|opencode|devin)
            [[ -n "$command" ]] || command="$name"
            command -v "$command" >/dev/null 2>&1
            ;;
        *)
            return 1
            ;;
    esac
}

# Prepare read-only install detection for a listing pass: put the managed bin
# dirs on PATH once and snapshot the catalog and brew receipts so per-row
# checks spawn neither brew nor a python lookup per name.
setup_list_prime() {
    declare -F export_local_bin_to_path >/dev/null && export_local_bin_to_path
    declare -F export_cargo_bin_to_path >/dev/null && export_cargo_bin_to_path
    local npm_bin
    if declare -F npm_global_bin_dir >/dev/null \
        && npm_bin="$(npm_global_bin_dir 2>/dev/null)"; then
        case ":${PATH}:" in
            *":${npm_bin}:"*) ;;
            *) export PATH="${npm_bin}:${PATH}" ;;
        esac
    fi
    MM_BREW_FORMULA_RECEIPTS=""
    MM_BREW_CASK_RECEIPTS=""
    if declare -F ensure_brew_on_path >/dev/null && ensure_brew_on_path >/dev/null 2>&1; then
        MM_BREW_FORMULA_RECEIPTS="$(brew list --versions 2>/dev/null || true)"
        MM_BREW_CASK_RECEIPTS="$(brew list --cask --versions 2>/dev/null || true)"
    fi
    MM_CATALOG_ROWS=""
    if declare -F catalog_query >/dev/null; then
        MM_CATALOG_ROWS="$(catalog_query rows 2>/dev/null || true)"
    fi
    MM_NPM_RECEIPTS=""
    if declare -F npm_public >/dev/null && command -v npm >/dev/null 2>&1; then
        MM_NPM_RECEIPTS="$(npm_public ls --global --parseable --depth=0 2>/dev/null || true)"
    fi
}

# Read-only "already installed?" probe for one listed setup name. A name that
# resolves to a catalog row — directly, or through the install_catalog_app
# call of its setup-<name> wrapper (setup-codex-cli -> codex) — delegates to
# that app's install engine; the remaining infrastructure scripts use the
# same receipts their installers check. Unknown state returns nonzero like
# not-installed: the list only marks confirmed installs.
setup_name_installed() {
    local root="$1" name="$2"
    local app
    if declare -F catalog_row_for >/dev/null; then
        if catalog_row_for "$name" >/dev/null 2>&1; then
            catalog_app_installed "$name"
            return
        fi
        if [[ -f "$root/setup-$name" ]]; then
            app="$(sed -n 's/^install_catalog_app \([a-zA-Z0-9_-]*\).*/\1/p' "$root/setup-$name" | head -1)"
            if [[ -n "$app" ]] && catalog_row_for "$app" >/dev/null 2>&1; then
                catalog_app_installed "$app"
                return
            fi
        fi
    fi
    case "$name" in
        brew)
            declare -F ensure_brew_on_path >/dev/null \
                && ensure_brew_on_path >/dev/null 2>&1 ;;
        gh)
            declare -F ensure_brew_on_path >/dev/null \
                && ensure_brew_on_path >/dev/null 2>&1 \
                && command -v gh >/dev/null 2>&1 ;;
        nvm)
            declare -F nvm_dir >/dev/null && [[ -s "$(nvm_dir)/nvm.sh" ]] ;;
        rust)
            declare -F export_cargo_bin_to_path >/dev/null && export_cargo_bin_to_path
            command -v rustup >/dev/null 2>&1 ;;
        bin)
            declare -F managed_machine_config_dir >/dev/null \
                && [[ -f "$(managed_machine_config_dir)/local-bin.manifest" ]] ;;
        zsh-functions)
            declare -F managed_machine_config_dir >/dev/null \
                && [[ -f "$(managed_machine_config_dir)/zsh-functions.manifest" ]] ;;
        hostname)
            [[ -f "$root/lib/hostname.sh" ]] || return 1
            # shellcheck source=/dev/null
            source "$root/lib/hostname.sh"
            [[ -n "$(hostname_manifest_name 2>/dev/null || true)" ]] ;;
        git-hooks)
            # Mirror setup-git-hooks: managed wiring means core.hooksPath is
            # the repo's git-hooks/ or the generated dispatcher, with the
            # managed pre-commit present and gitleaks installed. Any other
            # local hooksPath is a custom path the setup composes with — the
            # step is not complete.
            local hooks_path hooks_dir git_dir
            hooks_path="$(git -C "$root" config --local --get core.hooksPath 2>/dev/null || true)"
            [[ -n "$hooks_path" ]] || return 1
            case "$hooks_path" in
                git-hooks|"$root"/git-hooks)
                    hooks_dir="$root/git-hooks" ;;
                *)
                    git_dir="$(git -C "$root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)"
                    [[ -n "$git_dir" && "$hooks_path" == "$git_dir/managed-machine-hooks" ]] || return 1
                    hooks_dir="$hooks_path" ;;
            esac
            [[ -x "$hooks_dir/pre-commit" ]] || return 1
            declare -F ensure_brew_on_path >/dev/null && ensure_brew_on_path >/dev/null 2>&1
            command -v gitleaks >/dev/null 2>&1 ;;
        zsh)
            [[ -n "${LOCAL_BIN_PATH_BEGIN:-}" && -f "${HOME}/.zshrc" ]] \
                && grep -qF "$LOCAL_BIN_PATH_BEGIN" "${HOME}/.zshrc" ;;
        agent-bot)
            # Mirror setup-agent-bot's own outcome classes: the reviewed
            # runtime must be installed (a leftover binary or dev checkout
            # link is a conflict it parks, not a finished install), then the
            # machine wiring must verify — or fail only on a specific App's
            # credentials, which are bound lazily when that agent actually
            # runs, or on the provider-deferred codes wire_agent_bot_machine
            # defers instead of failing.
            [[ -f "$root/lib/agent-bot-gh.sh" && -f "$root/lib/agent-bot.sh" ]] || return 1
            # shellcheck source=/dev/null
            source "$root/lib/agent-bot-gh.sh"
            # shellcheck source=/dev/null
            source "$root/lib/agent-bot.sh"
            declare -F ensure_brew_on_path >/dev/null && ensure_brew_on_path >/dev/null 2>&1
            if [[ "${MM_BREW_FORMULA_RECEIPTS+x}" == x ]]; then
                receipt_list_has "$MM_BREW_FORMULA_RECEIPTS" "$AGENT_BOT_FORMULA" || return 1
            else
                agent_bot_formula_installed || return 1
            fi
            local cli out parsed slug code
            cli="$(agent_bot_cli_path 2>/dev/null)" || return 1
            if out="$(agent_bot_doctor_machine_gate "$cli" 2>/dev/null)"; then
                return 0
            fi
            parsed="$(MANAGED_MACHINE_DOCTOR_OUT="$out" python3 -c '
import json, os, sys
try:
    data = json.loads(os.environ["MANAGED_MACHINE_DOCTOR_OUT"])
except Exception:
    sys.exit(1)
f = data.get("first_actionable_failure") or {}
print("%s\t%s" % (f.get("app_slug") or "", f.get("code") or ""))
' 2>/dev/null || true)"
            slug="${parsed%%$'\t'*}"
            code="${parsed##*$'\t'}"
            [[ -n "$slug" ]] && return 0
            [[ -n "$code" ]] && grep -qE "^${AGENT_BOT_PROVIDER_CODES}\$" <<<"$code" ;;
        agent-bot-gh)
            [[ -f "$root/lib/agent-bot-gh.sh" ]] || return 1
            # shellcheck source=/dev/null
            source "$root/lib/agent-bot-gh.sh"
            agent_bot_gh_is_configured ;;
        *)
            return 1 ;;
    esac
}

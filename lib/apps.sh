#!/usr/bin/env bash
# Install engines driven by the config-repo catalog. New kinds require a
# managed-machine upgrade; new apps are catalog rows only.

# shellcheck source=cask-app.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cask-app.sh"
# shellcheck source=vendor-dmg.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/vendor-dmg.sh"
# shellcheck source=devin.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/devin.sh"

# OpenCode v2 (https://opencode.ai/download). The official v2 installer puts
# the binary at ~/.opencode/bin/opencode, the path v1 used, so a managed v1
# is replaced in place and ~/.local/bin/opencode stays the one link on PATH.
# Pinned to 2.0.18, published to npm as @opencode/cli on 2026-09-25 and
# fetched by the installer from @opencode/cli-darwin-<arch>@2.0.18.
OPENCODE_VERSION="2.0.18"
OPENCODE_INSTALL_URL="https://opencode.ai/v2/install"

# Major version an opencode executable reports ("2.0.18", "opencode v1.4.2");
# fails when the output carries no version.
opencode_major_version() {
    local output version
    output="$("$1" --version 2>/dev/null | head -n 1)" || return 1
    version="${output##* }"
    version="${version#v}"
    [[ "$version" =~ ^([0-9]+)\.[0-9]+ ]] || return 1
    printf '%s\n' "${BASH_REMATCH[1]}"
}

install_opencode_cli() {
    local opencode_bin opencode_link current major
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

    # True when the opencode on PATH is the managed binary, directly or
    # through the managed link.
    opencode_is_managed() {
        local resolved
        resolved="$(command -v opencode)" || return 1
        [[ "$resolved" == "$opencode_bin" ]] && return 0
        [[ "$resolved" == "$opencode_link" && -L "$opencode_link" \
            && "$(readlink "$opencode_link")" == "$opencode_bin" ]]
    }

    if ! managed_cli_available opencode; then
        link_opencode || true
    fi
    if managed_cli_available opencode; then
        current="$(command -v opencode)"
        major="$(opencode_major_version "$current" || true)"
        if [[ -n "$major" && "$major" -ge 2 ]]; then
            echo "OpenCode already installed: $current"
            opencode --version
            return 0
        fi
        if ! opencode_is_managed; then
            # Never replace or shadow a copy managed-machine did not install:
            # a second opencode on PATH would leave which one runs to PATH
            # order.
            skip_setup "OpenCode at $current is ${major:+v}${major:-of an unrecognized version} and not managed by managed-machine; remove it (for example brew uninstall opencode or npm uninstall -g opencode-ai), then re-run managed-machine setup opencode to install v2" || return $?
        fi
        echo "Upgrading managed OpenCode ${major:+v$major }at $opencode_bin to v2 ($OPENCODE_VERSION)..."
    else
        echo "Installing OpenCode $OPENCODE_VERSION..."
    fi
    if ! command -v curl >/dev/null 2>&1; then
        echo "Error: curl is required to install OpenCode" >&2
        return 1
    fi
    if ! curl -fsSL "$OPENCODE_INSTALL_URL" | bash -s -- --version "$OPENCODE_VERSION" --no-modify-path; then
        echo "Error: the OpenCode $OPENCODE_VERSION installer failed" >&2
        return 1
    fi
    link_opencode || true
    if ! managed_cli_available opencode; then
        echo "Install finished but opencode not found on PATH." >&2
        echo "Open a new shell or: export PATH=\"${HOME}/.local/bin:\$PATH\"" >&2
        return 1
    fi
    major="$(opencode_major_version "$(command -v opencode)" || true)"
    if [[ -z "$major" || "$major" -lt 2 ]]; then
        echo "Error: OpenCode install finished but $(command -v opencode) does not report v2" >&2
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

    if declare -F elevate_plan_step_completed >/dev/null \
        && [[ -n "${MANAGED_MACHINE_CURRENT_STEP:-}" ]] \
        && elevate_plan_step_completed "$MANAGED_MACHINE_CURRENT_STEP" brew-formula; then
        elevate_plan_step_status "$MANAGED_MACHINE_CURRENT_STEP" brew-formula >/dev/null || true
        verify_brew_formula_source "$formula" || return 1
        if ! brew_formula_has_receipt "$formula"; then
            echo "Install finished but $formula was not found." >&2
            return 1
        fi
        echo "$formula installed"
        brew list --versions "$formula" || true
        return 0
    fi

    verify_brew_formula_source "$formula" || return 1
    if brew_formula_has_receipt "$formula"; then
        echo "$formula already installed"
        brew list --versions "$formula" || true
        return 0
    fi

    qualified="$(brew_formula_qualified "$formula")"
    echo "Installing $formula from $qualified..."
    local install_status=0
    brew_run install "$qualified" || install_status=$?
    if [[ "$install_status" -eq "${MANAGED_MACHINE_SKIPPED_EXIT:-76}" \
        || "$install_status" -eq "${MANAGED_MACHINE_DEFERRED_EXIT:-75}" ]]; then
        return "$install_status"
    fi
    [[ "$install_status" -eq 0 ]] || return 1
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
    if [[ -n "${MANAGED_MACHINE_NPM_PREFIX_OWNER:-}" ]]; then
        printf '%s\n' "$MANAGED_MACHINE_NPM_PREFIX_OWNER"
        return 0
    fi
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
    [[ "${MANAGED_MACHINE_ASSUME_SYSTEM_NPM:-}" == "1" ]] && return 0
    case "$npm_bin" in
        /opt/homebrew/bin/npm|/usr/local/bin/npm) return 0 ;;
        *) return 1 ;;
    esac
}

resolved_npm_owner() {
    local owner=""
    owner="$(npm_prefix_owner 2>/dev/null || true)"
    if [[ "$owner" =~ ^[0-9]+$ ]]; then
        owner="$(resolve_brew_owner_name "$owner")"
    fi
    printf '%s\n' "$owner"
}

npm_needs_elevation() {
    local npm_bin owner
    if ! npm_bin="$(command -v npm 2>/dev/null)"; then
        return 1
    fi
    if ! npm_is_system_prefix "$npm_bin"; then
        return 1
    fi
    owner="$(resolved_npm_owner)"
    [[ -n "$owner" && "$owner" != "$(id -un)" ]]
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
# verify_npm_package_source <package> [version]: the public registry must
# know <package> by that exact name, and at <version> when the row pins one.
verify_npm_package_source() {
    local package="$1" version="${2:-}"
    local json spec="$1"
    if [[ ! "$package" =~ ^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$ ]]; then
        echo "Error: invalid npm package name: $package" >&2
        return 1
    fi
    if [[ -n "$version" ]]; then
        if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            echo "Error: invalid npm version pin for $package: $version (expected X.Y.Z)" >&2
            return 1
        fi
        spec="$package@$version"
    fi
    json="$(npm_public view --registry="$(npm_registry_url)" "$spec" name version --json 2>/dev/null)" || {
        echo "Error: could not read public npm registry metadata for $spec" >&2
        return 1
    }
    EXPECT_PACKAGE="$package" EXPECT_VERSION="$version" python3 -c '
import json, os, sys
data = json.load(sys.stdin)
expect = os.environ["EXPECT_PACKAGE"]
version = os.environ["EXPECT_VERSION"]
if not isinstance(data, dict) or data.get("name") != expect:
    sys.stderr.write("Error: refusing npm package %s from a non-public registry\n" % expect)
    sys.exit(1)
if version and data.get("version") != version:
    sys.stderr.write("Error: the public registry has no %s@%s\n" % (expect, version))
    sys.exit(1)
' <<<"$json"
}

# Version of <package> in the npm global tree, or nothing when absent.
npm_global_package_version() {
    npm_public ls --global --json --depth=0 "$1" 2>/dev/null | EXPECT_PACKAGE="$1" python3 -c '
import json, os, sys
try:
    data = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
dep = (data.get("dependencies") or {}).get(os.environ["EXPECT_PACKAGE"]) or {}
print(dep.get("version") or "")
'
}

npm_package_has_receipt() {
    local package="$1"
    npm_public ls --global --parseable --depth=0 "$package" 2>/dev/null | grep -qF "node_modules/$package"
}

install_npm_package_from_catalog() {
    local name="$1"
    local package command bin_dir install_status version current spec
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

    # An optional "version" pins the row to one release (X.Y.Z), installed
    # and converged to exactly that.
    version="$(catalog_app_field "$name" version 2>/dev/null || true)"
    verify_npm_package_source "$package" "$version" || return 1
    spec="$package${version:+@$version}"

    command="$(catalog_app_field "$name" command 2>/dev/null || printf '%s\n' "$package")"
    export_local_bin_to_path
    if bin_dir="$(npm_global_bin_dir)"; then
        case ":${PATH}:" in
            *":$bin_dir:"*) ;;
            *) export PATH="${bin_dir}:${PATH}" ;;
        esac
    fi
    if declare -F elevate_plan_step_completed >/dev/null \
        && [[ -n "${MANAGED_MACHINE_CURRENT_STEP:-}" ]] \
        && elevate_plan_step_completed "$MANAGED_MACHINE_CURRENT_STEP" npm-global; then
        elevate_plan_step_status "$MANAGED_MACHINE_CURRENT_STEP" npm-global >/dev/null || true
        if ! npm_package_has_receipt "$package"; then
            echo "Install finished but $package was not found." >&2
            return 1
        fi
        if [[ -n "$version" && "$(npm_global_package_version "$package")" != "$version" ]]; then
            echo "Install finished but $package is not at the pinned $version." >&2
            return 1
        fi
        if command -v "$command" >/dev/null 2>&1; then
            echo "$package installed: $(command -v "$command")"
            "$command" --version 2>/dev/null || true
        else
            echo "$package installed (npm global)"
            npm_public ls --global --parseable --depth=0 "$package" || true
        fi
        return 0
    fi
    if [[ -n "$version" ]]; then
        current="$(npm_global_package_version "$package")"
        if [[ "$current" == "$version" ]]; then
            echo "$spec already installed (npm global)"
            return 0
        fi
        if [[ -z "$current" ]] && command -v "$command" >/dev/null 2>&1; then
            echo "$command already installed outside the npm global tree: $(command -v "$command") — leaving it; the $version pin applies to npm installs"
            return 0
        fi
        [[ -z "$current" ]] || echo "$package $current is installed; converging to the pinned $version"
    else
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
    fi

    echo "Installing $spec from the public npm registry..."
    npm_run install --global --no-fund --no-audit --registry="$(npm_registry_url)" "$spec"
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
    if [[ -n "$version" && "$(npm_global_package_version "$package")" != "$version" ]]; then
        echo "Install finished but $package is not at the pinned $version." >&2
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

# uv-tool rows: a Python CLI from PyPI, pinned to one release and installed
# with `uv tool install <package>==<version>` into uv's per-user tool dir
# (~/.local/bin by default). uv itself comes from homebrew/core (the `uv`
# catalog row). Only pypi.org is used: uv's config files and index
# environment variables are ignored, so a machine-scoped index cannot
# substitute the package.
UV_TOOL_INDEX_URL="https://pypi.org/simple"

uv_public() {
    env -u UV_INDEX -u UV_INDEX_URL -u UV_DEFAULT_INDEX -u UV_EXTRA_INDEX_URL \
        -u UV_FIND_LINKS -u PIP_INDEX_URL -u PIP_EXTRA_INDEX_URL \
        UV_NO_CONFIG=1 uv "$@"
}

# Version of <package> in `uv tool list`, or nothing when absent.
uv_tool_installed_version() {
    uv_public tool list 2>/dev/null | awk -v p="$1" '$1 == p { v = $2; sub(/^v/, "", v); print v; exit }'
}

# The public PyPI index must know <package> at exactly <version>.
verify_pypi_release() {
    local package="$1" version="$2" json
    json="$(curl -fsSL --max-time 20 "https://pypi.org/pypi/$package/$version/json" 2>/dev/null)" || {
        echo "Error: PyPI has no $package $version" >&2
        return 1
    }
    EXPECT_PACKAGE="$package" EXPECT_VERSION="$version" python3 -c '
import json, os, re, sys
info = json.load(sys.stdin).get("info") or {}
norm = lambda n: re.sub(r"[-_.]+", "-", n or "").lower()
if norm(info.get("name")) != norm(os.environ["EXPECT_PACKAGE"]) or info.get("version") != os.environ["EXPECT_VERSION"]:
    sys.stderr.write("Error: PyPI metadata does not match %s %s\n" % (os.environ["EXPECT_PACKAGE"], os.environ["EXPECT_VERSION"]))
    sys.exit(1)
' <<<"$json"
}

install_uv_tool_from_catalog() {
    local name="$1" package version command current
    package="$(catalog_app_field "$name" package 2>/dev/null || true)"
    version="$(catalog_app_field "$name" version 2>/dev/null || true)"
    command="$(catalog_app_field "$name" command 2>/dev/null || printf '%s\n' "$package")"
    if [[ ! "$package" =~ ^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$ ]]; then
        echo "Error: $name needs a valid PyPI package; refusing unverified uv install" >&2
        return 1
    fi
    if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "Error: $name needs a pinned version (X.Y.Z); refusing an unpinned uv install" >&2
        return 1
    fi
    if ! command -v uv >/dev/null 2>&1; then
        echo "Error: uv required — run managed-machine setup uv first" >&2
        return 1
    fi
    export_local_bin_to_path
    current="$(uv_tool_installed_version "$package")"
    if [[ "$current" == "$version" ]]; then
        echo "$package $version already installed (uv tool)"
        return 0
    fi
    if [[ -z "$current" ]] && command -v "$command" >/dev/null 2>&1; then
        echo "$command already installed outside uv: $(command -v "$command") — leaving it; the $version pin applies to uv installs"
        return 0
    fi
    verify_pypi_release "$package" "$version" || return 1
    if [[ -n "$current" ]]; then
        echo "$package $current is installed; converging to the pinned $version"
    else
        echo "Installing $package $version from PyPI with uv..."
    fi
    uv_public tool install --force --default-index "$UV_TOOL_INDEX_URL" "$package==$version" || return 1
    if [[ "$(uv_tool_installed_version "$package")" != "$version" ]]; then
        echo "Install finished but $package is not at the pinned $version." >&2
        return 1
    fi
    if command -v "$command" >/dev/null 2>&1; then
        echo "$package installed: $(command -v "$command")"
    else
        echo "$package $version installed; open a new shell or add $(uv_public tool dir --bin 2>/dev/null || echo '~/.local/bin') to PATH" >&2
    fi
}

# Persistent record of catalog apps whose privileged install was declined by the
# owner (#174). A declined row is listed as `not installed (declined)` on later
# `--update` (and `--bootstrap`) plans instead of prompting again on every run,
# until the owner explicitly runs `managed-machine setup <name>`.
declined_apps_manifest_file() {
    printf '%s/declined-apps.manifest\n' "$(managed_machine_config_dir)"
}

declined_app_is_recorded() {
    local requested="$1" name manifest
    name="$(catalog_resolve_name "$requested" 2>/dev/null || printf '%s\n' "${requested#setup-}")"
    manifest="$(declined_apps_manifest_file)"
    [[ -f "$manifest" ]] || return 1
    awk -F'\t' -v want="$name" '$1 == want { found=1; exit } END { exit (found ? 0 : 1) }' "$manifest"
}

record_declined_app() {
    local requested="$1" reason="${2:-declined}" name manifest dir tmp
    name="$(catalog_resolve_name "$requested" 2>/dev/null || printf '%s\n' "${requested#setup-}")"
    [[ -n "$name" ]] || return 0
    manifest="$(declined_apps_manifest_file)"
    dir="$(dirname "$manifest")"
    mkdir -p "$dir"
    chmod 700 "$dir" 2>/dev/null || true
    tmp="$(mktemp "${manifest}.XXXXXX")"
    umask 077
    {
        if [[ -f "$manifest" ]]; then
            awk -F'\t' -v drop="$name" '$1 != "" && $1 != drop { print }' "$manifest"
        fi
        printf '%s\t%s\t%s\n' "$name" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$reason"
    } >"$tmp"
    chmod 600 "$tmp" 2>/dev/null || true
    mv "$tmp" "$manifest"
}

clear_declined_app() {
    local requested="$1" name manifest tmp
    name="$(catalog_resolve_name "$requested" 2>/dev/null || printf '%s\n' "${requested#setup-}")"
    manifest="$(declined_apps_manifest_file)"
    [[ -f "$manifest" ]] || return 0
    if ! declined_app_is_recorded "$name"; then
        return 0
    fi
    tmp="$(mktemp "${manifest}.XXXXXX")"
    umask 077
    awk -F'\t' -v drop="$name" '$1 != "" && $1 != drop { print }' "$manifest" >"$tmp"
    chmod 600 "$tmp" 2>/dev/null || true
    mv "$tmp" "$manifest"
}

# Install one catalog app, then run config/<name> when that script exists.
install_catalog_app() {
    local requested="$1"
    local name kind rc=0
    if ! name="$(catalog_resolve_name "$requested")"; then
        echo "Error: unknown catalog app: $requested" >&2
        return 1
    fi
    case "$name" in agent-bot|agent-bot-gh) echo "Error: catalog setup $name is retired; managed-machine no longer installs or wires agent-bot." >&2; return 1 ;; esac
    if [[ -z "${MANAGED_MACHINE_IN_BATCH_RUN:-}" ]]; then
        clear_declined_app "$name"
    elif declined_app_is_recorded "$name"; then
        echo "Skipped: $name — not installed (declined); run 'managed-machine setup $name' to install" >&2
        return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
    fi
    kind="$(catalog_app_kind "$name")" || return 1
    case "$kind" in
        signed-cask|cask)
            install_cask_from_catalog "$name" || rc=$?
            ;;
        vendor-dmg)
            install_vendor_dmg_from_catalog "$name" || rc=$?
            ;;
        brew-formula)
            install_brew_formula_from_catalog "$name" || rc=$?
            ;;
        official-cli)
            install_official_cli_from_catalog "$name" || rc=$?
            ;;
        npm)
            install_npm_package_from_catalog "$name" || rc=$?
            ;;
        uv-tool)
            install_uv_tool_from_catalog "$name" || rc=$?
            ;;
        opencode)
            install_opencode_cli || rc=$?
            ;;
        devin)
            install_devin_app || rc=$?
            ;;
        *)
            echo "Error: unknown app kind '$kind' for $name — upgrade managed-machine to install this app" >&2
            return 1
            ;;
    esac
    if [[ "$rc" -ne 0 ]]; then
        if [[ -n "${MANAGED_MACHINE_ELEVATION_DECLINED_FILE:-}" && -e "$MANAGED_MACHINE_ELEVATION_DECLINED_FILE" ]]; then
            record_declined_app "$name" "authorization declined"
        fi
        return "$rc"
    fi
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
            installed_app="$(vendor_dmg_find_app "$app_name" "$(vendor_dmg_appdir_override "$name")" 2>/dev/null)" || return 1
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
        official-cli|opencode|devin|uv-tool)
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
        *)
            return 1 ;;
    esac
}

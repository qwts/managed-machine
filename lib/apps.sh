#!/usr/bin/env bash
# Install engines driven by the config-repo catalog. New kinds require a
# managed-machine upgrade; new apps are catalog rows only.

# shellcheck source=cask-app.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cask-app.sh"
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

    if command -v opencode >/dev/null 2>&1; then
        echo "OpenCode already installed: $(command -v opencode)"
        opencode --version
        return 0
    fi
    if link_opencode && command -v opencode >/dev/null 2>&1; then
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
    if ! command -v opencode >/dev/null 2>&1; then
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
    if command -v devin >/dev/null 2>&1; then
        echo "Devin CLI already installed: $(command -v devin)"
    else
        if ! command -v curl >/dev/null 2>&1; then
            echo "Error: curl is required to install Devin CLI" >&2
            return 1
        fi
        echo "Installing Devin CLI without launching interactive setup..."
        install_devin_cli
    fi
    if ! command -v devin >/dev/null 2>&1; then
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
    local display command url env_json key value
    display="$(catalog_app_field "$name" display 2>/dev/null || catalog_app_field "$name" name)"
    command="$(catalog_app_field "$name" command)" || return 1
    url="$(catalog_app_field "$name" url)" || return 1
    env_json="$(catalog_app_field "$name" env 2>/dev/null || true)"
    if [[ -n "$env_json" && "$env_json" != "{}" ]]; then
        while IFS=$'\t' read -r key value; do
            [[ -n "$key" ]] || continue
            export "$key=$value"
        done < <(MANAGED_MACHINE_ENV_JSON="$env_json" python3 -c '
import json, os
for key, value in json.loads(os.environ["MANAGED_MACHINE_ENV_JSON"]).items():
    print("%s\t%s" % (key, value))
')
    fi
    install_official_cli "$display" "$command" "$url"
}

install_cask_from_catalog() {
    local name="$1"
    local token override
    token="$(catalog_app_field "$name" token)" || return 1
    override="$(cask_appdir_override_for_token "$token")"
    install_signed_cask_app "$token" "$override"
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
        official-cli)
            install_official_cli_from_catalog "$name" || return $?
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
    apply_config_script "$name"
}

print_catalog_app_names() {
    local name
    echo
    echo "Available catalog apps (bare and setup- prefixed forms are accepted):"
    while IFS= read -r name; do
        [[ -n "$name" ]] || continue
        printf '  %s\n' "$name"
    done < <(catalog_app_names 2>/dev/null || true)
}

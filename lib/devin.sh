#!/usr/bin/env bash
# Devin CLI installation and authentication helpers.

DEVIN_INSTALLER_URL="https://cli.devin.ai/install.sh"
DEVIN_INSTALLER_SETUP_LINE='"$VERSION_DIR/bin/$COMPILED_BIN_NAME" setup'

install_devin_cli() {
    local installer install_only final_line setup_line_count
    installer="$(mktemp)" || return 1
    install_only="$(mktemp)" || {
        rm -f "$installer"
        return 1
    }
    # shellcheck disable=SC2064
    trap 'rm -f "$installer" "$install_only"; trap - RETURN' RETURN

    echo "Downloading the official Devin CLI installer..."
    if ! curl -fsSL "$DEVIN_INSTALLER_URL" -o "$installer"; then
        echo "Error: could not download $DEVIN_INSTALLER_URL" >&2
        return 1
    fi

    final_line="$(tail -n 1 "$installer")"
    setup_line_count="$(grep -cFx "$DEVIN_INSTALLER_SETUP_LINE" "$installer" || true)"
    if [[ "$final_line" != "$DEVIN_INSTALLER_SETUP_LINE" || "$setup_line_count" -ne 1 ]]; then
        echo "Error: Devin installer no longer ends with the expected setup command; refusing to modify or execute it" >&2
        echo "Review $DEVIN_INSTALLER_URL and update setup-devin before retrying." >&2
        return 1
    fi

    # Preserve the upstream installer byte-for-byte except for its final,
    # unconditional interactive setup invocation. The installer still verifies
    # the downloaded CLI bundle checksum before installing it.
    sed '$d' "$installer" >"$install_only"
    /bin/bash "$install_only"
}

devin_is_authenticated() {
    devin auth status >/dev/null 2>&1
}

ensure_devin_authentication() {
    local interactive_input

    if devin_is_authenticated; then
        echo "Devin CLI authentication is already configured."
        return 0
    fi

    if [[ "${MANAGED_MACHINE_BOOTSTRAP_MODE:-}" == "noninteractive" ]] \
        || ! interactive_input="$(bootstrap_interactive_input)"; then
        defer_setup \
            "Devin CLI is installed, but browser authentication requires an interactive terminal" \
            "managed-machine setup devin"
        return $?
    fi

    echo "Starting Devin interactive setup..."
    if ! devin setup <"$interactive_input"; then
        echo "Error: Devin interactive setup did not complete" >&2
        return 1
    fi
    if ! devin_is_authenticated; then
        echo "Error: Devin setup completed without a usable authenticated session" >&2
        return 1
    fi
    echo "Devin CLI authentication complete."
}

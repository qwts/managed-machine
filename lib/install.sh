#!/usr/bin/env bash
# Shared helpers for managed-machine setup scripts.

managed_machine_config_dir() {
    printf '%s/.config/managed-machine\n' "$HOME"
}

# Managed PATH block markers used in ~/.zshrc. Kept identical to home-bin's
# markers so living machines keep managing the same block without migration.
HOME_BIN_PATH_BEGIN="# BEGIN home-bin"
HOME_BIN_PATH_END="# END home-bin"
HOME_BIN_PATH_EXPORT='export PATH="${HOME}/.local/bin:${PATH}"'

# Ensure ~/.local/bin is on PATH for the current process.
export_local_bin_to_path() {
    case ":${PATH}:" in
        *":${HOME}/.local/bin:"*) ;;
        *) export PATH="${HOME}/.local/bin:${PATH}" ;;
    esac
}

# Ensure ~/.zshrc has a managed block exporting ~/.local/bin on PATH.
#
# Rewrites the block in place if it already exists; appends a new block
# otherwise. Never touches lines outside the markers. Prints a one-line
# note if ~/.bin still appears in ~/.zshrc outside the managed block.
ensure_local_bin_in_zshrc() {
    local zshrc="${1:-${HOME}/.zshrc}"
    local outside tmp
    outside="$(mktemp)"
    tmp="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$outside" "$tmp"' RETURN

    mkdir -p "$(dirname "$zshrc")"
    [[ -f "$zshrc" ]] || : >"$zshrc"

    awk -v b="$HOME_BIN_PATH_BEGIN" -v e="$HOME_BIN_PATH_END" '
        $0 == b { skip = 1; next }
        $0 == e { skip = 0; next }
        !skip { print }
    ' "$zshrc" >"$outside"

    if grep -qF '.bin' "$outside"; then
        echo "note: ~/.bin still referenced in $zshrc outside the managed block — remove it manually if you no longer want ~/.bin on PATH" >&2
    fi

    {
        cat "$outside"
        printf '\n%s\n%s\n%s\n' "$HOME_BIN_PATH_BEGIN" "$HOME_BIN_PATH_EXPORT" "$HOME_BIN_PATH_END"
    } >"$tmp"
    mv "$tmp" "$zshrc"
    echo "ensured ~/.local/bin on PATH in $zshrc"
}

# Managed PATH block markers for rustup/cargo in ~/.zshrc.
RUSTUP_PATH_BEGIN="# BEGIN rustup"
RUSTUP_PATH_END="# END rustup"
RUSTUP_PATH_EXPORT='export PATH="${HOME}/.cargo/bin:${PATH}"'

# Ensure ~/.cargo/bin is on PATH for the current process.
export_cargo_bin_to_path() {
    case ":${PATH}:" in
        *":${HOME}/.cargo/bin:"*) ;;
        *) export PATH="${HOME}/.cargo/bin:${PATH}" ;;
    esac
}

# Ensure ~/.zshrc has a managed block exporting ~/.cargo/bin on PATH.
#
# Rewrites the block in place if it already exists; appends a new block
# otherwise. Never touches lines outside the markers.
ensure_cargo_bin_in_zshrc() {
    local zshrc="${1:-${HOME}/.zshrc}"
    local outside tmp
    outside="$(mktemp)"
    tmp="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$outside" "$tmp"' RETURN

    mkdir -p "$(dirname "$zshrc")"
    [[ -f "$zshrc" ]] || : >"$zshrc"

    awk -v b="$RUSTUP_PATH_BEGIN" -v e="$RUSTUP_PATH_END" '
        $0 == b { skip = 1; next }
        $0 == e { skip = 0; next }
        !skip { print }
    ' "$zshrc" >"$outside"

    {
        cat "$outside"
        printf '\n%s\n%s\n%s\n' "$RUSTUP_PATH_BEGIN" "$RUSTUP_PATH_EXPORT" "$RUSTUP_PATH_END"
    } >"$tmp"
    mv "$tmp" "$zshrc"
    echo "ensured ~/.cargo/bin on PATH in $zshrc"
}

# Install a repo file into the home directory when safe to do so.
#
# - Missing destination: copy from src and record in manifest.
# - Existing destination listed in manifest: leave unchanged.
# - Existing destination not in manifest: leave unchanged (user-owned).
install_home_file() {
    local src="$1"
    local dest="$2"
    local manifest="$3"
    local label="${4:-$(basename "$dest")}"

    if [[ ! -f "$src" ]]; then
        echo "Error: missing template $src" >&2
        return 1
    fi

    if [[ -e "$dest" ]]; then
        if [[ -f "$manifest" ]] && grep -qxF "$dest" "$manifest"; then
            echo "already installed: $label"
        else
            echo "skipping existing (not managed by managed-machine): $label"
        fi
        return 0
    fi

    mkdir -p "$(dirname "$dest")"
    cp "$src" "$dest"
    mkdir -p "$(dirname "$manifest")"
    touch "$manifest"
    printf '%s\n' "$dest" >>"$manifest"
    echo "installed: $label"
}

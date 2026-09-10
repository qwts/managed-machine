#!/usr/bin/env bash
# Shared helpers for managed-machine setup scripts.

managed_machine_config_dir() {
    printf '%s/.config/managed-machine\n' "$HOME"
}

# shellcheck source=config-repo.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/config-repo.sh"
# shellcheck source=elevate.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/elevate.sh"
# shellcheck source=brew.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/brew.sh"
# shellcheck source=catalog.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/catalog.sh"

# Managed PATH block markers used in ~/.zshrc. Kept identical to local-bin's
# markers so living machines keep managing the same block without migration.
# The block is guarded: shells inherit PATH from their parent, so an
# unconditional prepend would add a duplicate entry on every nested shell.
LOCAL_BIN_PATH_BEGIN="# BEGIN local-bin"
LOCAL_BIN_PATH_END="# END local-bin"
LOCAL_BIN_PATH_EXPORT='case ":${PATH}:" in
    *":${HOME}/.local/bin:"*) ;;
    *) export PATH="${HOME}/.local/bin:${PATH}" ;;
esac'

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
    trap 'rm -f "$outside" "$tmp"; trap - RETURN' RETURN

    mkdir -p "$(dirname "$zshrc")"
    [[ -f "$zshrc" ]] || : >"$zshrc"

    awk -v b="$LOCAL_BIN_PATH_BEGIN" -v e="$LOCAL_BIN_PATH_END" '
        $0 == b { skip = 1; next }
        $0 == e { skip = 0; next }
        !skip { print }
    ' "$zshrc" >"$outside"

    if grep -qF '.bin' "$outside"; then
        echo "note: ~/.bin still referenced in $zshrc outside the managed block — remove it manually if you no longer want ~/.bin on PATH" >&2
    fi

    {
        cat "$outside"
        printf '\n%s\n%s\n%s\n' "$LOCAL_BIN_PATH_BEGIN" "$LOCAL_BIN_PATH_EXPORT" "$LOCAL_BIN_PATH_END"
    } >"$tmp"
    mv "$tmp" "$zshrc"
    echo "ensured ~/.local/bin on PATH in $zshrc"
}

# Install a missing CLI from an official curl|bash installer.
# Extra arguments are forwarded to the installer (`bash -s -- ...`).
managed_cli_available() {
    local executable
    hash -r
    executable="$(command -v "$1")" || return 1
    [[ "${MANAGED_MACHINE_ACCOUNT_SETUP:-}" == 1 ]] || return 0
    python3 - "$executable" "$HOME" <<'PY'
import os, sys
path, home = map(os.path.realpath, sys.argv[1:])
try:
    valid = os.path.commonpath([path, home]) == home and os.stat(path).st_uid == os.getuid() and os.access(path, os.X_OK)
except (OSError, ValueError):
    valid = False
sys.exit(0 if valid else 1)
PY
}

# Link a vendor-installed binary that the vendor placed outside the managed
# PATH (e.g. kilo's $HOME/.kilo/bin) into ~/.local/bin, mirroring opencode's
# ~/.opencode/bin handling. Idempotent; leaves a user-managed file alone.
link_vendor_bin() {
    local bin_dir="$1"
    local command="$2"
    local vendor_bin vendor_link
    vendor_bin="${HOME}/${bin_dir}/${command}"
    vendor_link="${HOME}/.local/bin/${command}"
    [[ -x "$vendor_bin" ]] || return 1
    mkdir -p "${HOME}/.local/bin"
    if [[ -e "$vendor_link" || -L "$vendor_link" ]]; then
        if [[ -L "$vendor_link" && "$(readlink "$vendor_link")" == "$vendor_bin" ]]; then
            export_local_bin_to_path
            return 0
        fi
        echo "Error: $vendor_link exists and is not the managed $command vendor symlink" >&2
        return 1
    fi
    ln -s "$vendor_bin" "$vendor_link"
    export_local_bin_to_path
}

install_official_cli() {
    local display="$1"
    local cmd="$2"
    local url="$3"
    local bin_dir="${4:-}"
    shift 4

    ensure_local_bin_in_zshrc "${HOME}/.zshrc"
    export_local_bin_to_path

    if managed_cli_available "$cmd"; then
        echo "$display already installed: $(command -v "$cmd")"
        "$cmd" --version
        return 0
    fi
    # The vendor placed the binary outside the managed PATH (e.g. kilo's
    # ~/.kilo/bin); link it so setup both installs and exposes it. Accept the
    # link only when the managed command resolves and passes the account
    # home/ownership check.
    if [[ -n "$bin_dir" ]] && link_vendor_bin "$bin_dir" "$cmd" \
        && managed_cli_available "$cmd"; then
        echo "$display already installed: $(command -v "$cmd")"
        "$cmd" --version
        return 0
    fi

    if ! command -v curl >/dev/null 2>&1; then
        echo "Error: curl is required to install $display" >&2
        return 1
    fi

    echo "Installing $display..."
    # Vendor installers pick the rc file they append a PATH block to from
    # $SHELL. PATH is managed-machine's: ~/.local/bin is on it through
    # setup-zsh's guarded block, and installers that symlink into a PATH
    # directory still find it. So the installer runs with no login shell to
    # edit (#87), and an installer that edits ~/.zshrc anyway is reported
    # rather than left to leak silently.
    local zshrc="${HOME}/.zshrc" zshrc_before zshrc_after
    zshrc_before="$(shasum -a 256 "$zshrc" 2>/dev/null || true)"
    curl -fsSL "$url" | SHELL=/bin/sh bash -s -- "$@"
    zshrc_after="$(shasum -a 256 "$zshrc" 2>/dev/null || true)"
    if [[ "$zshrc_before" != "$zshrc_after" ]]; then
        echo "warn: $display's installer edited $zshrc outside managed-machine's guards — run 'managed-machine setup zsh' to re-own PATH" >&2
    fi

    export_local_bin_to_path
    if managed_cli_available "$cmd"; then
        echo "$display installed: $(command -v "$cmd")"
        "$cmd" --version
        return 0
    fi
    if [[ -n "$bin_dir" ]] && link_vendor_bin "$bin_dir" "$cmd" \
        && managed_cli_available "$cmd"; then
        echo "$display installed: $(command -v "$cmd")"
        "$cmd" --version
        return 0
    fi
    echo "Install finished but $cmd not found on PATH." >&2
    echo "Open a new shell or: export PATH=\"${HOME}/.local/bin:\$PATH\"" >&2
    return 1
}

# Managed PATH block markers for rustup/cargo in ~/.zshrc.
# Honors CARGO_HOME at shell startup (falls back to ~/.cargo).
# Guarded against duplicate entries when a nested shell inherits PATH.
RUSTUP_PATH_BEGIN="# BEGIN rustup"
RUSTUP_PATH_END="# END rustup"
RUSTUP_PATH_EXPORT='case ":${PATH}:" in
    *":${CARGO_HOME:-${HOME}/.cargo}/bin:"*) ;;
    *) export PATH="${CARGO_HOME:-${HOME}/.cargo}/bin:${PATH}" ;;
esac'

# Resolve cargo home / bin / env, honoring CARGO_HOME when set.
cargo_home_dir() {
    printf '%s\n' "${CARGO_HOME:-${HOME}/.cargo}"
}

cargo_bin_dir() {
    printf '%s/bin\n' "$(cargo_home_dir)"
}

cargo_env_file() {
    printf '%s/env\n' "$(cargo_home_dir)"
}

# Ensure cargo's bin dir is on PATH for the current process.
export_cargo_bin_to_path() {
    local cargo_bin
    cargo_bin="$(cargo_bin_dir)"
    case ":${PATH}:" in
        *":${cargo_bin}:"*) ;;
        *) export PATH="${cargo_bin}:${PATH}" ;;
    esac
}

# Ensure ~/.zshrc has a managed block exporting cargo's bin dir on PATH.
#
# Rewrites the block in place if it already exists; appends a new block
# otherwise. Never touches lines outside the markers.
ensure_cargo_bin_in_zshrc() {
    local zshrc="${1:-${HOME}/.zshrc}"
    local outside tmp
    outside="$(mktemp)"
    tmp="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$outside" "$tmp"; trap - RETURN' RETURN

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
    echo "ensured \${CARGO_HOME:-~/.cargo}/bin on PATH in $zshrc"
}

# NVM is installed in ~/.nvm and loaded from a small managed zsh block.
NVM_ZSH_BEGIN="# BEGIN nvm"
NVM_ZSH_END="# END nvm"

nvm_dir() {
    printf '%s\n' "${NVM_DIR:-${HOME}/.nvm}"
}

# Load NVM into the current shell. nvm is a shell function, so checking for
# nvm.sh is the reliable way to determine whether it is installed.
load_nvm() {
    local dir
    dir="$(nvm_dir)"
    if [[ ! -s "$dir/nvm.sh" ]]; then
        echo "Error: NVM is missing $dir/nvm.sh" >&2
        return 1
    fi

    export NVM_DIR="$dir"
    # shellcheck source=/dev/null
    . "$NVM_DIR/nvm.sh"

    if ! command -v nvm >/dev/null 2>&1; then
        echo "Error: NVM did not load from $NVM_DIR/nvm.sh" >&2
        return 1
    fi
}

# Ensure ~/.zshrc has the managed NVM initialization block. Rewrites only the
# block owned by managed-machine and leaves all other shell configuration alone.
# The block guards against duplicate PATH entries: when node already resolves
# under NVM_DIR (a nested shell inheriting a good PATH), nvm.sh is loaded with
# --no-use so the nvm function stays available without prepending again.
# Otherwise nvm.sh loads normally so the configured version wins over a
# system/Homebrew node that sorts ahead of an inherited nvm entry.
ensure_nvm_in_zshrc() {
    local zshrc="${1:-${HOME}/.zshrc}"
    local nvm_directory outside tmp
    nvm_directory="$(nvm_dir)"
    outside="$(mktemp)"
    tmp="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$outside" "$tmp"; trap - RETURN' RETURN

    mkdir -p "$(dirname "$zshrc")"
    [[ -f "$zshrc" ]] || : >"$zshrc"

    awk -v b="$NVM_ZSH_BEGIN" -v e="$NVM_ZSH_END" '
        $0 == b { skip = 1; next }
        $0 == e { skip = 0; next }
        !skip { print }
    ' "$zshrc" >"$outside"

    {
        cat "$outside"
        printf '\n%s\n' "$NVM_ZSH_BEGIN"
        if [[ "$nvm_directory" == "${HOME}/.nvm" ]]; then
            printf 'export NVM_DIR="${NVM_DIR:-${HOME}/.nvm}"\n'
        else
            printf 'export NVM_DIR=%q\n' "$nvm_directory"
        fi
        printf 'case "$(command -v node 2>/dev/null)" in\n'
        printf '    "${NVM_DIR}/versions/"*) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" --no-use ;;\n'
        printf '    *) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" ;;\n'
        printf 'esac\n'
        printf '%s\n' "$NVM_ZSH_END"
    } >"$tmp"
    mv "$tmp" "$zshrc"
    echo "ensured NVM initialization in $zshrc"
}

# True when a zsh startup file still has unguarded PATH prepends that would
# duplicate ~/.local/bin / cargo / nvm on every nested shell. A vendor
# installer comment alone is not enough — --update re-runs this and must
# not replace a custom profile that only sourced an alias.
zsh_profile_needs_refresh() {
    local file="$1"
    [[ -f "$file" ]] || return 1
    grep -qxF 'export PATH="${HOME}/.local/bin:${PATH}"' "$file" && return 0
    grep -qxF 'export PATH="${CARGO_HOME:-${HOME}/.cargo}/bin:${PATH}"' "$file" && return 0
    grep -qE '^export PATH="[^"]*/\.local/bin:\$PATH"$' "$file" && return 0
    if grep -qxF '# BEGIN nvm' "$file" && ! grep -qF 'command -v node' "$file"; then
        return 0
    fi
    return 1
}

# Move dest to dest.<epoch>.bak (next to the original). Prints the bak path.
backup_existing_home_file() {
    local dest="$1"
    local epoch bak
    epoch="$(date +%s)"
    bak="${dest}.${epoch}.bak"
    if [[ -e "$bak" ]]; then
        bak="${dest}.${epoch}.$$.bak"
    fi
    mv "$dest" "$bak"
    printf '%s\n' "$bak"
}

preserve_zsh_profile_extras() {
    local bak="$1"
    local dest="$2"
    local name
    name="$(basename "$dest")"
    case "$name" in
        .zprofile)
            grep -E 'brew shellenv' "$bak" >>"$dest" || true
            ;;
        .zshenv)
            grep -E '\.cargo/env' "$bak" >>"$dest" || true
            ;;
    esac
}

record_home_file_manifest() {
    local dest="$1"
    local manifest="$2"
    mkdir -p "$(dirname "$manifest")"
    touch "$manifest"
    grep -qxF "$dest" "$manifest" 2>/dev/null || printf '%s\n' "$dest" >>"$manifest"
}

# Install a zsh startup template. Stale unguarded/vendor PATH files are moved
# to <name>.<epoch>.bak first; brew shellenv and rustup's cargo/env lines are
# copied forward. Idempotent when the file no longer needs a refresh.
install_zsh_startup_file() {
    local src="$1"
    local dest="$2"
    local manifest="$3"
    local label="${4:-$(basename "$dest")}"
    local bak=""

    if [[ ! -f "$src" ]]; then
        echo "Error: missing template $src" >&2
        return 1
    fi

    mkdir -p "$(dirname "$dest")"

    if [[ -e "$dest" ]]; then
        if zsh_profile_needs_refresh "$dest"; then
            bak="$(backup_existing_home_file "$dest")"
            echo "backed up $label -> $bak"
        else
            echo "already current: $label"
            record_home_file_manifest "$dest" "$manifest"
            return 0
        fi
    fi

    cp "$src" "$dest"
    if [[ -n "$bak" ]]; then
        preserve_zsh_profile_extras "$bak" "$dest"
    fi
    record_home_file_manifest "$dest" "$manifest"
    echo "installed: $label"
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

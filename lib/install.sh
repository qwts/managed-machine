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

# Ensure a named "# BEGIN <name>" / "# END <name>" block with the given body
# (read from stdin) exists in a zsh startup file.
#
# Delegates to zsh-profile (qwts/zsh-functions) when it resolves on PATH;
# managed-machine installs zsh-functions, so it cannot hard-depend on it and
# keeps an internal fallback for bootstrap order. The fallback rewrites an
# existing block in place (duplicate blocks collapse to the first site) and
# strips trailing blank lines before appending a new one, so repeated runs
# never grow whitespace with or without zsh-profile. Writes commit through a
# same-directory temp + mv with mode preservation.
ensure_zsh_block() {
    local file="$1" name="$2" body_tmp="" out_tmp="" mode=""
    [[ -n "${name:-}" ]] || { echo "Error: ensure_zsh_block requires a block name" >&2; return 1; }

    mkdir -p "$(dirname "$file")"
    [[ -f "$file" ]] || : >"$file"

    if command -v zsh-profile >/dev/null 2>&1; then
        zsh-profile ensure-block --file "$file" --name "$name" >/dev/null
        return $?
    fi

    body_tmp="$(mktemp)"
    out_tmp="$(mktemp "$(dirname "$file")/.zsh-block.XXXXXX")"
    # shellcheck disable=SC2064
    trap 'rm -f "$body_tmp" "$out_tmp"; trap - RETURN' RETURN
    cat >"$body_tmp"
    if [[ ! -s "$body_tmp" ]]; then
        echo "Error: ensure_zsh_block received an empty body for block '$name'" >&2
        return 1
    fi
    if ! awk \
        -v begin="# BEGIN $name" \
        -v end="# END $name" \
        -v bodyfile="$body_tmp" '
        BEGIN {
            nbody = 0
            while ((getline l < bodyfile) > 0) body[nbody++] = l
            found = 0; emitted = 0; skipping = 0; n = 0
        }
        {
            if ($0 == begin) { skipping = 1; found = 1; next }
            if (skipping) {
                if ($0 == end) {
                    if (!emitted) {
                        out[n++] = begin
                        for (i = 0; i < nbody; i++) out[n++] = body[i]
                        out[n++] = end
                        emitted = 1
                    }
                    skipping = 0
                }
                next
            }
            out[n++] = $0
        }
        END {
            if (skipping) exit 3
            if (!found) {
                while (n > 0 && out[n-1] ~ /^[ \t]*$/) n--
                if (n > 0) out[n++] = ""
                out[n++] = begin
                for (i = 0; i < nbody; i++) out[n++] = body[i]
                out[n++] = end
            }
            for (i = 0; i < n; i++) print out[i]
        }
    ' "$file" >"$out_tmp"; then
        echo "Error: unterminated block \"# BEGIN $name\" in $file" >&2
        return 1
    fi
    if cmp -s "$out_tmp" "$file"; then
        return 0
    fi
    # GNU first: on GNU stat, `-f` means filesystem status and prints
    # garbage to stdout before failing, which would poison chmod input.
    # BSD stat rejects `-c` silently, so this order is safe on both.
    mode="$(stat -c %a "$file" 2>/dev/null || stat -f %Lp "$file" 2>/dev/null)" || {
        echo "Error: cannot read mode of $file" >&2
        return 1
    }
    chmod "$mode" "$out_tmp" && mv -f "$out_tmp" "$file"
}

# Ensure ~/.zshrc has a managed block exporting ~/.local/bin on PATH.
#
# Rewrites the block in place if it already exists; appends a new block
# otherwise. Never touches lines outside the markers. Prints a one-line
# note if ~/.bin still appears in ~/.zshrc outside the managed block.
ensure_local_bin_in_zshrc() {
    local zshrc="${1:-${HOME}/.zshrc}"
    local outside
    outside="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$outside"; trap - RETURN' RETURN

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

    printf '%s\n' "$LOCAL_BIN_PATH_EXPORT" | ensure_zsh_block "$zshrc" "local-bin"
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

# Snapshot the startup files an installer might touch as "<path>\t<line>"
# records. With ZDOTDIR set, zsh reads these from ${ZDOTDIR}, not $HOME, so
# both roots are captured; account setup writes under the effective directory.
# Missing files produce no records, so an installer that creates one shows all
# its lines as added.
snapshot_startup_files() {
    local dir file
    for dir in "${ZDOTDIR:-$HOME}" "$HOME"; do
        for file in "$dir/.zshrc" "$dir/.zprofile" "$dir/.zshenv"; do
            if [[ -f "$file" ]]; then
                awk -v tag="$file" '{ print tag "\t" $0 }' "$file"
            fi
        done
    done
}

# Rebuild one file's line sequence from a snapshot.
snapshot_file_lines() {
    local tag="$1" snapshot="$2"
    awk -v tag="$tag" 'index($0, tag "\t") == 1 { print substr($0, length(tag) + 2) }' "$snapshot"
}

# Print every content line that occurs in file at a position outside every
# properly closed "# BEGIN <name>" … "# END <name>" range. A range must close
# with a marker of the same name before EOF; an unterminated BEGIN never
# suppresses the lines after it, so a vendor cannot hide a bare export behind
# an orphan marker. The first pass records closed ranges, the second
# classifies each line.
zsh_unguarded_contents() {
    local file="$1"
    awk '
        NR == FNR {
            if ($0 ~ /^# BEGIN /) { sp++; name[sp] = $3; bln[sp] = NR; next }
            if ($0 ~ /^# END /) {
                if (sp > 0 && name[sp] == $3) { nclo++; clo[nclo] = bln[sp] ":" NR; sp-- }
                next
            }
            next
        }
        {
            if ($0 ~ /^# BEGIN / || $0 ~ /^# END /) next
            inside = 0
            for (i = 1; i <= nclo; i++) {
                split(clo[i], r, ":")
                if (FNR > r[1] && FNR < r[2]) { inside = 1; break }
            }
            if (!inside) seen[$0] = 1
        }
        END { for (k in seen) print k }
    ' "$file" "$file"
}

# After a vendor installer runs, compare the startup-file snapshot against the
# current files and report every PATH line the installer made newly unguarded —
# naming the file and the line. An "unguarded PATH line" is any bare
# `export PATH=...` outside a properly closed "# BEGIN"/"# END" range: managed
# dirs exactly as zsh_unguarded_path_line defines them (refresh-actionable,
# fixed by `managed-machine setup zsh`), and foreign vendor dirs (alert-only,
# removed by hand). "Newly unguarded" means the line had no unguarded
# occurrence before the run and does now: that covers additions and lines a
# vendor moves out from inside a guarded block, and it stays quiet when a
# pre-existing leak is merely re-run. carry-over statements (brew shellenv,
# cargo env) do not match the PATH predicate and are never reported. A change
# with no newly unguarded PATH line prints an ok note; no change prints
# nothing. The check is occurrence-aware, not a whole-file hash, so a
# shadowing edit elsewhere in the file cannot mask or fabricate an unguarded
# PATH add (managed-machine#158).
report_vendor_startup_edits() {
    local before="$1" after="$2" label="$3"
    local files file bf af line refreshable=0 foreign=0 any_change=0
    files="$(awk -F '\t' '{ print $1 }' "$before" "$after" | sort -u)"
    bf="$(mktemp)"
    af="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$bf" "$af"; trap - RETURN' RETURN
    while IFS= read -r file; do
        [[ -n "$file" ]] || continue
        snapshot_file_lines "$file" "$before" >"$bf"
        snapshot_file_lines "$file" "$after" >"$af"
        if cmp -s "$bf" "$af"; then
            continue
        fi
        any_change=1
        unguarded_before="$(zsh_unguarded_contents "$bf")"
        unguarded_after="$(zsh_unguarded_contents "$af")"
        while IFS= read -r line; do
            [[ -n "$line" ]] || continue
            if ! zsh_unguarded_path_line "$line" \
                && ! [[ "$line" =~ ^[[:space:]]*export[[:space:]]+PATH= ]]; then
                continue
            fi
            if grep -qxF "$line" <<<"$unguarded_before"; then
                continue
            fi
            if zsh_unguarded_path_line "$line"; then
                refreshable=1
            else
                foreign=1
            fi
            printf 'warn: %s added unguarded line to %s: %s\n' "$label" "$file" "$line" >&2
        done <<<"$unguarded_after"
    done <<<"$files"
    if (( refreshable || foreign )); then
        if (( refreshable )); then
            echo "warn: $label edited a startup file outside managed-machine's guards — run 'managed-machine setup zsh' to re-own PATH" >&2
        fi
        if (( foreign )); then
            echo "warn: $label added an unmanaged PATH line to a startup file — review it and remove it manually if unwanted" >&2
        fi
    elif (( any_change )); then
        echo "ok: $label touched a startup file but added no unguarded PATH line" >&2
    fi
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
    # edit (#87), and an installer that edits a startup file anyway is
    # reported rather than left to leak silently.
    local rc_before rc_after
    rc_before="$(mktemp)"
    rc_after="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$rc_before" "$rc_after"; trap - RETURN' RETURN
    snapshot_startup_files >"$rc_before"
    curl -fsSL "$url" | SHELL=/bin/sh bash -s -- "$@"
    snapshot_startup_files >"$rc_after"
    report_vendor_startup_edits "$rc_before" "$rc_after" "$display"

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

    mkdir -p "$(dirname "$zshrc")"
    [[ -f "$zshrc" ]] || : >"$zshrc"

    printf '%s\n' "$RUSTUP_PATH_EXPORT" | ensure_zsh_block "$zshrc" "rustup"
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
    local nvm_directory
    nvm_directory="$(nvm_dir)"

    mkdir -p "$(dirname "$zshrc")"
    [[ -f "$zshrc" ]] || : >"$zshrc"

    {
        if [[ "$nvm_directory" == "${HOME}/.nvm" ]]; then
            printf 'export NVM_DIR="${NVM_DIR:-${HOME}/.nvm}"\n'
        else
            printf 'export NVM_DIR=%q\n' "$nvm_directory"
        fi
        printf 'case "$(command -v node 2>/dev/null)" in\n'
        printf '    "${NVM_DIR}/versions/"*) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" --no-use ;;\n'
        printf '    *) [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" ;;\n'
        printf 'esac\n'
    } | ensure_zsh_block "$zshrc" "nvm"
    echo "ensured NVM initialization in $zshrc"
}

# True when a line is an unguarded PATH prepend that would duplicate a managed
# dir on every nested shell. Agent-bot era lines use $HOME (not ${HOME}) with
# trailing marker comments, so the match allows both variable forms plus an
# optional trailing comment; guarded case bodies never match because they do
# not start with export. This is the single source of truth for what counts as
# an unguarded line, shared by zsh_profile_needs_refresh and by the vendor
# installer diff (report_vendor_startup_edits).
zsh_unguarded_path_line() {
    local line="$1"
    [[ "$line" == 'export PATH="${HOME}/.local/bin:${PATH}"' ]] && return 0
    [[ "$line" == 'export PATH="${CARGO_HOME:-${HOME}/.cargo}/bin:${PATH}"' ]] && return 0
    grep -qE '^export PATH="[^"]*/\.local/bin:\$PATH"$' <<<"$line" && return 0
    grep -qE '^[ \t]*export[ \t]+PATH="(\$\{HOME\}|\$HOME)/(\.local/bin|\.config/agent-bot/bin):(\$PATH|\$\{PATH\})"[ \t]*(#.*)?$' <<<"$line" && return 0
    return 1
}

# True when a zsh startup file still has unguarded PATH prepends that would
# duplicate managed dirs on every nested shell. A vendor installer comment
# alone is not enough — --update re-runs this and must not replace a custom
# profile that only sourced an alias.
zsh_profile_needs_refresh() {
    local file="$1" line
    [[ -f "$file" ]] || return 1
    while IFS= read -r line; do
        if zsh_unguarded_path_line "$line"; then
            return 0
        fi
    done <"$file"
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
    preserve_managed_blocks "$bak" "$dest"
}

# Carry every well-formed "# BEGIN <name>" … "# END <name>" range from a
# backup into its rewritten file, regardless of startup file. Ranges whose
# name already exists in the destination are skipped, so template-owned
# blocks (e.g. the zsh-functions loader) are never duplicated. An orphan
# BEGIN without its END is left behind for manual repair. The destination's
# trailing blank lines are trimmed first, then ranges join with exactly one
# blank separator, so repeated refreshes never accumulate drift. The result
# commits through a same-directory temp + mv with mode preservation; a file
# with nothing carried is untouched.
preserve_managed_blocks() {
    local bak="$1" dest="$2" names name add_tmp final_tmp first mode
    names="$(awk '/^# BEGIN /{ b = substr($0, 9); open = b; next } open != "" && $0 == ("# END " open) { print open; open = "" }' "$bak")"
    [[ -n "$names" ]] || return 0
    add_tmp="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$add_tmp"; trap - RETURN' RETURN
    first=1
    while IFS= read -r name; do
        [[ -n "$name" ]] || continue
        if grep -qxF "# BEGIN $name" "$dest"; then
            continue
        fi
        if (( first )); then
            first=0
        else
            printf '\n' >>"$add_tmp"
        fi
        awk -v b="# BEGIN $name" -v e="# END $name" \
            '$0 == b { p = 1 } p { print } $0 == e && p { exit }' \
            "$bak" >>"$add_tmp"
    done <<<"$names"
    if [[ ! -s "$add_tmp" ]]; then
        return 0
    fi
    final_tmp="$(mktemp "$(dirname "$dest")/.zsh-preserve.XXXXXX")"
    # shellcheck disable=SC2064
    trap 'rm -f "$add_tmp" "$final_tmp"; trap - RETURN' RETURN
    awk '/^[ \t]*$/{ n++; next } { for (i = 0; i < n; i++) print ""; n = 0; print }' "$dest" >"$final_tmp"
    if [[ -s "$final_tmp" ]]; then
        printf '\n' >>"$final_tmp"
    fi
    cat "$add_tmp" >>"$final_tmp"
    if cmp -s "$final_tmp" "$dest"; then
        return 0
    fi
    # GNU first: on GNU stat, `-f` prints filesystem blurbage to stdout
    # before failing, which would poison chmod input.
    mode="$(stat -c %a "$dest" 2>/dev/null || stat -f %Lp "$dest" 2>/dev/null)" || {
        echo "Error: cannot read mode of $dest" >&2
        return 1
    }
    chmod "$mode" "$final_tmp" && mv -f "$final_tmp" "$dest"
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

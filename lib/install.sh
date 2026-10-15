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

# GeniusBar's "Install command-line tools" writes agent-bot and agent-comms
# into ~/.local/bin as small sh wrappers carrying a "# geniusbar-cli-tool"
# marker line, and adds a PATH line under the same marker to ~/.zprofile.
# Both belong to GeniusBar: managed-machine never replaces, links over, or
# parks a wrapper, and never drops the marked PATH line (qwts/managed-machine#179).
GENIUSBAR_CLI_TOOL_MARKER='# geniusbar-cli-tool'

# True when <file> is a GeniusBar command-line tool wrapper: a regular file
# (not a link), small enough to be a wrapper, with the marker line.
geniusbar_cli_tool() {
    local file="$1" size
    [[ -f "$file" && ! -L "$file" ]] || return 1
    size="$(wc -c <"$file" | tr -d '[:space:]')"
    [[ "$size" =~ ^[0-9]+$ ]] && (( size < 4096 )) || return 1
    grep -q "^${GENIUSBAR_CLI_TOOL_MARKER}" "$file"
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
    local file="$1" name="$2" body_tmp="" absorb_tmp="" out_tmp="" mode=""
    [[ -n "${name:-}" ]] || { echo "Error: ensure_zsh_block requires a block name" >&2; return 1; }
    shift 2

    mkdir -p "$(dirname "$file")"
    [[ -f "$file" ]] || : >"$file"

    body_tmp="$(mktemp)"
    absorb_tmp="$(mktemp)"
    out_tmp="$(mktemp "$(dirname "$file")/.zsh-block.XXXXXX")"
    # shellcheck disable=SC2064
    trap 'rm -f "$body_tmp" "$absorb_tmp" "$out_tmp"; trap - RETURN' RETURN
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --absorb-marker)
                if [[ -z "${2:-}" ]]; then
                    echo "Error: ensure_zsh_block: --absorb-marker requires a value" >&2
                    return 1
                fi
                printf '%s\n' "$2" >>"$absorb_tmp"
                shift 2
                ;;
            *)
                echo "Error: ensure_zsh_block: unknown option: $1" >&2
                return 1
                ;;
        esac
    done

    if command -v zsh-profile >/dev/null 2>&1; then
        local -a zp_args=(--file "$file" --name "$name")
        local marker=""
        while IFS= read -r marker; do
            [[ -n "$marker" ]] || continue
            zp_args+=(--absorb-marker "$marker")
        done <"$absorb_tmp"
        if [[ -s "$absorb_tmp" ]]; then
            while IFS= read -r marker; do
                [[ -n "$marker" ]] || continue
                zp_args+=(--absorb-marker "$marker")
            done < <(awk -v absorbfile="$absorb_tmp" '
                BEGIN {
                    nabsorb = 0
                    while ((getline l < absorbfile) > 0) absorb[nabsorb++] = l
                    in_block = ""; absorb_next = 0
                }
                {
                    if (in_block != "") {
                        if ($0 == in_block) in_block = ""
                        next
                    }
                    if ($0 ~ /^# BEGIN /) { in_block = "# END " substr($0, 9); absorb_next = 0; next }
                    if (absorb_next) {
                        absorb_next = 0
                        if ($0 ~ /^[ \t]*(export[ \t]+PATH=|path=\()/) print $0
                    }
                    for (i = 0; i < nabsorb; i++) {
                        if (absorb[i] != "" && index($0, absorb[i]) > 0 && $0 ~ /^[ \t]*#/) {
                            absorb_next = 1
                            break
                        }
                    }
                }
            ' "$file")
        fi
        zsh-profile ensure-block "${zp_args[@]}" >/dev/null
        return $?
    fi

    cat >"$body_tmp"
    if [[ ! -s "$body_tmp" ]]; then
        echo "Error: ensure_zsh_block received an empty body for block '$name'" >&2
        return 1
    fi
    if ! awk \
        -v begin="# BEGIN $name" \
        -v end="# END $name" \
        -v bodyfile="$body_tmp" \
        -v absorbfile="$absorb_tmp" '
        BEGIN {
            nbody = 0
            while ((getline l < bodyfile) > 0) body[nbody++] = l
            nabsorb = 0
            if (absorbfile != "") {
                while ((getline l < absorbfile) > 0) absorb[nabsorb++] = l
            }
            in_target = 0; in_other = ""; found = 0; emitted = 0; absorb_next_path = 0; n = 0
        }
        {
            if (in_target) {
                if ($0 == end) {
                    if (!emitted) {
                        out[n++] = begin
                        for (i = 0; i < nbody; i++) out[n++] = body[i]
                        out[n++] = end
                        emitted = 1
                    }
                    in_target = 0
                }
                next
            }
            if (in_other != "") {
                out[n++] = $0
                if ($0 == in_other) in_other = ""
                next
            }
            if ($0 == begin) { in_target = 1; found = 1; absorb_next_path = 0; next }
            if ($0 ~ /^# BEGIN /) { in_other = "# END " substr($0, 9); absorb_next_path = 0; out[n++] = $0; next }
            if (absorb_next_path) {
                absorb_next_path = 0
                if ($0 ~ /^[ \t]*(export[ \t]+PATH=|path=\()/) next
            }
            absorbed = 0
            for (i = 0; i < nabsorb; i++) {
                if (absorb[i] != "" && index($0, absorb[i]) > 0) { absorbed = 1; break }
            }
            if (absorbed) {
                if ($0 ~ /^[ \t]*#/) absorb_next_path = 1
                next
            }
            out[n++] = $0
        }
        END {
            if (in_target) exit 3
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
# otherwise. Absorbs vendor installer PATH lines (Muse, Antigravity) into the
# managed block. Prints a one-line note if ~/.bin still appears in ~/.zshrc
# outside the managed block.
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

    printf '%s\n' "$LOCAL_BIN_PATH_EXPORT" | ensure_zsh_block "$zshrc" "local-bin" \
        --absorb-marker 'added by muse installer' \
        --absorb-marker 'Added by Antigravity CLI installer'
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
# properly closed "# BEGIN <name>" … "# END <name>" range, preserving line
# multiplicity so callers can compare occurrence counts before and after an
# installer runs. A range must close with a marker of the same name before EOF;
# an unterminated BEGIN never suppresses the lines after it, so a vendor cannot
# hide a bare export behind an orphan marker. The first pass records closed
# ranges, the second prints each unguarded line.
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
            if (!inside) print $0
        }
    ' "$file" "$file"
}

# After a vendor installer runs, compare the startup-file snapshot against the
# current files and report every PATH line the installer made newly unguarded —
# naming the file and the line. An "unguarded PATH line" is any bare
# `export PATH=...` or `path=(...)` outside a properly closed "# BEGIN"/"# END"
# range: managed dirs exactly as zsh_unguarded_path_line defines them
# (refresh-actionable, fixed by `managed-machine setup zsh`), and foreign
# vendor dirs (alert-only, removed by hand). "Newly unguarded" means its
# unguarded occurrence count in the file is higher after the run than before:
# that covers additions, lines a vendor moves out from inside a guarded block,
# and duplicate appends on re-run, and it stays quiet when a pre-existing leak
# is merely re-run.
# carry-over statements (brew shellenv, cargo env) do not match the PATH
# predicate and are never reported. A change with no newly unguarded PATH line
# prints an ok note; no change prints nothing. The check is occurrence-aware,
# not a whole-file hash, so a shadowing edit elsewhere in the file cannot mask
# or fabricate an unguarded PATH add (managed-machine#158).
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
                && ! [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+PATH=|path=\() ]]; then
                continue
            fi
            if [[ "$(grep -Fxc -- "$line" <<<"$unguarded_after" || true)" -le \
                  "$(grep -Fxc -- "$line" <<<"$unguarded_before" || true)" ]]; then
                continue
            fi
            if zsh_unguarded_path_line "$line"; then
                refreshable=1
            else
                foreign=1
            fi
            printf 'warn: %s added unguarded line to %s: %s\n' "$label" "$file" "$line" >&2
        done < <(awk '!seen[$0]++' <<<"$unguarded_after")
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
    ensure_local_bin_in_zshrc "${HOME}/.zshrc"

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

# True when a line is an unguarded PATH prepend or path=(...) assignment that
# would duplicate a managed dir on every nested shell. Matches variable and
# literal home forms, multi-directory exports, and trailing comments; guarded
# case bodies never match because they do not start with export.
zsh_unguarded_path_line() {
    local line="$1"
    [[ "$line" == 'export PATH="${HOME}/.local/bin:${PATH}"' ]] && return 0
    [[ "$line" == 'export PATH="${CARGO_HOME:-${HOME}/.cargo}/bin:${PATH}"' ]] && return 0
    grep -qE '^[ \t]*export[ \t]+PATH=.*(/\.local/bin|/\.cargo/bin|/\.config/agent-bot/bin)(:|"|$|[ \t])' <<<"$line" && return 0
    grep -qE '^[ \t]*path=\(' <<<"$line" && return 0
    return 1
}

# Collapse blank-line runs (2+ consecutive blank lines -> 1) and strip leading
# and trailing blank lines in a zsh startup file. Delegates to `zsh-profile
# compact` when available on PATH; keeps an internal awk fallback with atomic
# same-directory temp + mv and mode preservation for bootstrap order.
compact_zsh_startup_file() {
    local file="$1" out_tmp="" mode=""
    [[ -f "$file" ]] || return 0

    if command -v zsh-profile >/dev/null 2>&1; then
        zsh-profile compact "$file" >/dev/null
        return $?
    fi

    out_tmp="$(mktemp "$(dirname "$file")/.zsh-compact.XXXXXX")"
    # shellcheck disable=SC2064
    trap 'rm -f "$out_tmp"; trap - RETURN' RETURN
    awk '
        { buf[n++] = $0 }
        END {
            m = 0
            for (i = 0; i < n; i++) {
                if (buf[i] ~ /^[ \t]*$/ && m > 0 && out[m-1] ~ /^[ \t]*$/) continue
                out[m++] = buf[i]
            }
            while (m > 0 && out[m-1] ~ /^[ \t]*$/) m--
            s = 0
            while (s < m && out[s] ~ /^[ \t]*$/) s++
            for (i = s; i < m; i++) print out[i]
        }
    ' "$file" >"$out_tmp"
    if cmp -s "$out_tmp" "$file"; then
        return 0
    fi
    mode="$(stat -c %a "$file" 2>/dev/null || stat -f %Lp "$file" 2>/dev/null)" || {
        echo "Error: cannot read mode of $file" >&2
        return 1
    }
    chmod "$mode" "$out_tmp" && mv -f "$out_tmp" "$file"
}

# Validate a zsh startup file against the zsh-profile lint contract (no
# unguarded export PATH= or path=(...) outside blocks, no broken/duplicate
# blocks, no 3+ blank runs or trailing blank lines). GeniusBar's marked PATH
# line in .zprofile (# geniusbar-cli-tool followed by export PATH=...) is
# owned by GeniusBar (#179) and exempted before delegating to `zsh-profile
# lint` or the identical built-in awk fallback.
lint_zsh_startup_file() {
    local file="$1" lint_tmp="" out="" rc=0
    [[ -f "$file" ]] || { echo "Error: file not found: $file" >&2; return 1; }

    if command -v zsh-profile >/dev/null 2>&1; then
        if grep -q "^${GENIUSBAR_CLI_TOOL_MARKER}" "$file"; then
            lint_tmp="$(mktemp)"
            # shellcheck disable=SC2064
            trap 'rm -f "$lint_tmp"; trap - RETURN' RETURN
            awk -v m="$GENIUSBAR_CLI_TOOL_MARKER" '
                prev_geniusbar && /^[ \t]*export[ \t]+PATH=/ {
                    print "# geniusbar-cli-tool export"
                    prev_geniusbar = 0
                    next
                }
                {
                    prev_geniusbar = (index($0, m) == 1)
                    print
                }
            ' "$file" >"$lint_tmp"
            out="$(zsh-profile lint "$lint_tmp" 2>&1)" || rc=$?
            if [[ -n "$out" ]]; then
                printf '%s\n' "${out//"$lint_tmp:"/"$file:"}"
            fi
            return "$rc"
        fi
        zsh-profile lint "$file"
        return $?
    fi

    awk -v file="$file" -v m="$GENIUSBAR_CLI_TOOL_MARKER" '
        BEGIN { open = ""; run = 0; findings = 0; prev_geniusbar = 0 }
        {
            is_geniusbar_export = (open == "" && prev_geniusbar && $0 ~ /^[ \t]*export[ \t]+PATH=/)
            prev_geniusbar = (index($0, m) == 1)
            if ($0 ~ /^# BEGIN /) {
                if (open != "") { printf "%s:%d: nested BEGIN inside an open block\n", file, NR; findings++ }
                else { open = $0; openline = NR }
                if (seen[$0]) { printf "%s:%d: duplicate block: %s\n", file, NR, $0; findings++ }
                seen[$0] = 1
            } else if ($0 ~ /^# END /) {
                if (open == "") { printf "%s:%d: END without BEGIN\n", file, NR; findings++ }
                else if ($0 != "# END " substr(open, 9)) {
                    printf "%s:%d: %s does not close %s\n", file, NR, $0, open; findings++
                }
                open = ""
            } else if (open == "" && !is_geniusbar_export && $0 ~ /^[ \t]*export[ \t]+PATH=/) {
                printf "%s:%d: unguarded PATH export: %s\n", file, NR, $0; findings++
            } else if (open == "" && $0 ~ /^[ \t]*path=\(/) {
                printf "%s:%d: unguarded path assignment: %s\n", file, NR, $0; findings++
            }
            if ($0 ~ /^[ \t]*$/) {
                run++
                if (run == 3) { printf "%s:%d: blank run (3+ consecutive lines)\n", file, NR - 2; findings++ }
            } else {
                run = 0
            }
        }
        END {
            if (open != "") { printf "%s:%d: unterminated block at EOF: %s\n", file, openline, open; findings++ }
            if (run > 0) { printf "%s:%d: trailing blank line(s) at EOF\n", file, NR - run + 1; findings++ }
            exit (findings > 0)
        }
    ' "$file"
}

# True when a zsh startup file still has unguarded PATH prepends, path=(...)
# assignments, broken blocks, or unguarded managed blocks that require a
# backup-and-refresh from the template. Shares its unguarded definition with
# lint_zsh_startup_file (`zsh-profile lint`); pure blank-line findings are
# collapsed in place by compact_zsh_startup_file without replacing user lines.
zsh_profile_needs_refresh() {
    local file="$1" findings=""
    [[ -f "$file" ]] || return 1
    findings="$(lint_zsh_startup_file "$file" 2>&1 || true)"
    if grep -qvE '(^$|: (blank run \(3\+ consecutive lines\)|trailing blank line\(s\) at EOF)$)' <<<"$findings"; then
        return 0
    fi
    case "$(basename "$file")" in
        .zshrc)
            if grep -qxF 'export PATH="${HOME}/.local/bin:${PATH}"' "$file" \
                || grep -qxF 'export PATH="${CARGO_HOME:-${HOME}/.cargo}/bin:${PATH}"' "$file"; then
                return 0
            fi
            if grep -qxF '# BEGIN nvm' "$file" && ! grep -qF 'command -v node' "$file"; then
                return 0
            fi
            ;;
        .zprofile)
            zsh_unguarded_contents "$file" | grep -vE '^[ \t]*#' | grep -qE 'brew shellenv' && return 0
            ;;
        .zshenv)
            zsh_unguarded_contents "$file" | grep -vE '^[ \t]*#' | grep -qE '\.cargo/env' && return 0
            ;;
    esac
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
            carry_zsh_lines_into_block "$bak" "$dest" brew-shellenv 'brew shellenv' || return 1
            ;;
        .zshenv)
            carry_zsh_lines_into_block "$bak" "$dest" cargo-env '\.cargo/env' || return 1
            ;;
    esac
    preserve_managed_blocks "$bak" "$dest"
    preserve_geniusbar_path_block "$bak" "$dest"
}

# Carry GeniusBar's marked PATH lines (the marker line and the line after it)
# from a backup into its rewritten file, unless the file already has them, so
# GeniusBar still finds its block and its uninstall can remove it.
preserve_geniusbar_path_block() {
    local bak="$1" dest="$2" block
    block="$(awk -v m="$GENIUSBAR_CLI_TOOL_MARKER" \
        'index($0, m) == 1 { marker = $0; if ((getline next_line) > 0 && next_line ~ /^[ \t]*export[ \t]+PATH=/) { print marker; print next_line } }' "$bak")"
    [[ -n "$block" ]] || return 0
    if grep -qF -- "$(head -n 1 <<<"$block")" "$dest"; then
        return 0
    fi
    local tmp mode
    tmp="$(mktemp "$(dirname "$dest")/.zsh-preserve.XXXXXX")"
    # shellcheck disable=SC2064
    trap 'rm -f "$tmp"; trap - RETURN' RETURN
    { cat "$dest"; printf '\n%s\n' "$block"; } >"$tmp"
    mode="$(stat -c %a "$dest" 2>/dev/null || stat -f %Lp "$dest" 2>/dev/null)" || {
        rm -f "$tmp"
        echo "Error: cannot read mode of $dest" >&2
        return 1
    }
    chmod "$mode" "$tmp" && mv -f "$tmp" "$dest"
}

# carry_zsh_lines_into_block <bak> <dest> <block-name> <regex>: move the
# backup's loose lines matching <regex> into a named block in <dest>.
# Comment lines never match (the template's own "brew shellenv" comment
# used to be copied on every refresh), lines inside a BEGIN/END range are
# carried with their block instead, repeats collapse, and a line <dest>
# already has is left out (#169).
carry_zsh_lines_into_block() {
    local bak="$1" dest="$2" name="$3" regex="$4" lines="" line
    while IFS= read -r line; do
        [[ -n "$line" ]] || continue
        grep -qxF -- "$line" "$dest" || lines+="$line"$'\n'
    done < <(awk -v re="$regex" '
        /^# BEGIN / { inblock = 1; next }
        inblock { if (/^# END /) inblock = 0; next }
        /^[ \t]*#/ { next }
        $0 ~ re && !seen[$0]++ { print }
    ' "$bak")
    [[ -n "$lines" ]] || return 0
    printf '%s' "$lines" | ensure_zsh_block "$dest" "$name"
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
    names="$(awk '/^# BEGIN /{ b = substr($0, 9); open = b; next } open != "" && $0 == ("# END " open) { if (!seen[open]++) print open; open = "" }' "$bak")"
    [[ -n "$names" ]] || return 0
    add_tmp="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$add_tmp"; trap - RETURN' RETURN
    first=1
    while IFS= read -r name; do
        [[ -n "$name" ]] || continue
        if grep -qxF "# BEGIN $name" "$dest" || { [[ -f "$add_tmp" ]] && grep -qxF "# BEGIN $name" "$add_tmp"; }; then
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

managed_machine_has_json_flag() {
    local arg
    for arg in "$@"; do
        [[ "$arg" == "--json" ]] && return 0
    done
    return 1
}

# Emit a structured JSON error envelope matching the account-readiness schema
# (schema_version=1, command, ready=false, status, error, checks[0]).
# Usage: managed_machine_json_failure <command> <code> <message> <action> [status] [evidence_json]
managed_machine_json_failure() {
    local command="$1" code="$2" message="$3" action="$4" status="${5:-failed}" evidence_json="${6:-{}}"
    python3 - "$command" "$code" "$message" "$action" "$status" "$evidence_json" <<'PY'
import json, os, sys
command, code, message, action, status, evidence_raw = sys.argv[1:]
try:
    evidence = json.loads(evidence_raw) if evidence_raw else {}
except Exception:
    evidence = {}
check_id = command.replace("-", ".") + ".preflight"
top_status = "pending_user_action" if status == "pending_user_action" else "not_ready"
check_status = "pending_user_action" if status == "pending_user_action" else "failed"
entry = {
    "id": check_id,
    "status": check_status,
    "code": code,
    "message": message,
    "action": action,
    "evidence": evidence,
}
report = {
    "schema_version": 1,
    "command": command,
    "ready": False,
    "status": top_status,
    "error": {
        "code": code,
        "message": message,
        "action": action,
        "evidence": evidence,
    },
    "checks": [entry],
}
try:
    print(json.dumps(report, indent=2))
    sys.stdout.flush()
except BrokenPipeError:
    devnull = os.open(os.devnull, os.O_WRONLY)
    os.dup2(devnull, sys.stdout.fileno())
    sys.exit(0)
PY
    [[ "$status" != pending_user_action ]] || return 75
    return 1
}


#!/bin/sh
# Verifying, non-elevating, profile-free installer for the managed-machine Go
# core on Linux (linux/amd64, linux/arm64; S6 R04, decision D4).
#
# Fetches SHA256SUMS from MANAGED_MACHINE_RELEASE_BASE (default:
# https://github.com/qwts/managed-machine/releases/latest/download, or any
# https:// URL or absolute directory), selects the _<os>_<arch>.tar.gz archive
# for the host, verifies its SHA-256 digest before extracting anything, and
# installs managed-machine atomically into --prefix (default: $HOME/.local/bin).
#
# Exits:
#   0  installed or already identical
#   1  download, digest or extraction failure (one Error: line, nothing left behind)
#   2  usage or unsupported host
set -eu

usage() {
    cat <<'EOF'
Usage: install-core.sh [--prefix <dir>] [--release-base <url-or-dir>]

Install the prebuilt managed-machine Go core binary on Linux (linux/amd64,
linux/arm64) after verifying SHA256SUMS.

Options:
  --prefix <dir>              Installation directory (default: $HOME/.local/bin)
  --release-base <url-or-dir> Release base URL (https://...) or absolute directory
                              (default: $MANAGED_MACHINE_RELEASE_BASE or
                              https://github.com/qwts/managed-machine/releases/latest/download)
  -h, --help                  Show this help and exit
EOF
}

die() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

die_usage() {
    printf 'Error: %s\n' "$*" >&2
    exit 2
}

prefix=""
prefix_set=0
release_base=""
release_base_set=0

while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)
            usage
            exit 0
            ;;
        --prefix)
            [ $# -ge 2 ] || die_usage "--prefix requires a directory argument"
            prefix="$2"
            prefix_set=1
            shift 2
            ;;
        --prefix=*)
            prefix="${1#--prefix=}"
            prefix_set=1
            shift
            ;;
        --release-base)
            [ $# -ge 2 ] || die_usage "--release-base requires a URL or directory argument"
            release_base="$2"
            release_base_set=1
            shift 2
            ;;
        --release-base=*)
            release_base="${1#--release-base=}"
            release_base_set=1
            shift
            ;;
        *)
            die_usage "unknown argument: $1"
            ;;
    esac
done

os_raw="$(uname -s 2>/dev/null || true)"
arch_raw="$(uname -m 2>/dev/null || true)"
os_lower="$(printf '%s' "$os_raw" | tr '[:upper:]' '[:lower:]')"
arch_lower="$(printf '%s' "$arch_raw" | tr '[:upper:]' '[:lower:]')"

case "$os_lower" in
    darwin)
        die_usage "macOS is supported through the Homebrew formula (qwts/managed-machine, Formula/managed-machine.rb); install-core.sh is for Linux targets only"
        ;;
    linux)
        target_os="linux"
        ;;
    *)
        die_usage "unsupported operating system '${os_raw:-unknown}'; install-core.sh supports Linux (linux/amd64, linux/arm64) only"
        ;;
esac

case "$arch_lower" in
    x86_64|amd64|x64)
        target_arch="amd64"
        ;;
    aarch64|arm64)
        target_arch="arm64"
        ;;
    *)
        die_usage "unsupported architecture '${arch_raw:-unknown}' on ${target_os}; supported: linux/amd64, linux/arm64"
        ;;
esac

if [ "$prefix_set" -eq 0 ]; then
    [ -n "${HOME:-}" ] || die "HOME is not set and --prefix was not provided"
    case "$HOME" in
        /*) ;;
        *) die "HOME must be an absolute path (got '$HOME')" ;;
    esac
    prefix="${HOME%/}/.local/bin"
fi

[ -n "$prefix" ] || die_usage "--prefix must not be empty"
case "$prefix" in
    /*) ;;
    *) die_usage "--prefix must be an absolute path (got '$prefix')" ;;
esac
if [ "$prefix" != "/" ]; then
    prefix="${prefix%/}"
fi

if [ "$release_base_set" -eq 0 ]; then
    if [ -n "${MANAGED_MACHINE_RELEASE_BASE+x}" ]; then
        release_base="$MANAGED_MACHINE_RELEASE_BASE"
    else
        release_base="https://github.com/qwts/managed-machine/releases/latest/download"
    fi
fi

[ -n "$release_base" ] || die_usage "release base must not be empty"
case "$release_base" in
    https://?*)
        base_kind="https"
        release_base="${release_base%/}"
        ;;
    http://*)
        die_usage "plain http release base is not allowed; use https:// or an absolute directory path (got '$release_base')"
        ;;
    /*)
        base_kind="dir"
        if [ "$release_base" != "/" ]; then
            release_base="${release_base%/}"
        fi
        ;;
    *)
        die_usage "release base must be an https:// URL or an absolute directory path (got '$release_base')"
        ;;
esac

compute_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" 2>/dev/null | awk '{print tolower($1)}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" 2>/dev/null | awk '{print tolower($1)}'
    else
        return 1
    fi
}

if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    die "neither sha256sum nor shasum is available to verify SHA-256 digests"
fi

tmpdir_base="${TMPDIR:-/tmp}"
workdir="$(mktemp -d "${tmpdir_base%/}/managed-machine-install-core.XXXXXX" 2>/dev/null)" \
    || die "failed to create temporary directory under $tmpdir_base"
stage_bin=""
cleanup() {
    if [ -n "$stage_bin" ] && [ -e "$stage_bin" ]; then
        rm -f "$stage_bin" 2>/dev/null || true
    fi
    if [ -n "${workdir:-}" ] && [ -d "$workdir" ]; then
        rm -rf "$workdir" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM HUP

fetch_resource() {
    src_name="$1"
    dest_path="$2"
    if [ "$base_kind" = "dir" ]; then
        src_path="$release_base/$src_name"
        [ -f "$src_path" ] || die "missing $src_name in $release_base"
        cp "$src_path" "$dest_path" 2>/dev/null || die "failed to read $src_path"
    else
        command -v curl >/dev/null 2>&1 || die "curl is required for https downloads"
        url="$release_base/$src_name"
        if [ -n "${SSL_CERT_FILE:-}" ]; then
            curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 --cacert "$SSL_CERT_FILE" -o "$dest_path" "$url" 2>/dev/null \
                || die "failed to download $url"
        else
            curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 -o "$dest_path" "$url" 2>/dev/null \
                || die "failed to download $url"
        fi
    fi
}

sums_file="$workdir/SHA256SUMS"
fetch_resource "SHA256SUMS" "$sums_file"

suffix="_${target_os}_${target_arch}.tar.gz"
match_line="$(awk -v sfx="$suffix" '
    NF == 2 && length($1) == 64 && $1 ~ /^[0-9A-Fa-f]+$/ && index($2, "managed-machine_") == 1 && substr($2, length($2) - length(sfx) + 1) == sfx && $2 !~ /\// && $2 !~ /\.\./ {
        print tolower($1) " " $2
    }
' "$sums_file")"

[ -n "$match_line" ] || die "SHA256SUMS has no valid entry ending in $suffix"
match_count="$(printf '%s\n' "$match_line" | awk 'END { print NR }')"
[ "$match_count" -eq 1 ] || die "SHA256SUMS has multiple entries ending in $suffix"

expected_sha="${match_line%% *}"
archive_name="${match_line#* }"

archive_file="$workdir/$archive_name"
fetch_resource "$archive_name" "$archive_file"

actual_sha="$(compute_sha256 "$archive_file" || true)"
[ -n "$actual_sha" ] || die "failed to compute SHA-256 digest of $archive_name"
if [ "$actual_sha" != "$expected_sha" ]; then
    die "SHA-256 digest mismatch for $archive_name: expected $expected_sha, got $actual_sha"
fi

command -v tar >/dev/null 2>&1 || die "tar is required to extract $archive_name"
extract_dir="$workdir/extract"
mkdir -p "$extract_dir" 2>/dev/null || die "failed to create extraction directory"
tar -xzf "$archive_file" -C "$extract_dir" managed-machine 2>/dev/null \
    || die "failed to extract managed-machine from $archive_name"

extracted_bin="$extract_dir/managed-machine"
if [ ! -f "$extracted_bin" ] || [ -L "$extracted_bin" ]; then
    die "archive $archive_name did not contain a regular managed-machine binary"
fi
chmod 0755 "$extracted_bin" 2>/dev/null || die "failed to set permissions on extracted binary"

bin_sha="$(compute_sha256 "$extracted_bin" || true)"
[ -n "$bin_sha" ] || die "failed to compute SHA-256 digest of extracted binary"

dest_bin="$prefix/managed-machine"
if [ -f "$dest_bin" ] && [ ! -L "$dest_bin" ]; then
    existing_sha="$(compute_sha256 "$dest_bin" || true)"
    if [ "$existing_sha" = "$bin_sha" ]; then
        printf 'managed-machine is already up to date at %s (sha256:%s)\n' "$dest_bin" "$bin_sha"
        printf 'Ensure %s is on your PATH:\n  export PATH="%s:$PATH"\n' "$prefix" "$prefix"
        exit 0
    fi
fi

mkdir -p "$prefix" 2>/dev/null || die "failed to create prefix directory $prefix"
stage_bin="$(mktemp "$prefix/.managed-machine.tmp.XXXXXX" 2>/dev/null)" \
    || die "failed to create staging file in $prefix"
cp "$extracted_bin" "$stage_bin" 2>/dev/null || die "failed to write staging file in $prefix"
chmod 0755 "$stage_bin" 2>/dev/null || die "failed to chmod staging file in $prefix"
mv -f "$stage_bin" "$dest_bin" 2>/dev/null || die "failed to move binary into $dest_bin"
stage_bin=""

printf 'Installed managed-machine to %s (sha256:%s)\n' "$dest_bin" "$bin_sha"
printf 'Ensure %s is on your PATH:\n  export PATH="%s:$PATH"\n' "$prefix" "$prefix"

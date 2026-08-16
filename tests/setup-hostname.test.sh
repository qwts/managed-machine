#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_HOME"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/hostname.sh
source "$ROOT/lib/hostname.sh"

hostname_is_valid MacbookPro16M2
hostname_is_valid MacminiM2
if hostname_is_valid 'Chris Mac'; then
    echo 'expected space to be invalid' >&2
    exit 1
fi
if hostname_is_valid '-leading'; then
    echo 'expected leading hyphen to be invalid' >&2
    exit 1
fi
if hostname_is_valid ChrissMacbookPro; then
    : # historically recorded names are still syntactically valid
else
    echo 'expected ChrissMacbookPro to be syntactically valid' >&2
    exit 1
fi

HOME="$TEST_HOME" MANAGED_MACHINE_HOSTNAME=MacbookProM5Max \
    name="$(prompt_hostname ignored)"
[[ "$name" == 'MacbookProM5Max' ]]

if HOME="$TEST_HOME" MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive \
    /bin/bash "$ROOT/setup-hostname" >"$TEST_DIR/nonint.out" 2>&1; then
    echo 'expected noninteractive hostname setup to skip' >&2
    exit 1
fi
grep -Fq 'Skipped:' "$TEST_DIR/nonint.out"
! grep -Fq 'managed-machine setup hostname' "$TEST_DIR/nonint.out"

echo 'hostname tests passed'

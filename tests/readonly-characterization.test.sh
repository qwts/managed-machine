#!/usr/bin/env bash
# F01 characterization fixtures (#243): byte-exact goldens for the read-only
# command surface that bin/managed-machine matches before it sources lib/ or
# refreshes the config checkout (ENG-0055): --version, skill path, the JSON
# and plain usage errors, unknown commands and help. Every probe runs against
# a copied dispatcher in a throwaway tree with an empty HOME, an empty
# environment and a stub lib/install.sh that only records being sourced, so
# the test is offline and cannot touch this machine's installation.
#
# Run: bash tests/readonly-characterization.test.sh
# Regenerate goldens after a reviewed contract change: ... --record
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GOLDEN="$ROOT/tests/fixtures/readonly-characterization"
RECORD=0
[[ "${1:-}" == "--record" ]] && RECORD=1

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT
FIX="$TEST_ROOT/fixture"
EMPTY_HOME="$TEST_ROOT/home"
SOURCED_LOG="$TEST_ROOT/sourced.log"
mkdir -p "$FIX/bin" "$FIX/lib" "$FIX/skills/managed-machine" "$EMPTY_HOME"
cp "$ROOT/legacy/bin/managed-machine" "$FIX/bin/managed-machine"
chmod +x "$FIX/bin/managed-machine"
# resolve_root needs lib/install.sh to exist. The stub records being sourced,
# and records any config refresh or install probe, which help must never do
# (qwts/managed-machine#173). One executable setup script gives help a
# deterministic name to list.
cat >"$FIX/lib/install.sh" <<'STUB'
printf 'install.sh sourced\n' >>"$MM_SOURCED_LOG"
managed_machine_config_repo_dir() { printf 'config dir resolved\n' >>"$MM_REFRESH_LOG"; printf '%s\n' "$REPO_ROOT/no-config"; }
refresh_managed_machine_config_repo() { printf 'config refresh\n' >>"$MM_REFRESH_LOG"; }
setup_name_installed() { printf 'install probe %s\n' "$2" >>"$MM_REFRESH_LOG"; return 1; }
STUB
printf '#!/usr/bin/env bash\nexit 0\n' >"$FIX/setup-alpha"
chmod +x "$FIX/setup-alpha"
REFRESH_LOG="$TEST_ROOT/refresh.log"
printf '0.0.0-fixture\n' >"$FIX/VERSION"
printf '0123456789abcdef0123456789abcdef01234567\n' >"$FIX/RELEASE_COMMIT"
printf '# fixture skill\n' >"$FIX/skills/managed-machine/SKILL.md"

PY_DIR="$(dirname "$(command -v python3)")"
MIN_PATH="$PY_DIR:/usr/bin:/bin"

fail() { echo "FAIL: $1" >&2; exit 1; }
failures=0

# run_case <mode> <name> <args...>; mode "pre-lib" asserts the route never
# sourced lib/ (ENG-0055), mode "help" allows sourcing but asserts no config
# refresh and no install probe ran.
run_case() {
    local mode="$1" name="$2"; shift 2
    local out="$TEST_ROOT/$name.out" err="$TEST_ROOT/$name.err" code=0
    : >"$SOURCED_LOG"; : >"$REFRESH_LOG"
    set +e
    env -i HOME="$EMPTY_HOME" PATH="$MIN_PATH" LANG=C.UTF-8 LC_ALL=C.UTF-8 \
        MM_SOURCED_LOG="$SOURCED_LOG" MM_REFRESH_LOG="$REFRESH_LOG" \
        bash "$FIX/bin/managed-machine" "$@" >"$out" 2>"$err"
    code=$?
    set -e
    if [[ "$mode" == pre-lib ]]; then
        [[ ! -s "$SOURCED_LOG" ]] || fail "$name: lib/install.sh was sourced on an ENG-0055 route"
    fi
    [[ ! -s "$REFRESH_LOG" ]] || fail "$name: help path refreshed config or probed installs: $(tr '\n' ';' <"$REFRESH_LOG")"
    local actual="$TEST_ROOT/$name.actual"
    {
        printf 'exit %s\n--- stdout\n' "$code"
        sed "s|$FIX|<ROOT>|g" "$out"
        printf -- '--- stderr\n'
        sed "s|$FIX|<ROOT>|g" "$err"
    } >"$actual"
    if [[ "$RECORD" == 1 ]]; then
        cp "$actual" "$GOLDEN/$name.golden"
        echo "recorded $name"
        return
    fi
    if ! diff -u "$GOLDEN/$name.golden" "$actual"; then
        echo "FAIL: $name differs from golden" >&2
        failures=$((failures + 1))
    fi
}

run_case help update-extra-json --update extra --json

[[ "$failures" == 0 ]] || fail "$failures golden case(s) differ"
if [[ "$RECORD" == 1 ]]; then echo "read-only characterization: goldens recorded"; else echo "read-only characterization: 1 cases match goldens"; fi

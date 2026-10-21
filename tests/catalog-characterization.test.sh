#!/usr/bin/env bash
# F01 characterization goldens for `managed-machine catalog list/resolve` and `setup --list`
# (#243, draft harness for review). The real dispatcher, scripts/catalog and
# lib/ are copied into a throwaway tree; only the host seams are overridden by
# appending stubs to the fixture copies: lib/apps.sh (sourced last by
# scripts/catalog) carries the config checkout location, refresh, install
# probes, declined manifest and provenance, and lib/agent-account.sh carries
# the four directory-service lookups, so the real dscl/dsmemberutil is never
# consulted. Everything else is the production code path, run with an empty
# HOME and environment against a fixture apps.json.
#
# Recorded values are normalized out of the goldens with literal (never
# regular-expression) replacements, and a leak assertion rejects any output
# that still carries a value of the machine that produced it: the invoking
# account name, the real home directory, the test root or the machine
# architecture. The assertion runs before a golden is compared and before
# `--record` writes one.
set -euo pipefail
# Git isolation for the harness's own git calls. The routes are launched with
# `env -i`, which drops the exported environment, so the child gets the same
# two variables inside the `env -i` argument list below.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GOLDEN="$ROOT/tests/fixtures/catalog-characterization"
RECORD=0
[[ "${1:-}" == "--record" ]] && RECORD=1

# The invoking account name is a fixture input: lib/agent-account.sh reports
# it in the ssh-status route, so it is normalized out of the goldens.
# lib/elevate.sh (line 270) additionally treats any '*-*-agent' login as an
# agent session, which flips agent_context/eligible in that route - and
# `env -i` cannot hide the uid, so the harness cannot stub the account away.
# Fail fast instead of recording a golden that only reproduces on a human
# account.
CURRENT_USER="$(/usr/bin/id -un)"
case "$CURRENT_USER" in
    *-*-agent)
        echo "FAIL: refusing to run as '$CURRENT_USER': lib/elevate.sh treats a '*-*-agent' login as an agent session, so the ssh-status golden would not match a human account's. Run this harness as the human owner." >&2
        exit 1
        ;;
esac

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT
FIX="$TEST_ROOT/fixture"
# Sibling layout matches the formula install (libexec/managed-machine-config), which
# lib/catalog.sh resolves through REPO_ROOT/../managed-machine-config.
CONFIG="$TEST_ROOT/managed-machine-config"
EMPTY_HOME="$TEST_ROOT/home"
PROBE_LOG="$TEST_ROOT/probe.log"
# The directory-service seam is asked about the invoking account, so its probe
# lines carry the account name; they go to their own log instead of the
# recorded probes section, which keeps every catalog golden byte-identical.
AGENT_PROBE_LOG="$TEST_ROOT/agent-probe.log"
SHARED="$TEST_ROOT/shared"
mkdir -p "$FIX/bin" "$FIX/scripts" "$CONFIG" "$EMPTY_HOME" "$SHARED/pictures"
cp "$ROOT/bin/managed-machine" "$FIX/bin/"
cp "$ROOT/scripts/catalog" "$ROOT/scripts/status" "$ROOT/scripts/fleet" "$ROOT/scripts/ssh" "$ROOT/scripts/account" "$ROOT/scripts/adopt" "$FIX/scripts/"
cp -R "$ROOT/lib" "$FIX/lib"
printf '0.0.0-fixture\n' >"$FIX/VERSION"
# One wrapper setup script and one standalone script, so both listing kinds appear.
printf '#!/usr/bin/env bash\ninstall_catalog_app grok-build "$@"\n' >"$FIX/setup-grok-build"
printf '#!/usr/bin/env bash\nexit 0\n' >"$FIX/setup-alpha"
chmod +x "$FIX/setup-grok-build" "$FIX/setup-alpha"
cat >"$CONFIG/apps.json" <<'JSON'
{
  "schema_version": 1,
  "apps": [
    {"name": "vscode", "kind": "signed-cask", "token": "visual-studio-code", "app_name": "Visual Studio Code.app", "command": "code", "team_id": "FIXTURETEAM", "aliases": ["code"]},
    {"name": "grok-build", "kind": "official-cli", "command": "grok", "aliases": ["grok"], "url": "https://example.invalid/install.sh"},
    {"name": "optional-app", "kind": "brew-formula", "formula": "minikube", "auto": false}
  ]
}
JSON
printf 'grok-build\t2026-10-15T00:00:00Z\tauthorization declined\n' >"$CONFIG/declined.manifest"
# print_setup_names (setup --list) resolves and refreshes the config checkout
# right after sourcing lib/install.sh, before lib/apps.sh is sourced, so the
# checkout seams must be overridden in the fixture copy of lib/config-repo.sh
# (sourced by install.sh) as well as in lib/apps.sh.
cat >>"$FIX/lib/config-repo.sh" <<'STUB'

# --- characterization stubs (test tree only) --------------------------------
existing_managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
refresh_managed_machine_config_repo() { printf 'refresh %s\n' "$1" >>"$MM_PROBE_LOG"; }
config_repo_provenance_json() { printf '{"fixture":true}\n'; }
STUB
cat >>"$FIX/lib/apps.sh" <<'STUB'

# --- characterization stubs (test tree only) --------------------------------
existing_managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
managed_machine_config_repo_dir() { printf '%s\n' "$MM_FIXTURE_CONFIG"; }
refresh_managed_machine_config_repo() { printf 'refresh %s\n' "$1" >>"$MM_PROBE_LOG"; }
setup_list_prime() { :; }
declined_apps_manifest_file() { printf '%s\n' "$MM_FIXTURE_CONFIG/declined.manifest"; }
config_repo_provenance_json() { printf '{"fixture":true}\n'; }
catalog_app_installed() { printf 'probe app %s\n' "$1" >>"$MM_PROBE_LOG"; [[ "$1" == "vscode" ]]; }
setup_name_installed() { printf 'probe script %s\n' "$2" >>"$MM_PROBE_LOG"; [[ "$2" == "alpha" ]]; }
STUB
# Directory-service lookups (dscl, dsmemberutil) answer from the fixture
# roster instead of the recording machine, the way the host-state harness
# stubs the same four seams: the real agent_current_context would otherwise
# ask dsmemberutil whether the invoking account is in the agents group, and
# its verdict would depend on the machine that recorded the golden.
#
# The probe lines go to the dedicated directory log (not the recorded probes
# section) because this seam is asked about the invoking account: recording
# it would bake the account name into a catalog golden.
cat >>"$FIX/lib/agent-account.sh" <<'STUB'

# --- characterization stubs (test tree only) --------------------------------
agent_account_exists() { printf 'probe directory exists %s\n' "$1" >>"$MM_AGENT_PROBE_LOG"; [[ "$1" == "fixture-agent" ]]; }
agent_account_in_group() { printf 'probe directory group %s %s\n' "$1" "$2" >>"$MM_AGENT_PROBE_LOG"; [[ "$1" == "fixture-agent" && "$2" == "agents" ]]; }
agent_account_home() { printf 'probe directory home %s\n' "$1" >>"$MM_AGENT_PROBE_LOG"; [[ "$1" == "fixture-agent" ]] && printf '%s\n' "$MM_FIXTURE_HOME"; }
agent_account_picture() { printf 'probe directory picture %s\n' "$1" >>"$MM_AGENT_PROBE_LOG"; [[ "$1" == "fixture-agent" ]] && printf '%s/pictures/fixture-agent.png\n' "$MM_FIXTURE_SHARED"; }
STUB

# ssh status reports the invoking OS account by name; normalize it so the golden is machine-independent.
# Only python3 joins the system PATH: the directory that holds it (often the
# Homebrew prefix) would otherwise expose brew, gh and node to the routes.
STUB_BIN="$TEST_ROOT/bin"
mkdir -p "$STUB_BIN"
ln -s "$(command -v python3)" "$STUB_BIN/python3"
MIN_PATH="$STUB_BIN:/usr/bin:/bin"
REAL_HOME="$HOME"
ARCH="$(/usr/bin/uname -m)"
fail() { echo "FAIL: $1" >&2; exit 1; }
failures=0
cases=0

# --- literal normalization -----------------------------------------------------
# Every value is replaced literally: the scan walks each line from left to
# right and, at each position, takes the longest literal that matches. No
# regular expression is ever built from the replaced text, so '+', '.', '/' or
# any other character in an account name, a path or a fingerprint is matched as
# itself. A replacement is emitted and the scan continues after it, so a
# replacement can never be rewritten by a later rule and *every* occurrence is
# replaced - the old sed pattern consumed its trailing delimiter and left the
# second occurrence in place ("a user user b" became "a <USER> user b").
#
# The account name is replaced as a whole word (the same word-character class
# the leak assertion uses) so a short name such as "user" cannot rewrite text
# like pending_user_action.
NORMALIZE_AWK='
function is_word_char(c) { return c != "" && c ~ /^[A-Za-z0-9_.-]$/ }
BEGIN {
    n = split(ENVIRON["MM_NORM_NEEDLES"], needles, "\001")
    split(ENVIRON["MM_NORM_REPLACEMENTS"], replacements, "\001")
    split(ENVIRON["MM_NORM_WHOLE"], whole, "\001")
    for (i = 1; i <= n; i++) order[i] = i
    for (i = 2; i <= n; i++) {                 # longest literal first
        k = order[i]; j = i - 1
        while (j >= 1 && length(needles[order[j]]) < length(needles[k])) { order[j + 1] = order[j]; j-- }
        order[j + 1] = k
    }
}
{
    line = $0; out = ""; pos = 1; len = length(line)
    while (pos <= len) {
        hit = 0
        for (k = 1; k <= n; k++) {
            i = order[k]; needle = needles[i]
            if (needle == "") continue
            if (index(substr(line, pos), needle) != 1) continue
            if (whole[i] == "1") {
                prev = (pos > 1) ? substr(line, pos - 1, 1) : ""
                nextc = substr(line, pos + length(needle), 1)
                if (is_word_char(prev) || is_word_char(nextc)) continue
            }
            out = out replacements[i]
            pos = pos + length(needle)
            hit = 1
            break
        }
        if (!hit) { out = out substr(line, pos, 1); pos++ }
    }
    print out
}
'
NORM_NEEDLES=""
NORM_REPLACEMENTS=""
NORM_WHOLE=""
add_normalization() { # <literal> <replacement> <whole-word: 0|1>
    local sep=""
    [[ -z "$NORM_NEEDLES" ]] || sep=$'\001'
    NORM_NEEDLES="${NORM_NEEDLES}${sep}$1"
    NORM_REPLACEMENTS="${NORM_REPLACEMENTS}${sep}$2"
    NORM_WHOLE="${NORM_WHOLE}${sep}$3"
}
add_normalization "$TEST_ROOT" "<TMP>" 0
add_normalization "$CONFIG" "<CONFIG>" 0
add_normalization "$FIX" "<ROOT>" 0
add_normalization "$EMPTY_HOME" "<HOME>" 0
add_normalization "$REAL_HOME" "<REAL_HOME>" 0
add_normalization "$CURRENT_USER" "<USER>" 1

normalize() {
    MM_NORM_NEEDLES="$NORM_NEEDLES" MM_NORM_REPLACEMENTS="$NORM_REPLACEMENTS" \
        MM_NORM_WHOLE="$NORM_WHOLE" awk "$NORMALIZE_AWK"
}

# --- leak assertion -----------------------------------------------------------
# The normalized output must not carry any value of the recording machine: the
# account name (as a whole word), the real home directory, the test root or
# the machine architecture. These routes have no enrolled key, so there is no
# fingerprint to leak. The assertion is the gate for both paths: a golden is
# compared only after it passes, and --record refuses to write a golden that
# fails it.
LEAK_CHECK_AWK='
function is_word_char(c) { return c != "" && c ~ /^[A-Za-z0-9_.-]$/ }
function has_whole_word(text, needle,   p, rest, prev, nextc) {
    if (needle == "") return 0
    p = index(text, needle)
    while (p > 0) {
        prev = (p > 1) ? substr(text, p - 1, 1) : ""
        nextc = substr(text, p + length(needle), 1)
        if (!is_word_char(prev) && !is_word_char(nextc)) return 1
        rest = substr(text, p + length(needle))
        p = index(rest, needle)
        if (p > 0) p = p + length(needle) - 1
    }
    return 0
}
BEGIN {
    user = ENVIRON["MM_LEAK_USER"]
    home = ENVIRON["MM_LEAK_HOME"]
    root = ENVIRON["MM_LEAK_ROOT"]
    fingerprint = ENVIRON["MM_LEAK_FINGERPRINT"]
    arch = ENVIRON["MM_LEAK_ARCH"]
}
{
    if (has_whole_word($0, user)) leaks = leaks " account-name"
    if (home != "" && index($0, home) > 0) leaks = leaks " home-directory"
    if (root != "" && index($0, root) > 0) leaks = leaks " test-root"
    if (fingerprint != "" && index($0, fingerprint) > 0) leaks = leaks " fingerprint"
    if (arch != "" && index($0, arch) > 0) leaks = leaks " machine-architecture"
}
END { if (leaks != "") { print leaks; exit 1 } }
'
assert_no_leak() { # <case-name> <normalized-output-file>
    local name="$1" file="$2" leaks=""
    leaks="$(MM_LEAK_USER="$CURRENT_USER" MM_LEAK_HOME="$REAL_HOME" MM_LEAK_ROOT="$TEST_ROOT" \
        MM_LEAK_FINGERPRINT="" MM_LEAK_ARCH="$ARCH" awk "$LEAK_CHECK_AWK" "$file" || true)"
    if [[ -n "$leaks" ]]; then
        fail "$name: normalized output still carries machine-specific values:$leaks"
    fi
}

# run_case <refresh-expected: yes|no> <name> <args...>
run_case() {
    local refresh="$1" name="$2"; shift 2
    local out="$TEST_ROOT/$name.out" err="$TEST_ROOT/$name.err" code=0
    cases=$((cases + 1))
    : >"$PROBE_LOG"
    : >"$AGENT_PROBE_LOG"
    set +e
    env -i HOME="$EMPTY_HOME" PATH="$MIN_PATH" LANG=C.UTF-8 LC_ALL=C.UTF-8 \
        GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
        MM_FIXTURE_CONFIG="$CONFIG" MM_PROBE_LOG="$PROBE_LOG" \
        MM_AGENT_PROBE_LOG="$AGENT_PROBE_LOG" MM_FIXTURE_HOME="$EMPTY_HOME" MM_FIXTURE_SHARED="$SHARED" \
        bash "$FIX/bin/managed-machine" "$@" >"$out" 2>"$err"
    code=$?
    set -e
    if [[ "$refresh" == no ]] && grep -q '^refresh ' "$PROBE_LOG"; then
        fail "$name: --no-refresh still refreshed the config checkout"
    fi
    local actual="$TEST_ROOT/$name.actual"
    {
        printf 'exit %s\n--- stdout\n' "$code"
        normalize <"$out"
        printf -- '--- stderr\n'
        normalize <"$err"
        printf -- '--- probes\n'
        LC_ALL=C sort "$PROBE_LOG" | normalize
    } >"$actual"
    assert_no_leak "$name" "$actual"
    if [[ "$RECORD" == 1 ]]; then
        mkdir -p "$GOLDEN"; cp "$actual" "$GOLDEN/$name.golden"; echo "recorded $name"; return
    fi
    if [[ ! -f "$GOLDEN/$name.golden" ]]; then
        echo "FAIL: $name: missing golden ($GOLDEN/$name.golden)" >&2
        failures=$((failures + 1))
        return
    fi
    if ! diff -u "$GOLDEN/$name.golden" "$actual"; then
        echo "FAIL: $name differs from golden" >&2
        failures=$((failures + 1))
    fi
}

run_case no  list-json catalog list --json --no-refresh
run_case no  list-human catalog list --no-refresh
run_case yes list-json-refresh catalog list --json
run_case no  resolve-alias-json catalog resolve code --json --no-refresh
run_case no  resolve-token-setup-prefix catalog resolve setup-visual-studio-code --json --no-refresh
run_case no  resolve-wrapper catalog resolve grok-build --json --no-refresh
run_case no  resolve-standalone catalog resolve alpha --json --no-refresh
run_case no  resolve-unknown-json catalog resolve nothing-here --json --no-refresh
run_case no  resolve-unknown-plain catalog resolve nothing-here --no-refresh
run_case no  resolve-bogus-flag catalog resolve code --bogus --json --no-refresh
run_case no  list-bogus-flag catalog list --bogus --json
run_case no  usage catalog
run_case yes usage-json catalog --json
run_case yes setup-list setup --list
run_case yes setup-list-json setup --list --json
run_case no  setup-list-bogus setup --list --bogus
run_case no  setup-list-bogus-json setup --list --bogus --json
run_case no  setup-help setup --help
run_case no  status-bad-section-json status --json --section nothing-here
run_case no  status-bad-section status --section nothing-here
run_case no  status-bogus-flag-json status --bogus --json
run_case no  status-help status --help
run_case no  fleet-bogus-json fleet --bogus --json
run_case no  fleet-list-bogus-json fleet list --bogus --json
run_case no  ssh-bogus-json ssh --bogus --json
run_case no  ssh-status-bogus-json ssh status --bogus --json
run_case no  account-bogus-json account --bogus --json
run_case no  account-doctor-noname-json account doctor --json
run_case no  adopt-bogus-json adopt --bogus --json
run_case no  adopt-unknown-json adopt nothing-here --json
run_case no  fleet-list-json fleet list --json
run_case no  ssh-status-json ssh status --json
run_case no  account-doctor-unknown-json account doctor no-such-account-fixture --json

if [[ "$RECORD" == 1 ]]; then
    echo "catalog characterization: $cases cases recorded in $GOLDEN"
    exit 0
fi
[[ "$failures" -eq 0 ]] || fail "$failures of $cases catalog characterization case(s) differ"
echo "catalog characterization: $cases cases match goldens"

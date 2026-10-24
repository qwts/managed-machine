#!/usr/bin/env bash
# Representative workflow and output-contract tests for the managed-machine
# skill (ENG-0055 release-gate check 5). The shared cli-skill-gate runs them
# against the packaged executable through CLI_SKILL_GATE_EXECUTABLE; run
# directly, they use this checkout's bin/managed-machine. Every probe is
# read-only and offline. Help never refreshes the config checkout (#173); it
# runs here under an empty HOME so it lists only what the tree itself carries.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MM="${CLI_SKILL_GATE_EXECUTABLE:-$ROOT/bin/managed-machine}"
SKILL="$ROOT/skills/managed-machine/SKILL.md"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

# --- --version prints the bare release version --------------------------------
out="$("$MM" --version 2>"$TEST_DIR/err")"
[[ "$out" == "$(tr -d '[:space:]' <"$ROOT/VERSION")" ]] || fail "--version prints VERSION (got '$out')"
[[ ! -s "$TEST_DIR/err" ]] || fail "--version writes nothing to stderr"

# --- skill path reports the bundled skill and its source commit ----------------
"$MM" skill path >"$TEST_DIR/skill"
bundle="$(sed -n 1p "$TEST_DIR/skill")"
# A packaged release must name its source commit; only a direct run from a
# tree with no git metadata may report unknown.
commit_re='commit ([0-9a-f]{40}|unknown)'
[[ -z "${CLI_SKILL_GATE_EXECUTABLE:-}" ]] || commit_re='commit [0-9a-f]{40}'
sed -n 2p "$TEST_DIR/skill" | grep -Eqx "$commit_re" || fail "skill path reports the source commit"
cmp -s "$bundle/SKILL.md" "$SKILL" || fail "bundled SKILL.md matches the source"

# --- skill path --json reports structured JSON and tolerates SIGPIPE -----------
"$MM" skill path --json >"$TEST_DIR/skill.json" 2>"$TEST_DIR/err"
[[ ! -s "$TEST_DIR/err" ]] || fail "skill path --json writes nothing to stderr"
python3 - "$TEST_DIR/skill.json" "$bundle" "$SKILL" <<'PY' || fail "skill path --json schema"
import json, re, sys
data = json.load(open(sys.argv[1]))
assert data["schema_version"] == 1
assert data["command"] == "skill-path"
assert data["path"] == sys.argv[2]
assert data["skill_file"] == sys.argv[2] + "/SKILL.md"
assert re.fullmatch(r"[0-9a-f]{40}|unknown", data["commit"])
PY
"$MM" skill path --json | head -n 1 >/dev/null || fail "skill path --json tolerates SIGPIPE"

# --- a new-core skill request in an unbuilt tree fails on stderr with no stdout
set +e
"$MM" skill list >"$TEST_DIR/out" 2>"$TEST_DIR/err"
status=$?
set -e
[[ $status -eq 1 && ! -s "$TEST_DIR/out" ]] || fail "skill list exits 1 with no stdout"
grep -q '^Error: the managed-machine core is missing at ' "$TEST_DIR/err" || fail "errors carry the Error: prefix"

# --- a new-core skill request with --json emits structured core-missing envelope
set +e
"$MM" skill list --json >"$TEST_DIR/out.json" 2>"$TEST_DIR/err"
status=$?
set -e
[[ $status -eq 1 && ! -s "$TEST_DIR/err" ]] || fail "skill list --json exits 1 with no stderr"
python3 - "$TEST_DIR/out.json" "$bundle" <<'PY' || fail "skill list --json error envelope"
import json, os, sys
data = json.load(open(sys.argv[1]))
want_core = os.path.dirname(os.path.dirname(sys.argv[2])) + "/core/managed-machine"
assert data["schema_version"] == 1
assert data["command"] == "skill"
assert data["ready"] is False
assert data["status"] == "not_ready"
assert data["error"]["code"] == "core-missing"
assert data["error"]["action"] == "Run: brew reinstall managed-machine"
assert data["error"]["evidence"] == {"path": want_core}
assert data["checks"][0]["code"] == "core-missing"
assert data["checks"][0]["evidence"] == {"path": want_core}
PY

# --- help starts nothing: bare, help, -h, --help print usage and exit 0 -------
mkdir -p "$TEST_DIR/home"
for args in '' help -h --help; do
    set +e
    # shellcheck disable=SC2086 # the empty case must pass no argument at all
    HOME="$TEST_DIR/home" "$MM" $args >"$TEST_DIR/out" 2>"$TEST_DIR/err" </dev/null
    status=$?
    set -e
    [[ $status -eq 0 ]] || fail "'managed-machine $args' exits 0 (got $status)"
    grep -Fq '[legacy] --bootstrap' "$TEST_DIR/out" || fail "'managed-machine $args' prints usage"
done
[[ -z "$(ls -A "$TEST_DIR/home")" ]] || fail "help writes nothing under HOME"

# --- an unknown command prints usage to stderr and exits nonzero ---------------
set +e
HOME="$TEST_DIR/home" "$MM" helpme >"$TEST_DIR/out" 2>"$TEST_DIR/err" </dev/null
status=$?
set -e
[[ $status -ne 0 && ! -s "$TEST_DIR/out" ]] || fail "an unknown command exits nonzero with no stdout"
grep -q '^Error: unknown command: helpme' "$TEST_DIR/err" || fail "an unknown command names itself"
grep -Fq '[legacy] --bootstrap' "$TEST_DIR/err" || fail "an unknown command prints usage"

# --- every command the skill classifies is one the CLI documents ---------------
# Core lines read "  managed-machine <command> ...", legacy lines "  [legacy] <command> ..."
# with alternatives joined by "|" (S7 N00); normalize both to "<command> <args>".
usage="$(sed -n '/^usage()/,/^EOF$/p' "$MM" | sed -nE 's/^  (managed-machine|\[legacy\]) //p' | tr '|' ' ')"
count=0
while read -r command; do
    first="${command%% *}"
    if [[ "$command" == *" "* ]]; then
        pattern="^${first}( .*)? ${command#* }( |$)"
    else
        pattern="^${first}( |$)"
    fi
    grep -Eq -- "$pattern" <<<"$usage" || fail "usage documents $command"
    count=$((count + 1))
done < <(grep -E '^\| (read-only|local-write|remote-write|destructive) \|' "$SKILL" | cut -d'|' -f3 \
    | grep -oE '`[a-z-]+( [a-z-]+)?' | tr -d '`')
[[ $count -ge 14 ]] || fail "the side-effect table lists the commands (found $count)"

echo "skill-workflows: ok"

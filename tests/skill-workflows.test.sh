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
SKILL="$ROOT/skills/managed-machine/SKILL.md"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

if [[ -n "${CLI_SKILL_GATE_EXECUTABLE:-}" ]]; then
    MM="$CLI_SKILL_GATE_EXECUTABLE"
else
    STAGE_OPT="$TEST_DIR/pkg/opt/managed-machine"
    mkdir -p "$STAGE_OPT/bin" "$STAGE_OPT/libexec/skills"
    (cd "$ROOT" && GOTOOLCHAIN=local go build -trimpath -ldflags "-X main.version=$(tr -d '[:space:]' <"$ROOT/VERSION")" -o "$STAGE_OPT/bin/managed-machine" ./cmd/managed-machine)
    cp "$ROOT/VERSION" "$STAGE_OPT/libexec/VERSION"
    cp -R "$ROOT/skills/managed-machine" "$STAGE_OPT/libexec/skills/managed-machine"
    git -C "$ROOT" rev-parse HEAD >"$STAGE_OPT/libexec/RELEASE_COMMIT" 2>/dev/null || printf 'unknown\n' >"$STAGE_OPT/libexec/RELEASE_COMMIT"
    MM="$STAGE_OPT/bin/managed-machine"
fi

# --- --version prints the bare release version --------------------------------
out="$("$MM" --version 2>"$TEST_DIR/err")"
[[ "$out" == "$(tr -d '[:space:]' <"$ROOT/VERSION")" ]] || fail "--version prints VERSION (got '$out')"
[[ ! -s "$TEST_DIR/err" ]] || fail "--version writes nothing to stderr"

# --- skill path --human reports the bundled skill and its source commit -------
"$MM" skill path --human >"$TEST_DIR/skill"
bundle="$(sed -n 1p "$TEST_DIR/skill")"
# A packaged release must name its source commit; only a direct run from a
# tree with no git metadata may report unknown.
commit_re='commit ([0-9a-f]{40}|unknown)'
[[ -z "${CLI_SKILL_GATE_EXECUTABLE:-}" ]] || commit_re='commit [0-9a-f]{40}'
sed -n 2p "$TEST_DIR/skill" | grep -Eqx "$commit_re" || fail "skill path reports the source commit"
cmp -s "$bundle/SKILL.md" "$SKILL" || fail "bundled SKILL.md matches the source"

# --- skill path [--json] reports structured JSON and tolerates SIGPIPE --------
"$MM" skill path --json >"$TEST_DIR/skill.json" 2>"$TEST_DIR/err"
[[ ! -s "$TEST_DIR/err" ]] || fail "skill path --json writes nothing to stderr"
python3 - "$TEST_DIR/skill.json" "$bundle" <<'PY' || fail "skill path --json schema"
import json, re, sys
data = json.load(open(sys.argv[1]))
assert data["command"] == "skill-path"
assert data["name"] == "managed-machine"
assert data["path"] == sys.argv[2]
assert data["skill_file"] == sys.argv[2] + "/SKILL.md"
assert re.fullmatch(r"[0-9a-f]{40}|unknown", data["commit"])
PY
"$MM" skill path --json | head -n 1 >/dev/null || fail "skill path --json tolerates SIGPIPE"

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

# --- an unknown command writes MM_USAGE to stderr and exits nonzero -----------
set +e
HOME="$TEST_DIR/home" "$MM" helpme >"$TEST_DIR/out" 2>"$TEST_DIR/err" </dev/null
status=$?
set -e
[[ $status -eq 2 && ! -s "$TEST_DIR/out" ]] || fail "an unknown command exits 2 with no stdout"
grep -Fq 'MM_USAGE' "$TEST_DIR/err" || fail "an unknown command emits MM_USAGE"
grep -Fq 'helpme' "$TEST_DIR/err" || fail "an unknown command names itself"

# --- every command the skill classifies is one the CLI documents ---------------
# Core lines read "  managed-machine <command> ...", legacy lines "  [legacy] <command> ..."
# with alternatives joined by "|" (S7 N00 / S8 D3); normalize both to "<command> <args>".
help_text="$("$MM" --help)"
usage="$(sed -nE 's/^  (managed-machine|\[legacy\]) //p' <<<"$help_text" | tr '|' ' ')"
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

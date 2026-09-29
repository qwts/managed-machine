#!/usr/bin/env bash
# Representative workflow and output-contract tests for the managed-machine
# skill (ENG-0055 release-gate check 5). The shared cli-skill-gate runs them
# against the packaged executable through CLI_SKILL_GATE_EXECUTABLE; run
# directly, they use this checkout's bin/managed-machine. Every probe is
# read-only and offline: --help and unknown commands list setup names, which
# refreshes the config checkout, so the usage text is read from the
# executable instead.
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

# --- a malformed skill request fails on stderr with no stdout -----------------
set +e
"$MM" skill list >"$TEST_DIR/out" 2>"$TEST_DIR/err"
status=$?
set -e
[[ $status -eq 1 && ! -s "$TEST_DIR/out" ]] || fail "skill list exits 1 with no stdout"
grep -q '^Error: ' "$TEST_DIR/err" || fail "errors carry the Error: prefix"

# --- every command the skill classifies is one the CLI documents ---------------
usage="$(sed -n '/^usage()/,/^EOF$/p' "$MM")"
count=0
while read -r command; do
    grep -Eq "^  managed-machine ${command}( |$)" <<<"$usage" || fail "usage documents $command"
    count=$((count + 1))
done < <(grep -E '^\| (read-only|local-write|remote-write|destructive) \|' "$SKILL" | cut -d'|' -f3 \
    | grep -oE '`[a-z-]+( [a-z-]+)?' | tr -d '`')
[[ $count -ge 14 ]] || fail "the side-effect table lists the commands (found $count)"

echo "skill-workflows: ok"

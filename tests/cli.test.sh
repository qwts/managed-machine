#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
FIXTURE="$TEST_ROOT/fixture"
CLI="$FIXTURE/bin/managed-machine"
RUN_LOG="$TEST_ROOT/run.log"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$FIXTURE/bin" "$FIXTURE/lib" "$FIXTURE/scripts"
cp "$ROOT/bin/managed-machine" "$CLI"
chmod +x "$CLI"
touch "$FIXTURE/lib/install.sh"
cat >"$FIXTURE/scripts/bootstrap" <<EOF
#!/usr/bin/env bash
printf 'bootstrap' >>'$RUN_LOG'
printf '\t%s' "\$@" >>'$RUN_LOG'
printf '\n' >>'$RUN_LOG'
EOF
chmod +x "$FIXTURE/scripts/bootstrap"

write_setup_fixture() {
    local name="$1"
    cat >"$FIXTURE/setup-$name" <<EOF
#!/usr/bin/env bash
printf '%s' '$name' >>'$RUN_LOG'
printf '\t%s' "\$@" >>'$RUN_LOG'
printf '\n' >>'$RUN_LOG'
EOF
    chmod +x "$FIXTURE/setup-$name"
}

write_setup_fixture alpha
write_setup_fixture beta-tool
for setup_script in "$ROOT"/setup-*; do
    [[ -f "$setup_script" && -x "$setup_script" ]] || continue
    setup_name="${setup_script##*/}"
    write_setup_fixture "${setup_name#setup-}"
done
printf '#!/usr/bin/env bash\n' >"$FIXTURE/setup-hidden"

"$CLI" setup alpha
"$CLI" setup setup-beta-tool
[[ "$(sed -n '1p' "$RUN_LOG")" == 'alpha' ]]
[[ "$(sed -n '2p' "$RUN_LOG")" == 'beta-tool' ]]

"$CLI" --bootstrap --non-interactive
[[ "$(sed -n '3p' "$RUN_LOG")" == $'bootstrap\t--non-interactive' ]]

cat >"$FIXTURE/scripts/adopt" <<EOF
#!/usr/bin/env bash
printf 'adopt' >>'$RUN_LOG'
if [[ \$# -gt 0 ]]; then
    printf '\t%s' "\$@" >>'$RUN_LOG'
fi
printf '\n' >>'$RUN_LOG'
EOF
chmod +x "$FIXTURE/scripts/adopt"
"$CLI" adopt
"$CLI" adopt vscode
[[ "$(sed -n '4p' "$RUN_LOG")" == 'adopt' ]]
[[ "$(sed -n '5p' "$RUN_LOG")" == $'adopt\tvscode' ]]

"$CLI" setup alpha --restore
[[ "$(sed -n '6p' "$RUN_LOG")" == $'alpha\t--restore' ]]

HELP_OUTPUT="$("$CLI" --help)"
[[ "$HELP_OUTPUT" == *'name may be bin or setup-bin'* ]]
[[ "$HELP_OUTPUT" == *'--interactive|--non-interactive'* ]]
[[ "$HELP_OUTPUT" == *'managed-machine status'* ]]
[[ "$HELP_OUTPUT" == *'managed-machine adopt'* ]]
[[ "$HELP_OUTPUT" == *$'  alpha'* ]]
[[ "$HELP_OUTPUT" == *$'  beta-tool'* ]]
[[ "$HELP_OUTPUT" != *$'  hidden'* ]]

assert_invalid_setup() {
    local expected="$1"
    shift
    if "$CLI" setup "$@" >"$TEST_ROOT/invalid.out" 2>&1; then
        echo "expected setup command to fail: $*" >&2
        exit 1
    fi
    grep -Fq "$expected" "$TEST_ROOT/invalid.out"
    grep -Fq 'Available setup names' "$TEST_ROOT/invalid.out"
    grep -Fq '  alpha' "$TEST_ROOT/invalid.out"
}

assert_invalid_setup 'unknown setup name: missing' missing
assert_invalid_setup 'invalid setup name: ../alpha' ../alpha
assert_invalid_setup 'setup requires a script name'

# Execute every concrete setup example published by the skill. This keeps the
# documentation and accepted CLI forms coupled without running real installers.
SKILL_EXAMPLES=0
while IFS= read -r example; do
    [[ -n "$example" ]] || continue
    "$CLI" setup "$example"
    SKILL_EXAMPLES=$((SKILL_EXAMPLES + 1))
done < <(sed -n 's/^managed-machine setup \([^ #<]*\).*/\1/p' "$ROOT/skills/SKILL.md")
[[ "$SKILL_EXAMPLES" -gt 0 ]]

# Every setup script documented in the skill table must exist and be executable.
while IFS= read -r script; do
    [[ -x "$ROOT/$script" ]]
done < <(sed -n 's/^| \(setup-[a-z0-9-]*\) |.*/\1/p' "$ROOT/skills/SKILL.md")

echo 'CLI tests passed'

#!/usr/bin/env bash
# Reviewed npm scripts: fresh install, repair, isolation and planned execution.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
ROOT="$(cd "$(dirname "$0")/../legacy" && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_DIR/bin" "$TEST_DIR/config" "$TEST_DIR/home" "$TEST_DIR/prefix/bin"
export NPM_TEST_DIR="$TEST_DIR"
export NPM_CONFIG_REGISTRY=https://unreviewed.invalid/
export PATH="$TEST_DIR/bin:/usr/bin:/bin"
cat >"$TEST_DIR/bin/npm" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$NPM_TEST_DIR/npm.log"
[[ -f "$NPM_CONFIG_USERCONFIG" && -f "$NPM_CONFIG_GLOBALCONFIG" ]]
[[ ! -s "$NPM_CONFIG_USERCONFIG" && ! -s "$NPM_CONFIG_GLOBALCONFIG" ]]
[[ "$PWD" != "$NPM_TEST_DIR" ]]
# Empty npmrc files do not override registry settings inherited from the
# caller. Both mutation phases must select the public registry explicitly.
case "$1" in
  install|rebuild)
    effective_registry="${NPM_CONFIG_REGISTRY:-}"
    for arg in "$@"; do
      case "$arg" in --registry=*) effective_registry="${arg#--registry=}" ;; esac
    done
    [[ "$effective_registry" == https://registry.npmjs.org/ ]] || exit 99 ;;
esac
case "$1" in
  view)
    if [[ "${NPM_TEST_NPM12_METADATA:-}" == 1 ]]; then
      echo '[{"name":"@deepseek-ai/dsh","version":"0.2.0-rc.2"}]'
    elif [[ "${NPM_TEST_NPM12_METADATA:-}" == ambiguous ]]; then
      echo '[{"name":"@deepseek-ai/dsh","version":"0.2.0-rc.2"},{"name":"wrong","version":"1.0.0"}]'
    else
      echo '{"name":"@deepseek-ai/dsh","version":"0.2.0-rc.2"}'
    fi ;;
  prefix) echo "$NPM_TEST_DIR/prefix" ;;
  root) echo "$NPM_TEST_DIR/prefix/lib/node_modules" ;;
  ls)
    [[ -f "$NPM_TEST_DIR/receipt" ]] || exit 1
    echo "$NPM_TEST_DIR/prefix/lib/node_modules/@deepseek-ai/dsh" ;;
  install)
    [[ "$*" == *'--ignore-scripts'* ]] || exit 98
    [[ ! -f "$NPM_TEST_DIR/fail-install" ]] || exit 41
    touch "$NPM_TEST_DIR/receipt"
    cat >"$NPM_TEST_DIR/prefix/bin/dsh" <<'DSH'
#!/bin/sh
[ ! -f "$NPM_TEST_DIR/fail-version" ] || exit 45
echo 0.2.0-rc.2
DSH
    chmod +x "$NPM_TEST_DIR/prefix/bin/dsh" ;;
  rebuild)
    [[ ! -f "$NPM_TEST_DIR/fail-rebuild" ]] || exit 42
    touch "$NPM_TEST_DIR/healthy" ;;
  *) exit 97 ;;
esac
STUB
cat >"$TEST_DIR/bin/node" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >>"$NPM_TEST_DIR/node.log"
[ -f "$NPM_TEST_DIR/healthy" ] && [ ! -f "$NPM_TEST_DIR/fail-smoke" ]
STUB
chmod +x "$TEST_DIR/bin/"*
source "$ROOT/lib/install.sh"
source "$ROOT/lib/apps.sh"
NAMES='@deepseek-ai/dsh-subprocess-local,koffi,node-pty'
POLICY='@deepseek-ai/dsh-subprocess-local@0.2.0-rc.2,koffi@3.1.1,node-pty@1.2.0-beta.15'
write_catalog() {
  cat >"$TEST_DIR/config/apps.json" <<JSON
{"schema_version":1,"apps":[{"name":"deepseek","kind":"npm","package":"@deepseek-ai/dsh","command":"dsh","install_scripts":$1}]}
JSON
}
write_catalog '["@deepseek-ai/dsh-subprocess-local@0.2.0-rc.2","koffi@3.1.1","node-pty@1.2.0-beta.15"]'
run_install() {
  HOME="$TEST_DIR/home" CONFIG_REPO_ROOT="$TEST_DIR/config" install_catalog_app deepseek
}
expect_failure() {
  if run_install >"$TEST_DIR/failure.out" 2>&1; then
    echo "expected setup failure: $1" >&2
    exit 1
  fi
}
export NPM_TEST_NPM12_METADATA=ambiguous
expect_failure ambiguous-metadata
! grep -Eq '^(install|rebuild) ' "$TEST_DIR/npm.log"
export NPM_TEST_NPM12_METADATA=1
run_install >"$TEST_DIR/fresh.out"
unset NPM_TEST_NPM12_METADATA
grep -Fxq 'install --global --no-fund --no-audit --ignore-scripts --registry=https://registry.npmjs.org/ @deepseek-ai/dsh' "$TEST_DIR/npm.log"
grep -Fxq "rebuild --global --registry=https://registry.npmjs.org/ --rebuild-bundle=false --ignore-scripts=false --dangerously-allow-all-scripts=false --allow-scripts=$NAMES ${POLICY//,/ }" "$TEST_DIR/npm.log"
grep -Fq "$ROOT/lib/check-deepseek.mjs $TEST_DIR/prefix/lib/node_modules/@deepseek-ai/dsh" "$TEST_DIR/node.log"
: >"$TEST_DIR/npm.log"
run_install >"$TEST_DIR/rerun.out"
! grep -Eq '^(install|rebuild) ' "$TEST_DIR/npm.log"
# A package receipt and working version command must not mask missing helpers.
rm "$TEST_DIR/healthy"
run_install >"$TEST_DIR/repair.out"
grep -q '^rebuild ' "$TEST_DIR/npm.log"
# Both install and rebuild failure propagate; install failure never rebuilds.
rm "$TEST_DIR/healthy"
touch "$TEST_DIR/fail-install"
: >"$TEST_DIR/npm.log"
expect_failure install
! grep -q '^rebuild ' "$TEST_DIR/npm.log"
rm "$TEST_DIR/fail-install"
touch "$TEST_DIR/fail-rebuild"
expect_failure rebuild
rm "$TEST_DIR/fail-rebuild"
touch "$TEST_DIR/fail-smoke"
expect_failure smoke
grep -Fq 'setup is incomplete' "$TEST_DIR/failure.out"
rm "$TEST_DIR/fail-smoke"
touch "$TEST_DIR/fail-version"
expect_failure version
rm "$TEST_DIR/fail-version"
# Malformed policy fails before any install side effect, even on an existing install.
for bad in '[]' 'null' 'true' '"koffi"' '["koffi"]' '["koffi@*"]' '["koffi@^3.1.1"]' '["--ignore-scripts=false"]' '["file:/tmp/evil"]' '["koffi@01.2.3"]' '["koffi@1.2.3-01"]' '["koffi@1.2.3-rc..2"]'; do
  write_catalog "$bad"
  : >"$TEST_DIR/npm.log"
  expect_failure policy
  ! grep -Eq '^(install|rebuild) ' "$TEST_DIR/npm.log"
  grep -Fq 'install_scripts must be a nonempty array' "$TEST_DIR/failure.out"
done
write_catalog '[]'
python3 - "$TEST_DIR/config/apps.json" <<'PYJSON'
import json, sys
p = sys.argv[1]
data = json.load(open(p))
del data['apps'][0]['install_scripts']
with open(p, 'w') as f:
    json.dump(data, f)
PYJSON
expect_failure missing-policy
grep -Fq 'DeepSeek requires reviewed install_scripts' "$TEST_DIR/failure.out"
write_catalog '["@deepseek-ai/dsh-subprocess-local@0.2.0-rc.2","koffi@3.1.1","node-pty@1.2.0-beta.15"]'
# Planning does not mistake a version string for native readiness, and the
# approved action includes the exact policy that execution will use.
(
  source "$ROOT/lib/bootstrap.sh"
  ensure_brew_on_path() { return 0; }
  mkdir -p "$TEST_DIR/planning"
  npm_needs_elevation() { return 0; }
  resolved_npm_owner() { id -un; }
  CONFIG_REPO_ROOT="$TEST_DIR/config" bootstrap_inspect_step_for_plan "$TEST_DIR/planning" setup-deepseek
  [[ ! -s "$TEST_DIR/planning/actions.tsv" ]]
  rm "$TEST_DIR/healthy"
  CONFIG_REPO_ROOT="$TEST_DIR/config" bootstrap_inspect_step_for_plan "$TEST_DIR/planning" setup-deepseek
  grep -Fq "$TEST_DIR/bin/npm|$POLICY" "$TEST_DIR/planning/actions.tsv"
  grep -Fq "run reviewed scripts: $POLICY" "$TEST_DIR/planning/actions.tsv"
)
# Direct elevation carries the same exact policy; skipped stays skipped.
rm -f "$TEST_DIR/healthy"
npm_is_system_prefix() { return 0; }
npm_prefix_owner() { echo otheradmin; }
elevate_run() {
  printf '%s\n' "$*" >>"$TEST_DIR/elevate.log"
  npm_public "${@:6}"
}
run_install >"$TEST_DIR/elevated.out"
grep -Fq "managed-install @deepseek-ai/dsh $POLICY" "$TEST_DIR/elevate.log"
rm "$TEST_DIR/healthy"
elevate_run() { return 76; }
rc=0
run_install >"$TEST_DIR/skipped.out" || rc=$?
[[ "$rc" -eq 76 ]]
# The real plan runner executes both phases with isolated config as one action.
mkdir -p "$TEST_DIR/session/"{status,out,detail}
printf 'act-001\tsetup-deepseek\tnpm-global\t%s\t@deepseek-ai/dsh\t%s\t%s|%s\treviewed install\n' \
  "$(id -un)" "$TEST_DIR/prefix/lib/node_modules/@deepseek-ai/dsh" "$TEST_DIR/bin/npm" "$POLICY" >"$TEST_DIR/plan"
plan_sha="$(shasum -a 256 "$TEST_DIR/plan" | awk '{print $1}')"
MANAGED_MACHINE_ASSUME_SYSTEM_NPM=1 /bin/sh "$ROOT/lib/elevate-plan-run" test "$plan_sha" "$TEST_DIR/plan" "$TEST_DIR/session" ''
[[ "$(cat "$TEST_DIR/session/status/act-001")" == 0 ]]
[[ -f "$TEST_DIR/healthy" ]]
# A completed privileged action is still subject to the native health check.
unset -f elevate_run
export MANAGED_MACHINE_CURRENT_STEP=setup-deepseek
export MANAGED_MACHINE_APPROVED_PLAN_FILE="$TEST_DIR/plan"
export MANAGED_MACHINE_PLAN_SESSION_DIR="$TEST_DIR/session"
run_install >"$TEST_DIR/planned.out"
touch "$TEST_DIR/fail-smoke"
expect_failure planned-health
# Matching refuses a substituted script policy.
source "$ROOT/lib/elevate.sh"
MANAGED_MACHINE_CURRENT_STEP=setup-deepseek elevate_plan_match_action \
  "run npm managed-install @deepseek-ai/dsh $POLICY as $(id -un)" \
  /bin/sh "$ROOT/lib/npm-public-run" "$(id -un)" "$TEST_DIR/bin/npm" managed-install @deepseek-ai/dsh "$POLICY" >"$TEST_DIR/match"
[[ "$(cat "$TEST_DIR/match")" == act-001 ]]
if MANAGED_MACHINE_CURRENT_STEP=setup-deepseek elevate_plan_match_action \
  "run npm managed-install @deepseek-ai/dsh koffi@9.9.9 as $(id -un)" \
  /bin/true; then
  echo 'accepted substituted policy' >&2
  exit 1
fi
echo 'npm install-script tests passed'

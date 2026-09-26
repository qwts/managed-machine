#!/usr/bin/env bash
# Every suite test isolates fixture git from the developer's gitconfig, so a
# broken global core.hooksPath (or signing key, or credential helper) cannot
# fail the suite. setup-git-hooks is the one exception: it deliberately
# writes --global config, scoped to its fixture HOME.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

fail=0
for t in "$ROOT"/tests/*.test.sh; do
    name="$(basename "$t")"
    case "$name" in
        setup-git-hooks.test.sh)
            want='GIT_CONFIG_GLOBAL="$TEST_HOME/.gitconfig"'
            ;;
        *)
            want='GIT_CONFIG_GLOBAL=/dev/null'
            ;;
    esac
    grep -Fq 'export GIT_CONFIG_NOSYSTEM=1' "$t" || {
        echo "$name: missing GIT_CONFIG_NOSYSTEM export" >&2
        fail=1
    }
    grep -Fq "$want" "$t" || {
        echo "$name: missing $want" >&2
        fail=1
    }
done

if [[ "$fail" -ne 0 ]]; then
    exit 1
fi
echo 'gitconfig isolation tests passed'

#!/usr/bin/env bash
# Verify the onboard-harness skill is present, well-formed, and safe.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SKILL="$ROOT/skills/onboard-harness/SKILL.md"

[[ -f "$SKILL" ]]

# Required frontmatter fields.
grep -qE '^name:[[:space:]]+onboard-harness$' "$SKILL"
grep -qE '^description:' "$SKILL"

# Description is in third person and mentions the right trigger terms.
grep -iq 'harness' "$SKILL"
grep -iq 'onboard' "$SKILL"

# No host-specific paths (anchored, not just present in prose about /Users).
if grep -qE '/Users/[A-Za-z0-9_-]+/' "$SKILL"; then
    echo 'Error: skill contains a host-specific /Users/ path' >&2
    exit 1
fi

# No instructions to run git config user.name/user.email. Prose that says
# "never run git config user.name" is allowed, so anchor at line start.
if grep -qE '^[[:space:]]*git config[[:space:]]+user\.(name|email)' "$SKILL"; then
    echo 'Error: skill instructs git config user.name/user.email' >&2
    exit 1
fi

# No literal secret placeholders.
if grep -qE '[A-Za-z0-9_-]+_TOKEN[[:space:]]*=[[:space:]]*[^$]' "$SKILL"; then
    echo 'Error: skill contains a literal token assignment' >&2
    exit 1
fi
if grep -qE 'password[[:space:]]*=[[:space:]]*' "$SKILL"; then
    echo 'Error: skill contains a literal password assignment' >&2
    exit 1
fi

# References the runbook and AGENTS.md.
grep -q 'onboard-new-harness.md' "$SKILL"
grep -q 'AGENTS.md' "$SKILL"

# Main skill file stays under the 500-line guidance.
[[ "$(wc -l < "$SKILL")" -lt 500 ]]

echo 'onboard-harness skill tests passed'

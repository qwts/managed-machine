#!/usr/bin/env bash
# Verify the onboard-harness skill is present, well-formed, and safe.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SKILL="$ROOT/skills/onboard-harness/SKILL.md"

[[ -f "$SKILL" ]]

# Required frontmatter fields.
grep -qE '^name:[[:space:]]+onboard-harness$' "$SKILL"
grep -qE '^description:' "$SKILL"

# Description is in third person and mentions the right trigger terms.
grep -iq 'harness' "$SKILL"
grep -iq 'onboard' "$SKILL"

# No host-specific paths or git config instructions.
! grep -qE '/Users/[A-Za-z0-9_-]+/' "$SKILL"
! grep -qE 'git config[[:space:]]+user\.(name|email)' "$SKILL"

# No literal secrets placeholders.
! grep -qE '[A-Za-z0-9_-]+_TOKEN[[:space:]]*=[[:space:]]*[^$]' "$SKILL"
! grep -qE 'password[[:space:]]*=[[:space:]]*' "$SKILL"

# References the runbook and AGENTS.md.
grep -q 'onboard-new-harness.md' "$SKILL"
grep -q 'AGENTS.md' "$SKILL"

# Main skill file stays under the 500-line guidance.
[[ "$(wc -l < "$SKILL")" -lt 500 ]]

echo 'onboard-harness skill tests passed'

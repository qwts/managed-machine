#!/usr/bin/env bash
# The machine-daemon plan names this script as the phase gate. It checks the
# P2 constraints in AGENTS.md and runs the daemon package tests. It does not
# install a service or call launchctl.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

phrases=(
	"prefix owner"
	"does not execute shell scripts"
	"loads it with launchctl or systemctl --user"
	"does not register a Windows service"
	"refuses agent accounts"
	"needs elevation"
	"Publishing a catalog entry is the approval"
	"thin client of that daemon"
)
for phrase in "${phrases[@]}"; do
	if ! grep -F -q "$phrase" "$ROOT/AGENTS.md"; then
		printf 'AGENTS.md is missing %q\n' "$phrase" >&2
		exit 1
	fi
done

(cd "$ROOT" && go test ./internal/daemon/ ./internal/diag/)

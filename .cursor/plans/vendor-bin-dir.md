---
name: vendor-bin-dir
status: active
overview: Official-cli catalog rows can declare a `bin_dir` whose vendor-installed binary gets linked onto the managed ~/.local/bin, so CLIs like Kilo Code that refuse PATH edits stay setup-able.
related_prs: []
---

# Vendor bin dir linking

## Intent

`official-cli` rows install from a vendor `curl|bash` installer. Some installers
hardcode a bin directory off the managed PATH and, with `--no-modify-path` or an
`env` guard, never create the `~/.local/bin` link managed-machine relies on.
Kilo Code's installer puts `kilo` in `$HOME/.kilo/bin` and exposes only
`--no-modify-path`; without a link, `setup kilocode` finishes but `kilo` is not
found on PATH.

## Target

- Optional `bin_dir` catalog field on `official-cli` rows, relative to `$HOME`
  (e.g. `.kilo/bin`).
- `install_official_cli` links `$HOME/$bin_dir/$command` into
  `~/.local/bin/$command` before running the installer (an already-present
  vendor binary, from a partial setup or the tool's own updater, is linked
  without re-downloading) and again after the installer runs.
- The link follows the opencode `~/.opencode/bin` rules: idempotent; a
  user-managed file at the link is left alone and reported as an error, never
  clobbered.
- `managed-machine status` reports the row through `~/.local/bin` (already on
  the status PATH via `ensure_status_path`), so `kilo` shows its version once
  linked.
- Rows without `bin_dir` behave exactly as before.

## Acceptance

- `setup kilocode` installs `kilo`, creates `~/.local/bin/kilo` → `~/.kilo/bin`,
  and reports `kilo` on PATH.
- Re-running is a no-op and does not re-download.
- A user-managed `~/.local/bin/kilo` is never replaced.
- Missing vendor binary after install still fails with the existing
  "not found on PATH" message.
- Full test suite passes.

## Replay

`/bin/bash tests/setup-agent-clis.test.sh`. After a formula release,
`managed-machine setup kilocode` installs the vendor CLI and runs
`config/kilocode`.
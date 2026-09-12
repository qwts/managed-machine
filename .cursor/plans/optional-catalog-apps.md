# Optional catalog apps

## Intent

Fleet-default apps stay in `apps.json` and install on every machine. On-demand apps can live in the same catalog so an agent can run `managed-machine setup <name>`, but they must not install during bootstrap or `--update`.

## Target

Optional field `auto` on a catalog row. Omitted or `true` means fleet-default. `false` means setup-only. Any other value fails closed.

## Acceptance

- Rows with no `auto` key still install on bootstrap and `--update`.
- A row with `"auto": false` is skipped by those walks and succeeds via `managed-machine setup <name>`.
- Invalid `auto` fails before any install.
- Help lists on-demand names as `(setup only)`.
- The setup list marks names whose install is already present with `✓`, using the same "done" state each install engine checks: a Homebrew receipt plus the bundle on disk for signed casks (a receiptless occupier is `adopt` territory, not an install), a Team ID-verified bundle for vendor DMGs, a formula receipt (batched `brew list` snapshots, not one brew call per row), or the CLI command on `PATH`. `setup-*` wrappers resolve through their `install_catalog_app` call, and infrastructure scripts check their own markers — `git-hooks` requires the managed hooksPath/dispatcher plus gitleaks, `agent-bot` requires the reviewed runtime plus a doctor gate that passes or fails only on a specific App's lazily-provisioned credentials. State that cannot be determined simply shows no mark. The listing refreshes the persistent managed-machine-config checkout (pull-only, never over a dirty tree) before reading the catalog, so merged rows appear without waiting for `setup-gh`. The formula pins the bundled config seed to a `managed-machine-config` tag+revision that `scripts/release` creates and rewrites, so a release ships a known catalog snapshot rather than whatever `main` is at install time.

## Replay

`/bin/bash tests/catalog.test.sh`, `/bin/bash tests/bootstrap.test.sh`, `/bin/bash tests/setup-list-status.test.sh`, and `/bin/bash tests/cli.test.sh`. After a formula release, an `"auto": false` config row is skipped by `--update` and installed by `setup <name>`.

## Status

completed

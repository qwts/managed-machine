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
- The setup list marks names whose install is already present with `✓`: catalog rows check the receipt their install engine consults (batched `brew list` snapshots for casks and formulae, a staged bundle for desktop apps, the CLI command on `PATH`), `setup-*` wrappers resolve through their `install_catalog_app` call, and infrastructure scripts check their own markers. State that cannot be determined simply shows no mark.

## Replay

`/bin/bash tests/catalog.test.sh`, `/bin/bash tests/bootstrap.test.sh`, `/bin/bash tests/setup-list-status.test.sh`, and `/bin/bash tests/cli.test.sh`. After a formula release, an `"auto": false` config row is skipped by `--update` and installed by `setup <name>`.

## Status

completed

# local-bin Cellar ownership

## Intent

`setup-bin` must work from a Homebrew install. The bundled `libexec/local-bin` is prefix-owned; Git 2.35+ refuses that from the invoking user.

## Target

Trust that exact path for the pin/fetch/checkout git commands only. Do not write `safe.directory` into the user gitconfig.

## Acceptance

- `tests/setup-bin.test.sh` passes, including a stub that fails unless `-c safe.directory` is set.
- A non-admin invoking user can complete `setup-bin` against `/opt/homebrew/Cellar/managed-machine/*/libexec/local-bin`.

## Replay

`/bin/bash tests/setup-bin.test.sh`

## Status

completed

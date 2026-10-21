---
name: winget-provider
status: active
overview: The built-in WinGet provider for the Windows targets (S3 X02) — read-only planning, a pinned version, an already-elevated machine scope and no custom roots.
related_prs: []
---

# WinGet provider

## Intent

The Windows targets had no installation provider: `internal/provider/brew`
covers macOS and Linux only. WinGet ships with Windows, so it is the
provider for `windows/arm64` and `windows/amd64`. Like every provider it
plans read-only, pins what apply will do, never elevates and never writes
outside what it planned.

## Target

- Package `core/internal/provider/winget`, provider name `winget`, registered
  in the `providers` function of `core/cmd/managed-machine/main.go`.
- One recipe option, `package_id`: letters, digits, `.`, `-` and `_`, with at
  least one dot. The package is the package id.
- Targets: `user` is the winget user scope, `global` is the machine scope and
  is honored only when the process is already elevated (`Elevated func() bool`;
  the default reads `TOKEN_ELEVATION` from the current process on Windows and
  is false everywhere else). `custom:<path>` is refused: winget installs into
  its own scopes, so there is no custom root and no privilege question about
  a directory.
- The tool is looked up in the PATH of the injected environment with an
  injectable `LookPath`; its absolute path and the winget version are a
  precondition (`software`, subject `winget`) that `Check` re-observes, so a
  moved or changed winget rejects the plan before anything runs.
- Planning reads `winget --version`, `winget show --id <id> --exact` and
  `winget list --id <id> --exact`, tolerates the progress bars winget prints,
  and writes nothing. The resolved version is pinned in the operation params
  (`package`, `version` and, for an install, `scope`); `Step` accepts no other
  param set.
- An installed package at the planned version plans no operation; another
  installed version plans an update; Verify re-reads `winget list` and requires
  the planned version.

## Acceptance

- Unit evidence: a fake runner models the installed state and the tables
  winget prints; no test starts the real winget.
- The user target plans an install with `--scope user`, the global target an
  install with `--scope machine` and is refused with `ErrPrivilege` in a plain
  process, and a custom root is refused with `ErrUnsupported`.
- A missing winget is `ErrMissing`, an unknown package is `ErrUnsupported`,
  and a bad option set is refused before any process starts.
- Every invocation carries `--source winget --accept-source-agreements
  --disable-interactivity` and the explicit environment of the injected base
  environment, unchanged.
- A failed apply reports the hex exit code; a verification that finds another
  version, or no package at all, fails.

## Replay

```
cd core && go test ./internal/provider/winget
```

The provider never elevates and never installs outside the scope it planned.
Real installs belong to the S3 gate on the Windows runners where winget is
present, with explicit evidence where it is not.

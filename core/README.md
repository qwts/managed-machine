# managed-machine Go core

Module `github.com/qwts/managed-machine/core` (ADR-0016, ADR-0017). It holds
the portable core during the compatibility window; the Bash CLI at the
repository root remains the installed product until S6 moves packaging to the
compiled binary, when this module is promoted to the repository root.

| Path | Purpose |
| --- | --- |
| `cmd/managed-machine` | Entry point. Exposes the shared boundary and the read-only S1 commands `platform`, `software` (discovery only) and `skill`. |
| `cmd/mm-catalog-report` | F05 migration tool that classifies a legacy app catalog against the F04 schema. Not part of the CLI contract. |
| `cmd/mm-walkthrough` | F08 first-slice gate and the T01-T05 per-target evidence run. Not part of the CLI contract. |
| `internal/target` | The five canonical platform identifiers of ADR-0014 as amended 2026-10-21 (`darwin/arm64`, `linux/arm64`, `linux/amd64`, `windows/arm64`, `windows/amd64`), host-fact normalization onto them, and the native-architecture probe (`uname -m`; `IsWow64Process2` on Windows). Identifiers given as input accept no aliases. |
| `internal/output` | Ordered records serialized as compact JSON Lines (default), one JSON document (`--json`) or a readable table (`--human`): fixed key order, deterministic bytes, optional fields omitted and never null, `custom:<path>` target encoding. |
| `internal/diag` | The accepted diagnostic identifier registry (`MM_<COMMAND>_<CONDITION>`) with its exit codes 1–4, the stderr JSON form and the `--human` form `MM_<ID>: <message>`. |
| `internal/cli` | Flag parsing shared by every command: `-h`/`--help` precedence (no action runs), `--json`/`--human` mutual exclusion (`MM_USAGE`, exit 2), stdout for records and help only, stderr for diagnostics only, exit codes from the registry. |
| `internal/schema` | Strict YAML decoding shared by every declarative document: unknown fields, duplicate keys, a second document, anchors, aliases and explicit tags are rejected. Load failures carry a kind (`unknown-provider`, `unknown-option`, `conflict`, `invalid-version`, `digest-mismatch`, ...) so later commands map them to diagnostic identifiers. |
| `internal/registry` | The versioned provider registry. Built-in providers (`brew`, `uv`, `npm`, `winget`, `download`) and extension providers register the same way, by option schema only; a second provider with a taken name is a conflict, never an override. The registry never claims platform support. |
| `internal/definition` | The accepted F04 minimal software schema (two levels: software, recipe). Platforms are exact canonical targets, installation targets are `global`/`user`/`custom`, options are checked against the provider's schema. Lists are normalized to canonical order, so declaration order never changes the output. |
| `internal/source` | Source snapshots with an explicit identity (`builtin`, `local`, `https`, `gist` plus a stable location, never a host path), a content revision and a per-file digest; catalog merging, where an id defined by two sources is a conflict; normalization to JSON Lines. |
| `internal/extension` | Extension manifest schema v1 and protocol `mm-extension-1` (ADR-0013): discovery reads the manifest from an `fs.FS` and verifies pinned executables and assets by SHA-256 without running anything; `Describe` runs the verified executable once and checks it matches its manifest. |
| `internal/protocol` | The `mm-extension-1` client: one JSON request line in, exactly one JSON response line out, with a bounded size, a timeout and an explicit environment. Text, extra lines, wrong ids or protocol versions are violations, never parsed. |
| `internal/software` | `managed-machine software` for S1 (F06): discovery over the built-in illustrative catalog plus one optional local source named by `MANAGED_MACHINE_SOURCE_DIR`, filtered to the current target and intersected with `--category`, `--provider`, `--recipe` and `--global`/`--user`/`--custom`; one record per recipe. `--install` is rejected with `MM_SOFTWARE_INSTALL_UNAVAILABLE` (exit 2) until S2. `Select` is the pure recipe-selection resolver (recipe, provider, desired preference, sole compatible recipe; no catalog-order fallback), reachable only from fixtures in S1. |
| `internal/statedir` | The ADR-0012 platform state directory (`~/Library/Application Support/managed-machine`, `$XDG_DATA_HOME/managed-machine` or `~/.local/share/managed-machine`, `%LOCALAPPDATA%\managed-machine`), overridable by `MANAGED_MACHINE_STATE_DIR` for isolated harnesses. It only computes the path. |
| `internal/plan` | Plan document and results-journal schema v1 (ADR-0012): fixed key order, plan ids `plan-<UTC second>-<4 hex>` in ISO 8601 basic form (`plan-20261021T120000Z-3f9c`; no colon, so the id is a valid Windows file name), the states `saved`, `executing`, `applied`, `failed`, `rejected` and `superseded` with their allowed transitions, atomic saves, supersession of a saved plan for the same command, name, target, account and inputs, and selection (explicit reference, else the newest eligible plan by creation time then id). Journal appends are synced. The store holds an OS file lock for its whole use, so a plan found `executing` is recovered as `failed` with an `interrupted` receipt. |
| `internal/execute` | The sequential executor: bindings, pinned digests, provider versions and preconditions are re-verified first (any change rejects the plan and names it); every performer is resolved before anything runs; each operation is journaled as started, applied and independently verified; the first failure stops the run and the receipt lists the operations not run. A plan runs at most once. |
| `internal/provider/brew` | The built-in Homebrew formula provider (S2 E02). Planning runs `brew --version` and `brew info --json=v2 --formula` and resolves the version including its rebuild revision. An installed formula plans no operation. Apply runs `brew install --formula`, and Verify independently requires the planned version in `brew list --versions`. Only `<prefix>/bin/brew` is ever started, with an argv and an explicit `HOMEBREW_NO_*` environment. A prefix not writable by the current user fails the plan: it never elevates or runs brew as the prefix owner. Casks, custom roots and other options are rejected. |
| `internal/skill` | `managed-machine skill` (F07): the core bundle (embedded) and the bundles of installed, pinned extensions under `MANAGED_MACHINE_EXTENSIONS_DIR`, served as listing, content and pointer-only search records that all carry `bundle`, `version`, `revision` and `tokens_est`. Content is chunked within `MaxResponseBytes` (4096) in both serialized forms, with version-bound `--next` handles; mandatory caveats lead every first chunk and a bundle whose caveats cannot fit is rejected at load; items over `MaxItemBytes` (64 KiB) are `MM_SKILL_CONTENT_OVERSIZED`; search returns at most `MaxSearchRecords` (50) hits per page with cursors bound to the query, filters and bundle revisions. Reading never runs anything, and the name `path` stays with the legacy `skill path`. |

The contract this implements is the v1 specification's "Output and diagnostics
contract" in the documentation repository. Three identifiers were added with this
module and recorded there: `MM_PLATFORM_UNSUPPORTED` (the host is outside the
five targets, exit 1), `MM_PLATFORM_PROBE_FAILED` (the native-architecture
probe failed, exit 1) and `MM_OUTPUT_WRITE_FAILED` (records could not be
written to stdout, exit 1).

## Toolchain and evidence

Go 1.27.3 is pinned by the `go` directive in `go.mod`; CI runs with
`GOTOOLCHAIN=local` and `CGO_ENABLED=0`, so the toolchain changes only by a
pull request that edits `go.mod`. `.github/workflows/go-core.yml` runs `gofmt`,
`go vet` and `go test ./...` on the five native hosted runners (`macos-15`,
`ubuntu-24.04-arm`, `ubuntu-24.04`, `windows-11-arm`, `windows-2025`) and
checks that the built binary's `platform` record names the runner's target
without emulation. Each job then runs the F08 walkthrough (below) and attaches
its evidence record, and the `first-slice gate (F08)` job accepts the five
records only when they carry one source revision, every walkthrough passed
natively and the platform-independent outputs are byte-identical. A target is tested only when that workflow passed on its
native runner for the exact revision.

## Developing

```
cd core
go test ./...                                   # all packages
go test ./internal/cli -run TestPlatformGoldens -update   # re-record goldens after an intended change
go test ./internal/source -run TestCatalogGolden -update  # re-record the normalized catalog golden
go test ./internal/software -run TestDiscoveryGoldens -update  # re-record the software command goldens
go test ./internal/skill -update                               # re-record the skill goldens and the caveat-fit report
go run ./cmd/managed-machine platform [--json|--human]
```

Goldens under `internal/cli/testdata` are byte-exact; review every diff before
re-recording.

## First-slice walkthrough and capability limits (F08)

S1 delivers read-only discovery and guidance. It does not install software,
and it is not a release. `cmd/mm-walkthrough run` drives a built binary through
the S1 workflow in an isolated home whose paths contain spaces and non-ASCII
characters:

1. `platform`, then `software --available` over the built-in catalog plus an
   external local definition, with category, provider and absent custom-root
   filters.
2. Inspecting a chosen recipe with `--recipe` and with `-h`.
3. `skill` listing, every content kind, a reference, pointer-only `--search`,
   and an oversized extension procedure read to the end through `--next`.
4. `--install` for each target, which must fail with
   `MM_SOFTWARE_INSTALL_UNAVAILABLE` (exit 2), and usage errors, which must
   produce exactly one F03 diagnostic line.

Every query runs in the default, `--json` and `--human` modes, and the record
sets must agree. Guards fail the run when any harness path is created, removed
or rewritten, when the custom root appears, when the extension's executable
runs, or when an installer, provider, package-manager or elevation program is
started from `PATH`. On macOS the job also re-runs the F01 characterization
goldens (`tests/*-characterization.test.sh`) and embeds their case counts. The
evidence record carries digests and harness-relative names only.

```
go test ./cmd/mm-walkthrough        # guard negatives and the real-core run
go build -o mmw ./cmd/mm-walkthrough && go build -o mm ./cmd/managed-machine
./mmw run -binary ./mm -target "$(go env GOOS)/$(go env GOARCH)" -evidence walkthrough.json
```

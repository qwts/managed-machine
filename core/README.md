# managed-machine Go core

Module `github.com/qwts/managed-machine/core` (ADR-0016, ADR-0017). It holds
the portable core during the compatibility window; the Bash CLI at the
repository root remains the installed product until S6 moves packaging to the
compiled binary, when this module is promoted to the repository root.

| Path | Purpose |
| --- | --- |
| `cmd/managed-machine` | Entry point. Exposes the shared boundary and the read-only `platform` probe until the command contracts land (F04 onward). |
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
without emulation. A target is tested only when that workflow passed on its
native runner for the exact revision.

## Developing

```
cd core
go test ./...                                   # all packages
go test ./internal/cli -run TestPlatformGoldens -update   # re-record goldens after an intended change
go test ./internal/source -run TestCatalogGolden -update  # re-record the normalized catalog golden
go run ./cmd/managed-machine platform [--json|--human]
```

Goldens under `internal/cli/testdata` are byte-exact; review every diff before
re-recording.

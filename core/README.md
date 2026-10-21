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

The contract this implements is the v1 specification's "Output and diagnostics
contract" in the documentation repository. Two identifiers were added with this
module and recorded there: `MM_PLATFORM_UNSUPPORTED` (the host is outside the
five targets, exit 1) and `MM_PLATFORM_PROBE_FAILED` (the native-architecture
probe failed, exit 1).

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
go run ./cmd/managed-machine platform [--json|--human]
```

Goldens under `internal/cli/testdata` are byte-exact; review every diff before
re-recording.

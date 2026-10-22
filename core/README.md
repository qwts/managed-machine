# managed-machine Go core

Module `github.com/qwts/managed-machine/core` (ADR-0016, ADR-0017). It holds
the portable core during the compatibility window; the Bash CLI at the
repository root remains the installed product until S6 moves packaging to the
compiled binary, when this module is promoted to the repository root.

| Path | Purpose |
| --- | --- |
| `cmd/managed-machine` | Entry point. Exposes the shared boundary, the read-only S1 commands `platform` and `skill`, `software` (discovery and S2/S3 installation), `config` (discovery and S4 C02 planning, applying, checking and status), `status` (S4 C08 read-only `software`, `config` and `flavor` section reporting) and `fleet` (S5 L02 read-only `--keys`, `--access` and `--refresh` actions over key directories and authorization policy). |
| `cmd/mm-catalog-report` | F05 migration tool that classifies a legacy app catalog against the F04 schema. Not part of the CLI contract. |
| `cmd/mm-walkthrough` | F08 first-slice gate and the T01-T05 per-target evidence run, the S2 install gate (issue #273), the S3 provider gate (issue #285) and the S4 configuration gate (issue #301). Not part of the CLI contract. |
| `internal/target` | The five canonical platform identifiers of ADR-0014 as amended 2026-10-21 (`darwin/arm64`, `linux/arm64`, `linux/amd64`, `windows/arm64`, `windows/amd64`), host-fact normalization onto them, and the native-architecture probe (`uname -m`; `IsWow64Process2` on Windows). Identifiers given as input accept no aliases. |
| `internal/output` | Ordered records serialized as compact JSON Lines (default), one JSON document (`--json`) or a readable table (`--human`): fixed key order, deterministic bytes, optional fields omitted and never null, `custom:<path>` target encoding. |
| `internal/diag` | The accepted diagnostic identifier registry (`MM_<COMMAND>_<CONDITION>`) with its exit codes 1–4, the stderr JSON form and the `--human` form `MM_<ID>: <message>`. |
| `internal/cli` | Flag parsing shared by every command: `-h`/`--help` precedence (no action runs), `--json`/`--human` mutual exclusion (`MM_USAGE`, exit 2), stdout for records and help only, stderr for diagnostics only, exit codes from the registry. |
| `internal/schema` | Strict YAML decoding shared by every declarative document: unknown fields, duplicate keys, a second document, anchors, aliases and explicit tags are rejected. Load failures carry a kind (`unknown-provider`, `unknown-option`, `conflict`, `invalid-version`, `digest-mismatch`, ...) so later commands map them to diagnostic identifiers. |
| `internal/registry` | The versioned provider registry. Built-in providers (`brew`, `uv`, `npm`, `winget`, `go`, `download`) and extension providers register the same way, by option schema only; a second provider with a taken name is a conflict, never an override. The registry never claims platform support. |
| `internal/registry` | The versioned provider registry. Built-in providers (`brew`, `uv`, `npm`, `cargo`, `winget`, `download`) and extension providers register the same way, by option schema only; a second provider with a taken name is a conflict, never an override. The registry never claims platform support. |
| `internal/definition` | The accepted F04 minimal software schema (two levels: software, recipe) and the kinds a definition document may declare (`software`, `config`, `flavor`; a document without a kind is a software definition). Platforms are exact canonical targets, installation targets are `global`/`user`/`custom`, options are checked against the provider's schema. Lists are normalized to canonical order, so declaration order never changes the output. |
| `internal/catalog` | The definition sources the core reads: the embedded built-in catalog (software and configuration definitions), the optional local source named by `MANAGED_MACHINE_SOURCE_DIR` and the cached snapshots of the configured remote sources. Every source is named by an identity that never carries a host path, and the state directory is computed only when a remote source is configured, so a machine without one never creates it. |
| `internal/config` | The configuration schema and executor of S4 (C01, C02). A `kind: config` definition declares its operations (the `file`, `template`, `structured` and `managed-block` providers), its `activation` map and its `requires[].command`; every failure names the document and the field and is `MM_CONFIG_SOURCE_INVALID`. A template or file source is a file inside the source, so it lives in a subdirectory of it; a remote source, whose index names plain files only, cannot carry one yet. The desired configuration of one target (`desired.yaml`: the inputs, the destination mappings, the software preferences and the flavors) is resolved from the explicit `MANAGED_MACHINE_DESIRED_FILE`, from the platform configuration directory, or from the custom root's own file; a missing, unmapped, extra or disagreeing mapping is `MM_CONFIG_DESIRED_INVALID`, and a definition that does not declare the requested target or platform is `MM_CONFIG_UNSUPPORTED_COMBINATION`. `managed-machine config --available` lists the configuration definitions of every source with the record fields `id`, `categories` and `targets`. `--plan` builds a schema-1 `config` plan pinning the definition and every operation's `source` file, checks `requires` (`command` on `PATH` or `<customRoot>/bin`, else an applied `software --install` receipt), detects conflicts and orphaned owned entries, and emits the plan record plus one operation record per change; `--apply` selects and executes the saved plan through `internal/execute`, appends to the ownership ledger, and emits apply records plus rendered `activation` records; `--check` compares rendered desired state with destinations on disk (`MM_CONFIG_DRIFT_FOUND` exit 3 on drift, `MM_CONFIG_CHECK_INCOMPLETE` exit 4 when unreadable); `--status` reads only the ownership ledger. |
| `internal/config/render` | The logic-free substitution engine (D4): `{{ inputs.<name> }}`, `{{ destinations.<name> }}` and `{{ settings.<key> }}`, with the inner whitespace free and the values inserted verbatim. There are no conditionals, loops, filters, escapes or `text/template`; an unknown token, an unknown name and an unbalanced brace are errors. |
| `internal/config/ledger` | The append-only JSON Lines ownership ledger (D3): `{schema_version, kind, path, id, format, sha256, config, operation, target, account, plan, at, state}` keyed by `(target, path, kind, id)` with latest-record-wins semantics, stored at `<state>/ownership.jsonl` for `user`/`global` targets and `<root>/.managed-machine/ownership.jsonl` for custom targets. |
| `internal/config/secret` | Secret reference resolution (S4 C05, C06; D5): `secret:<name>` in a desired input resolves from `MM_SECRET_<NAME>` through an injected `getenv` seam first (with `<NAME>` uppercased and every character outside `[A-Z0-9]` replaced by `_`), then from `pass show <name>` through the `internal/provider.Runner` seam (argv only, inherited environment, first stdout line, `pass` stderr never echoed, missing `pass` or nonzero exit treated as unresolved), records digest-only `{"name":"secret:<name>","sha256":...}` assets computed as `sha256(name || 0x00 || value)` regardless of source, and fails with `MM_CONFIG_SECRET_UNRESOLVED` naming only the reference when neither source resolves. |
| `internal/config/provider/file` | The whole-file configuration provider (S4 C02). Planning hashes the source asset into `bindings.assets`, plans `conflict replace-file <path>` when an existing destination is not owned by the same configuration in the ledger, plans no operation when the owned file already matches, and plans `create` or `update` otherwise; Apply writes atomically and Verify independently re-reads and checks the SHA-256 digest before appending to the ledger. |
| `internal/config/provider/template` | The template configuration provider (S4 C02). Planning pins the raw template source file in `bindings.assets`, renders `settings` first with `inputs` and `destinations` and then renders the template with `inputs`, `destinations` and `settings`, and shares whole-file conflict detection, atomic write, independent verify and ledger ownership with `internal/config/provider/file`. |
| `internal/config/provider/block` | The managed-block configuration provider (S4 C04). Planning renders `content` through `internal/config/render`, wraps it in `<comment> managed-machine:begin <block_id>` / `<comment> managed-machine:end <block_id>` markers (default comment `#`) using the destination file's line-ending convention, plans `conflict foreign-block <path>:<block_id>` when markers for `block_id` exist without a matching `block` ledger entry, plans no operation when the owned block already matches, and plans `create` or `update` otherwise; Apply creates, appends, replaces in place or deletes only the marked block while keeping everything outside the markers byte-identical, and Verify independently re-reads and checks the block before appending a `block` record to the ledger. |
| `internal/config/provider/structured` | The structured key-level configuration provider for JSON and TOML files (S4 C03; TOML parsed with `github.com/pelletier/go-toml/v2`). Requires an explicit `format: json|toml` (never inferred from the path) and dot-separated key paths with scalar or string-list values; preserves unrelated keys, their order and CRLF line endings, rejects JSON with comments as malformed, does not preserve TOML comments (recorded limitation), plans `conflict foreign-key <path>:<key>` when an existing key has no `key` ledger entry, creates absent files with only the managed keys, reports per-key `actual=... desired=...` drift on `--check`, and records `key` ledger entries only. |
| `internal/compose` | `kind: flavor` definitions and the pure composer (S4 C07, D6, ADR-0011). A flavor definition declares `software` (`id`, `target`, optional `version`) and `config` (`id`, `target`, optional `inputs`) entries whose `target` must be `global`, `user` or `custom` without a root (`custom:<path>` and `root` are rejected as `MM_CONFIG_SOURCE_INVALID`); `Compose` deduplicates by resource identity `software:<id>@<target>` and `config:<id>@<target>:<operation>`, compares version strings as opaque strings, and reports destination collisions with differing values as `config:<id>@<target>:<operation>` conflicts while deduplicating identical destination-and-value pairs. |
| `internal/status` | `managed-machine status` (S4 C08, D6, D7): read-only status reporting for `--section software|config|flavor [name]` with fixed record key order `section`, `resource`, `target`, `state`, `facts`, `evidence`, `at`. `software` reports only applied receipts in the plan store (`evidence: "recorded"`, `state: "unknown"`, `facts.receipt`, `at` = receipt timestamp; never rendered as `present`); `config` combines the ownership ledger with fresh read-only on-disk inspection (`present`/`observed` when the digest matches, `absent`/`observed` when deleted or modified, `unknown`/`recorded` when unreadable) and reports an absent custom root as one `config` record (`resource: "root"`, `state: "absent"`) without creating a directory; `flavor` composes member status via `internal/compose` (D6). An unreadable ledger marks the report partial and exits 4 with `MM_STATUS_PARTIAL` while other sections still print. Opens no plan-store lock, writes nothing and spawns no process. |
| `internal/fleet` | `managed-machine fleet` (S5 L02, L04; D1, D2, D3, D4, D5, D7, ADR-0010): read-only `--keys` (default when no action flag is given; optional `<owner>` positional filter), `--access` (evaluating authorization for the current OS account via `internal/fleet/authz` and omitting any published key no group approves), `--refresh` (resolving `history[0].version` from `GET <api>/gists/<id>` and caching the verified `GET <api>/gists/<id>/<revision>` snapshot under `<state>/fleet/directory/` with identity `gist:<id>@<revision>`), and controlled local publication `--publish` / `--unpublish` (atomically rewriting `keys/<owner>.yaml` in a `path:` directory or staging `<state>/fleet/staged/<owner>.yaml` with `next_step` for a `gist:` directory without contacting GitHub, and recording `<state>/fleet/enrollment.json`). `--user` is the sole target; `--global` and `--custom` fail with `MM_USAGE` naming the D1 rule that `authorized_keys` lives in OS accounts, not system-wide or in Souls. Spawns no external processes. |
| `internal/fleet/policy` | Fleet authorization policy schemas and read-only checkout loader (S5 L01; D2, D6, ADR-0010): `kind: access-group` (`fleet/access-groups/*.yaml`, normalizing lists to deterministic JSON and rejecting duplicate group ids, duplicate fingerprints, bad `SHA256:` digests, bad account names and empty `maintained_by` with `MM_FLEET_POLICY_INVALID`), `kind: key-directory-source` (`fleet/key-directory.yaml`, requiring exactly one of lowercase hex `gist` or absolute `path`), and `kind: fleet-cutover` (`fleet/cutover.yaml`, requiring RFC 3339 UTC `cut_over_at` and non-empty `reviewed_by`). Resolves the policy directory from `MANAGED_MACHINE_POLICY_DIR` or `<state>/managed-machine-config`, rejects `kind: key-directory` documents under `fleet/` as `MM_FLEET_POLICY_INVALID`, reports missing policy directories as `MM_FLEET_POLICY_UNAVAILABLE`, and pins every loaded policy document's SHA-256 digest as `policy:<relative path>`. |
| `internal/fleet/directory` | Fleet key-directory loader, standard-library OpenSSH public-key fingerprint calculator and `machine_id` derivation (S5 L01; D3, ADR-0010). Validates `kind: key-directory` entries (`<id>.yaml` base name matching `id`, `revision >= 1`, RFC 3339 UTC `published_at`, optional printable `label` up to 64 characters), parses `ssh-ed25519`, `ssh-rsa`, `ecdsa-sha2-nistp256|384|521`, `sk-ssh-ed25519@openssh.com` and `sk-ecdsa-sha2-nistp256@openssh.com` keys and verifies the wire blob's embedded type string, computes `SHA256:<unpadded base64>` fingerprints and `sha256-<unpadded base64url>` machine ids from the key bytes, marks mismatched recorded fingerprints `invalid, fingerprint-mismatch`, unparseable keys `invalid, malformed-key`, and duplicate blobs across the directory `invalid, duplicate`, and rejects policy or definition documents inside a key directory with `MM_FLEET_DIRECTORY_INVALID`. |
| `internal/fleet/authz` | The pure fleet authorization evaluator (S5 L01; D1, D4). Takes access groups, a parsed key directory, an OS account name and a target `GOOS` (matching account names case-insensitively on Windows and case-sensitively on Unix per D1), returns the authorized `(owner, key)` pairs in owner-then-fingerprint order (rendering `<type> <base64 blob> <owner>` lines with published comments dropped), excludes invalid keys even when their recorded or computed fingerprint is approved, excludes unapproved keys, and reports approved-unpublished fingerprints in deterministic order. |
| `internal/source` | Source snapshots with an explicit identity (`builtin`, `local`, `https`, `gist` plus a stable location, never a host path), a content revision and a per-file digest; catalog merging, where an id defined by two sources is a conflict; normalization to JSON Lines. The definition files of a source are the `*.yaml` files directly under it: a file that is not one is rejected rather than skipped, and a subdirectory is left to the assets the definitions may reference. A loader takes the documents of its own kind and ignores the others, so software discovery never sees a configuration definition. |
| `internal/source/remote` | The configured remote source transports (an https index document named by `MANAGED_MACHINE_SOURCE_URL`, a pinned gist named by `MANAGED_MACHINE_SOURCE_GIST`, and the fleet key-directory gist named by `fleet/key-directory.yaml` with base URL override `MANAGED_MACHINE_GITHUB_API`) and the content-addressed snapshot caches under `<state>/sources/` and `<state>/fleet/directory/`: TLS verification, bounded sizes and timeouts, redirects only to https on the source's own host, every file verified against its digest, and no host path in an identity, a revision or a diagnostic. Fetching happens only on `software --refresh` or `fleet --refresh`; discovery and read-only fleet actions read the cache. |
| `internal/extension` | Extension manifest schema v1 and protocol `mm-extension-1` (ADR-0013): discovery reads the manifest from an `fs.FS` and verifies pinned executables and assets by SHA-256 without running anything; `Describe` runs the verified executable once and checks it matches its manifest. |
| `internal/protocol` | The `mm-extension-1` client: one JSON request line in, exactly one JSON response line out, with a bounded size, a timeout and an explicit environment. Text, extra lines, wrong ids or protocol versions are violations, never parsed. |
| `internal/software` | `managed-machine software` for S1 (F06): discovery over the built-in illustrative catalog plus one optional local source named by `MANAGED_MACHINE_SOURCE_DIR`, filtered to the current target and intersected with `--category`, `--provider`, `--recipe` and `--global`/`--user`/`--custom`; one record per recipe. `--install` (S2 E03) selects exactly one recipe with `Select` (recipe, provider, desired preference, sole compatible recipe; no catalog-order fallback) and hands it to `internal/install`; its result record is software, recipe, provider, target, resolved_version, outcome (`ok` or `unchanged`), plan. The remote sources configured by `MANAGED_MACHINE_SOURCE_URL` and `MANAGED_MACHINE_SOURCE_GIST` join the catalog through their cached snapshots, never through an implicit fetch: `--refresh` fetches them and reports one record per source (`source`, `revision`, `files`, `outcome`), a configured but uncached source is `MM_SOFTWARE_SOURCE_UNAVAILABLE` (exit 2), and a failed fetch is `MM_SOFTWARE_SOURCE_FETCH_FAILED` (exit 1). |
| `internal/install` | The install path: plans with the selected recipe's provider from the registry (read-only; S3 X01), builds a schema-1 plan bound to the account, machine id, platform, source revision and provider version, saves it in the state directory and runs it once through `internal/execute`. A provider without an executor is `MM_SOFTWARE_INSTALL_UNAVAILABLE` before anything is written. |
| `internal/provider` | The provider contract (S3 X01): `Name`, `Version`, read-only `Plan`, `Check`, `Step`, the shared `Request`/`Resolution`, the refusal kinds `ErrUnsupported`, `ErrPrivilege`, `ErrMissing`, and the argv-only process seam `Runner`/`Exec`. |
| `internal/statedir` | The ADR-0012 platform state directory (`~/Library/Application Support/managed-machine`, `$XDG_DATA_HOME/managed-machine` or `~/.local/share/managed-machine`, `%LOCALAPPDATA%\managed-machine`), overridable by `MANAGED_MACHINE_STATE_DIR` for isolated harnesses. It only computes the path. |
| `internal/plan` | Plan document and results-journal schema v1 (ADR-0012): fixed key order, plan ids `plan-<UTC second>-<4 hex>` in ISO 8601 basic form (`plan-20261021T120000Z-3f9c`; no colon, so the id is a valid Windows file name), the states `saved`, `executing`, `applied`, `failed`, `rejected` and `superseded` with their allowed transitions, atomic saves, supersession of a saved plan for the same command, name, target, account and inputs, and selection (explicit reference, else the newest eligible plan by creation time then id). Journal appends are synced. The store holds an OS file lock for its whole use, so a plan found `executing` is recovered as `failed` with an `interrupted` receipt. |
| `internal/execute` | The sequential executor: bindings, pinned digests, provider versions and preconditions are re-verified first (any change rejects the plan and names it); every performer is resolved before anything runs; each operation is journaled as started, applied and independently verified; the first failure stops the run and the receipt lists the operations not run. A plan runs at most once. |
| `internal/provider/brew` | The built-in Homebrew formula provider (S2 E02). Planning runs `brew --version` and `brew info --json=v2 --formula` and resolves the version including its rebuild revision. An installed formula plans no operation. Apply runs `brew install --formula`, and Verify independently requires the planned version in `brew list --versions`. Only `<prefix>/bin/brew` is ever started, with an argv and an explicit `HOMEBREW_NO_*` environment. A prefix not writable by the current user fails the plan: it never elevates or runs brew as the prefix owner. Casks, custom roots and other options are rejected. |
| `internal/provider/uv` | The built-in uv tool provider (S3 X02). Planning reads `uv --version`, the installed tools and the distribution's version in the PyPI index (over https), and writes nothing at all: not a custom root, not a cache. A tool is installed for the current user with `uv tool install`, or into a custom root whose tools, Python runtime and caches all live inside it; the root is created by apply alone. The global target and a relative root are refused, and a root the current user may not write fails the plan: the provider never elevates and never installs uv itself. Every process gets an explicit environment without any `UV_*` variable of the base one, so no shell profile or PATH is edited. |
| `internal/provider/npm` | The built-in npm package provider (S3 X03). Planning reads `npm --version` and the registry's metadata over HTTPS — never through npm, so no machine configuration can redirect the lookup — and pins the version it will install. The installed state comes from `npm ls --global --prefix <prefix> --json --depth=0 <package>`: the planned version already installed plans no operation, another installed version plans an update. Apply runs `npm install --global --prefix <prefix> --ignore-scripts --no-fund --no-audit --registry=<registry> <package>@<version>`, so lifecycle scripts never run, and Verify independently requires the planned version. The prefix is the per-user one (`$HOME/.local`, `%APPDATA%\npm`) or the custom root, never the system global prefix; a root the current user cannot write fails the plan: it never elevates. Every process runs with an explicit environment: the base environment without any `npm_config_*` variable, plus the provider's own. |
| `internal/provider/winget` | The built-in WinGet provider for the Windows targets (S3 X02). Planning reads `winget --version`, resolves the version with `winget show --id <id> --exact` and the installed state with `winget list --id <id> --exact`; it writes nothing. A package already installed at the planned version plans no operation, another installed version plans an update. Apply runs `winget install` (or `winget upgrade`) with the pinned version, and Verify independently requires the planned version in `winget list`. The recipe declares one option, `package_id`. The user target is the user scope; the global target is the machine scope and is refused unless the process is already elevated, because managed-machine never elevates. Custom roots are refused: winget installs into its own scopes. Only the winget executable the injected environment names is ever started, with an argv and an explicit environment. |
| `internal/provider/golang` | The built-in Go module provider (S3 X02). Planning is read-only: it reads the go tool's version, reads the installed binary and resolves the module's latest version from the module proxy over HTTPS (`<proxy>/<escaped module>/@latest`), never through the go tool. The resolved version is pinned in the operation, and Apply runs exactly `go install <package>@<version>`; Verify independently requires the planned module version in `go version -m`. Only the located go tool is ever started, with an argv and an explicit environment: `GOBIN` points at the target's bin directory, the toolchain never switches, and a custom root keeps `GOPATH`, `GOMODCACHE` and `GOCACHE` inside itself. The user and `custom:` targets are supported; a global target, a relative root, a root that is not a directory and a root whose nearest existing ancestor is not writable by the current user are refused: the provider never elevates. |
| `internal/provider/cargo` | The built-in Rust crate provider (S3 X03). Planning runs `cargo --version` and `cargo install --list --root`, resolves the crate's highest stable version from the crates.io sparse index over HTTPS and pins it. An installed crate plans no operation; another installed version plans an update. Apply runs `cargo install --locked --root` for exactly the pinned version, and Verify independently requires the planned version in `cargo install --list`. The targets are the user's cargo root and a custom root; a global target and a relative custom root are refused. The located tool is pinned by path and version, and a root the current user cannot write fails the plan: it never elevates. |
| `internal/skill` | `managed-machine skill` (F07): the core bundle (embedded) and the bundles of installed, pinned extensions under `MANAGED_MACHINE_EXTENSIONS_DIR`, served as listing, content and pointer-only search records that all carry `bundle`, `version`, `revision` and `tokens_est`. Content is chunked within `MaxResponseBytes` (4096) in both serialized forms, with version-bound `--next` handles; mandatory caveats lead every first chunk and a bundle whose caveats cannot fit is rejected at load; items over `MaxItemBytes` (64 KiB) are `MM_SKILL_CONTENT_OVERSIZED`; search returns at most `MaxSearchRecords` (50) hits per page with cursors bound to the query, filters and bundle revisions. Reading never runs anything, and the name `path` stays with the legacy `skill path`. |

The contract this implements is the v1 specification's "Output and diagnostics
contract" in the documentation repository. Three identifiers were added with this
module and recorded there: `MM_PLATFORM_UNSUPPORTED` (the host is outside the
five targets, exit 1), `MM_PLATFORM_PROBE_FAILED` (the native-architecture
probe failed, exit 1) and `MM_OUTPUT_WRITE_FAILED` (records could not be
written to stdout, exit 1). Two more were added with the remote sources:
`MM_SOFTWARE_SOURCE_UNAVAILABLE` (a configured remote source has no cached
snapshot, exit 2) and `MM_SOFTWARE_SOURCE_FETCH_FAILED` (a remote source could
not be fetched, exit 1). Three more came with the configuration schema of S4
C01: `MM_CONFIG_SOURCE_INVALID` (a configuration definition failed validation,
exit 2), `MM_CONFIG_DESIRED_INVALID` (the desired configuration is missing,
malformed, unmapped or disagrees with the command, exit 2) and
`MM_CONFIG_UNSUPPORTED_COMBINATION` (the definition does not declare the
requested target or the host platform, exit 2). Four more came with the
configuration executor of S4 C02 (D8): `MM_CONFIG_SECRET_UNRESOLVED` (a
`secret:<name>` input resolved from neither secret source, exit 1),
`MM_CONFIG_PRIVILEGE_REQUIRED` (a destination or its directory is not writable
by the current user, exit 1), `MM_CONFIG_APPLY_FAILED` (an operation failed or
its independent verify disagreed with the write, exit 1) and `MM_CONFIG_BUSY`
(another execution holds the state directory, exit 1). Eighteen more came with
fleet policy, key directory, authorized-keys management and publication in S5
(D2, D3, D4, D5, D6, D7): `MM_FLEET_POLICY_UNAVAILABLE` (exit 2),
`MM_FLEET_POLICY_INVALID` (exit 2), `MM_FLEET_DIRECTORY_UNAVAILABLE` (exit 2),
`MM_FLEET_DIRECTORY_INVALID` (exit 2), `MM_FLEET_DIRECTORY_FETCH_FAILED`
(exit 1), `MM_FLEET_KEY_INVALID` (exit 2), `MM_FLEET_KEY_UNKNOWN` (exit 2),
`MM_FLEET_NOT_CUT_OVER` (exit 1), `MM_FLEET_CONFLICT` (exit 1),
`MM_FLEET_NO_PLAN` (exit 2), `MM_FLEET_PLAN_MISMATCH` (exit 2),
`MM_FLEET_PLAN_REJECTED` (exit 1), `MM_FLEET_APPLY_FAILED` (exit 1),
`MM_FLEET_BUSY` (exit 1), `MM_FLEET_PRIVILEGE_REQUIRED` (exit 1),
`MM_FLEET_PUBLISH_FAILED` (exit 1), `MM_FLEET_DRIFT_FOUND` (exit 3) and
`MM_FLEET_CHECK_INCOMPLETE` (exit 4).

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
natively and the platform-independent outputs are byte-identical. The same job
runs the S2 install gate, the S3 provider gate and the S4 configuration gate
(below) and attaches their evidence; the `S2 install gate (E05)`,
`S3 provider gate (X07)` and `S4 config gate (C09)` jobs accept the five
records only when they carry one source revision and every gate passed. A
target is tested only when that workflow passed on its
native runner for the exact revision.

## Developing

```
cd core
go test ./...                                   # all packages
go test ./internal/cli -run TestPlatformGoldens -update   # re-record goldens after an intended change
go test ./internal/source -run TestCatalogGolden -update  # re-record the normalized catalog golden
go test ./internal/software -run TestDiscoveryGoldens -update  # re-record the software command goldens
go test ./internal/config -update               # re-record the configuration goldens
go test ./internal/config/render -update        # re-record the substitution engine goldens
go test ./internal/fleet -update                # re-record the fleet command goldens
go test ./internal/fleet/policy -update         # re-record the fleet policy goldens
go test ./internal/fleet/directory -update      # re-record the fleet directory fingerprint goldens
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
4. `--install` of the fixture, whose provider (`download`) has no executor,
   which must fail with `MM_SOFTWARE_INSTALL_UNAVAILABLE` (exit 2) on every
   target before any state is written, and usage errors, which must
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

## The S2 install gate (issue #273)

S2 installs software, so its gate changes a machine. `cmd/mm-walkthrough install`
drives a built binary through the install path in an isolated harness whose
home, state directory, definition source and sentinel directory are its own,
and writes one evidence record per target. The gate refuses to change a
machine outside a GitHub-hosted runner: without `GITHUB_ACTIONS=true` and
`RUNNER_ENVIRONMENT=github-hosted` it fails with "the install gate changes the
machine; it runs only on GitHub-hosted runners" before it starts a process, so
no developer or owner machine is ever touched.

The mode follows the target: the `brew` mode when the target has a usable
Homebrew prefix, the `refusal` mode everywhere else. macOS in the `refusal`
mode is a failure ("macOS must run real installs"): the other targets have no
provider that can install yet.

The `brew` mode installs for real and checks every step:

1. `software ripgrep --install --global` exits 0 with one record whose keys
   are `software`, `recipe`, `provider`, `target`, `resolved_version`,
   `outcome`, `plan` in that order and the outcome `ok`; the resolved version
   is the version `brew list --versions` reports; the saved plan is `applied`
   with one operation, and its journal holds the `started` and `ok` records of
   that operation and a final `applied` receipt.
2. The same command again exits 0 with the outcome `unchanged` under a new
   plan, and that plan is `applied` with no operation.
3. `software mm-gate-fail --install --global`, a fixture whose download never
   succeeds, exits 1 with `MM_SOFTWARE_INSTALL_FAILED`; its plan is `failed`
   with a `failed` receipt, and the retry uses a new plan.
4. `software mm-gate-slow --install --global`, a fixture whose install never
   finishes, is killed as a process group while it runs; the next install
   recovers the killed plan as `failed` with an `interrupted` receipt under a
   new plan of its own.
5. A source served by a local https fixture server is refreshed explicitly
   (`software --refresh`, outcome `refreshed`) and then used to plan an
   install, whose plan names the snapshot identity
   (`https://127.0.0.1:PORT/index.json@sha256:...`).

The `refusal` mode proves the same path refuses: the install exits 1 with
`MM_SOFTWARE_PROVIDER_MISSING` (or `MM_SOFTWARE_PRIVILEGE_REQUIRED` when the
prefix is not writable), and on Windows it exits 2 with
`MM_SOFTWARE_INSTALL_UNAVAILABLE`; the https source is still refreshed; and
no plan is ever written.

Both modes end in the same guards: no guarded program (`sudo`, `osascript`,
`security`, `npm`, `uv`, `winget`, `pip`, `pipx`) was started, and every file
under the harness home, the shell profiles of the runner's home and the
managed-machine configuration directory are byte-identical before and after.
The evidence record carries digests, plan ids and the placeholders `<harness>`,
`<home>` and `<tmp>` instead of the runner's paths, and `install-aggregate`
closes the gate over the five records: one per first-release target, one
source revision, every gate passed and macOS in the `brew` mode.

```
go test ./cmd/mm-walkthrough        # the refusal end-to-end run and the aggregate
go build -o mmw ./cmd/mm-walkthrough && go build -o mm ./cmd/managed-machine
./mmw install -binary ./mm -target "$(go env GOOS)/$(go env GOARCH)" -evidence install.json
./mmw install-aggregate -source-revision "$REVISION" install-*.json
```

## The S3 provider gate (issue #285)

S3 adds providers, so its gate installs software with every provider of the
first release. `cmd/mm-walkthrough providers` drives a built binary through
one install per provider in an isolated harness whose home, state directory,
definition source and sentinel directory are its own, and writes one evidence
record per target: the fields of the install gate with the combination matrix
beside them. Like the S2 gate it refuses to install outside a GitHub-hosted
runner: without `GITHUB_ACTIONS=true` and `RUNNER_ENVIRONMENT=github-hosted`
it fails with "the provider gate installs software; it runs only on
GitHub-hosted runners" before it starts a provider tool, so no developer or
owner machine is ever touched.

The gate writes its own definitions, one small real package per provider, and
proves every combination they declare:

| provider | fixture | package | targets | platforms |
| --- | --- | --- | --- | --- |
| `uv` | `mm-gate-uv` | PyPI `cowsay` | user, custom | all five |
| `npm` | `mm-gate-npm` | npm `semver` | user, custom | all five |
| `cargo` | `mm-gate-cargo` | crates.io `textdistance` | user, custom | all five |
| `go` | `mm-gate-go` | `golang.org/x/tools/cmd/stringer` | user, custom | all five |
| `winget` | `mm-gate-winget` | `jqlang.jq` | user | the two Windows targets |

For every fixture of the runner's own target the gate proves:

1. `software <fixture> --install --user` exits 0 with one record whose keys
   are `software`, `recipe`, `provider`, `target`, `resolved_version`,
   `outcome`, `plan` in that order and the outcome `ok`; the saved plan is
   `applied` with exactly one operation of that provider, and its journal
   holds the `started` and `ok` records of that operation and a final
   `applied` receipt. The installed binary exists where the provider puts it
   and runs: `cowsay --version`, `semver --help`,
   `textdistance levenshtein kitten sitting`, `stringer -help` and
   `jq --version`.
2. The same command again exits 0 with the outcome `unchanged` under a new
   plan, and that plan is `applied` with no operation.
3. `software <fixture> --install --custom <harness root>/custom/<provider>`
   does the same for a custom root, and the binary is inside that root.
   Around that install the gate hashes every file of the harness root: the
   only changes allowed are inside the custom root and the guarded state
   directory. Anything else fails the step and names the paths.
4. `software <fixture> --install --global` exits 2 with
   `MM_SOFTWARE_UNSUPPORTED_COMBINATION` and writes no plan: no fixture
   declares a global target.
5. A relative custom root is refused with the same diagnostic. The `software`
   command resolves a relative `--custom` against the working directory, so
   the rule that a custom root is an absolute path lives in the provider: the
   gate asks the provider itself to plan such an install and requires the
   refusal of the unsupported-combination kind. Planning is read-only, so no
   tool is started and nothing is written.

The WinGet provider installs into its own scopes, so its fixture declares the
user target only and the custom target is refused like the global one.

A provider whose tool is not on the runner's PATH is never skipped: the
install fails with `MM_SOFTWARE_PROVIDER_MISSING` and the combination is
recorded as failed. The workflow provides the toolchains: uv with
`pipx install uv==0.13.4` where it is missing, Rust with the official
`rustup-init.exe` for `aarch64-pc-windows-msvc` on `windows-11-arm`, and
node with `actions/setup-node` pinned by commit.

The guards are the S2 ones with one change: the providers under test are not
shadowed, so their real tools are found. `sudo`, `osascript`, `security`,
`brew`, `pip`, `pipx` and every provider that is not under test on the target
stay sentinels, and the shell profiles of the runner's home and the
managed-machine configuration directory stay byte-identical.

The evidence record carries the install-gate fields, the `combinations` array
(`provider`, `software`, `target_kind`, `expected`, `observed`, `ok`,
`detail`) and the placeholders `<harness>`, `<home>` and `<tmp>` instead of the
runner's paths. `providers-aggregate` closes the gate over the five records:
one per first-release target, one source revision, every gate passed, every
combination proven. It prints the combination matrix: rows are the provider
and the target kind, columns are the five targets, and a cell is `ok`, `FAIL`
or `n-a`.

```
go test ./cmd/mm-walkthrough        # the provider gate's fixtures, guards and aggregate
go build -o mmw ./cmd/mm-walkthrough && go build -o mm ./cmd/managed-machine
./mmw providers -binary ./mm -target "$(go env GOOS)/$(go env GOARCH)" -evidence providers.json
./mmw providers-aggregate -source-revision "$REVISION" providers-*.json
```

## The S4 configuration gate (issue #301)

S4 adds configuration management, observations and flavor composition, and
because configuration touches only files inside an isolated harness, all five
targets run the same case list natively with no refusal mode.
`cmd/mm-walkthrough config` drives a built binary through the D9 cases in an
isolated temp harness whose home, state directory (`MANAGED_MACHINE_STATE_DIR`),
definition source (`MANAGED_MACHINE_SOURCE_DIR`), desired file
(`MANAGED_MACHINE_DESIRED_FILE`), custom root path (with spaces and non-ASCII
characters), fixture `pass` script and sentinel directory are its own, and
writes one sanitized `s4-config-gate` evidence record per target:

1. `ac-t-04-user` and `ac-t-04-custom`: the full AC-T-04 lifecycle (`--plan`,
   `--apply`, `--status`, fresh zero-operation `--plan`, `--apply`, and
   `--apply` again yielding `MM_CONFIG_NO_PLAN` exit 2) for a definition
   exercising all four configuration providers (`file`, `template`,
   `structured`, `managed-block`) and a rendered `activation` record, on both
   the user target and the custom target (resolving `<root>/.managed-machine/desired.yaml`
   and `<root>/.managed-machine/ownership.jsonl` inside the custom root).
2. `check-exit-0`, `check-exit-3` and `check-exit-4`: `--check` returns 0 when
   the destinations match, 3 (`MM_CONFIG_DRIFT_FOUND`) after an external edit,
   and 4 (`MM_CONFIG_CHECK_INCOMPLETE`) against an unreadable destination (a
   directory replacing an owned file, portable across Unix and Windows).
3. `conflict-replace-file`, `conflict-foreign-block`, `conflict-foreign-key`
   and `conflict-flavor-cross-def`: unowned whole files (`conflict replace-file`),
   unowned marker blocks (`conflict foreign-block`), unowned structured keys
   (`conflict foreign-key`) and a cross-definition collision composed through
   two `kind: flavor` fixtures are planned as conflicts and refused by
   `--apply` (and `--check` for the cross-definition conflict) with
   `MM_CONFIG_CONFLICT` while keeping the destination byte-identical.
4. `secret-matrix`: an input resolved from `MM_SECRET_<NAME>` only, an input
   resolved from the harness's fixture `pass` script only, and an input in
   neither source failing with `MM_CONFIG_SECRET_UNRESOLVED` at `--plan`,
   `--check` and `--apply`; the gate then greps the state directory, the custom
   root, every captured stdout/stderr stream and the encoded evidence record
   for both fixture secret values and requires zero hits.
5. `status-sections`: `status --section software`, `status --section config`
   and `status --section flavor` in one run, proving `recorded` (`software`
   receipt with `state: "unknown"`), `observed` (`config` entries verified on
   disk) and `unknown` (`config` entry whose destination is unreadable, plus
   the composed flavor member states) in a single run.
6. `guards`: no guarded program (`sudo`, `osascript`, `security`, `brew`, `uv`,
   `npm`, `cargo`, `winget`, `go`, `pip`, `pipx`, `gpg`) was started, the
   real home's shell profiles and `~/.config/managed-machine` are
   byte-identical, and the custom-target cycle changed nothing outside the
   custom root except the state directory.

`config-aggregate` closes the gate over the five records: one per first-release
target, one source revision, every gate and every D9 case passed.

```
go test ./cmd/mm-walkthrough        # the config gate's fixtures, guards, secret grep and aggregate
go build -o mmw ./cmd/mm-walkthrough && go build -o mm ./cmd/managed-machine
./mmw config -binary ./mm -target "$(go env GOOS)/$(go env GOARCH)" -evidence config.json
./mmw config-aggregate -source-revision "$REVISION" config-*.json
```

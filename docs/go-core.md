# managed-machine Go core

Module `github.com/qwts/managed-machine` at the repository root (ADR-0016,
ADR-0017; promoted from `core/` in S6 R01, issue #327). It holds the portable
core (`cmd/`, `internal/`, `go.mod` and `go.sum` at the repository root); the
Bash CLI at `bin/managed-machine` remains the installed product until S6 moves
packaging to the compiled binary.

| Path | Purpose |
| --- | --- |
| `cmd/managed-machine` | Entry point. Exposes the shared boundary (`--version`, embedding `-ldflags "-s -w -X main.version=<VERSION>"` and reporting `0.0.0-dev` when unflagged), the read-only commands `platform`, `version` (`version`, `target`, `toolchain`) and `skill`, `software` (discovery and S2/S3 installation), `config` (discovery and S4 C02 planning, applying, checking and status), `status` (S4 C08 and S5 L06 read-only `software`, `config`, `flavor` and `fleet` section reporting) and `fleet` (S5 L02/L03/L04 `--keys`, `--access`, `--refresh`, `--plan`, `--apply`, `--check`, `--publish` and `--unpublish` actions over key directories, authorization policy and the `fleet-authorized-keys` block). |
| `cmd/mm-release` | S6 R02 deterministic five-target release builder and verifier (D2, D3): `mm-release build -version <X.Y.Z> -out <dir>` cross-builds `cmd/managed-machine` for the five targets with `CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-s -w -X main.version=<X.Y.Z>"`, writes `managed-machine_<X.Y.Z>_<os>_<arch>.tar.gz` (`darwin_arm64`, `linux_amd64`, `linux_arm64`) and `managed-machine_<X.Y.Z>_<os>_<arch>.zip` (`windows_amd64`, `windows_arm64`) containing `managed-machine` (`managed-machine.exe` on Windows; mode `0755`) and `LICENSE` (mode `0644`) with sorted entries, uid/gid 0, empty user and group names, mtime `1980-01-01T00:00:00Z`, gzip with empty name and zero mtime at a fixed level, and zip deflate at a fixed level with no extra fields, plus `SHA256SUMS`; `mm-release verify -dist <dir> [-formula Formula/managed-machine.rb -require-pin]` verifies every archive digest against `SHA256SUMS` and, when `-require-pin` is given, requires the formula's `core` resource sha256 to equal the darwin archive digest and its `tag` and `version` to equal `v<X.Y.Z>`/`<X.Y.Z>`. Not part of the CLI contract. |
| `cmd/mm-catalog-report` | F05 migration tool that classifies a legacy app catalog against the F04 schema. Not part of the CLI contract. |
| `cmd/mm-fleet-import` | S5 L05 one-time migration tool (D6): reads `<checkout>/fleet/machines/*.toml`, computes OpenSSH SHA-256 fingerprints from `public_key` bytes via `internal/fleet/directory`, writes `<out>/keys/<owner>.yaml` (`label` = `hostname`, `published_at` = `managed_at`, `revision` 1) and `<out>/fleet/access-groups/<group>.yaml` (`maintained_by: [<owner>]`, `accounts: [<account>]`, `approved_fingerprints`) validated through the L01 loaders, reports disagreeing `ssh_key_fingerprint` records by `machine_id` without importing them (and without emitting hostnames or key bytes), never reads `.git`, and refuses `-out` inside the checkout or the state directory. Not part of the CLI contract. |
| `cmd/mm-walkthrough` | F08 first-slice gate and the T01-T05 per-target evidence run, the S2 install gate (issue #273), the S3 provider gate (issue #285), the S4 configuration gate (issue #301), the S5 fleet gate (issue #319) and the S6 release gate (issues #332, #335). Not part of the CLI contract. |
| `internal/target` | The five canonical platform identifiers of ADR-0014 as amended 2026-10-21 (`darwin/arm64`, `linux/arm64`, `linux/amd64`, `windows/arm64`, `windows/amd64`), host-fact normalization onto them, and the native-architecture probe (`uname -m`; `IsWow64Process2` on Windows). Identifiers given as input accept no aliases. |
| `internal/output` | Ordered records serialized as compact JSON Lines (default), one JSON document (`--json`) or a readable table (`--human`): fixed key order, deterministic bytes, optional fields omitted and never null, `custom:<path>` target encoding. |
| `internal/diag` | The accepted diagnostic identifier registry (`MM_<COMMAND>_<CONDITION>`) with its exit codes 1–4, the stderr JSON form and the `--human` form `MM_<ID>: <message>`. |
| `internal/cli` | Flag parsing shared by every command: `--version` single-argument fast path before option parsing (printing `<version>\n` on stdout and exiting 0 without running any probe; `--version` with any other argument is `MM_USAGE`, exit 2), the read-only `version` command (`version`, `target`, `toolchain`), `-h`/`--help` precedence (no action runs), `--json`/`--human` mutual exclusion (`MM_USAGE`, exit 2), `MM_LEGACY_ROUTE_UNSUPPORTED` refusal of D5 legacy routes via `internal/legacy`, stdout for records and help only, stderr for diagnostics only, exit codes from the registry. |
| `internal/legacy` | F05 legacy catalog (`apps.json`) classification against the F04 software schema, and the S6 R03 static legacy route table (`Routes`, `LegacyStatusSections`, `Match`, `Diagnostic`) that refuses direct core invocations of legacy Bash routes (`setup`, `catalog`, `adopt`, `add-agent`, `account`, `ssh`, `--bootstrap`, `--update`, `fleet list`, `fleet remove`, `skill path`, and `status --section <19 legacy names>`) with `MM_LEGACY_ROUTE_UNSUPPORTED` (exit 2) before any probe or write. |
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
| `internal/status` | `managed-machine status` (S4 C08, S5 L06; D6, D7, D8): read-only status reporting for `--section software|config|flavor|fleet [name]` with fixed record key order `section`, `resource`, `target`, `state`, `facts`, `evidence`, `at`. `software` reports only applied receipts in the plan store (`evidence: "recorded"`, `state: "unknown"`, `facts.receipt`, `at` = receipt timestamp; never rendered as `present`); `config` combines the ownership ledger with fresh read-only on-disk inspection (`present`/`observed` when the digest matches, `absent`/`observed` when deleted or modified, `unknown`/`recorded` when unreadable) and reports an absent custom root as one `config` record (`resource: "root"`, `state: "absent"`) without creating a directory; `flavor` composes member status via `internal/compose` (D6); `fleet` (`--user` only; `--global` and `--custom` are `MM_USAGE`) reports the four D8 resources `directory` (`present`/`recorded` with `at` = snapshot fetch time for a cached gist, `present`/`observed` for a `path:` directory, `absent`/`observed` when neither exists; facts `source`, `owners`, `keys`, `invalid`), `policy` (`present`/`observed` with facts `groups`, `accounts`, `cutover: present|absent`; `absent`/`observed` when the policy directory is missing), `authorized-keys` (from `<state>/fleet/ownership.jsonl` plus fresh inspection of the destination: `present`/`observed`, `absent`/`observed` or `unknown`/`recorded`, with facts `account`, `authorized`, `legacy_block: present|absent`) and `enrollment` (`present`/`recorded` from `<state>/fleet/enrollment.json` with `at` = `enrollment.At` and facts `owner`, `fingerprint`, `machine_id`, `outcome`; `absent`/`observed` when none). An unreadable ledger marks the report partial and exits 4 with `MM_STATUS_PARTIAL` while other sections or resources still print. Opens no plan-store lock, writes nothing, performs no network request and spawns no process. |
| `internal/fleet` | `managed-machine fleet` (S5 L02, L03, L04; D1, D2, D3, D4, D5, D6, D7, ADR-0010, ADR-0012): read-only `--keys` (default when no action flag is given; optional `<owner>` positional filter), `--access` (evaluating authorization for the current OS account via `internal/fleet/authz` and omitting any published key no group approves) and `--refresh` (resolving `history[0].version` from `GET <api>/gists/<id>` and caching the verified `GET <api>/gists/<id>/<revision>` snapshot under `<state>/fleet/directory/` with identity `gist:<id>@<revision>`, returning `unchanged` for `path:` directories), controlled local publication `--publish` / `--unpublish` (atomically rewriting `keys/<owner>.yaml` in a `path:` directory or staging `<state>/fleet/staged/<owner>.yaml` with `next_step` for a `gist:` directory without contacting GitHub, and recording `<state>/fleet/enrollment.json`), plus `--plan`, `--apply` and `--check` for the `fleet-authorized-keys` block on `internal/config/provider/block` and `internal/execute`. `--plan` and `--apply` require `fleet/cutover.yaml` (`MM_FLEET_NOT_CUT_OVER`, exit 1), bind every policy document (`policy:<path>`) and the directory (`directory:<identity>`) in `bindings.assets`, record ownership in `<state>/fleet/ownership.jsonl` (isolated from `status --section config`), migrate legacy `# BEGIN managed-machine` and `# BEGIN home-bin new-machine` blocks in one atomic write, and enforce `0700`/`0600` modes on Unix (`mode: "0600"` in receipt evidence); `--check` exits 0 when in sync, 3 (`MM_FLEET_DRIFT_FOUND`) on digest-only block drift or legacy blocks, and 4 (`MM_FLEET_CHECK_INCOMPLETE`) when unreadable. `--user` is the sole target; `--global` and `--custom` fail with `MM_USAGE` naming the D1 rule that `authorized_keys` lives in OS accounts, not system-wide or in Souls. Spawns no external processes. |
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
`MM_FLEET_CHECK_INCOMPLETE` (exit 4). One more came with S6 R03 (D5, D12):
`MM_LEGACY_ROUTE_UNSUPPORTED` (a legacy Bash route invoked directly on the core,
exit 2).

## Toolchain and evidence

Go 1.27.3 is pinned by the `go` directive in `go.mod`; CI runs with
`GOTOOLCHAIN=local` and `CGO_ENABLED=0`, so the toolchain changes only by a
pull request that edits `go.mod`. `.github/workflows/go-core.yml` runs the
`build` job on `ubuntu-24.04` (`mm-release build` with the version from
`VERSION`, `mm-release verify` with `-require-pin` on tags and on pull requests
whose diff touches `VERSION`, and uploading `dist/` as the `dist` artifact) and
runs `gofmt`, `go vet` and `go test ./...` on the five native hosted runners (`macos-15`,
`ubuntu-24.04-arm`, `ubuntu-24.04`, `windows-11-arm`, `windows-2025`) and
checks that the built binary's `platform` record names the runner's target
without emulation. Each job then runs the F08 walkthrough (below) and attaches
its evidence record, and the `first-slice gate (F08)` job accepts the five
records only when they carry one source revision, every walkthrough passed
natively and the platform-independent outputs are byte-identical. The same job runs the S2 install gate, the S3 provider gate, the S4
configuration gate, the S5 fleet gate and the S6 release gate (below) and
attaches their evidence; the `S2 install gate (E05)`, `S3 provider gate (X07)`,
`S4 config gate (C09)`, `S5 fleet gate (L09)` and `S6 release gate (R09)` jobs accept
the five records only when they carry one source revision and every gate
passed (and `S6 release gate (R09)` writes and uploads `support-matrix.md`). A target is tested only when that workflow passed on its native runner
for the exact revision.

## Developing

```
go test ./...                                   # all packages
go test ./internal/...                          # internal packages
go test ./internal/cli -run TestPlatformGoldens -update   # re-record goldens after an intended change
go test ./internal/source -run TestCatalogGolden -update  # re-record the normalized catalog golden
go test ./internal/software -run TestDiscoveryGoldens -update  # re-record the software command goldens
go test ./internal/config -update               # re-record the configuration goldens
go test ./internal/config/render -update        # re-record the substitution engine goldens
go test ./internal/fleet -update                # re-record the fleet command goldens
go test ./internal/fleet/policy -update         # re-record the fleet policy goldens
go test ./internal/fleet/directory -update      # re-record the fleet directory fingerprint goldens
go test ./cmd/mm-fleet-import -update           # re-record the legacy fleet import goldens
go test ./internal/status -update               # re-record the status fleet section goldens
go test ./internal/skill -update                               # re-record the skill goldens and the caveat-fit report
go run ./cmd/managed-machine platform [--json|--human]
go run ./cmd/managed-machine version [--json|--human]
go run ./cmd/mm-release build -version "$(tr -d '[:space:]' <VERSION)" -out dist
go run ./cmd/mm-release verify -dist dist
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

## The S5 fleet gate (issue #319)

S5 adds the fleet SSH key directory, account authorization policy,
`authorized_keys` managed-block lifecycle, legacy cutover and local key
publication, and because fleet touches only files inside an isolated harness,
all five targets run the same D9 case list natively with no refusal mode.
`cmd/mm-walkthrough fleet` drives a built binary through the D9 cases in an
isolated temp harness whose home (with spaces and non-ASCII characters), state
directory (`MANAGED_MACHINE_STATE_DIR`), fixture policy tree
(`MANAGED_MACHINE_POLICY_DIR`, a plain directory with no git), desired file
(`MANAGED_MACHINE_DESIRED_FILE`), in-process Ed25519 keys (`crypto/ed25519`,
public halves written as `.pub` files and private halves zeroed in memory and
never written), loopback HTTPS gist fixture (`MANAGED_MACHINE_GITHUB_API`,
certificate trusted like the S2 gate) and sentinel directory are its own, and
writes one sanitized `s5-fleet-gate` evidence record per target:

1. `directory-path`: two owners (`alice` and `bob`) listed with computed
   OpenSSH SHA-256 fingerprints, while a tampered entry whose recorded
   fingerprint disagrees with its key bytes is marked `invalid`
   (`fingerprint-mismatch`) and excluded from `--access`.
2. `directory-gist`: an unpinned `gist:<id>` source fails with
   `MM_FLEET_DIRECTORY_UNAVAILABLE` before `--refresh`, resolves to
   `gist:<id>@<revision>` on `--refresh` against the loopback HTTPS server,
   serves `--keys` from the local cache after the server is closed, reports
   `unchanged` on a repeat `--refresh`, and refuses a truncated gist file with
   `MM_FLEET_DIRECTORY_FETCH_FAILED` while leaving the cached snapshot intact.
3. `publication-grants-nothing`: owner `mallory` published in the key directory
   and in no access group is absent from `--access`, absent from the applied
   `authorized_keys` block, and absent by zero-hits grep from the destination,
   state directory, captures and evidence record.
4. `recorded-fingerprint-untrusted`: an entry whose recorded `fingerprint`
   equals an approved fingerprint in policy but whose public-key bytes differ
   is marked `invalid` (`fingerprint-mismatch`) and never authorized.
5. `approval-independent-of-publication`: a fingerprint approved in policy with
   no published key in the directory is reported as `approved-unpublished` by
   `--access` and omitted from the `authorized_keys` block.
6. `revocation-policy`: removing a fingerprint from the access-group fixture
   plans an `update` operation that drops the key line and leaves `--check`
   exiting 0.
7. `revocation-directory`: running `fleet --unpublish` while the fingerprint
   remains approved in policy moves the key to `approved-unpublished` in
   `--access` and drops the key line on the next plan and apply.
8. `ac-t-04-cycle`: the full AC-T-04 lifecycle (`--plan`, `--apply`,
   `status --section fleet`, fresh zero-operation `--plan`, `--apply`, and a
   second `--apply` failing with `MM_FLEET_NO_PLAN` exit 2).
9. `check-exits`: `fleet --check` exits 0 when in sync, 3
   (`MM_FLEET_DRIFT_FOUND`) after an external edit inside the managed block,
   and 4 (`MM_FLEET_CHECK_INCOMPLETE`) against an unreadable destination.
10. `conflict-foreign-block`: `fleet-authorized-keys` markers in the
    destination without an ownership ledger entry plan `conflict foreign-block`
    and refuse `--apply` with `MM_FLEET_CONFLICT` while leaving the destination
    byte-identical.
11. `legacy-transition`: a destination with a user-owned line and a legacy
    `# BEGIN managed-machine` block reports `legacy-block` drift (exit 3) on
    `--check`, refuses `--plan` with `MM_FLEET_NOT_CUT_OVER` before
    `fleet/cutover.yaml` is present, emits the `legacy_block` removal record
    once `cutover.yaml` is present, and on `--apply` preserves the user-owned
    line byte-identical while removing the legacy block; in addition,
    `mm-fleet-import` over a fixture `fleet/machines/` directory imports the
    matching machine record and excludes the mismatched one.
12. `publish-stage`: `fleet --publish` to a `path:` directory increments the
    revision and is idempotent on repeat; `--publish` to a `gist:` directory
    stages `<state>/fleet/staged/<owner>.yaml` and prints `next_step`;
    `<state>/fleet/enrollment.json` is written; and a private-key fixture
    passed to `--key` is refused with `MM_FLEET_KEY_INVALID` without echoing
    any bytes.
13. `status-fleet`: `status --section fleet` reports all four fleet resources
    (`directory`, `policy`, `authorized-keys`, `enrollment`) with `recorded`,
    `observed` and `unknown` in one read-only run without mutating the state
    directory.
14. `legacy-cutover-refusal`: on `darwin/*`, embeds the macOS F01 compatibility
    step's `tests/fleet-cutover-characterization.test.sh` pass result (`ok`);
    recorded `n-a` on the other four targets.
15. `guards`: no guarded program (`ssh-keygen`, `ssh-add`, `ssh`, `gh`, `git`,
    `osascript`, `sudo`, `security`, `dscl`, `gpg`, `brew`, `winget`, `npm`,
    `uv`, `cargo`, `go`, `pip`, `pipx`) was started, the runner's real
    `~/.ssh`, shell profiles and `~/.config/managed-machine` are byte-identical
    by hash, static harness fixtures outside the destination and state
    directory are unchanged, and zero hits exist for every unauthorized key
    blob and for the private-key fixture bytes across the state directory,
    harness home, captured streams and evidence record.

`fleet-aggregate` closes the gate over the five records: one per first-release
target, one source revision, every gate passed, every D9 case passed, and
`legacy-cutover-refusal` recorded `ok` on `darwin/*` and `n-a` on the other
four targets.

```
go test ./cmd/mm-walkthrough -run TestFleet   # the fleet gate's fixtures, guards, grep and aggregate
go build -o mmw ./cmd/mm-walkthrough && go build -o mm ./cmd/managed-machine
./mmw fleet -binary ./mm -target "$(go env GOOS)/$(go env GOARCH)" -evidence fleet.json
./mmw fleet-aggregate -source-revision "$REVISION" fleet-*.json
```

## The S6 release gate (issues #332, #335)

S6 unifies release delivery across the five targets, adds the `bin/managed-machine`
dispatcher on macOS, proves private-seed and upgrade compatibility, and proves
interrupted-apply and plan-store recovery. `cmd/mm-walkthrough release` drives a
built binary and local `dist/` directory through the fifteen D10 cases in an
isolated harness and writes one sanitized `s6-release-gate` evidence record per
target:

1. `checksums` (all five targets per D3, D10): verifies `dist/SHA256SUMS` has
   five two-space-separated lines in sorted archive-name order, every archive
   exists and its SHA-256 digest matches its line in `SHA256SUMS`, and no extra
   files exist in `dist/`.
2. `archive-layout` (all five targets per D3, D10): verifies the runner's own
   archive contains only `managed-machine` (or `managed-machine.exe`, mode
   `0755`) and `LICENSE` (mode `0644`) in sorted order with mtime
   `1980-01-01T00:00:00Z`, uid/gid 0 and empty user/group names, and extracts
   the binary into a harness directory.
3. `version` (all five targets per D2, D10): runs `managed-machine --version`
   and `managed-machine version` (default, `--json`, `--human`) on the extracted
   binary; verifies `version` equals `<VERSION>`, `target` equals the runner's
   canonical target, `toolchain` equals `go1.27.3`, `emulated` is `false` in
   `platform`, and no state directory is created.
4. `reproducible-build` (all five targets per D3, D10): rebuilds
   `./cmd/managed-machine` natively on the runner from the checked-out commit
   with the D3 `go build` flags and requires its SHA-256 digest to equal the
   binary extracted from the `build` job's archive.
5. `walkthrough` (all five targets per D10): runs the F08 read-only walkthrough
   (`platform`, `software --available`, `skill`, `--install` refusal, usage
   errors) against the extracted archive binary.
6. `config-cycle` (all five targets per D10): runs the S4 AC-T-04 configuration
   cycle (`--plan`, `--apply`, `--status`, `--check`, fresh zero-op `--plan` and
   `--apply`, and `--apply` again returning `MM_CONFIG_NO_PLAN` exit 2) on both
   `--user` and `--custom` against the extracted archive binary.
7. `fleet-read` (all five targets per D10): runs `fleet --keys`, `fleet --access`
   and `status --section fleet` against a fixture policy tree across default,
   `--json` and `--human` modes on the extracted archive binary.
8. `legacy-refusal` (all five targets per D5, D10): verifies `setup --list`,
   `catalog list`, `adopt`, `add-agent x`, `account doctor x`, `ssh status`,
   `--bootstrap`, `--update`, `fleet list`, `fleet remove x`, `skill path` and
   `status --section <legacy>` invoked on the core each exit 2 with
   `MM_LEGACY_ROUTE_UNSUPPORTED`, writing nothing and starting no process.
9. `installer` (all five targets per D4, D10): on Linux and Windows, runs
   `install-core.sh` / `install-core.ps1` from a directory base and from a
   loopback HTTPS base into a harness prefix, verifies the installed binary's
   SHA-256 digest and `--version`, verifies idempotent rerun, and verifies a
   tampered archive is refused with exit 1 and no file written; on `darwin/*`,
   verifies `install-core.sh` refuses with exit 2 naming
   `Formula/managed-machine.rb`.
10. `upgrade` (all five targets per D6): installs the candidate from the local
    `dist/` into a harness prefix through the target installer, runs the AC-T-04
    cycle, reruns the installer idempotently without rewriting the binary, reads
    the state with the natively rebuilt `next-version fixture`
    (`-X main.version=<X.Y.Z>+gate`, labeled a fixture in the evidence) and gets
    `MM_CONFIG_NO_PLAN`, restores the release binary and reads the state again,
    and verifies `schema_version: 1` across plans, journals, receipts and
    ledgers in both directions.
11. `recovery` (all five targets per D9): kills an executing apply process group
    mid-run while it holds the store lock (verifying `MM_CONFIG_BUSY` while held),
    verifies the next command recovers the plan as `failed` with an `interrupted`
    receipt and refuses `--from-plan` with `MM_CONFIG_NO_PLAN`, applies a fresh
    plan, verifies a truncated plan file in `plans/` makes `config --plan` exit 1
    naming the file while `status` exits 0 and removing the file restores
    planning, and verifies no consumed plan was replayed across downgrade and
    re-upgrade.
12. `upgrade-from-bash` (`darwin/arm64` per D6; `n-a` with the reason
    `"first release; no upgrade path from an earlier release exists"` on the
    other four targets): derives the `v0.8.12` and candidate formula files from
    repository history by rewriting only URLs and pins to local bare
    repositories and the local `dist/` archive, installs `v0.8.12` with
    `brew install --formula`, seeds the host-state fixture home, materializes the
    writable checkout from the synthetic seed, records `status --json`, upgrades
    with `brew upgrade --formula` (repointing `opt` and retaining the `0.8.12`
    keg), verifies `--version` through both entry points, verifies `status --json`
    matches with the version normalized while legacy manifests, `machine.toml`
    and the checkout remain untouched, runs new-core routes and the AC-T-04
    `config` cycle through the dispatcher, downgrades to `v0.8.12` and
    re-upgrades verifying `MM_CONFIG_NO_PLAN`, removes the keg at the end and
    verifies `brew list --versions` is unchanged by hash. Refuses outside a
    GitHub-hosted runner before starting `brew`.
13. `private-seed` (`darwin/arm64` per D7; `n-a` with the reason on the other
    four targets): builds in-process a synthetic seed repository and fake origin
    with the private layout (commits A, B, C) and proves scenarios (a)–(f):
    fresh install materialization at commit A on branch `main`, clean checkout
    behind origin across the upgrade fast-forwarding to B then C on refresh,
    dirty checkout preserved byte-identical and reported, divergent checkout
    preserved and reported, older seed pin never rewinding a newer live checkout,
    and the new core reading the materialized checkout as its policy directory
    through the dispatcher while a `git` sentinel first on `PATH` is never
    started.
14. `dispatcher` (`darwin/arm64` per D5; `n-a` with the reason on the other four
    targets): proves every D5 new-core and legacy route through the installed
    `bin/managed-machine`, direct core `MM_LEGACY_ROUTE_UNSUPPORTED` (exit 2)
    refusals on legacy routes, `core-missing` failure (exit 1) without fallback
    when `MANAGED_MACHINE_CORE` names a missing file or `libexec/core/managed-machine`
    is removed, and F01 characterization goldens (`f01_compatibility: "pass"`).
15. `guards` (all five targets): no guarded sentinel program was started by the
    core, the runner's real `~/.ssh`, shell profiles, `~/.config/managed-machine`
    and platform state directory are byte-identical by hash, static harness
    directories are unchanged, and zero hits exist for runner paths, the
    invoking account name, synthetic seed markers and key bytes across the state
    directories, captures and evidence record.

`release-aggregate` closes the gate over the five records: one per first-release
target, one source revision, one version, every archive SHA-256 matching
`SHA256SUMS`, every gate passed, `f01_compatibility: "pass"` on `darwin/arm64`,
`upgrade-from-bash`, `private-seed` and `dispatcher` recorded `ok` on `darwin/*`
and `n-a` with the exact reason on the other four targets, and the remaining
twelve cases recorded `ok` on all five; when `-out support-matrix.md` and the
S2/S3 `install-gate-*` and `provider-gate-*` records are given, it also writes
 the release's `support-matrix.md` (target table, provider matrix, case matrix
and known v1 limitations).

```
go test ./cmd/mm-walkthrough -run TestRelease   # formula derivation, seed scenarios, grep and aggregate
go build -o mmw ./cmd/mm-walkthrough
./mmw release -dist dist -version "$(tr -d '[:space:]' <VERSION)" -target "$(go env GOOS)/$(go env GOARCH)" -evidence release.json
./mmw release-aggregate -source-revision "$REVISION" -version "$(tr -d '[:space:]' <VERSION)" -checksums dist/SHA256SUMS -out support-matrix.md release-*.json provider-gate-*.json install-gate-*.json
```

## R09 documentation pass: `core/` references

In-repo plans (`docs/plans/f01-caller-characterization.md`,
`docs/plans/winget-provider.md` and `docs/plans/go-portability-proof.md`),
`README.md` and `AGENTS.md` are updated in R01 alongside `docs/go-core.md`. In
the external `managed-machine-documentation` repository, the following
documents reference `core/` (`core/go.mod`, `core/cmd/...`, `core/internal/...`
or `core/README.md`) and are listed for the R09 documentation pass and F03 /
ADR-0017 status update:

- `docs/migration-plan.md` (F03 status line, S1–S5 story tables and S6 D1 promotion record)
- `docs/decisions/ADR-0016-portable-core-and-compatibility-routing.md`
- `docs/decisions/ADR-0017-go-core-module-layout.md` (status line recording the S6 R01 promotion to the repository root)
- `docs/spec/v1/` command and architecture specifications referencing `core/internal/...` and `core/cmd/...`
- `docs/spec/v1/` command registry and CLI boundary (S6 R02, D2): add `managed-machine --version` (single-argument fast path before option parsing printing `<version>\n` on stdout, exiting 0 and running no probe; `--version` with any further argument is `MM_USAGE` exit 2; unflagged builds report `0.0.0-dev`) and `managed-machine version [--json | --human]` (read-only command on all five targets)
- `docs/spec/v1/` record-fields table (S6 R02, D2): add the `version` record with fixed key order `version`, `target`, `toolchain` (`toolchain` equals `runtime.Version()`)
- `docs/spec/v1/` and `docs/migration-plan.md` release build specification (S6 R02, D3): record `-ldflags "-s -w -X main.version=<VERSION>"` embedding and `cmd/mm-release build|verify` producing the five deterministic archives (`managed-machine_<X.Y.Z>_<os>_<arch>.tar.gz|zip`) and `SHA256SUMS`
- `docs/spec/v1/` and `docs/migration-plan.md` per-target delivery specification (S6 R04, D4, D11): record `install-core.sh` for `linux/amd64` and `linux/arm64`, `install-core.ps1` for `windows/amd64` and `windows/arm64`, the manual `<state>/managed-machine-config` clone on Linux and Windows, and `Formula/managed-machine.rb` on `darwin/arm64` gaining `depends_on arch: :arm64`, `depends_on macos: :sequoia`, and the SHA-256-pinned `core` resource (`libexec/core/managed-machine`) with the `v0.9.0` release pull request (`scripts/release prepare`, S6 R05)
- `docs/migration-plan.md` release workflow and two-step release procedure (S6 R05, D3, D11): record `scripts/release prepare|tag`, `.github/workflows/release.yml` calling `./.github/workflows/go-core.yml` (`on: workflow_call`), and the `publish` job (`permissions: contents: write` as the sole elevated permission, `GITHUB_TOKEN` only)

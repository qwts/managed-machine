---
name: npm-kind
status: active
overview: Catalog kind for CLI tools that install exclusively from the public npm registry, so setup-only tools like Command Code do not need a vendor script or a signed-cask row.
related_prs: []
---

# npm catalog kind

## Intent

Some requested setup-only tools are npm packages, not desktop casks or curl installers. Command Code is `command-code` on the public registry only; there is no homebrew/core formula and no `curl | bash` installer. The catalog already has `official-cli` and `brew-formula`; it still needs an install engine that refuses non-registry packages.

## Target

- Kind `npm` with a required `package` field (`[@scope/]name`, scoped and unscoped both allowed) and an optional `command` field for the installed binary (defaults to the package name).
- All npm invocations (`view`, `install`, `ls`, `prefix`) run through `npm_public`, which forces `--registry=https://registry.npmjs.org/` and isolates npm from machine configuration: user and global `.npmrc` are replaced with empty throwaway files (`NPM_CONFIG_USERCONFIG`/`NPM_CONFIG_GLOBALCONFIG`) and the subcommand runs from an empty working directory so no project `.npmrc` applies. A machine-scoped or scope-specific registry can therefore never redirect the metadata lookup or the install to a private server, and a same-named private package's lifecycle scripts can never run globally.
- `npm view --registry=https://registry.npmjs.org/ <package> name version --json` must report the exact catalog package name from the public registry.
- Install with `npm install --global --no-fund --no-audit --registry=https://registry.npmjs.org/ <package>`. A receipt from `npm ls --global --parseable --depth=0` avoids re-install.
- An optional `version` (`X.Y.Z`) pins the row: the registry must report `<package>@<version>` by that exact name and version, the install is `<package>@<version>`, an npm-global copy at another version converges to the pin, a copy of the command outside the npm global tree is left alone, and an install that does not end at the pin fails.
- `managed-machine status` reports npm-kind rows through their `command` field, resolving `$(npm prefix -g)/bin` onto the status PATH so configured binaries show their version.
- Missing npm fails during verification with "npm required — run setup-nvm first".
- The npm global `bin` dir (from `npm prefix -g`) is added to PATH for the current process when present, mirroring `export_local_bin_to_path`, so agents running without a login shell still find the tool; status resolves the same dir for its report.
- On-demand apps (Command Code) live in managed-machine-config with `"auto": false` when they should stay out of bootstrap/`--update`.

## Reviewed lifecycle scripts and DeepSeek readiness (#231)

An optional nonempty `install_scripts` array holds reviewed registry dependency
specs at exact versions, including prereleases (`name@X.Y.Z[-prerelease]`). Bare names,
ranges, URLs, empty lists and non-array values fail before installation. The field
requires an engine release that implements it; publish the companion config change after
that release.

For these rows, `npm-reviewed-install` installs the requested package with
`--ignore-scripts`, then runs `npm rebuild --global` for only the listed specs,
with `--allow-scripts` restricted to those target names for that invocation.
The version pins stay on rebuild targets: npm’s global `loadActual` drops
resolved URLs and its policy matcher cannot match pinned approval entries. The
two phases are intentional: npm 11 can warn and still run unreviewed scripts, while npm 12 blocks
them. Selecting rebuild targets also repairs an unchanged installed dependency,
without approving unrelated packages in the global tree. Both phases explicitly pin `--registry=https://registry.npmjs.org/`, including
when the caller exports `NPM_CONFIG_REGISTRY`, and share the config isolation and, when needed, one owner elevation. Neither
writes to the user npm config. Unlisted scripts remain disabled; a dependency
version change requires a new review rather than inheriting a name-only approval.

DeepSeek requires this field and checks the actual npm-global `dsh --version`,
loads `koffi` from the subprocess provider dependency tree and calls native
`getpid`, then launches a short Node process through that same tree’s `node-pty`.
The PTY check has an eight-second timeout and exercises the spawn helper’s
executable permission without credentials, profiles, a model call or a login
shell. Only macOS and Linux have a supported native check. Failure makes setup
incomplete. Existing receipts, matching pins and successful privileged actions
must also pass; failed existing installs are repaired rather than skipped.
Bootstrap planning checks readiness before declaring a no-op and carries the
exact reviewed list through the approved owner action and root handoff.

Review for `@deepseek-ai/dsh` 0.2.0-rc.2 (public npm package contents):

- `@deepseek-ai/dsh-subprocess-local@0.2.0-rc.2`: restores executable permission
  on node-pty’s prebuilt or compiled spawn helper.
- `node-pty@1.2.0-beta.15`: chooses a shipped platform prebuild or compiles it;
  its postinstall cleans build artifacts and handles Windows ConPTY files.
- `koffi@3.1.1`: selects or builds the native FFI addon through cnoke.
- `@google/genai@2.21.0`: a no-op preinstall; leave disabled.
- `protobufjs@7.6.6`: prints dependency version-scheme advice; leave disabled.

These are pinned script approvals, not a pin of the whole DeepSeek graph.
The smoke check validates native readiness, not all agent functionality.

The registry verifier accepts npm 11’s object metadata and npm 12’s single-record
array, still refusing ambiguous arrays and mismatched names or pins.

## Acceptance

- An `npm` row installs from the public registry and is a no-op on re-run (already-installed via receipt or on PATH).
- A missing `package`, invalid name, non-public-registry package, or missing `npm` fails before install side effects.
- Unknown kinds still fail closed with the upgrade message.
- Bootstrap and `--update` skip `"auto": false` rows (existing optional-catalog-apps behavior).

- Pinned rows: a pinned row installs `<package>@<version>`, is a no-op at the pin, converges another npm-global version, and refuses a pin the registry lacks or a malformed pin before installing.

- Reviewed scripts: first install is script-free, only exact rebuild targets run,
  malformed policy fails before mutation, healthy re-runs do not rebuild, damaged
  helpers trigger repair, and install/rebuild/version/native failures propagate.
- Approved plans retain the policy and do not open another administrator dialog;
  a skipped elevation remains skipped and a completed action still checks health.

## Replay

`/bin/bash tests/npm.test.sh` and `/bin/bash tests/npm-install-scripts.test.sh`. After a formula release, `managed-machine setup commandcode` installs the npm package and runs `config/commandcode`.

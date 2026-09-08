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
- `managed-machine status` reports npm-kind rows through their `command` field, resolving `$(npm prefix -g)/bin` onto the status PATH so configured binaries show their version.
- Missing npm fails during verification with "npm required — run setup-nvm first".
- The npm global `bin` dir (from `npm prefix -g`) is added to PATH for the current process when present, mirroring `export_local_bin_to_path`, so agents running without a login shell still find the tool; status resolves the same dir for its report.
- On-demand apps (Command Code) live in managed-machine-config with `"auto": false` when they should stay out of bootstrap/`--update`.

## Acceptance

- An `npm` row installs from the public registry and is a no-op on re-run (already-installed via receipt or on PATH).
- A missing `package`, invalid name, non-public-registry package, or missing `npm` fails before install side effects.
- Unknown kinds still fail closed with the upgrade message.
- Bootstrap and `--update` skip `"auto": false` rows (existing optional-catalog-apps behavior).

## Replay

`/bin/bash tests/npm.test.sh`. After a formula release, `managed-machine setup commandcode` installs the npm package and runs `config/commandcode`.
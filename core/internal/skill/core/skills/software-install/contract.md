Synopsis: managed-machine software [name] [--provider <provider> | --recipe <id>] [--category <category>] [--available] [--global | --user | --custom <path>] [--json | --human] [-h]

Discovery prints one JSON Lines record per matching recipe with the keys software, recipe, provider, package, targets, in that order. It filters to the current platform (darwin/arm64, linux/arm64, linux/amd64, windows/arm64 or windows/amd64). Every filter intersects. Preferred providers never hide a recipe from discovery.

Validation: --global, --user and --custom are mutually exclusive; --recipe and --provider are mutually exclusive; --json and --human are mutually exclusive; -h takes precedence and never changes the machine.

Diagnostics (stderr, one JSON object per line): MM_USAGE, MM_SOFTWARE_UNKNOWN_NAME, MM_SOFTWARE_UNKNOWN_PROVIDER, MM_SOFTWARE_UNKNOWN_RECIPE, MM_SOFTWARE_UNKNOWN_CATEGORY, MM_SOFTWARE_SOURCE_INVALID, MM_SOFTWARE_AMBIGUOUS_PROVIDER, MM_SOFTWARE_AMBIGUOUS_RECIPE, MM_SOFTWARE_UNSUPPORTED_COMBINATION and MM_SOFTWARE_INSTALL_UNAVAILABLE exit 2; MM_SOFTWARE_PRIVILEGE_REQUIRED, MM_SOFTWARE_PROVIDER_MISSING, MM_SOFTWARE_PLAN_REJECTED, MM_SOFTWARE_BUSY and MM_SOFTWARE_INSTALL_FAILED exit 1.

Installation: --install with exactly one target selects one recipe, saves a plan in the state directory, re-verifies its preconditions and runs it once, journaling every operation. On success it prints one record with the keys software, recipe, provider, target, resolved_version, outcome (ok, or unchanged when already installed at the planned version), plan. Only the brew provider installs in this release, and only into a prefix the current user can write.

Selection (used by --install): an explicit recipe wins, then an explicit provider, then the preferred provider from desired configuration, then the only compatible recipe. Several candidates are an ambiguity error (MM_SOFTWARE_AMBIGUOUS_PROVIDER or MM_SOFTWARE_AMBIGUOUS_RECIPE); there is no catalog-order fallback.

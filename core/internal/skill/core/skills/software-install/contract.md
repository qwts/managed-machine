Synopsis: managed-machine software [name] [--provider <provider> | --recipe <id>] [--category <category>] [--available] [--global | --user | --custom <path>] [--json | --human] [-h]

Discovery prints one JSON Lines record per matching recipe with the keys software, recipe, provider, package, targets, in that order. It filters to the current platform (darwin/arm64, linux/arm64, linux/amd64, windows/arm64 or windows/amd64). Every filter intersects. Preferred providers never hide a recipe from discovery.

Validation: --global, --user and --custom are mutually exclusive; --recipe and --provider are mutually exclusive; --json and --human are mutually exclusive; -h takes precedence and never changes the machine.

Diagnostics (stderr, one JSON object per line): MM_USAGE, MM_SOFTWARE_UNKNOWN_NAME, MM_SOFTWARE_UNKNOWN_PROVIDER, MM_SOFTWARE_UNKNOWN_RECIPE, MM_SOFTWARE_UNKNOWN_CATEGORY, MM_SOFTWARE_SOURCE_INVALID and MM_SOFTWARE_INSTALL_UNAVAILABLE, all exit 2.

Selection (used by installation from S2): an explicit recipe wins, then an explicit provider, then the preferred provider from desired configuration, then the only compatible recipe. Several candidates are an ambiguity error (MM_SOFTWARE_AMBIGUOUS_PROVIDER or MM_SOFTWARE_AMBIGUOUS_RECIPE); there is no catalog-order fallback.

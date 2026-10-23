1. Run `managed-machine software --available` to list every recipe supported on this platform.

2. Narrow the list with filters, which combine: a name (`managed-machine software ruff`), `--category <name>`, `--provider <name>`, `--recipe <id>`, and one of `--global`, `--user` or `--custom <path>`.

3. Read each record's provider and targets. A record is declared support from the catalog, not proof that the provider works on this machine.

4. If several recipes match, choose one explicitly by recipe id or provider. Discovery never chooses for you.

5. Install with `--install` and exactly one target, naming the recipe or provider when several match. Only Homebrew formulae install in this release; other providers exit 2 with MM_SOFTWARE_INSTALL_UNAVAILABLE. A prefix the current user cannot write fails with MM_SOFTWARE_PRIVILEGE_REQUIRED: the command never elevates, so fix ownership yourself or choose another target.

6. Read the result record. `unchanged` means the planned version was already installed. On MM_SOFTWARE_INSTALL_FAILED nothing after the failed operation ran; rerun the command to make a new plan.

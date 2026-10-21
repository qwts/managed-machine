1. Run `managed-machine software --available` to list every recipe supported on this platform.

2. Narrow the list with filters, which combine: a name (`managed-machine software ruff`), `--category <name>`, `--provider <name>`, `--recipe <id>`, and one of `--global`, `--user` or `--custom <path>`.

3. Read each record's provider and targets. A record is declared support from the catalog, not proof that the provider works on this machine.

4. If several recipes match, choose one explicitly by recipe id or provider. Discovery never chooses for you.

5. Stop here in this release. Installation lands in S2; until then `--install` is rejected with MM_SOFTWARE_INSTALL_UNAVAILABLE and nothing is changed.

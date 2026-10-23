Illustrative, on darwin/arm64 with the built-in catalog:

$ managed-machine software ruff
{"software":"ruff","recipe":"homebrew","provider":"brew","package":"ruff","targets":["global","user"]}
{"software":"ruff","recipe":"uv-tool","provider":"uv","package":"ruff","targets":["user","custom"]}

$ managed-machine software ruff --install --user
{"diagnostic":"MM_SOFTWARE_AMBIGUOUS_PROVIDER","message":"ruff user on darwin/arm64: valid providers: brew, uv"}
(exit 2)

$ managed-machine software ruff --install --user --provider brew
{"software":"ruff","recipe":"homebrew","provider":"brew","target":"user","resolved_version":"0.7.4","outcome":"ok","plan":"plan-20261021T120000Z-3f9c"}

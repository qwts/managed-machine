Illustrative, on darwin/arm64 with the built-in catalog:

$ managed-machine software ruff
{"software":"ruff","recipe":"homebrew","provider":"brew","package":"ruff","targets":["global","user"]}
{"software":"ruff","recipe":"uv-tool","provider":"uv","package":"ruff","targets":["user","custom"]}

$ managed-machine software ruff --install --user
{"diagnostic":"MM_SOFTWARE_INSTALL_UNAVAILABLE","message":"installation is not available in this release; use --available to discover recipes"}
(exit 2)

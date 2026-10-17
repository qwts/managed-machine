---
status: completed
overview: Execute the authenticated installer only after its download succeeds, with cleanup and preserved exit status.
related_prs: [230]
---

# Installer fetch (#227)

## Intent

Invalid GitHub credentials must stop the fresh-Mac install before a response body reaches Bash. The previous direct pipeline executed the HTTP 401 JSON response and obscured the authentication failure with shell errors.

## Target

README.md, the managed-machine skill, and install.sh usage carry the same subshell command. Stage the authenticated fetch in a private mktemp file, check gh's exit status, then run Bash only on success. The EXIT trap removes the file on success or failure. A staging failure stops immediately; a download or installer failure remains nonzero. Documentation explains how a human checks and restores authentication, retaining the existing agent-session refusal guidance.

## Acceptance

- The actual documented commands execute a successful response exactly once.
- JSON and executable shell returned by a failed fetch never execute.
- Fetch and installer exit statuses are preserved, and staging failure stops execution.
- Temporary paths with spaces work and no staged file remains after any tested outcome.
- All three command copies agree; the commands work in Bash and, where installed, zsh.

## Replay

Run `bash tests/install-fetch.test.sh` for offline response fixtures. Run the full `tests/*.test.sh` suite from a neutral directory per AGENTS.md. No live installer or machine setup is needed to verify the download boundary.

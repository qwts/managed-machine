---
status: completed
overview: ADR-0016 F02 — bounded Go portability proof: an isolated fixture, a hardened single-target harness, and a five-target native evidence gate on GitHub-hosted runners before any substantial porting.
related_prs: [240, 242]
---

# Go portability proof

## Intent

Validate the bounded portable-core direction on five native targets before substantial porting. This fixture changes no installed command, installer, account, fleet record, credentials or production protocol. Go is the core language decision; the proof does not reopen provider build order.

## Target

The isolated fixture lived in `proofs/go-portability/` until F03 landed; it was removed on 2026-10-21 with the `core/` module (ADR-0017; promoted to the repository root in S6 R01, issue #327), and the evidence below stands as recorded from the merged runs (#239, #240). Its successor is `.github/workflows/go-core.yml`, which runs the core module's tests and native platform probe on the same five runners. As run, a hardened Python harness built one target and ran seven native checks; a separate aggregate required all five reports from the same source commit and exact fixture hashes. Official actions were pinned to verified commit SHAs. Go 1.27.3 archives were verified in actions/go-versions for all target architectures.

Hosted labels, verified against GitHub's runner reference: macos-15 (ARM64), ubuntu-24.04-arm, ubuntu-24.04 (x64), windows-11-arm, windows-2025 (x64). Native Windows detection calls IsWow64Process2 in the actual Go fixture process; an emulated x64 process cannot satisfy ARM64 evidence. Unix compares process architecture to uname. Platform release and process identity are evidence, not minimum-OS support claims.

## Acceptance

- Launch, exact LF/UTF-8 JSONL object count, spaced/Unicode path roundtrip, harmless subprocess JSON success/failure, unsupported legacy rejection and no automatic legacy fallback pass natively.
- Validation uses explicit exceptions and still rejects bad evidence under Python optimization. Build/native status are separate; check presence and green flags alone do not establish success.
- The aggregate rejects missing, duplicate, unknown, failed or mixed-source targets and requires Go version consistency.
- Only bounded sanitized JSON evidence is uploaded, with seven-day retention. No secrets, private config, local reports or machine paths are committed. Earlier local proof evidence remains untouched.
- Workflow permissions are read-only; checkout credentials are not persisted; no pull_request_target, provider calls, release or merge.

## Outcome

Merged to `main` as `e617a0ff` on 2026-10-21 (issue #239, PR #240). Workflow run 39564982158 on the final PR head passed all five native jobs and the aggregate gate. Every target built and passed seven native checks with Go 1.27.3, `CGO_ENABLED=0` and `GOTOOLCHAIN=local`, and each fixture process confirmed its own architecture:

| Target | Runner | Host reported | Emulated |
| --- | --- | --- | --- |
| darwin/arm64 | macos-15 | Darwin 24.6.0 arm64 | no |
| linux/arm64 | ubuntu-24.04-arm | Linux 6.17.0 aarch64 | no |
| linux/amd64 | ubuntu-24.04 | Linux 6.17.0 x86_64 | no |
| windows/arm64 | windows-11-arm | Windows 11 ARM64 | no |
| windows/amd64 | windows-2025 | Windows 2025Server AMD64 | no |

Hosted reports record `github.sha`, which on a `pull_request` event is the merge commit rather than the branch head; the merge commit's tree was identical to the branch head's, and the downloaded reports re-aggregated locally against that commit. Deltas from the original scope: the build timeout was raised to 600 s for cold hosted compiles, and the F01 routing inventory named as a dependency in the migration plan did not exist, so fixed representative rejection cases were used. The ADR-0016 F02 gate is met; F01 remains the earliest open S0 story and F03 is unblocked. This outcome does not authorize porting, installer migration or a release.

## Replay (historical)

The fixture no longer exists in the tree, so these commands are the record of how the evidence was produced, not a procedure to run today. At the proof revisions (#239, #240): from `proofs/go-portability`, `python3 -O -m unittest -v test_proof.py` ran the harness tests; with approved Go 1.27.3 available, `python3 proof.py native --target <target> --source-commit <40-character-checkout-SHA> --output <new-report-path>` produced one native report (the actual native target was required; a cross-built binary was not native evidence); `python3 proof.py aggregate --source-commit <same-SHA> --reports <report-directory> --output <new-summary-path>` closed the five-target gate from five fresh reports, and existing report paths were never overwritten. The removed workflow performed that fan-out and fan-in on a PR affecting the proof or on manual dispatch. To reproduce the evidence, check out the proof revision.

The live replacement is `.github/workflows/go-core.yml`: it builds the core module natively on each of the five runners and requires the `platform` probe to report the runner's own identifier without emulation.

This is a proof fixture, not the first functional discovery/skills slice or an end-user installation workflow. No production schema is accepted through fixture behavior. CGO_ENABLED=0 is used here without claiming every future binary is universally static. Reported timings have no cold/warm CI guarantee.

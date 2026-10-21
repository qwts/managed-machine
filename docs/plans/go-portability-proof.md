---
status: active
overview: ADR-0016 F02 — bounded Go portability proof: an isolated fixture, a hardened single-target harness, and a five-target native evidence gate on GitHub-hosted runners before any substantial porting.
related_prs: []
---

# Go portability proof

## Intent

Validate the bounded portable-core direction on five native targets before substantial porting. This fixture changes no installed command, installer, account, fleet record, credentials or production protocol. Go is the core language decision; the proof does not reopen provider build order.

## Target

The isolated fixture is in `proofs/go-portability/`. A hardened Python harness builds one target and runs seven native checks; a separate aggregate requires all five reports from the same source commit and exact fixture hashes. Official actions are pinned to verified commit SHAs. Go 1.27.3 archives were verified in actions/go-versions for all target architectures.

Hosted labels, verified against GitHub's runner reference: macos-15 (ARM64), ubuntu-24.04-arm, ubuntu-24.04 (x64), windows-11-arm, windows-2025 (x64). Native Windows detection calls IsWow64Process2 in the actual Go fixture process; an emulated x64 process cannot satisfy ARM64 evidence. Unix compares process architecture to uname. Platform release and process identity are evidence, not minimum-OS support claims.

## Acceptance

- Launch, exact LF/UTF-8 JSONL object count, spaced/Unicode path roundtrip, harmless subprocess JSON success/failure, unsupported legacy rejection and no automatic legacy fallback pass natively.
- Validation uses explicit exceptions and still rejects bad evidence under Python optimization. Build/native status are separate; check presence and green flags alone do not establish success.
- The aggregate rejects missing, duplicate, unknown, failed or mixed-source targets and requires Go version consistency.
- Only bounded sanitized JSON evidence is uploaded, with seven-day retention. No secrets, private config, local reports or machine paths are committed. Earlier local proof evidence remains untouched.
- Workflow permissions are read-only; checkout credentials are not persisted; no pull_request_target, provider calls, release or merge.

## Replay

From `proofs/go-portability`, run `python3 -O -m unittest -v test_proof.py`. With approved Go 1.27.3 available, run `python3 proof.py native --target darwin/arm64 --source-commit <40-character-checkout-SHA> --output <new-report-path>`. Choose the actual native target; a cross-built binary is not native evidence.

Collect five fresh reports and run `python3 proof.py aggregate --source-commit <same-SHA> --reports <report-directory> --output <new-summary-path>`. Existing report paths are never overwritten. The workflow performs this fan-out/fan-in automatically on a PR affecting this proof or manual dispatch. Per-target jobs can pass independently; only the aggregate closes the five-target gate.

This is a proof fixture, not the first functional discovery/skills slice or an end-user installation workflow. No production schema is accepted through fixture behavior. CGO_ENABLED=0 is used here without claiming every future binary is universally static. Reported timings have no cold/warm CI guarantee.

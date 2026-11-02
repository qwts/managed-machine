---
status: active
overview: >-
  Ship the darwin core as a Developer ID signed, notarized AgentBotMachine.app
  with an embedded provisioning profile, so the daemon key is held in the
  Data Protection keychain and wrapped by the Secure Enclave. Signing happens
  during `scripts/release prepare`, before the formula pin, in an
  approval-gated GitHub environment.
related_prs: [440]
related_issues: []
---

# macOS signing

## Intent

Owner direction (2026-11-01, PR #440 review): critical secrets live in the
deepest store available, not in a file. On macOS the daemon's Ed25519 seed is
stored in the Data Protection keychain under the access group
`Z5DM34QS5U.ai.true-line.agent-bot-machine` and wrapped by a Secure Enclave
P-256 key. Both need the process to carry the `keychain-access-groups`
entitlement, which macOS honours only for a Developer ID signed executable
whose bundle embeds a matching provisioning profile. Unsigned builds fall back
to the dedicated `daemon.keychain-db` in the private state directory.

The owner chose (2026-11-01) to sign during the release PR: the formula keeps
pinning the darwin archive before the tag exists, and the pin names the signed
archive.

## Target

**Apple setup (owner, done 2026-11-01).** Team `Z5DM34QS5U`; App ID
`ai.true-line.agent-bot-machine` with no capabilities; Developer ID
provisioning profile "Agent Bot Machine" for that App ID and the Developer ID
Application certificate expiring 2031-07-14; an App Store Connect team key
with the Developer role for notarization. The six secrets
(`MACOS_DEVID_P12`, `MACOS_DEVID_P12_PASSWORD`, `MACOS_PROVISION_PROFILE`,
`NOTARY_KEY_P8`, `NOTARY_KEY_ID`, `NOTARY_ISSUER_ID`) live only in the GitHub
environment `release`, which requires the owner's approval and admits the
`main` branch and `v*` tags.

**Archive.** `mm-release build` writes the darwin archive as
`LICENSE`, `managed-machine` (for `install-core.sh`), and
`AgentBotMachine.app/Contents/{Info.plist,MacOS/managed-machine}` with the same
binary. It stays deterministic. `packaging/darwin/entitlements.plist` names
the application identifier, team identifier and the one keychain access group.

**Signing.** `.github/workflows/sign.yml` is dispatched on `main` with a
version. In the `release` environment on a macOS runner it builds the
deterministic dist, imports the identity into a temporary keychain, and runs
`scripts/sign-darwin`: embed the profile (refused unless it names the App ID
and team), sign the bare binary and the bundle with the hardened runtime and a
secure timestamp, check authority, Team ID, runtime flag and the exact
entitlements, notarize both, staple the bundle, require Gatekeeper's
`Notarized Developer ID` assessment, repack with `mm-release repack`, and
check with `mm-release verify-signed`. The signed dist is uploaded to the
draft release `vX.Y.Z`. It is the only workflow that may read secrets.

**Verification.** `mm-release verify-signed -unsigned D -signed S -team T`
needs no macOS: the four other archives are byte-identical; in the darwin
archive every unsigned file is present with the same mode and bytes, except
the two Mach-O binaries, which must match outside the code signature (only the
`LC_CODE_SIGNATURE` datasize and `__LINKEDIT` vmsize/filesize may differ, the
signature must end the file, and it must name the Team ID); the only added
files are the profile, `_CodeSignature/CodeResources` and the stapled
`CodeResources`; and the profile names `T.ai.true-line.agent-bot-machine`.
With `-require-pin` the formula must pin the signed darwin digest.

**Release flow.** `scripts/release prepare` builds the dist, reuses a draft
that verifies or dispatches `sign.yml` at origin/main and waits for the run
(the owner approves the deployment), verifies the draft, writes the formula
with the signed digest and the bundle install block, and commits
`Release vX.Y.Z`. `scripts/release tag` rebuilds, downloads the draft and
runs `verify-signed -require-pin` before pushing the tag. On the tag,
`release.yml` verifies the same way and publishes the draft instead of
creating a release.

**Formula.** The install block written by `prepare` installs
`AgentBotMachine.app` into the keg and links `bin/managed-machine` to its main
executable. The Go core maps `<keg>/AgentBotMachine.app/Contents/MacOS/<exe>`
to `<keg>/bin/<exe>` when resolving `libexec` (`cli.BundleHostExecutable`), so
the legacy tree and skill bundle resolve as before. The formula on `main` is
rewritten only by a release PR, so it never pins an archive without the
bundle.

## Acceptance

- `go test ./cmd/mm-release` covers the bundle entries, repack, and
  verify-signed refusals (changed code, wrong Team ID, data after the
  signature, profile for another App ID, extra or changed files, a changed
  other archive), and on darwin/arm64 proves a real `codesign` re-sign passes.
- `TestSignWorkflowContract` pins sign.yml: dispatch only, `main` only, the
  `release` environment, `contents: write` on the job alone, and exactly the
  six secrets through `env`.
- `tests/release.test.sh` and `tests/formula-core.test.sh` run `prepare` and
  `tag` against `tests/fixtures/fake-gh`: a failed signing run aborts prepare
  cleanly, a resume reuses the draft, `tag` refuses a missing or unsigned
  draft, the formula pins the signed digest, and the bundle formula installs
  and passes its `test do` in the Ruby harness.
- The first signed release shows `spctl -a -t exec -vv` reporting
  `Notarized Developer ID` for the installed bundle, and the daemon storing
  `daemon.key` in the Data Protection keychain with an enclave-wrapped
  payload, with no `daemon.keychain-db` created.

## Replay

1. Merge #440 and this change.
2. On clean main, `scripts/release prepare vX.Y.Z`; approve the `release`
   deployment when the script prints the run URL.
3. Open and merge the printed release PR.
4. With the owner's confirmation, `scripts/release tag vX.Y.Z`.
5. `brew upgrade managed-machine`, then check the bundle with `spctl` and
   `codesign -d --entitlements -`, and restart the daemon.

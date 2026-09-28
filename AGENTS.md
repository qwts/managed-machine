# managed-machine

Machine setup and orchestration, distributed as a self-tapped Homebrew formula. Idempotent; safe to re-run.

## Agent migration protocol

If asked to migrate a machine with this repo:

1. Do not explore this repo.
2. Do not clone this repo.
3. Copy `skills/managed-machine/SKILL.md` to your agent's skill directory:
   - Devin: `~/.config/devin/skills/managed-machine/SKILL.md`
4. Invoke the `managed-machine` skill and follow it.
5. If the curl install fails because the Homebrew formula has no released tag/sha, stop and tell the user the repo needs a release before migration can proceed.

## Agent contribution mode

- If contributing changes to this repo: read the constraints below, then explore as needed.

## Constraints

- no secrets in repo; auth/keys generated per machine or from macOS Keychain
- setup-* scripts source lib/install.sh; keep helpers reusable
- desktop apps install from homebrew/cask only into `/Applications`; lib/cask-app.sh verifies tap, sha256, vendor hosts, and Developer ID Team ID before trusting the bundle
- bundle integrity passes on `codesign --verify --deep --strict` or, failing that, on a Gatekeeper assessment that proves notarization and names the same Team ID; identity checks are never skipped
- a catalog row with `allow_rolling_url` accepts `sha256 :no_check` for vendors serving one rolling URL, and nothing else — malformed digests and off-allowlist hosts are still refused; such a row must pass the Gatekeeper notarization check, since no checksum stands behind it
- desktop apps with no cask use vendor-dmg rows (pinned vendor URL + DMG sha256 + Team ID, staged bundle verified before anything under /Applications moves); a sparkle row resolves its appcast enclosure at download time and still requires that host on url_hosts; a bare url_hosts entry also matches a www. prefix, a leading-dot entry is matched as written, and a redirect is kept only when the final host is allowlisted; the sha256 rule is enforced before an already-installed sparkle app short-circuits; on-disk occupiers converge in place, adopt stays cask-only
- CLI formulae install from homebrew/core only via brew-formula catalog rows; lib/apps.sh verifies the tap before `brew install`
- which apps to install is declared in managed-machine-config/apps.json; optional config/<name> scripts apply settings after install
- Homebrew prefix stays with an admin-group owner (typically `admin`); never chown it to a non-admin invoking user
- dotfiles/config live in managed-machine-config; development uses the sibling repo, while Homebrew installs materialize a persistent writable checkout outside the Cellar
- state manifests live in ~/.config/managed-machine/*.manifest; never commit *.manifest
- bootstrap outcomes live in ~/.config/managed-machine/bootstrap.manifest; never record command output or secrets there
- local machine identity lives in ~/.config/managed-machine/machine.toml; never commit machine.toml to this repo
- SSH enrollment is explicit and human-only: bootstrap/--update/setup-gh/account setup never create, upload, or register SSH keys, never request key-upload scopes, never switch git_protocol to SSH, and never enable SSH signing; `managed-machine ssh enroll --authentication|--signing|--fleet` is the only path, behind an OS authorization dialog that names the local account, GitHub login, and purposes; agent accounts/sessions, root, and foreign-HOME invocations are refused before any mutation, and there is no --yes or env opt-out
- SSH passphrase policy lives in ~/.config/managed-machine/ssh-key-policy.toml; never commit ssh-key-policy.toml
- versioned fleet records and public SSH keys live only in the private managed-machine-config repo; default setup refreshes that checkout pull-only — publishing managed fleet state is reserved for `ssh enroll --fleet` and `managed-machine fleet`
- local-bin.ref in managed-machine-config pins qwts/local-bin ref
- git-hooks/ runs gitleaks protect --staged; setup-git-hooks wires hooks only when the script directory is the git toplevel, and chains an existing core.hooksPath instead of replacing it
- bin/managed-machine is the CLI entry point; resolves libexec via HOMEBREW_PREFIX or git clone
- scripts/update upgrades the tree it runs from, so it sources no library on both sides of `brew upgrade`: it re-execs itself afterward, and everything past the upgrade belongs to one version. Adding a `source` before the re-exec guard reintroduces the mixed-version run that broke Chrome installs on the upgrading run only
- Formula/managed-machine.rb is tag/version pinned; release with `scripts/release vX.Y.Z` from clean main at origin/main (refuses other branches, dirty tree, or reused tag; re-run resumes a failed push). The script bumps formula tag/version and VERSION together, refuses a version outside the skill's `qwts-versions` (revalidate skills/managed-machine in a reviewed PR first, ENG-0055), and pins the bundled managed-machine-config resource: it tags the sibling managed-machine-config checkout at the same version (clean main at origin/main, override path with MANAGED_MACHINE_CONFIG_DIR) and rewrites the resource's tag/revision so a release ships a known catalog snapshot
- bootstrap runs SETUP_SCRIPTS in order in scripts/bootstrap (setup-gh before setup-agent-bot before setup-bin, setup-zsh-functions right after setup-bin); `--update` re-runs only setup-gh, setup-agent-bot, setup-zsh, setup-bin, setup-zsh-functions plus auto catalog apps. A new concrete setup-* script is auto-available via `managed-machine setup <name>` but runs in bootstrap only if added to SETUP_SCRIPTS
- tests are plain bash, no runner: `bash tests/<name>.test.sh`. Run the full suite from a neutral working directory so Git credential fixtures do not inherit a checkout's local bot helper: `root="$(pwd)"; (cd /tmp && for t in "$root"/tests/*.test.sh; do bash "$t" || exit "$?"; done)`
- `--update` refuses from agent sessions/harness markers (exits skipped before any dialog); run it from a human Terminal in the owner's account
- .cursor/plans/ are durable specs (Intent, Target, Acceptance, Replay): update the matching plan in the same change, never delete; .cursor/rules/ (commit-identity, plans-as-specs) always apply
- install.sh is curlable; checks brew ownership before proceeding
- confirm before destructive/repo-wide actions
- never `git config` user.name/user.email. The macOS account decides whose work a commit is (ENG-0339, which supersedes ENG-0045): in a `qwts-<harness>-agent` account every checkout is bot work, attributed to that harness's `qwts-<harness>-agent[bot]` App by agent-bot's hooks (they add the `Agent-Identity` trailer; never hand-write it). In the owner's account the harness is the owner's delegate by default: plain human commits as `qwts`, with no trailer or other agent marker, unless it is told to act as the bot through `GH_AGENT_APP`, `--app`, or a worktree pin (`agentbot.app`), which keep bot attribution wherever that worktree lives. Worktree directories (`.claude/worktrees/` and the like) are a layout choice, not an identity boundary. As the delegate, if git would otherwise use a macOS full name or a `*.local`/`*.lan` hostname email, set `GIT_AUTHOR_NAME`/`GIT_COMMITTER_NAME` to `qwts` and `GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_EMAIL` to `91036491+qwts@users.noreply.github.com` for the commit rather than committing with them
- setup-agent-bot runs after setup-gh in bootstrap and --update: its private-tap fetch rides on setup-gh's GitHub auth, and it is the only step that moves the pinned agent-bot runtime

<!-- governed:shared-agent-discovery:start -->

## Shared agent conventions and skills

PR-first workflow, validation-before-push, commit and PR hygiene, and the
untrusted-input threat model are defined once, for every repo, in the
[org-wide agent conventions](https://github.com/qwts/agent-sop/blob/main/docs/reference/agent-conventions.md).
Before creating or copying a repo-local skill, consult the reviewed
[shared agent skills](https://github.com/qwts/agent-sop/blob/74e775ef23d8e7d8f8e693ccc2329f430978c096/skills/README.md)
index. Reuse only the pinned version supplied by the governed harness; a skill
genuinely specific to this repository belongs in its local context.
This repository is governed by
[agent-sop](https://github.com/qwts/agent-sop) — its
[shared SOPs](https://github.com/qwts/agent-sop/blob/main/docs/sop/README.md)
and [engineering decisions](https://github.com/qwts/agent-sop/blob/main/docs/decisions/README.md)
apply here by default
([ENG-0008](https://github.com/qwts/agent-sop/blob/main/docs/decisions/ENG-0008-shared-sop-inheritance.md):
inherit by default, vary by explicit delta).
<!-- governed:shared-agent-discovery:end -->

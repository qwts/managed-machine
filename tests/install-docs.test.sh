#!/usr/bin/env bash
# Installing needs qwts org access (the formula's config and local-bin
# resources are private): every documented install path must be an
# authenticated fetch, and no bootstrap-time repository URL may require SSH
# before setup-gh has provisioned a key.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

INSTALL_API_PATH='repos/qwts/managed-machine/contents/install.sh'

# README and the skill document the same authenticated installer command.
grep -q "$INSTALL_API_PATH" "$ROOT/README.md"
grep -q "$INSTALL_API_PATH" "$ROOT/skills/managed-machine/SKILL.md"
grep -q "$INSTALL_API_PATH" "$ROOT/install.sh"

# An unauthenticated raw.githubusercontent.com fetch would start an install
# that then fails at the private resources; it must not be documented as an
# install path anywhere.
if grep -rn 'raw.githubusercontent.com/qwts' "$ROOT/README.md" "$ROOT/skills/managed-machine/SKILL.md" "$ROOT/install.sh"; then
    echo 'unauthenticated raw.githubusercontent.com install path is documented' >&2
    exit 1
fi

# Bootstrap-time repository URLs are authenticated HTTPS, never SSH. Match
# concrete repository URLs only: lib/config-repo.sh legitimately pattern-matches
# the SSH form to migrate legacy remotes.
for f in install.sh Formula/managed-machine.rb legacy/setup-bin legacy/lib/config-repo.sh; do
    if grep -n 'git@github.com:qwts' "$ROOT/$f"; then
        echo "SSH repository URL in $f runs before setup-gh provisions SSH" >&2
        exit 1
    fi
done

# The installer authenticates gh and wires the git credential helper before
# tapping and installing the private formula resources.
grep -q 'gh auth setup-git' "$ROOT/install.sh"
grep -q 'ensure_gh_access' "$ROOT/install.sh"

# When agent-bot's gh shim refuses the session, ensure_gh_access names the
# refusal and the human-Terminal remedy, not gh auth login (#108). A plain
# missing login still gets the gh auth login advice.
GH_TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$GH_TEST_DIR"' EXIT
mkdir -p "$GH_TEST_DIR/bin"
cat >"$GH_TEST_DIR/bin/gh" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${GH_STUB_REFUSE:-}" ]]; then
    echo 'agent-bot: qwts-claude-agent is outside bot territory — refusing stock human gh' >&2
    exit 1
fi
exit 1
EOF
chmod +x "$GH_TEST_DIR/bin/gh"
sed -n '/^ensure_gh_access() {$/,/^}$/p' "$ROOT/install.sh" >"$GH_TEST_DIR/ensure_gh_access.sh"
grep -q '^ensure_gh_access()' "$GH_TEST_DIR/ensure_gh_access.sh"
for mode in refused missing; do
    if PATH="$GH_TEST_DIR/bin:/usr/bin:/bin" GH_STUB_REFUSE="$([[ "$mode" == refused ]] && echo 1)" \
        bash -c 'source "$1"; ensure_gh_access' _ "$GH_TEST_DIR/ensure_gh_access.sh" \
        >"$GH_TEST_DIR/$mode.out" 2>&1; then
        echo "expected ensure_gh_access to fail ($mode)" >&2
        exit 1
    fi
done
grep -Fq 'refused this session' "$GH_TEST_DIR/refused.out"
grep -Fq 'agent-bot: qwts-claude-agent is outside bot territory' "$GH_TEST_DIR/refused.out"
grep -Fq 'human Terminal' "$GH_TEST_DIR/refused.out"
! grep -q '^  gh auth login' "$GH_TEST_DIR/refused.out"
grep -Fq 'no active authenticated account' "$GH_TEST_DIR/missing.out"
grep -q '^  gh auth login -h github.com' "$GH_TEST_DIR/missing.out"

# Private brew operations run as the prefix owner must carry the invoking
# user's GitHub credentials without putting the token in process arguments.
grep -q 'write_brew_github_auth_run' "$ROOT/install.sh"
grep -q 'mm-gh-token' "$ROOT/install.sh"
grep -q 'brew-github-auth-run' "$ROOT/legacy/lib/brew.sh"
if grep -n 'HOMEBREW_GITHUB_API_TOKEN=\${token}' "$ROOT/install.sh" "$ROOT/legacy/lib/brew.sh"; then
    echo 'GitHub token must not be interpolated into argv' >&2
    exit 1
fi
if grep -n 'github_auth_env_for_brew_owner' "$ROOT/install.sh"; then
    echo 'argv-based github_auth_env_for_brew_owner should be gone' >&2
    exit 1
fi

# osascript admin / stub brew-owner homes have no PATH. The helper and the
# installer heredoc must use absolute chown and export a usable PATH.
for f in "$ROOT/legacy/lib/brew-github-auth-run" "$ROOT/legacy/lib/npm-public-run" "$ROOT/legacy/lib/elevate-plan-run" "$ROOT/install.sh"; do
    grep -q '/usr/sbin/chown' "$f" || {
        echo "missing /usr/sbin/chown in $f" >&2
        exit 1
    }
    grep -q 'PATH=/usr/sbin:/usr/bin:/bin' "$f" || {
        echo "missing login-less PATH in $f" >&2
        exit 1
    }
    if grep -nE '^chown |[^/[:alnum:]_]chown "' "$f"; then
        echo "bare chown in $f would fail in an empty osascript PATH" >&2
        exit 1
    fi
    # Every absolute binary path the helper runs in its empty login-less shell
    # must exist on macOS (e.g. touch is /usr/bin/touch, never /bin/touch).
    while IFS= read -r tool; do
        [[ -n "$tool" ]] || continue
        if [[ ! -x "$tool" ]]; then
            echo "$f references $tool which does not exist (single tool was run natively)" >&2
            exit 1
        fi
    done < <(grep -oE '(^|[[:space:]])/(usr/sbin|usr/bin|bin)/[A-Za-z0-9._-]+' "$f" | grep -oE '^/(usr/sbin|usr/bin|bin)/[A-Za-z0-9._-]+$' | sort -u)
done
if grep -nE '/usr/bin/sudo -H -u' "$ROOT/legacy/lib/elevate.sh" "$ROOT/legacy/lib/brew-github-auth-run" "$ROOT/legacy/lib/npm-public-run" "$ROOT/install.sh"; then
    echo 'sudo -H -u resets a stub brew-owner home to an empty shell' >&2
    exit 1
fi

# After chown to the brew owner, that user cannot traverse the invoking
# user's /var/folders TMPDIR. The helper workdir must be created under /tmp.
for f in "$ROOT/legacy/lib/brew-github-auth-run" "$ROOT/legacy/lib/npm-public-run" "$ROOT/install.sh"; do
    grep -q 'TMPDIR=/tmp' "$f" || {
        echo "missing TMPDIR=/tmp in $f" >&2
        exit 1
    }
    grep -q 'mktemp -d /tmp/mm-gh-auth.XXXXXX' "$f" || grep -q 'mktemp -d /tmp/mm-npm-run.XXXXXX' "$f" || {
        echo "helper workdir in $f must be created under /tmp" >&2
        exit 1
    }
    if grep -nE '\$\{TMPDIR:-/tmp\}/mm-gh-auth' "$f"; then
        echo "helper workdir in $f inherits the invoking user TMPDIR" >&2
        exit 1
    fi
done
for f in "$ROOT/legacy/lib/brew-github-auth-run" "$ROOT/legacy/lib/npm-public-run" "$ROOT/legacy/lib/elevate-plan-run" "$ROOT/install.sh"; do
    grep -q '^cd /tmp$' "$f" || {
        echo "missing cd /tmp in $f (dropped-privilege commands fail when caller PWD is mode 0700)" >&2
        exit 1
    }
done

# run_as_brew_owner is called more than once (tap, trust, update). A RETURN
# trap that is not cleared fires on the next function return with tokenfile
# unset under set -u.
if ! grep -q "rm -f \"\$tokenfile\" \"\$helper\"; trap - RETURN" "$ROOT/install.sh"; then
    echo 'install.sh RETURN trap must clear itself after removing the token file' >&2
    exit 1
fi
if ! grep -q "rm -f \"\$tokenfile\"; trap - RETURN" "$ROOT/legacy/lib/brew.sh"; then
    echo 'lib/brew.sh RETURN trap must clear itself after removing the token file' >&2
    exit 1
fi

# brew re-execs with env -i and drops GIT_CONFIG_*/GH_TOKEN. The helper must
# install a HOME .gitconfig so the private resource fetches can authenticate.
for f in "$ROOT/legacy/lib/brew-github-auth-run" "$ROOT/install.sh"; do
    grep -q 'gitconfig_home=$owner_home/.gitconfig' "$f" || {
        echo "missing owner HOME gitconfig install in $f" >&2
        exit 1
    }
    grep -q 'credential "https://github.com"' "$f" || {
        echo "missing GitHub credential helper gitconfig in $f" >&2
        exit 1
    }
    grep -q 'created_gitconfig' "$f" || {
        echo "missing gitconfig cleanup marker in $f" >&2
        exit 1
    }
done

echo 'install docs tests passed'

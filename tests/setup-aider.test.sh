#!/usr/bin/env bash
# setup-aider: official installer, managed PATH, idempotent when present, the
# vendor installer sees UV_NO_MODIFY_PATH and never edits ~/.zshrc (#87), and
# install_catalog_app chains into config/aider without ever clobbering a
# user-owned ~/.aider.conf.yml.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
CURL_LOG="$TEST_ROOT/curl.log"
INSTALL_LOG="$TEST_ROOT/install.log"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"
CONFIG_REPO="$TEST_ROOT/managed-machine-config"
mkdir -p "$CONFIG_REPO/config" "$CONFIG_REPO/dotfiles/aider"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
cp "$ROOT/tests/fixtures/config-aider" "$CONFIG_REPO/config/aider"
chmod +x "$CONFIG_REPO/config/aider"
cat >"$CONFIG_REPO/dotfiles/aider/aider.conf.yml" <<'CONF'
# Managed by managed-machine.
git-commit-verify: true
CONF

# The fixture is a copy of the live config/aider. Pin it against the sibling
# config repo whenever one is on disk — tests/fixtures has drifted from a live
# config script before. $ROOT may be a linked worktree rather than the primary
# checkout (worktrees are a harness layout choice, ENG-0339), so resolve the
# sibling from the common git dir too; a Homebrew layout has neither and skips
# the check.
common_dir="$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)"
for candidate in \
    "${MANAGED_MACHINE_CONFIG_REPO:-}" \
    "$ROOT/../managed-machine-config" \
    "${common_dir:+$(dirname "$common_dir")/../managed-machine-config}"
do
    [[ -n "$candidate" && -f "$candidate/config/aider" ]] || continue
    cmp -s "$candidate/config/aider" "$ROOT/tests/fixtures/config-aider" \
        || { echo "tests/fixtures/config-aider has drifted from $candidate/config/aider" >&2; exit 1; }
    break
done

git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed
: >"$CURL_LOG"
: >"$INSTALL_LOG"

# The fake installer mirrors the real one's PATH handling: it symlinks into
# ~/.local/bin, and picks a shell rc file to append its block to from $SHELL
# (zsh -> ~/.zshrc). With MOCK_INSTALLER_IGNORES_SHELL=1 it appends the block
# whatever $SHELL says, standing in for an installer that cannot be steered.
cat >"$TEST_BIN/curl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >>'$CURL_LOG'
cat <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
printf 'url-args=%s shell=%s\n' "\$*" "\${SHELL:-}" >>'$INSTALL_LOG'
printf 'UV_NO_MODIFY_PATH=%s\n' "\${UV_NO_MODIFY_PATH-}" >>'$INSTALL_LOG'
mkdir -p "\$HOME/.local/bin"
cat >"\$HOME/.local/bin/aider" <<'AIDER'
#!/usr/bin/env bash
echo 'aider 0.1.0-test'
AIDER
chmod +x "\$HOME/.local/bin/aider"
case "\$(basename "\${SHELL:-}")" in
    zsh) rc="\$HOME/.zshrc" ;;
    bash) rc="\$HOME/.bashrc" ;;
    *) rc="" ;;
esac
[[ -n "\$rc" || "\${MOCK_INSTALLER_IGNORES_SHELL:-0}" == 1 ]] || exit 0
printf '\n# >>> aider installer >>>\nexport PATH="\$HOME/.uv/bin:\$PATH"\n# <<< aider installer <<<\n' >>"\${rc:-\$HOME/.zshrc}"
echo "  Updated \$HOME/.uv/bin in PATH in \${rc:-\$HOME/.zshrc}." >&2
INSTALLER
EOF
chmod +x "$TEST_BIN/curl"

run_setup() {
    HOME="$TEST_HOME" SHELL=/bin/zsh PATH="$TEST_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-aider"
}

MANIFEST="$TEST_HOME/.config/managed-machine/aider.manifest"

# 1. Missing aider: official URL, the installer sees UV_NO_MODIFY_PATH=1 and no
# login shell to edit, the binary lands through ~/.local/bin, and ~/.zshrc
# carries managed-machine's guard and no vendor block.
run_setup >"$TEST_ROOT/install.out" 2>&1
grep -Fq 'https://aider.chat/install.sh' "$CURL_LOG"
grep -Fxq 'UV_NO_MODIFY_PATH=1' "$INSTALL_LOG"
[[ -x "$TEST_HOME/.local/bin/aider" ]]
grep -Fq 'Aider installed:' "$TEST_ROOT/install.out"
grep -Fq 'aider 0.1.0-test' "$TEST_ROOT/install.out"
grep -Fq 'shell=/bin/sh' "$INSTALL_LOG"
grep -Fq '# BEGIN local-bin' "$TEST_HOME/.zshrc"
! grep -Fq 'aider installer' "$TEST_HOME/.zshrc"
[[ ! -e "$TEST_HOME/.bashrc" ]]
! grep -Fq 'edited' "$TEST_ROOT/install.out"

# 2. install_catalog_app chains into config/aider: the managed config lands
# verbatim, is recorded once, and carries nothing secret.
grep -Fq '==> config/aider' "$TEST_ROOT/install.out"
grep -Fq 'installed: aider.conf.yml' "$TEST_ROOT/install.out"
cmp -s "$CONFIG_REPO/dotfiles/aider/aider.conf.yml" "$TEST_HOME/.aider.conf.yml"
grep -qxF "$TEST_HOME/.aider.conf.yml" "$MANIFEST"
[[ "$(wc -l <"$MANIFEST")" -eq 1 ]]
grep -Fq 'git-commit-verify: true' "$TEST_HOME/.aider.conf.yml"
! grep -qiE 'api[_-]?key|sk-[A-Za-z0-9]|token' "$TEST_HOME/.aider.conf.yml"

# 3. Re-run is a no-op: curl is not invoked again, the config is untouched, and
# the manifest is not appended twice.
: >"$CURL_LOG"
: >"$INSTALL_LOG"
before="$(shasum -a 256 "$TEST_HOME/.aider.conf.yml" | awk '{print $1}')"
HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-aider" >"$TEST_ROOT/rerun.out"
[[ ! -s "$CURL_LOG" ]]
[[ ! -s "$INSTALL_LOG" ]]
grep -Fq 'Aider already installed:' "$TEST_ROOT/rerun.out"
grep -Fq 'already installed: aider.conf.yml' "$TEST_ROOT/rerun.out"
[[ "$(shasum -a 256 "$TEST_HOME/.aider.conf.yml" | awk '{print $1}')" == "$before" ]]
[[ "$(wc -l <"$MANIFEST")" -eq 1 ]]

# 4. A managed config the user then edited is still left alone. install_home_file
# is install-if-missing forever, never a refresher like install_zsh_startup_file.
printf '# user added this\n' >>"$TEST_HOME/.aider.conf.yml"
HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-aider" >"$TEST_ROOT/edited.out"
grep -Fq 'already installed: aider.conf.yml' "$TEST_ROOT/edited.out"
grep -qxF '# user added this' "$TEST_HOME/.aider.conf.yml"

# 5. A user-owned config is never clobbered and never adopted into the manifest.
USER_HOME="$TEST_ROOT/user-home"
mkdir -p "$USER_HOME/.local/bin"
cp "$TEST_HOME/.local/bin/aider" "$USER_HOME/.local/bin/aider"
printf 'model: my-own-choice\n' >"$USER_HOME/.aider.conf.yml"
user_hash="$(shasum -a 256 "$USER_HOME/.aider.conf.yml" | awk '{print $1}')"
HOME="$USER_HOME" PATH="$USER_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-aider" >"$TEST_ROOT/userowned.out"
grep -Fq 'skipping existing (not managed by managed-machine): aider.conf.yml' "$TEST_ROOT/userowned.out"
[[ "$(shasum -a 256 "$USER_HOME/.aider.conf.yml" | awk '{print $1}')" == "$user_hash" ]]
[[ ! -e "$USER_HOME/.config/managed-machine/aider.manifest" ]]

# 6. A non-executable config/aider is a silent no-op. catalog_config_script
# tests -x, so losing the exec bit in git would otherwise fail invisibly.
NOEXEC_HOME="$TEST_ROOT/noexec-home"
mkdir -p "$NOEXEC_HOME/.local/bin"
cp "$TEST_HOME/.local/bin/aider" "$NOEXEC_HOME/.local/bin/aider"
chmod -x "$CONFIG_REPO/config/aider"
HOME="$NOEXEC_HOME" PATH="$NOEXEC_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-aider" >"$TEST_ROOT/noexec.out"
! grep -Fq '==> config/aider' "$TEST_ROOT/noexec.out"
[[ ! -e "$NOEXEC_HOME/.aider.conf.yml" ]]
chmod +x "$CONFIG_REPO/config/aider"

# 7. A missing template fails loudly rather than silently skipping.
MISSING_HOME="$TEST_ROOT/missing-home"
mkdir -p "$MISSING_HOME/.local/bin"
cp "$TEST_HOME/.local/bin/aider" "$MISSING_HOME/.local/bin/aider"
mv "$CONFIG_REPO/dotfiles/aider/aider.conf.yml" "$TEST_ROOT/aider.conf.yml.bak"
if HOME="$MISSING_HOME" PATH="$MISSING_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-aider" >"$TEST_ROOT/missing.out" 2>&1; then
    echo 'expected setup-aider to fail with a missing template' >&2
    exit 1
fi
grep -Fq 'Error: missing template' "$TEST_ROOT/missing.out"
mv "$TEST_ROOT/aider.conf.yml.bak" "$CONFIG_REPO/dotfiles/aider/aider.conf.yml"

# 8. An installer that edits ~/.zshrc regardless is not silent: the run names
# the leak and the remedy, and still succeeds.
rm -f "$TEST_HOME/.local/bin/aider"
: >"$CURL_LOG"
MOCK_INSTALLER_IGNORES_SHELL=1 run_setup >"$TEST_ROOT/leak.out" 2>&1
grep -Fq 'aider installer' "$TEST_HOME/.zshrc"
grep -Fq "warn: Aider's installer edited $TEST_HOME/.zshrc outside managed-machine's guards" "$TEST_ROOT/leak.out"
grep -Fq 'managed-machine setup zsh' "$TEST_ROOT/leak.out"
grep -Fq 'Aider installed:' "$TEST_ROOT/leak.out"

echo 'setup-aider tests passed'

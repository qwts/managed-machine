#!/usr/bin/env bash
# uv-tool catalog rows install one pinned PyPI release with uv, from pypi.org
# only, and converge to that pin.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
UV_LOG="$TEST_DIR/uv.log"
CURL_LOG="$TEST_DIR/curl.log"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME/.local/bin" "$TEST_BIN" "$CONFIG_REPO"
# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/apps.sh
source "$ROOT/lib/apps.sh"

write_catalog() {
    cat >"$CONFIG_REPO/apps.json"
}
write_row() {
    write_catalog <<EOF
{"schema_version":1,"apps":[{"name":"mistral-vibe","kind":"uv-tool","display":"Mistral Vibe","package":"mistral-vibe","command":"vibe","version":"$1"}]}
EOF
}

# uv stub: records each call with the index environment it saw, keeps the
# installed version in $TEST_DIR/installed, and puts `vibe` in ~/.local/bin.
cat >"$TEST_BIN/uv" <<EOF
#!/usr/bin/env bash
printf '%s | no_config=%s index_url=%s\n' "\$*" "\${UV_NO_CONFIG-}" "\${UV_INDEX_URL-unset}" >>'$UV_LOG'
case "\$1 \$2" in
    'tool list')
        [[ -f '$TEST_DIR/installed' ]] && printf 'mistral-vibe v%s\n- vibe\n' "\$(cat '$TEST_DIR/installed')"
        exit 0
        ;;
    'tool dir') echo '$TEST_HOME/.local/bin' ;;
    'tool install')
        spec="\${@: -1}"
        [[ -z "\${UV_FAIL:-}" ]] || exit 2
        printf '%s\n' "\${spec##*==}" >'$TEST_DIR/installed'
        printf '#!/usr/bin/env bash\necho vibe\n' >'$TEST_HOME/.local/bin/vibe'
        chmod +x '$TEST_HOME/.local/bin/vibe'
        ;;
esac
EOF
# curl stub: the PyPI JSON API, which knows mistral-vibe 2.25.8 only.
cat >"$TEST_BIN/curl" <<EOF
#!/usr/bin/env bash
url="\${@: -1}"
printf '%s\n' "\$url" >>'$CURL_LOG'
case "\$url" in
    https://pypi.org/pypi/mistral-vibe/2.25.8/json)
        printf '{"info":{"name":"mistral-vibe","version":"2.25.8"}}\n' ;;
    *) exit 22 ;;
esac
EOF
chmod +x "$TEST_BIN/uv" "$TEST_BIN/curl"

run_install() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    UV_INDEX_URL='https://private.example/simple' \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    install_catalog_app mistral-vibe
}

# 1. A pinned row installs exactly that release from pypi.org, ignoring uv
# config files and a machine-wide index override.
write_row 2.25.8
run_install >"$TEST_DIR/install.out"
grep -Fxq 'https://pypi.org/pypi/mistral-vibe/2.25.8/json' "$CURL_LOG"
grep -Fxq 'tool install --force --default-index https://pypi.org/simple mistral-vibe==2.25.8 | no_config=1 index_url=unset' "$UV_LOG"
[[ "$(cat "$TEST_DIR/installed")" == 2.25.8 ]]
grep -Fq "mistral-vibe installed: $TEST_HOME/.local/bin/vibe" "$TEST_DIR/install.out"

# 2. A rerun at the pin is a no-op.
: >"$UV_LOG"
run_install >"$TEST_DIR/rerun.out"
grep -Fq 'mistral-vibe 2.25.8 already installed (uv tool)' "$TEST_DIR/rerun.out"
! grep -q '^tool install' "$UV_LOG"

# 3. Another uv-installed version converges to the pin.
printf '2.26.1\n' >"$TEST_DIR/installed"
run_install >"$TEST_DIR/drift.out"
grep -Fq 'mistral-vibe 2.26.1 is installed; converging to the pinned 2.25.8' "$TEST_DIR/drift.out"
[[ "$(cat "$TEST_DIR/installed")" == 2.25.8 ]]

# 4. A vibe installed some other way is left alone.
rm -f "$TEST_DIR/installed"
: >"$UV_LOG"
run_install >"$TEST_DIR/foreign.out"
grep -Fq 'already installed outside uv' "$TEST_DIR/foreign.out"
! grep -q '^tool install' "$UV_LOG"
rm -f "$TEST_HOME/.local/bin/vibe"

# 5. Refused before any install: a pin PyPI lacks, a missing or malformed
# pin, a malformed package, and a machine without uv.
for bad in 9.9.9 '' latest; do
    write_row "$bad"
    : >"$UV_LOG"
    if run_install >"$TEST_DIR/bad.out" 2>&1; then
        echo "expected the '$bad' pin to fail" >&2
        exit 1
    fi
    ! grep -q '^tool install' "$UV_LOG"
done
grep -Fq 'needs a pinned version' "$TEST_DIR/bad.out"
write_catalog <<'EOF'
{"schema_version":1,"apps":[{"name":"mistral-vibe","kind":"uv-tool","package":"../evil","command":"vibe","version":"2.25.8"}]}
EOF
if run_install >"$TEST_DIR/evil.out" 2>&1; then
    echo 'expected a malformed package to fail' >&2
    exit 1
fi
grep -Fq 'needs a valid PyPI package' "$TEST_DIR/evil.out"
write_row 2.25.8
mv "$TEST_BIN/uv" "$TEST_DIR/uv.off"
if run_install >"$TEST_DIR/nouv.out" 2>&1; then
    echo 'expected a missing uv to fail' >&2
    exit 1
fi
grep -Fq 'uv required — run managed-machine setup uv first' "$TEST_DIR/nouv.out"
mv "$TEST_DIR/uv.off" "$TEST_BIN/uv"

# 6. An install that does not end at the pin fails.
if UV_FAIL=1 run_install >"$TEST_DIR/fail.out" 2>&1; then
    echo 'expected a failed uv install to fail' >&2
    exit 1
fi

echo 'uv-tool tests passed'

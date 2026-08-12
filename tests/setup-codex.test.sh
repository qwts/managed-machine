#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
CONFIG="$TEST_HOME/.codex/config.toml"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME"
git init --quiet "$CONFIG_REPO_ROOT"
mkdir -p "$CONFIG_REPO_ROOT/dotfiles/codex/meta"
printf '{"models": []}\n' >"$CONFIG_REPO_ROOT/dotfiles/codex/meta/meta-models.json"
cat >"$CONFIG_REPO_ROOT/dotfiles/codex/meta/codex.toml.fragment" <<'EOF'
model = "meta-muse-spark"
model_provider = "meta"

[model_providers.meta]
name = "Meta"
base_url = "http://localhost:1234/v1"
EOF

run_setup() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" "$@"
}

# 1. Fresh machine: fragment lands in a managed block, catalog wired, and the
# active provider is verified.
run_setup >"$TEST_DIR/fresh.out"
grep -Fq 'applied provider configuration' "$TEST_DIR/fresh.out"
grep -qxF '# BEGIN managed-machine codex' "$CONFIG"
grep -qxF 'model_provider = "meta"' "$CONFIG"
grep -q 'model_catalog_json' "$CONFIG"
[[ -f "$TEST_HOME/.codex/meta-models.json" ]]

# 2. Re-run is byte-for-byte idempotent.
HASH_BEFORE="$(shasum -a 256 "$CONFIG")"
run_setup >"$TEST_DIR/rerun.out"
grep -Fq 'provider configuration already active' "$TEST_DIR/rerun.out"
[[ "$(shasum -a 256 "$CONFIG")" == "$HASH_BEFORE" ]]
[[ "$(grep -c 'model_catalog_json' "$CONFIG")" == "1" ]]
[[ "$(grep -c '# BEGIN managed-machine codex' "$CONFIG")" == "1" ]]

# 3. A fragment update rewrites the managed block without duplicating it.
sed -i '' 's/meta-muse-spark/meta-muse-nova/' "$CONFIG_REPO_ROOT/dotfiles/codex/meta/codex.toml.fragment"
run_setup >"$TEST_DIR/update.out"
grep -Fq 'applied provider configuration' "$TEST_DIR/update.out"
grep -qxF 'model = "meta-muse-nova"' "$CONFIG"
! grep -q 'meta-muse-spark' "$CONFIG"
[[ "$(grep -c '# BEGIN managed-machine codex' "$CONFIG")" == "1" ]]

# 4. User-customized keys outside the block defer with the manual merge
# reported; the user's config is not touched.
USER_HOME="$TEST_DIR/user-home"
mkdir -p "$USER_HOME/.codex"
cat >"$USER_HOME/.codex/config.toml" <<'EOF'
model = "my-own-model"
EOF
USER_HASH="$(shasum -a 256 "$USER_HOME/.codex/config.toml" | awk '{print $1}')"
set +e
HOME="$USER_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/conflict.out" 2>&1
STATUS=$?
set -e
[[ "$STATUS" == "75" ]]
grep -Fq 'already sets' "$TEST_DIR/conflict.out"
grep -Fq 'model' "$TEST_DIR/conflict.out"
grep -Fq 'Deferred:' "$TEST_DIR/conflict.out"
grep -Fq 'managed-machine setup codex' "$TEST_DIR/conflict.out"
[[ "$(shasum -a 256 "$USER_HOME/.codex/config.toml" | awk '{print $1}')" == "$USER_HASH" ]]
! grep -q '# BEGIN managed-machine codex' "$USER_HOME/.codex/config.toml"

# 5. No bundled fragment: setup completes with the catalog wiring only.
NOFRAG_ROOT="$TEST_DIR/nofrag-config"
git init --quiet "$NOFRAG_ROOT"
mkdir -p "$NOFRAG_ROOT/dotfiles/codex/meta"
printf '{"models": []}\n' >"$NOFRAG_ROOT/dotfiles/codex/meta/meta-models.json"
NOFRAG_HOME="$TEST_DIR/nofrag-home"
mkdir -p "$NOFRAG_HOME"
HOME="$NOFRAG_HOME" CONFIG_REPO_ROOT="$NOFRAG_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/nofrag.out"
grep -Fq 'no provider fragment bundled' "$TEST_DIR/nofrag.out"
grep -q 'model_catalog_json' "$NOFRAG_HOME/.codex/config.toml"

echo 'setup-codex tests passed'

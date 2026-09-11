#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
CONFIG="$TEST_HOME/.codex/config.toml"
PROFILES_STATE="$TEST_HOME/.config/managed-machine/codex.profiles"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME"
git init --quiet "$CONFIG_REPO_ROOT"
git -C "$CONFIG_REPO_ROOT" config user.name 'test'
git -C "$CONFIG_REPO_ROOT" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO_ROOT" config commit.gpgsign false
mkdir -p "$CONFIG_REPO_ROOT/dotfiles/codex/profiles" "$CONFIG_REPO_ROOT/config"
cp "$ROOT/tests/fixtures/config-codex" "$CONFIG_REPO_ROOT/config/codex"
chmod +x "$CONFIG_REPO_ROOT/config/codex"
printf '{"models": []}\n' >"$CONFIG_REPO_ROOT/dotfiles/codex/meta-models.json"
cat >"$CONFIG_REPO_ROOT/dotfiles/codex/defaults.toml" <<'EOF'
model = "managed-default-model"
EOF
cat >"$CONFIG_REPO_ROOT/dotfiles/codex/profiles/muse.toml" <<'EOF'
[profiles.muse]
model = "meta-muse-spark"
model_provider = "meta"
model_catalog_json = "~/.codex/meta-models.json"

[model_providers.meta]
name = "Meta"
base_url = "http://localhost:1234/v1"
EOF

run_setup() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    MANAGED_MACHINE_ROOT="$ROOT" \
    PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" "$@"
}

root_region() {
    awk '/^[[:space:]]*\[/ { exit } { print }' "$1"
}

# 1. Fresh machine, no profiles enabled: only defaults land, in a managed
#    block, at root level; the catalog file is installed. No profile tables,
#    no provider wiring — profiles are opt-in.
run_setup >"$TEST_DIR/fresh.out"
grep -Fq 'applied codex configuration' "$TEST_DIR/fresh.out"
grep -qxF '# BEGIN managed-machine codex' "$CONFIG"
grep -qxF '# END managed-machine codex' "$CONFIG"
root_region "$CONFIG" | grep -qxF 'model = "managed-default-model"'
! grep -q 'profiles\.muse' "$CONFIG"
! root_region "$CONFIG" | grep -q 'model_provider'
[[ -f "$TEST_HOME/.codex/meta-models.json" ]]

# 2. --profile enables a named profile: its tables land inside the managed
#    block and the enablement persists in state.
run_setup --profile muse >"$TEST_DIR/profile.out"
grep -Fq 'applied codex configuration' "$TEST_DIR/profile.out"
grep -qxF '[profiles.muse]' "$CONFIG"
grep -qxF 'model_provider = "meta"' "$CONFIG"
grep -qxF 'muse' "$PROFILES_STATE"

# 3. Re-run is byte-for-byte idempotent and the enabled profile persists.
HASH_BEFORE="$(shasum -a 256 "$CONFIG")"
run_setup >"$TEST_DIR/rerun.out"
grep -Fq 'codex configuration already applied' "$TEST_DIR/rerun.out"
[[ "$(shasum -a 256 "$CONFIG")" == "$HASH_BEFORE" ]]
grep -qxF '[profiles.muse]' "$CONFIG"
[[ "$(grep -c '# BEGIN managed-machine codex' "$CONFIG")" == "1" ]]

# 4. A profile fragment update rewrites the managed block without duplicating.
sed -i '' 's/meta-muse-spark/meta-muse-nova/' "$CONFIG_REPO_ROOT/dotfiles/codex/profiles/muse.toml"
run_setup >"$TEST_DIR/update.out"
grep -Fq 'applied codex configuration' "$TEST_DIR/update.out"
grep -qxF 'model = "meta-muse-nova"' "$CONFIG"
! grep -q 'meta-muse-spark' "$CONFIG"
[[ "$(grep -c '# BEGIN managed-machine codex' "$CONFIG")" == "1" ]]

# 5. --disable-profile removes the profile tables but keeps the defaults.
run_setup --disable-profile muse >"$TEST_DIR/disable.out"
! grep -q 'profiles\.muse' "$CONFIG"
! grep -q 'model_providers\.meta' "$CONFIG"
root_region "$CONFIG" | grep -qxF 'model = "managed-default-model"'
[[ ! -s "$PROFILES_STATE" ]]

# 6. A user-set root key with a DIFFERENT value defers with the manual merge
#    reported; the user's config is not touched.
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
[[ "$STATUS" == "76" ]]
grep -Fq 'already sets' "$TEST_DIR/conflict.out"
grep -Fq 'model' "$TEST_DIR/conflict.out"
grep -Fq 'Skipped:' "$TEST_DIR/conflict.out"
[[ "$(shasum -a 256 "$USER_HOME/.codex/config.toml" | awk '{print $1}')" == "$USER_HASH" ]]
! grep -q '# BEGIN managed-machine codex' "$USER_HOME/.codex/config.toml"

# 6b. A user-set root key with the SAME value is converged, not a conflict:
#     the line is absorbed and the managed block supplies it.
SAME_HOME="$TEST_DIR/same-home"
mkdir -p "$SAME_HOME/.codex"
cat >"$SAME_HOME/.codex/config.toml" <<'EOF'
model = "managed-default-model"
EOF
HOME="$SAME_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/same.out"
grep -Fq 'applied codex configuration' "$TEST_DIR/same.out"
[[ "$(grep -c 'managed-default-model' "$SAME_HOME/.codex/config.toml")" == "1" ]]
grep -qxF '# BEGIN managed-machine codex' "$SAME_HOME/.codex/config.toml"
# Idempotent after absorption.
SAME_HASH="$(shasum -a 256 "$SAME_HOME/.codex/config.toml" | awk '{print $1}')"
HOME="$SAME_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/same-rerun.out"
grep -Fq 'codex configuration already applied' "$TEST_DIR/same-rerun.out"
[[ "$(shasum -a 256 "$SAME_HOME/.codex/config.toml" | awk '{print $1}')" == "$SAME_HASH" ]]

# 6c. A user-defined table matching a managed table header is a conflict
#     (duplicate TOML table) — defer, untouched.
TABLE_CONFLICT_HOME="$TEST_DIR/table-conflict-home"
mkdir -p "$TABLE_CONFLICT_HOME/.codex"
cat >"$TABLE_CONFLICT_HOME/.codex/config.toml" <<'EOF'
[profiles.muse]
model = "their-own-muse"
EOF
set +e
HOME="$TABLE_CONFLICT_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" --profile muse >"$TEST_DIR/table-conflict.out" 2>&1
STATUS=$?
set -e
[[ "$STATUS" == "76" ]]
grep -Fq 'profiles.muse' "$TEST_DIR/table-conflict.out"
grep -qxF 'model = "their-own-muse"' "$TABLE_CONFLICT_HOME/.codex/config.toml"

# 7. A same-named key inside the user's own unrelated table is NOT a global
#    conflict, and the managed block lands BEFORE their tables so fragment
#    root assignments stay root-level.
TABLE_HOME="$TEST_DIR/table-home"
mkdir -p "$TABLE_HOME/.codex"
cat >"$TABLE_HOME/.codex/config.toml" <<'EOF'
[model_providers.other]
name = "Other"
base_url = "http://other.example/v1"

[projects."/Users/someone/code"]
trust_level = "trusted"
EOF
HOME="$TABLE_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" --profile muse >"$TEST_DIR/tables.out"
grep -Fq 'applied codex configuration' "$TEST_DIR/tables.out"
TABLE_CONFIG="$TABLE_HOME/.codex/config.toml"
# Root region (before the first table) carries the managed defaults.
FIRST_TABLE_LINE="$(grep -nm1 '^\[' "$TABLE_CONFIG" | cut -d: -f1)"
head -n "$((FIRST_TABLE_LINE - 1))" "$TABLE_CONFIG" | grep -qE '^model = "managed-default-model"$'
# The user's own tables survive, after the managed block.
grep -qxF '[model_providers.other]' "$TABLE_CONFIG"
grep -qxF '[projects."/Users/someone/code"]' "$TABLE_CONFIG"
grep -qxF 'trust_level = "trusted"' "$TABLE_CONFIG"
# Idempotent on re-run even with user tables present.
TABLE_HASH="$(shasum -a 256 "$TABLE_CONFIG" | awk '{print $1}')"
HOME="$TABLE_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/tables-rerun.out"
grep -Fq 'codex configuration already applied' "$TEST_DIR/tables-rerun.out"
[[ "$(shasum -a 256 "$TABLE_CONFIG" | awk '{print $1}')" == "$TABLE_HASH" ]]

# 8. Migration: a config written by the old Muse-as-default version — a
#    prepended root-level model_catalog_json plus a block carrying root-level
#    provider keys — converges to defaults-only with the stale line removed.
MIG_HOME="$TEST_DIR/mig-home"
mkdir -p "$MIG_HOME/.codex"
cat >"$MIG_HOME/.codex/config.toml" <<EOF
model_catalog_json = "$MIG_HOME/.codex/meta-models.json"

# BEGIN managed-machine codex
model = "muse-spark-1.2-contributor"
model_provider = "meta"
model_catalog_json = "~/.codex/meta-models.json"
model_reasoning_effort = "high"

[model_providers.meta]
name = "Meta Model API"
base_url = "https://api.meta.ai/v1"
# END managed-machine codex
EOF
HOME="$MIG_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/mig.out"
MIG_CONFIG="$MIG_HOME/.codex/config.toml"
grep -Fq 'applied codex configuration' "$TEST_DIR/mig.out"
! grep -q 'model_catalog_json' "$MIG_CONFIG"
! grep -q 'model_provider' "$MIG_CONFIG"
! grep -q 'muse-spark' "$MIG_CONFIG"
root_region "$MIG_CONFIG" | grep -qxF 'model = "managed-default-model"'
MIG_HASH="$(shasum -a 256 "$MIG_CONFIG" | awk '{print $1}')"
HOME="$MIG_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/mig-rerun.out"
[[ "$(shasum -a 256 "$MIG_CONFIG" | awk '{print $1}')" == "$MIG_HASH" ]]

# 9. Unknown profile names fail before any write.
set +e
BAD_HOME="$TEST_DIR/bad-home"; mkdir -p "$BAD_HOME"
HOME="$BAD_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" --profile nosuch >"$TEST_DIR/bad.out" 2>&1
STATUS=$?
set -e
[[ "$STATUS" == "1" ]]
grep -Fq "unknown codex profile 'nosuch'" "$TEST_DIR/bad.out"
[[ ! -f "$BAD_HOME/.codex/config.toml" ]]

# 10. No managed content bundled: setup completes, catalog file installed,
#     no managed block, no root keys written.
NOFRAG_ROOT="$TEST_DIR/nofrag-config"
git init --quiet "$NOFRAG_ROOT"
mkdir -p "$NOFRAG_ROOT/dotfiles/codex" "$NOFRAG_ROOT/config"
cp "$ROOT/tests/fixtures/config-codex" "$NOFRAG_ROOT/config/codex"
chmod +x "$NOFRAG_ROOT/config/codex"
printf '{"models": []}\n' >"$NOFRAG_ROOT/dotfiles/codex/meta-models.json"
NOFRAG_HOME="$TEST_DIR/nofrag-home"
mkdir -p "$NOFRAG_HOME"
HOME="$NOFRAG_HOME" CONFIG_REPO_ROOT="$NOFRAG_ROOT" PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-codex" >"$TEST_DIR/nofrag.out"
grep -Fq 'no managed codex configuration bundled' "$TEST_DIR/nofrag.out"
[[ -f "$NOFRAG_HOME/.codex/meta-models.json" ]]
! grep -q 'managed-machine codex' "$NOFRAG_HOME/.codex/config.toml"

echo 'setup-codex tests passed'

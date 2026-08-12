#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
CARGO_HOME="$TEST_DIR/cargo"
BREW_PREFIX="$TEST_DIR/brew"
RUSTUP_LOG="$TEST_DIR/rustup.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$CARGO_HOME/bin" "$BREW_PREFIX/opt/rustup/bin"

# Homebrew-style layout: the real rustup lives in opt/rustup/bin; the wrapper
# in $BREW_PREFIX/bin strips argv[0] (the historical breakage).
cat >"$BREW_PREFIX/opt/rustup/bin/rustup" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$RUSTUP_LOG"
case "$1" in
    default)
        # Recreate healthy cargo proxies the way real rustup does.
        for tool in rustc cargo rustfmt cargo-clippy; do
            cat >"$CARGO_HOME/bin/$tool" <<'PROXY'
#!/usr/bin/env bash
[[ "$1" == "--version" ]] && echo "stub 1.0.0"
PROXY
            chmod +x "$CARGO_HOME/bin/$tool"
        done
        ;;
    show) echo 'stub toolchain: stable' ;;
esac
exit 0
EOF
chmod +x "$BREW_PREFIX/opt/rustup/bin/rustup"
mkdir -p "$BREW_PREFIX/bin"
printf '#!/usr/bin/env bash\nexit 1\n' >"$BREW_PREFIX/bin/rustup"
printf '#!/usr/bin/env bash\nexit 1\n' >"$BREW_PREFIX/bin/rustup-init"
chmod +x "$BREW_PREFIX/bin/rustup" "$BREW_PREFIX/bin/rustup-init"

# Legacy breakage: proxies pointing at rustup-init, at the argv[0]-stripping
# Homebrew wrapper, and at a target that no longer exists.
ln -s "$BREW_PREFIX/bin/rustup-init" "$CARGO_HOME/bin/rustup"
ln -s "$BREW_PREFIX/bin/rustup" "$CARGO_HOME/bin/rustc"
ln -s "$TEST_DIR/definitely-missing" "$CARGO_HOME/bin/cargo"
# A broken user-owned symlink that is NOT a rustup proxy must survive repair.
ln -s "$TEST_DIR/unavailable-volume/my-tool" "$CARGO_HOME/bin/my-user-tool"

run_setup() {
    HOME="$TEST_HOME" \
    CARGO_HOME="$CARGO_HOME" \
    RUSTUP_LOG="$RUSTUP_LOG" \
    HOMEBREW_PREFIX="$BREW_PREFIX" \
    PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-rust" "$@"
}

run_setup >"$TEST_DIR/repair.out"

# Broken proxies were removed and named for what they were.
grep -Fq 'removing stale rustup-init proxy: rustup' "$TEST_DIR/repair.out"
grep -Fq 'removing argv[0]-stripping wrapper proxy: rustc' "$TEST_DIR/repair.out"
grep -Fq 'removing broken cargo proxy: cargo' "$TEST_DIR/repair.out"

# The Homebrew opt wrappers were used, the toolchain converged, and the
# validated compilers report as working.
grep -Fq "Using Homebrew rustup wrappers: $BREW_PREFIX/opt/rustup/bin" "$TEST_DIR/repair.out"
grep -qxF 'set profile default' "$RUSTUP_LOG"
grep -qxF 'default stable' "$RUSTUP_LOG"
grep -qxF 'component add rustfmt clippy' "$RUSTUP_LOG"
[[ -x "$CARGO_HOME/bin/rustc" && ! -L "$CARGO_HOME/bin/rustc" ]]
grep -Fq 'rustc: ' "$TEST_DIR/repair.out"

# The unrelated broken user symlink is untouched.
[[ -L "$CARGO_HOME/bin/my-user-tool" ]]
! grep -q 'my-user-tool' "$TEST_DIR/repair.out"

# Re-run is idempotent: healthy proxies stay untouched.
: >"$RUSTUP_LOG"
run_setup >"$TEST_DIR/rerun.out"
! grep -q 'removing' "$TEST_DIR/rerun.out"

# When rustup cannot produce working compilers, setup fails loudly instead of
# printing "command not found" and exiting zero.
BROKEN_BREW="$TEST_DIR/broken-brew"
mkdir -p "$BROKEN_BREW/opt/rustup/bin"
cat >"$BROKEN_BREW/opt/rustup/bin/rustup" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$BROKEN_BREW/opt/rustup/bin/rustup"
BROKEN_CARGO="$TEST_DIR/broken-cargo"
mkdir -p "$BROKEN_CARGO/bin"
if HOME="$TEST_HOME" CARGO_HOME="$BROKEN_CARGO" HOMEBREW_PREFIX="$BROKEN_BREW" \
    PATH="/usr/bin:/bin" /bin/bash "$ROOT/setup-rust" >"$TEST_DIR/broken.out" 2>&1; then
    echo 'expected setup-rust to fail when compilers do not run' >&2
    exit 1
fi
grep -Fq 'Error: rustc not found on PATH after setup' "$TEST_DIR/broken.out"

echo 'setup-rust tests passed'

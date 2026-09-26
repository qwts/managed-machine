#!/usr/bin/env bash
# brew re-execs with env -i and keeps HOME but drops GIT_CONFIG_* / GH_TOKEN.
# Private tap clone must authenticate from ~/.gitconfig in that HOME.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

home="$(mktemp -d /tmp/mm-gh-auth-home.XXXXXX)"
workdir="$(mktemp -d /tmp/mm-gh-auth.XXXXXX)"
cleanup() {
    rm -rf "$home" "$workdir"
}
trap cleanup EXIT INT TERM

printf 'secret-token\n' >"$workdir/token"
cat >"$workdir/cred" <<EOF
#!/bin/sh
if [ "\$1" = get ]; then
    printf 'username=x-access-token\\n'
    printf 'password=%s\\n' "\$(cat '$workdir/token')"
fi
EOF
chmod 700 "$workdir/cred"
cat >"$home/.gitconfig" <<EOF
[credential "https://github.com"]
	helper = $workdir/cred
EOF

# Same filtered environment brew leaves after env -i: HOME kept, no GIT_CONFIG_*.
out="$(printf 'protocol=https\nhost=github.com\n\n' |
    env -i HOME="$home" PATH=/usr/bin:/bin \
        GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null \
        GIT_TERMINAL_PROMPT=0 \
        git credential fill)"

echo "$out" | grep -q 'username=x-access-token' || {
    echo "HOME gitconfig helper did not supply username" >&2
    exit 1
}
echo "$out" | grep -q 'password=secret-token' || {
    echo "HOME gitconfig helper did not supply token" >&2
    exit 1
}

echo 'brew github auth tests passed'

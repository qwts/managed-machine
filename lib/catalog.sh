#!/usr/bin/env bash
# Declarative app catalog from managed-machine-config/apps.json.
# Install engines live in managed-machine; this file only loads policy.

catalog_file() {
    local repo="${CONFIG_REPO_ROOT:-}"
    if [[ -z "$repo" ]]; then
        if [[ -n "${REPO_ROOT:-}" && -f "$REPO_ROOT/../managed-machine-config/apps.json" ]]; then
            repo="$REPO_ROOT/../managed-machine-config"
        elif [[ -f "$(managed_machine_config_checkout_dir)/apps.json" ]]; then
            repo="$(managed_machine_config_checkout_dir)"
        fi
    fi
    [[ -n "$repo" && -f "$repo/apps.json" ]] || return 1
    printf '%s/apps.json\n' "$repo"
}

catalog_config_script() {
    local name="$1"
    local repo="${CONFIG_REPO_ROOT:-}"
    if [[ -z "$repo" ]]; then
        if [[ -n "${REPO_ROOT:-}" && -d "$REPO_ROOT/../managed-machine-config/config" ]]; then
            repo="$REPO_ROOT/../managed-machine-config"
        elif [[ -d "$(managed_machine_config_checkout_dir)/config" ]]; then
            repo="$(managed_machine_config_checkout_dir)"
        fi
    fi
    [[ -n "$repo" && -x "$repo/config/$name" ]] || return 1
    printf '%s/config/%s\n' "$repo" "$name"
}

# catalog_query <command> [args...]  — python3 JSON lookups against apps.json
catalog_query() {
    local file
    file="$(catalog_file)" || return 1
    MANAGED_MACHINE_CATALOG="$file" python3 -c '
import json, os, sys

path = os.environ["MANAGED_MACHINE_CATALOG"]
with open(path) as fh:
    data = json.load(fh)
apps = data.get("apps") or []
if not isinstance(apps, list):
    sys.stderr.write("Error: apps.json must contain an apps array\n")
    sys.exit(1)

def aliases(app):
    names = [app.get("name") or ""]
    token = app.get("token") or ""
    if token:
        names.append(token)
    for alias in app.get("aliases") or []:
        names.append(alias)
    extra = []
    for name in names:
        if name.startswith("setup-"):
            continue
        extra.append("setup-" + name)
    return {n for n in names + extra if n}

def find(name):
    for app in apps:
        if name in aliases(app):
            return app
    return None

def is_auto(app):
    if "auto" not in app:
        return True
    value = app["auto"]
    if value is True:
        return True
    if value is False:
        return False
    sys.stderr.write("Error: apps.json auto must be a boolean\n")
    sys.exit(1)

# Vendors that serve a rolling "latest" URL publish no per-build checksum, so
# homebrew/cask records sha256 :no_check. Opting a row in trades that checksum
# for notarized Developer ID verification and must be declared per app.
def allows_rolling_url(app):
    if "allow_rolling_url" not in app:
        return False
    value = app["allow_rolling_url"]
    if value is True:
        return True
    if value is False:
        return False
    sys.stderr.write("Error: apps.json allow_rolling_url must be a boolean\n")
    sys.exit(1)

for app in apps:
    is_auto(app)
    allows_rolling_url(app)

cmd = sys.argv[1]
if cmd == "names":
    for app in apps:
        name = app.get("name") or ""
        if name:
            print(name)
elif cmd == "auto-names":
    for app in apps:
        name = app.get("name") or ""
        if name and is_auto(app):
            print(name)
elif cmd == "is-auto":
    app = find(sys.argv[2])
    if not app:
        sys.exit(1)
    sys.exit(0 if is_auto(app) else 1)
elif cmd == "kinds":
    seen = set()
    for app in apps:
        kind = app.get("kind") or ""
        if kind and kind not in seen:
            seen.add(kind)
            print(kind)
elif cmd == "resolve":
    app = find(sys.argv[2])
    if not app or not app.get("name"):
        sys.exit(1)
    print(app["name"])
elif cmd == "field":
    app = find(sys.argv[2])
    if not app:
        sys.exit(1)
    key = sys.argv[3]
    value = app.get(key)
    if value is None:
        sys.exit(1)
    if isinstance(value, list):
        print(",".join(str(v) for v in value))
    elif isinstance(value, dict):
        json.dump(value, sys.stdout)
        print()
    else:
        print(value)
elif cmd == "cask-tokens":
    for app in apps:
        if app.get("kind") in ("signed-cask", "cask") and app.get("token"):
            print(app["token"])
elif cmd == "cask-resolve":
    name = sys.argv[2]
    for app in apps:
        if app.get("kind") not in ("signed-cask", "cask"):
            continue
        if name in aliases(app):
            print(app.get("token") or "")
            sys.exit(0)
    sys.exit(1)
elif cmd == "cask-name":
    token = sys.argv[2]
    for app in apps:
        if app.get("token") == token and app.get("kind") in ("signed-cask", "cask"):
            print(app.get("name") or "")
            sys.exit(0)
    sys.exit(1)
elif cmd == "cask-row":
    token = sys.argv[2]
    for app in apps:
        if app.get("token") == token and app.get("kind") in ("signed-cask", "cask"):
            print("|".join([
                app.get("app_name") or "",
                app.get("team_id") or "",
                ",".join(app.get("url_hosts") or []),
                ",".join(app.get("homepage_hosts") or []),
                "1" if allows_rolling_url(app) else "",
            ]))
            sys.exit(0)
    sys.exit(1)
elif cmd == "dmg-row":
    name = sys.argv[2]
    arch = sys.argv[3] if len(sys.argv) > 3 else ""
    for app in apps:
        if app.get("kind") != "vendor-dmg":
            continue
        if name in aliases(app):
            # Per-arch builds fall back to the single url/sha256 pair, so a
            # row serves one build everywhere or one build per architecture.
            if arch in ("arm64", "aarch64"):
                url = app.get("url_arm64") or app.get("url") or ""
                digest = app.get("sha256_arm64") or app.get("sha256") or ""
            elif arch == "x86_64":
                url = app.get("url_x86_64") or app.get("url") or ""
                digest = app.get("sha256_x86_64") or app.get("sha256") or ""
            else:
                url = app.get("url") or ""
                digest = app.get("sha256") or ""
            print("|".join([
                app.get("app_name") or "",
                app.get("team_id") or "",
                url,
                digest,
                ",".join(app.get("url_hosts") or []),
                app.get("version") or "",
                "1" if allows_rolling_url(app) else "",
            ]))
            sys.exit(0)
    sys.exit(1)
elif cmd == "args":
    app = find(sys.argv[2])
    if not app:
        sys.exit(1)
    args = app.get("args") or []
    if not isinstance(args, list):
        sys.stderr.write("Error: apps.json args must be an array\n")
        sys.exit(1)
    for arg in args:
        print(arg)
elif cmd == "json":
    app = find(sys.argv[2])
    if not app:
        sys.exit(1)
    json.dump(app, sys.stdout)
    print()
else:
    sys.stderr.write("Error: unknown catalog query %s\n" % cmd)
    sys.exit(1)
' "$@"
}

catalog_app_names() {
    catalog_query names
}

catalog_auto_app_names() {
    catalog_query auto-names
}

catalog_app_is_auto() {
    catalog_query is-auto "$1"
}

catalog_resolve_name() {
    catalog_query resolve "$1"
}

catalog_app_kind() {
    catalog_query field "$1" kind
}

catalog_app_field() {
    catalog_query field "$1" "$2"
}

catalog_app_args() {
    catalog_query args "$1"
}

catalog_has_app() {
    catalog_resolve_name "$1" >/dev/null 2>&1
}

# Apply config/<name> from the config repo when the script exists.
apply_config_script() {
    local name="$1"
    shift
    local script root
    if ! script="$(catalog_config_script "$name")"; then
        return 0
    fi
    root="${REPO_ROOT:-}"
    echo "==> config/$name"
    MANAGED_MACHINE_ROOT="$root" CONFIG_REPO_ROOT="${CONFIG_REPO_ROOT:-}" "$script" "$@"
}

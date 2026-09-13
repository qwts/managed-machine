#!/usr/bin/env bash
# Agent account helpers (ENG-0339): one standard macOS account per harness,
# short name = the harness-level roster slug, full name = the persona. The
# account name IS the mapping — no registry beyond the roster plus this
# convention, so every check here derives from the slug and the live
# directory, never from a state file.

AGENT_ACCOUNT_SLUG_RE='^[a-z0-9][a-z0-9-]*$'

# Where machine-global agent coordination state lives (ENG-0339 §7): a sticky
# shared root so no account can delete another's entries, with a non-sticky,
# world-writable lock area inside it so a surviving account can clear a
# crashed account's stale lock across UIDs.
agent_shared_space_root() {
    if [[ -n "${MANAGED_MACHINE_AGENT_SHARED_ROOT:-}" ]]; then
        printf '%s\n' "$MANAGED_MACHINE_AGENT_SHARED_ROOT"
    elif [[ -d /Users/Shared ]]; then
        printf '/Users/Shared/Public\n'
    else
        printf '/tmp/agent-shared\n'
    fi
}

# Resolve the roster source used to validate slugs. Precedence: an explicit
# override, then the installed agent-bot runtime config (its validated
# organization profile carries the identities), then a raw profile the
# operator dropped next to the machine config.
agent_roster_source() {
    if [[ -n "${MANAGED_MACHINE_ORG_PROFILE:-}" ]]; then
        printf '%s\n' "$MANAGED_MACHINE_ORG_PROFILE"
        return 0
    fi
    local candidate
    for candidate in \
        "$HOME/.config/agent-bot/config.json" \
        "$(managed_machine_config_dir)/organization-profile.json"; do
        if [[ -f "$candidate" ]]; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done
    return 1
}

# agent_roster_query <status|harness> <slug>
#
# Reads any of the three shapes the roster travels in — a raw organization
# profile ({identities: [...]}), the agent-bot runtime config
# ({profile: {identities: [...]}}), or playbook's agents.json
# ({agents: [...]}) — and prints the requested field. Unknown slugs print
# "unknown" for status and nothing for harness; a malformed file fails.
agent_roster_query() {
    local mode="$1" slug="$2" source
    source="$(agent_roster_source)" || {
        echo "Error: no roster source found — install agent-bot with its organization profile, or set MANAGED_MACHINE_ORG_PROFILE" >&2
        return 1
    }
    AGENT_ROSTER_FILE="$source" AGENT_ROSTER_MODE="$mode" AGENT_ROSTER_SLUG="$slug" python3 -c '
import json, os, sys
with open(os.environ["AGENT_ROSTER_FILE"]) as handle:
    data = json.load(handle)
rows = data.get("identities") or data.get("agents") \
    or (data.get("profile") or {}).get("identities") or []
row = next((r for r in rows if r.get("slug") == os.environ["AGENT_ROSTER_SLUG"]), None)
mode = os.environ["AGENT_ROSTER_MODE"]
if mode == "status":
    print(row.get("status", "unknown") if row else "unknown")
elif mode == "harness":
    if row:
        print(row.get("harness", ""))
else:
    sys.exit(f"unknown roster query mode: {mode}")
'
}

# The persona shown as the account full name: the harness key, capitalized
# (goose -> "Goose"). ENG-0339 §2 keeps the roster free of new fields, so
# anything fancier is the operator's --full-name to give.
agent_persona_name() {
    local harness="$1"
    printf '%s\n' "$(tr '[:lower:]' '[:upper:]' <<<"${harness:0:1}")${harness:1}"
}

agent_account_exists() {
    /usr/bin/dscl . -read "/Users/$1" UniqueID >/dev/null 2>&1
}

agent_account_home() {
    /usr/bin/dscl . -read "/Users/$1" NFSHomeDirectory 2>/dev/null | /usr/bin/sed -n 's/^NFSHomeDirectory: //p'
}

agent_account_full_name() {
    # dscl prints one-word values inline ("RealName: Goose") and multiword
    # values on a continuation line; accept both.
    /usr/bin/dscl . -read "/Users/$1" RealName 2>/dev/null \
        | /usr/bin/sed -n -e 's/^RealName: //p' -e '2s/^ //p' | /usr/bin/head -1
}

# agent_account_in_group <slug> <group>: the OS membership verdict.
# dsmemberutil counts nested groups, UUID-only members, and a matching
# primary gid; the dscl listing is only the fallback when it cannot answer.
agent_account_in_group() {
    local verdict
    if verdict="$(/usr/bin/dsmemberutil checkmembership -U "$1" -G "$2" 2>/dev/null)"; then
        case "$verdict" in
            *'not a member'*) return 1 ;;
            *'is a member'*) return 0 ;;
        esac
    fi
    /usr/bin/dscl . -read "/Groups/$2" GroupMembership 2>/dev/null \
        | tr ' ' '\n' | grep -Fxq "$1"
}

# ENG-0339 §2: agent accounts are standard, never admin. Membership is a
# hard compliance failure, not something to demote silently.
agent_account_is_admin() {
    agent_account_in_group "$1" admin
}

# Every agent account is also a member of one supplementary group, so machine
# policy (shared-space ACLs, launchd and firewall rules) can address the whole
# agent population by a single gid instead of enumerating slugs.
AGENT_ACCOUNT_GROUP='agents'

# True when the invoking process runs inside an agent harness or session, or
# as an OS account provisioned as an agent: a harness session marker, an
# account in the OS-level agents group every roster account joins (add-agent
# guarantees membership — this is a directory fact, not a name glob), or an
# account name that IS a rostered identity slug (ENG-0339: the name is the
# mapping). An absent or unreadable roster source does not by itself prove
# the account is human, but the group check remains authoritative for
# provisioned agent accounts. Shared by `managed-machine status` and
# `ssh enroll` so both use the exact same definition of "this is an agent,
# not the human operator".
agent_current_context() {
    local account="${1:-$(command /usr/bin/id -un 2>/dev/null || true)}" status
    if PATH=/usr/bin:/bin:/usr/sbin:/sbin managed_machine_agent_session; then
        echo 'agent session markers are present in this environment' >&2
        return 0
    fi
    if [[ -n "$account" ]] && agent_account_in_group "$account" "$AGENT_ACCOUNT_GROUP"; then
        echo "account $account is a member of the $AGENT_ACCOUNT_GROUP group" >&2
        return 0
    fi
    if [[ -n "$account" ]] && agent_roster_source >/dev/null 2>&1; then
        status="$(agent_roster_query status "$account" 2>/dev/null || true)"
        if [[ -n "$status" && "$status" != "unknown" ]]; then
            echo "account $account is a rostered agent identity (status: $status)" >&2
            return 0
        fi
    fi
    return 1
}

# Narrower than agent_current_context: true only for facts about the OS
# account itself (its name, its agents-group membership, its roster
# registration) — never merely because ambient harness session markers
# (CLAUDECODE, CURSOR_AGENT, a CODEX_-prefixed var, ...) are set in the
# current process's environment. A harness running with those markers
# inside the human owner's own normally-named account has a $HOME that IS
# the account this machine enrolls under — machine.toml being absent there
# means "never enrolled" (missing), not "recorded elsewhere, indeterminate".
# Callers that decide where enrollment state physically lives
# (machine_status, the ssh-status fleet line) need this distinction;
# callers that decide enrollment *eligibility* (ssh enroll's human-only
# gate) want the broader agent_current_context instead.
agent_provisioned_account() {
    local account="${1:-$(command /usr/bin/id -un 2>/dev/null || true)}" status
    case "$account" in *-*-agent) return 0 ;; esac
    if [[ -n "$account" ]] && agent_account_in_group "$account" "$AGENT_ACCOUNT_GROUP"; then
        return 0
    fi
    if [[ -n "$account" ]] && agent_roster_source >/dev/null 2>&1; then
        status="$(agent_roster_query status "$account" 2>/dev/null || true)"
        [[ -n "$status" && "$status" != "unknown" ]] && return 0
    fi
    return 1
}

# Numeric ids are the OS's to assign. The account name is the mapping
# (ENG-0339 §2) and nothing in the identity chain keys on the uid or gid;
# directory-joined fleets never had consistent numbers either. Pinning them
# would also require modifying records after creation, which macOS refuses
# even to root reached through the authorization dialog (eDSPermissionError
# on dscl -change / -delete and sysadminctl -deleteUser, observed on macOS
# 26 — where sysadminctl -addUser also ignores -UID outright).

# The account picture is the App's GitHub avatar, installed root-owned under
# the system pictures directory so the login window and fast user switching
# can read it from any session. The path is derived from the slug alone.
AGENT_ACCOUNT_PICTURES_DIR='/Library/User Pictures/agents'

agent_account_picture_path() {
    printf '%s/%s.png\n' "$AGENT_ACCOUNT_PICTURES_DIR" "$1"
}

agent_account_picture() {
    /usr/bin/dscl . -read "/Users/$1" Picture 2>/dev/null \
        | /usr/bin/sed -n -e 's/^Picture: //p' -e '2s/^ //p' | /usr/bin/head -1
}

# Resolve the App's avatar URL without touching the account: the URL agent-bot
# cached for this App in the operator's own ~/.config/<slug> first (no
# network), then the public GitHub users API for "<slug>[bot]" over plain
# curl (App avatars are public, and the human's gh is refused to an agent
# session — an agent account has none to give), then gh as a last resort
# when the anonymous API is unavailable (rate limit). A URL the API supplied
# is cached next to the
# App's key material, in the format agent-bot writes, so later runs and the
# seeded agent home read it without the network. Only GitHub's avatar host
# is accepted — the bytes end up root-owned under /Library.
agent_avatar_url() {
    local slug="$1" cached url=""
    cached="$HOME/.config/$slug/bot-avatar-url"
    if [[ -r "$cached" ]]; then
        url="$(/usr/bin/head -1 "$cached" | tr -d '[:space:]')"
    fi
    if [[ -z "$url" ]]; then
        url="$(curl -fsSL --max-time 20 "https://api.github.com/users/${slug}%5Bbot%5D" 2>/dev/null \
            | python3 -c 'import json, sys; print(json.load(sys.stdin).get("avatar_url", ""))' 2>/dev/null || true)"
    fi
    if [[ -z "$url" ]] && command -v gh >/dev/null 2>&1; then
        url="$(gh api "users/${slug}%5Bbot%5D" --jq '.avatar_url' 2>/dev/null || true)"
    fi
    [[ "$url" =~ ^https://avatars\.githubusercontent\.com/ ]] || return 1
    if [[ ! -e "$cached" && -d "$HOME/.config/$slug" ]]; then
        printf '%s\n' "$url" >"$cached" 2>/dev/null || true
    fi
    printf '%s\n' "$url"
}

# agent_fetch_avatar <slug> <dest>: download the avatar into <dest>. The
# picture is presentation, not identity, so any failure is the caller's to
# report as a warning, never a reason to stop provisioning.
agent_fetch_avatar() {
    local url
    url="$(agent_avatar_url "$1")" || return 1
    curl -fsSL --max-time 20 -o "$2" "$url" 2>/dev/null || return 1
    [[ -s "$2" ]]
}

# agent_account_picture_converged <slug> <staged-avatar>: true when the
# directory record points at the managed picture path and, if a freshly
# fetched avatar is available, the installed file already has its bytes.
agent_account_picture_converged() {
    local slug="$1" staged="$2" expected current
    expected="$(agent_account_picture_path "$slug")"
    current="$(agent_account_picture "$slug")"
    [[ "$current" == "$expected" && -f "$expected" ]] || return 1
    [[ -z "$staged" ]] || cmp -s "$staged" "$expected"
}

# The App's key material and the per-account agent-bot wiring. The operator's
# own ~/.config/<slug> (app-id and private-key.pem, fetched from the secret
# provider by `agent-bot ensure-private-key`) is the source: add-agent seeds a
# copy into the agent home in the elevated phase and then runs agent-bot's
# machine wiring as the account. The agent home is opaque to the operator
# afterwards (0700), so convergence is judged from non-secret markers root
# writes under the system support directory: a fingerprint of the seeded key
# material, and the account's own `doctor --machine-only` verdict.
AGENT_ACCOUNT_MARKERS_DIR='/Library/Application Support/managed-machine/agents'

agent_key_source_dir() {
    printf '%s/.config/%s\n' "$HOME" "$1"
}

agent_key_source_ready() {
    local dir
    dir="$(agent_key_source_dir "$1")"
    [[ -r "$dir/app-id" && -r "$dir/private-key.pem" ]]
}

# agent_key_fingerprint <dir>: sha256 over app-id and private-key.pem, the
# same computation the elevated phase records. It identifies a key, it does
# not reveal one; the marker is world-readable by design.
agent_key_fingerprint() {
    /bin/cat "$1/app-id" "$1/private-key.pem" 2>/dev/null \
        | /usr/bin/shasum -a 256 | /usr/bin/cut -d ' ' -f 1
}

agent_key_seed_marker() {
    printf '%s/%s.keys.sha256\n' "$AGENT_ACCOUNT_MARKERS_DIR" "$1"
}

agent_doctor_marker() {
    printf '%s/%s.doctor.json\n' "$AGENT_ACCOUNT_MARKERS_DIR" "$1"
}

# True when the account holds the same key material the operator has now.
agent_key_seed_converged() {
    local slug="$1" marker recorded
    agent_key_source_ready "$slug" || return 1
    marker="$(agent_key_seed_marker "$slug")"
    [[ -r "$marker" ]] || return 1
    recorded="$(/usr/bin/head -1 "$marker" | tr -d '[:space:]')"
    [[ -n "$recorded" && "$recorded" == "$(agent_key_fingerprint "$(agent_key_source_dir "$slug")")" ]]
}

# agent_doctor_verdict <slug>: "ready" or "not-ready: <code>: <message>" from
# the verdict agent-bot recorded as the account; fails when none was recorded
# or the record is not a readiness report.
agent_doctor_verdict() {
    local marker
    marker="$(agent_doctor_marker "$1")"
    [[ -r "$marker" ]] || return 1
    AGENT_DOCTOR_FILE="$marker" python3 -c '
import json, os, sys
try:
    with open(os.environ["AGENT_DOCTOR_FILE"]) as handle:
        data = json.load(handle)
except (OSError, ValueError):
    sys.exit(1)
if not isinstance(data, dict) or "ready" not in data:
    sys.exit(1)
if data["ready"] is True:
    print("ready")
else:
    failure = data.get("first_actionable_failure") or {}
    if not isinstance(failure, dict):
        failure = {"message": str(failure)}
    code = failure.get("code") or "unknown"
    message = failure.get("message") or "see the doctor output"
    action = failure.get("action")
    print(f"not-ready: {code}: {message}" + (f" (fix: {action})" if action else ""))
'
}

agent_bot_account_wired() {
    [[ "$(agent_doctor_verdict "$1" 2>/dev/null)" == "ready" ]]
}

agent_account_report_marker() {
    printf '%s/%s.account.json\n' "$AGENT_ACCOUNT_MARKERS_DIR" "$1"
}

agent_account_report_verdict() {
    local marker
    marker="$(agent_account_report_marker "$1")"
    [[ -r "$marker" ]] || return 1
    python3 - "$marker" "$1" <<'PY'
import json, sys
try:
    with open(sys.argv[1]) as handle:
        data = json.load(handle)
    if not isinstance(data, dict) or type(data.get("schema_version")) is not int or data["schema_version"] != 1:
        raise ValueError()
    if data.get("command") not in ("account-setup", "account-doctor", "account") or data.get("account") != sys.argv[2]:
        raise ValueError()
    status = data.get("status")
    if status not in ("ready", "not_ready", "pending_user_action") or type(data.get("ready")) is not bool:
        raise ValueError()
    checks = data.get("checks")
    if not isinstance(checks, list) or not checks or any(not isinstance(c, dict) or c.get("status") not in ("ready", "failed", "pending_user_action", "warning", "skipped") for c in checks):
        raise ValueError()
    if data["ready"] != (status == "ready") or (status == "ready" and any(c["status"] != "ready" for c in checks)):
        raise ValueError()
    if status == "ready" and any(not any(isinstance(c.get("id"), str) and c["id"].startswith(prefix) for c in checks) for prefix in ("account.", "shell.", "local_bin.", "harness.", "identity.")):
        raise ValueError()
    print({"ready": "ready", "not_ready": "not-ready", "pending_user_action": "pending"}[status])
except (OSError, ValueError, TypeError):
    sys.exit(1)
PY
}

agent_account_report_line() {
    local verdict
    if verdict="$(agent_account_report_verdict "$1")"; then
        echo "account setup snapshot: $verdict — run 'managed-machine account doctor $1' for live readiness"
    else
        echo "warn: account setup and harness checks unverified — run 'managed-machine add-agent $1 --with-harness'"
    fi
}

# The harness install. ENG-0339 gives every agent account its own harness,
# installed into its own home: `add-agent --with-harness` runs
# `managed-machine setup <name>` as the account in the elevated phase and
# records the verdict as a world-readable marker — "ok <name>" or
# "failed <name> <exit status>" — next to the install's log and stderr. The
# install is per-home (official CLIs land in the account's ~/.local/bin and
# the config repo is materialized from the bundled seed), so nothing here
# reads the agent home; the marker is the verdict, exactly as for the key
# seed and the doctor report.
agent_harness_marker() {
    printf '%s/%s.harness\n' "$AGENT_ACCOUNT_MARKERS_DIR" "$1"
}

agent_harness_error_log() {
    printf '%s/%s.harness.err\n' "$AGENT_ACCOUNT_MARKERS_DIR" "$1"
}

# The setup name that installs a harness: the roster's harness key, which is
# the catalog name for every CLI harness. When the repo ships a
# `setup-<harness>-cli` script, that is the CLI install and the bare name is
# something else (setup-codex applies dotfiles, setup-kiro is the IDE cask),
# so the agent account installs through the -cli script. Names the catalog
# does not know fail inside the elevated phase and are recorded as such.
agent_harness_setup_name() {
    local repo_root="$1" harness="$2"
    if [[ -x "$repo_root/setup-$harness-cli" ]]; then
        printf '%s-cli\n' "$harness"
    else
        printf '%s\n' "$harness"
    fi
}

# agent_harness_state <slug>: the recorded verdict line, or failure when no
# install was recorded.
agent_harness_state() {
    local marker
    marker="$(agent_harness_marker "$1")"
    [[ -r "$marker" ]] || return 1
    /usr/bin/head -1 "$marker"
}

# True when the recorded install of exactly this setup name succeeded.
agent_harness_installed() {
    [[ "$(agent_harness_state "$1" 2>/dev/null)" == "ok $2" ]]
}

# One report line for the harness install of <slug>, expected under setup
# name <name>. Only "ok:" when the marker records a success for that name.
agent_harness_report_line() {
    local slug="$1" name="$2" state verdict recorded status detail
    if ! state="$(agent_harness_state "$slug")"; then
        echo "warn: harness $name installation unverified for $slug — run 'managed-machine add-agent $slug --with-harness' (ENG-0339: each agent account needs its harness)"
        return 0
    fi
    read -r verdict recorded status <<<"$state"
    case "$verdict" in
        ok)
            if [[ "$recorded" == "$name" ]]; then
                echo "ok: harness $name setup snapshot for $slug (historical success, not live readiness)"
            else
                echo "warn: the recorded harness install for $slug is $recorded, not $name — rerun 'managed-machine add-agent $slug --with-harness'"
            fi
            ;;
        failed)
            echo "warn: harness $name setup snapshot for $slug failed — saved outcomes: $(agent_account_report_marker "$slug"); run 'managed-machine account doctor $slug' for current checks"
            ;;
        pending)
            echo "warn: harness $name setup snapshot for $slug pending attended follow-up — saved outcomes: $(agent_account_report_marker "$slug"); run 'managed-machine account doctor $slug'"
            ;;
        *)
            echo "warn: the harness install record for $slug is unreadable — rerun 'managed-machine add-agent $slug --with-harness'"
            ;;
    esac
}

# Converge the shared coordination space. Runs unprivileged: /Users/Shared is
# world-writable on macOS, and the fallback lives in /tmp. Modes are
# converged on every run, not only at creation — a pre-existing restrictive
# mode silently breaks cross-account coordination. chmod succeeds only for
# the owner (or root); a failure is left for the compliance pass to report
# rather than aborting provisioning.
ensure_agent_shared_space() {
    local root locks
    root="$(agent_shared_space_root)"
    locks="$root/agent-locks"
    mkdir -p "$locks"
    chmod 1777 "$root" 2>/dev/null || true
    chmod 0777 "$locks" 2>/dev/null || true
}

# One compliance line: "ok:"/"warn:"/"fail:" prefixes are the contract the
# report and tests key on.
# The third argument is the setup name the harness installs under; when it
# is given, the report says whether that install is recorded.
agent_compliance_report() {
    local slug="$1" expected_full_name="$2" harness_setup="${3:-}"
    local status=0 home full_name

    if agent_account_exists "$slug"; then
        echo "ok: account $slug exists"
    else
        echo "fail: account $slug does not exist"
        return 1
    fi

    if agent_account_is_admin "$slug"; then
        echo "fail: account $slug is an administrator — agent accounts must be standard (ENG-0339)"
        status=1
    else
        echo "ok: account is standard (not admin)"
    fi

    if agent_account_in_group "$slug" "$AGENT_ACCOUNT_GROUP"; then
        echo "ok: account is a member of the $AGENT_ACCOUNT_GROUP group"
    else
        echo "fail: account $slug is not in the $AGENT_ACCOUNT_GROUP group — rerun add-agent to converge it"
        status=1
    fi

    local picture
    picture="$(agent_account_picture "$slug")"
    if [[ -n "$picture" && -f "$picture" ]]; then
        echo "ok: account picture is $picture"
    else
        echo "warn: account picture is ${picture:-unset} — rerun add-agent once the App avatar is reachable"
    fi

    full_name="$(agent_account_full_name "$slug")"
    if [[ -n "$expected_full_name" && "$full_name" != "$expected_full_name" ]]; then
        echo "warn: full name is '${full_name:-unset}' (expected '$expected_full_name')"
    else
        echo "ok: full name is '${full_name:-unset}'"
    fi

    home="$(agent_account_home "$slug")"
    if [[ -n "$home" && -d "$home" ]]; then
        echo "ok: home directory $home exists"
    else
        echo "warn: home directory ${home:-unset} is missing — created on first login, or run createhomedir"
    fi

    if command -v agent-bot >/dev/null 2>&1; then
        echo "ok: agent-bot is installed machine-wide"
        # The account's own state is judged from the markers the elevated
        # phase recorded, never by reading its home: the App key and the
        # wiring live behind a 0700 home the operator cannot see into.
        if agent_key_source_ready "$slug"; then
            if agent_key_seed_converged "$slug"; then
                echo "ok: App key material for $slug is seeded from ~/.config/$slug"
            else
                echo "warn: App key material for $slug is not seeded, or differs from ~/.config/$slug — rerun add-agent"
            fi
        else
            echo "warn: App key pending — run 'agent-bot ensure-private-key --app $slug' as yourself, then rerun add-agent to seed it"
        fi
        local verdict
        if verdict="$(agent_doctor_verdict "$slug")"; then
            if [[ "$verdict" == "ready" ]]; then
                echo "ok: agent-bot is wired for $slug (doctor --machine-only ready)"
            else
                echo "warn: agent-bot wiring for $slug is $verdict — rerun add-agent once addressed"
            fi
        else
            echo "warn: agent-bot bootstrap pending for $slug — rerun add-agent to wire it (needs the App key seeded)"
        fi
    else
        echo "warn: agent-bot is not installed — see qwts/agent-bot-identity"
    fi

    if [[ -n "$harness_setup" ]]; then
        agent_harness_report_line "$slug" "$harness_setup"
    fi
    agent_account_report_line "$slug"

    if [[ -d "${MANAGED_MACHINE_APPLICATIONS_DIR:-/Applications}/Little Snitch.app" ]]; then
        echo "warn: Little Snitch is active — its alerts render only in the running user's session; pre-seed allow rules for node and the harness before running $slug headless"
    fi

    local root locks root_mode locks_mode
    root="$(agent_shared_space_root)"
    locks="$root/agent-locks"
    if [[ -d "$locks" ]]; then
        # %Mp carries the sticky bit that %Lp alone drops.
        root_mode="$(stat -f '%Mp%Lp' "$root" 2>/dev/null || true)"
        locks_mode="$(stat -f '%Mp%Lp' "$locks" 2>/dev/null || true)"
        if [[ "$root_mode" == "1777" && "$locks_mode" == "0777" ]]; then
            echo "ok: shared agent space $root is present (modes 1777/777)"
        else
            # Wrong modes break the space's whole purpose: other agent UIDs
            # cannot write coordination state or clear stale locks
            # (ENG-0339 §7), and only the owner or root can correct them.
            echo "fail: shared agent space modes are ${root_mode:-?}/${locks_mode:-?} (need 1777/777) — chmod $root as its owner or root"
            status=1
        fi
    else
        echo "warn: shared agent space $locks is missing"
    fi

    return "$status"
}

# agent_roster_rows: every roster row as "slug<TAB>status", in roster order,
# from the same source and shapes agent_roster_query reads. Fails when no
# roster source exists.
agent_roster_rows() {
    local source
    source="$(agent_roster_source)" || return 1
    AGENT_ROSTER_FILE="$source" python3 -c '
import json, os
with open(os.environ["AGENT_ROSTER_FILE"]) as handle:
    data = json.load(handle)
rows = data.get("identities") or data.get("agents") \
    or (data.get("profile") or {}).get("identities") or []
for row in rows:
    slug = row.get("slug")
    if slug:
        print(slug + "\t" + str(row.get("status") or "unknown"))
'
}

# agent_account_summary <slug>: the compliance report on one line, for
# `managed-machine status`. The doctor verdict leads ("ready",
# "not-ready: <code>", or "unwired"), followed by every finding that would be
# a fail or warn in agent_compliance_report. Read-only, never elevates, and
# reads nothing from the agent home: the markers root recorded are the
# source, exactly as in the report.
agent_account_summary() {
    local slug="$1" head flags="" picture home verdict
    if ! agent_account_exists "$slug"; then
        echo "not provisioned"
        return 0
    fi
    add_flag() { flags="${flags:+$flags, }$1"; }
    agent_account_is_admin "$slug" && add_flag "admin (fail)"
    agent_account_in_group "$slug" "$AGENT_ACCOUNT_GROUP" || add_flag "not in $AGENT_ACCOUNT_GROUP group"
    picture="$(agent_account_picture "$slug")"
    [[ -n "$picture" && -f "$picture" ]] || add_flag "no picture"
    home="$(agent_account_home "$slug")"
    [[ -n "$home" && -d "$home" ]] || add_flag "no home"
    if command -v agent-bot >/dev/null 2>&1; then
        if agent_key_source_ready "$slug"; then
            agent_key_seed_converged "$slug" || add_flag "key not seeded"
        else
            add_flag "key pending"
        fi
        if verdict="$(agent_doctor_verdict "$slug")"; then
            case "$verdict" in
                ready) head="identity-only ready snapshot" ;;
                not-ready:*)
                    head="${verdict#not-ready: }"
                    head="not-ready: ${head%%:*}"
                    ;;
                *) head="$verdict" ;;
            esac
        else
            head="unwired"
        fi
    else
        head="agent-bot missing"
    fi
    # The harness install is opt-in (add-agent --with-harness), so only a
    # recorded failure is flagged here; the compliance report carries the
    # "not installed" warning.
    if verdict="$(agent_account_report_verdict "$slug")"; then
        head="account $verdict snapshot"
    else
        add_flag "account/harness checks unverified"
    fi
    case "$(agent_harness_state "$slug" 2>/dev/null)" in
        failed*) add_flag "harness install failed" ;;
        pending*) add_flag "harness setup pending" ;;
    esac
    echo "${head}${flags:+, $flags}"
}

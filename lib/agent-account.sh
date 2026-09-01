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
    /usr/bin/dscl . -read "/Users/$1" NFSHomeDirectory 2>/dev/null | /usr/bin/awk '{print $2}'
}

agent_account_full_name() {
    # dscl prints one-word values inline ("RealName: Goose") and multiword
    # values on a continuation line; accept both.
    /usr/bin/dscl . -read "/Users/$1" RealName 2>/dev/null \
        | /usr/bin/sed -n -e 's/^RealName: //p' -e '2s/^ //p' | /usr/bin/head -1
}

# ENG-0339 §2: agent accounts are standard, never admin. Membership is the
# check — a slug-named account holding admin is a hard compliance failure,
# not something to demote silently.
agent_account_is_admin() {
    /usr/bin/dscl . -read /Groups/admin GroupMembership 2>/dev/null \
        | tr ' ' '\n' | grep -Fxq "$1"
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
agent_compliance_report() {
    local slug="$1" expected_full_name="$2"
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
        # "Pending" is a claim about the account's state, so only make it
        # when this account can actually see that state: a home (or
        # ~/.config) the operator cannot traverse must read as unverifiable,
        # not as unprovisioned.
        if [[ -z "$home" || ! -x "$home" || ( -e "$home/.config" && ! -x "$home/.config" ) ]]; then
            echo "warn: cannot inspect ${home:-<no home>}/.config from this account — verify agent-bot bootstrap and the App key as $slug"
        else
            if [[ -f "$home/.config/agent-bot/config.json" ]]; then
                echo "ok: agent-bot is bootstrapped for $slug"
            else
                echo "warn: agent-bot bootstrap pending — run 'agent-bot bootstrap --profile <path>' as $slug"
            fi
            if [[ -d "$home/.config/$slug" ]]; then
                echo "ok: App key material is provisioned for $slug"
            else
                echo "warn: App key pending — run 'agent-bot ensure-private-key --app $slug' as $slug"
            fi
        fi
    else
        echo "warn: agent-bot is not installed — see qwts/agent-bot-identity"
    fi

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

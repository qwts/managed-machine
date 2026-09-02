#!/usr/bin/env bash
# GitHub SSH key registration for setup-gh: which scopes the upload needs,
# whether the machine key is already registered, and the upload itself.
#
# Scopes are demanded only when a key must actually be uploaded (#86). A
# machine whose keys are registered runs `--update` without a device flow
# even after its token lost admin:public_key / admin:ssh_signing_key (a gh
# upgrade or re-login drops them): the presence check reads the account's
# public key listings, which need no scope at all.

# The complete scope set the upload needs, declared up front so one
# authorization flow covers everything. Each entry is scope<TAB>reason.
GH_REQUIRED_SCOPE_TABLE="$(cat <<'EOF'
admin:public_key	upload the authentication SSH key (gh ssh-key add --type authentication)
admin:ssh_signing_key	upload the signing SSH key (gh ssh-key add --type signing)
EOF
)"
GH_SSH_KEY_SCOPES="admin:public_key,admin:ssh_signing_key"

report_required_scopes() {
    echo "GitHub token scopes needed to upload a key:"
    while IFS=$'\t' read -r scope reason; do
        printf '  %s — %s\n' "$scope" "$reason"
    done <<<"$GH_REQUIRED_SCOPE_TABLE"
}

# Query only the ACTIVE github.com account. `gh auth status` exits nonzero
# when any stale or inactive account sits in the keyring, which must not make
# the caller treat a logged-in user as logged out.
gh_active_account() {
    gh auth status -h github.com --json hosts --jq '
        (.hosts["github.com"] // [])[]
        | select(.active == true)
        | .login
    ' 2>/dev/null || true
}

gh_active_scopes() {
    gh auth status -h github.com --json hosts --jq '
        (.hosts["github.com"] // [])[]
        | select(.active == true)
        | (.scopes // "")
    ' 2>/dev/null || true
}

# Scopes from the required set that the active account is missing.
gh_missing_scopes() {
    local scopes missing scope reason
    scopes="$(gh_active_scopes)"
    missing=()
    while IFS=$'\t' read -r scope reason; do
        [[ "$scopes" == *"$scope"* ]] || missing+=("$scope")
    done <<<"$GH_REQUIRED_SCOPE_TABLE"
    [[ ${#missing[@]} -gt 0 ]] || return 0
    (IFS=','; printf '%s\n' "${missing[*]}")
}

# Request every missing upload scope in one flow. Called only when an upload
# is about to happen, never as a preflight.
ensure_gh_scopes_for_upload() {
    local missing
    missing="$(gh_missing_scopes)"
    [[ -n "$missing" ]] || return 0
    report_required_scopes
    echo "Token missing scopes: $missing — refreshing all of them in one flow (browser)..."
    gh auth refresh -h github.com -s "$missing"
}

# The key bodies (second field) of every key of one type registered on the
# active account, one per line, read from the account's public listing.
github_registered_key_bodies() {
    local type="$1" login api_path
    login="$(gh_active_account)"
    if [[ -z "$login" ]]; then
        echo "Error: no active GitHub account to list $type keys for" >&2
        return 1
    fi
    case "$type" in
        authentication) api_path="users/${login}/keys" ;;
        signing) api_path="users/${login}/ssh_signing_keys" ;;
        *)
            echo "Error: unknown SSH key type: $type" >&2
            return 1
            ;;
    esac
    # Capture before filtering: the listing's own failure must be the
    # function's status, whatever the caller's pipefail setting.
    local keys
    keys="$(gh api --paginate "$api_path" --jq '.[].key')" || return 1
    awk 'NF >= 2 { print $2 }' <<<"$keys"
}

ssh_public_key_fingerprint() {
    ssh-keygen -lf "$1" 2>/dev/null | awk '{print $2}'
}

# upload_ssh_key_as <authentication|signing> <title> <public key path>:
# register the key unless the account already has it. A listing failure is an
# error, not a reason to upload: a blind upload is how a registered key got a
# duplicate (#86).
upload_ssh_key_as() {
    local type="$1" title="$2" pub_path="$3"
    local key_body registered count fingerprint login

    key_body="$(awk 'NF >= 2 { print $2; exit }' "$pub_path")"
    if [[ -z "$key_body" ]]; then
        echo "Error: could not read public key from $pub_path" >&2
        return 1
    fi
    fingerprint="$(ssh_public_key_fingerprint "$pub_path")"
    login="$(gh_active_account)"

    if ! registered="$(github_registered_key_bodies "$type")"; then
        echo "Error: could not list ${login:-the account}'s registered $type keys on GitHub; not uploading blind — retry when the API is reachable" >&2
        return 1
    fi
    if grep -qxF -- "$key_body" <<<"$registered"; then
        echo "Public key already registered on GitHub ($type${fingerprint:+, $fingerprint})"
        return 0
    fi

    count="$(grep -c . <<<"$registered" || true)"
    echo "Public key ${fingerprint:-$pub_path} is not among ${login}'s $count registered $type key(s) on GitHub"
    ensure_gh_scopes_for_upload || {
        echo "Error: could not obtain the GitHub scopes needed to upload the $type key; not uploaded" >&2
        return 1
    }
    echo "Uploading SSH public key to GitHub ($type, title: $title)..."
    gh ssh-key add "$pub_path" --title "$title" --type "$type"
    echo "SSH key uploaded ($type)"
}

// Package authz implements the pure fleet authorization evaluator (S5 story
// L01; decisions D1 and D4, ADR-0010). Given access groups, a parsed key
// directory, an OS account name and a target GOOS, it deterministically
// returns the authorized (owner, key) pairs in owner-then-fingerprint order
// and the approved-unpublished fingerprints in fingerprint order without
// touching the filesystem or network.
package authz

import (
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/fleet/directory"
	"github.com/qwts/managed-machine/core/internal/fleet/policy"
)

// AuthorizedKey is one valid published key approved for the target account.
type AuthorizedKey struct {
	Owner       string        `json:"owner"`
	Fingerprint string        `json:"fingerprint"`
	Group       string        `json:"group"`
	Groups      []string      `json:"groups,omitempty"`
	Key         directory.Key `json:"key"`
}

// Line formats the authorized_keys entry "<type> <base64 blob> <owner>",
// dropping any published comment and writing no options (D4).
func (a AuthorizedKey) Line() string {
	return a.Key.AuthorizedLine(a.Owner)
}

// Unpublished is an approved fingerprint that no valid published key in the
// directory carries.
type Unpublished struct {
	Fingerprint string   `json:"fingerprint"`
	Group       string   `json:"group"`
	Groups      []string `json:"groups,omitempty"`
}

// Result is the output of the pure authorization evaluator for one account.
type Result struct {
	Account             string          `json:"account"`
	Authorized          []AuthorizedKey `json:"authorized"`
	ApprovedUnpublished []Unpublished   `json:"approved_unpublished"`
}

// UnpublishedFingerprints returns the approved-unpublished fingerprints in
// deterministic order.
func (r Result) UnpublishedFingerprints() []string {
	out := make([]string, len(r.ApprovedUnpublished))
	for i, u := range r.ApprovedUnpublished {
		out[i] = u.Fingerprint
	}
	return out
}

// Lines returns the rendered "<type> <base64 blob> <owner>" lines for the
// authorized set in owner-then-fingerprint order.
func (r Result) Lines() []string {
	out := make([]string, len(r.Authorized))
	for i, a := range r.Authorized {
		out[i] = a.Line()
	}
	return out
}

// MatchesAccount reports whether a group account entry matches the target OS
// account under goos (D1: case-insensitive on Windows, case-sensitive on
// Unix).
func MatchesAccount(groupAccount, account, goos string) bool {
	if groupAccount == "" || account == "" {
		return false
	}
	if goos == "windows" {
		if strings.EqualFold(groupAccount, account) {
			return true
		}
		if i := strings.LastIndexByte(account, '\\'); i >= 0 {
			return strings.EqualFold(groupAccount, account[i+1:])
		}
		return false
	}
	return groupAccount == account
}

// Evaluate computes the authorized and approved-unpublished sets for account
// on goos from groups and dir (D4).
//
// Only valid published keys (k.State == "valid") can match an approved
// fingerprint: invalid keys are excluded even when their recorded or
// computed fingerprint is approved by a group, and their fingerprints remain
// in ApprovedUnpublished unless another valid key carries them (which cannot
// happen because duplicate fingerprints across valid keys mark both invalid).
func Evaluate(groups []policy.AccessGroup, dir directory.Directory, account, goos string) Result {
	res := Result{
		Account:             account,
		Authorized:          []AuthorizedKey{},
		ApprovedUnpublished: []Unpublished{},
	}

	sortedGroups := append([]policy.AccessGroup(nil), groups...)
	sort.Slice(sortedGroups, func(i, j int) bool { return sortedGroups[i].ID < sortedGroups[j].ID })

	approvedBy := map[string][]string{}
	for _, g := range sortedGroups {
		if !groupMatchesAccount(g.Accounts, account, goos) {
			continue
		}
		for _, fp := range g.ApprovedFingerprints {
			if !containsString(approvedBy[fp], g.ID) {
				approvedBy[fp] = append(approvedBy[fp], g.ID)
			}
		}
	}
	if len(approvedBy) == 0 {
		return res
	}

	type ownerFP struct {
		owner string
		fp    string
	}
	seenAuth := map[ownerFP]bool{}
	publishedValid := map[string]bool{}

	for _, entry := range dir.Entries {
		for _, k := range entry.Keys {
			if !k.Valid() {
				continue
			}
			grps, ok := approvedBy[k.Fingerprint]
			if !ok {
				continue
			}
			publishedValid[k.Fingerprint] = true
			pair := ownerFP{owner: entry.ID, fp: k.Fingerprint}
			if seenAuth[pair] {
				continue
			}
			seenAuth[pair] = true
			res.Authorized = append(res.Authorized, AuthorizedKey{
				Owner:       entry.ID,
				Fingerprint: k.Fingerprint,
				Group:       grps[0],
				Groups:      append([]string(nil), grps...),
				Key:         k,
			})
		}
	}

	sort.Slice(res.Authorized, func(i, j int) bool {
		if res.Authorized[i].Owner != res.Authorized[j].Owner {
			return res.Authorized[i].Owner < res.Authorized[j].Owner
		}
		return res.Authorized[i].Fingerprint < res.Authorized[j].Fingerprint
	})

	for fp, grps := range approvedBy {
		if publishedValid[fp] {
			continue
		}
		res.ApprovedUnpublished = append(res.ApprovedUnpublished, Unpublished{
			Fingerprint: fp,
			Group:       grps[0],
			Groups:      append([]string(nil), grps...),
		})
	}
	sort.Slice(res.ApprovedUnpublished, func(i, j int) bool {
		return res.ApprovedUnpublished[i].Fingerprint < res.ApprovedUnpublished[j].Fingerprint
	})

	return res
}

func groupMatchesAccount(accounts []string, account, goos string) bool {
	for _, a := range accounts {
		if MatchesAccount(a, account, goos) {
			return true
		}
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

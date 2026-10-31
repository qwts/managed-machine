package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"runtime"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
)

// Item is one pinned catalog row the daemon loaded itself.
type Item struct {
	Name              string   `json:"name"`
	Options           []string `json:"options"`
	Targets           []string `json:"targets"`
	Provider          string   `json:"provider,omitempty"`
	Formula           string   `json:"formula,omitempty"`
	PrivilegeRequired bool     `json:"privilege_required,omitempty"`
}

// Catalog is the installable set. A name that is not in it does not exist.
type Catalog struct {
	Items   []Item   `json:"items"`
	Flavors []Flavor `json:"flavors,omitempty"`
	Version int      `json:"version,omitempty"`
}

// Flavor is a declared set of catalog names. Update reapplies one flavor.
type Flavor struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

func (c Catalog) get(name string) (Item, bool) {
	for _, item := range c.Items {
		if item.Name == name {
			return item, true
		}
	}
	return Item{}, false
}

func (item Item) allows(option, target string) string {
	if item.PrivilegeRequired {
		return diag.DaemonPrivilegeRequired
	}
	if option != "" && !contains(item.Options, option) {
		return diag.DaemonUndeclared
	}
	if !contains(item.Targets, target) {
		return diag.DaemonUndeclared
	}
	return ""
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Planner saves and applies catalog selections. Exec is the only process
// start, and it is called only after the saved plan is checked again.
// current, when set, is the catalog accepted on the paired channel.
type Planner struct {
	Catalog  Catalog
	current  func() Catalog
	StateDir string
	Account  string
	Exec     func(item Item, option, target string) error
	Now      func() time.Time
}

func (p Planner) catalog() Catalog {
	if p.current != nil {
		return p.current()
	}
	return p.Catalog
}

func (p Planner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Plan records a saved plan for a declared selection. It never starts a process.
func (p Planner) Plan(name, option, target string) (string, string) {
	item, ok := p.catalog().get(name)
	if !ok {
		return "", diag.DaemonNotInCatalog
	}
	if id := item.allows(option, target); id != "" {
		return "", id
	}
	store, err := plan.Open(p.StateDir, p.now)
	if err != nil {
		return "", diag.DaemonPlanFailed
	}
	defer store.Close()
	id, err := plan.NewID(p.now())
	if err != nil {
		return "", diag.DaemonPlanFailed
	}
	doc := &plan.Document{
		Plan:           id,
		SchemaVersion:  plan.SchemaVersion,
		Command:        "software",
		Software:       name,
		Target:         target,
		Account:        p.Account,
		SourceRevision: "pinned",
		CreatedAt:      p.now().UTC().Format(time.RFC3339),
		State:          plan.Saved,
		Bindings: plan.Bindings{
			Machine:      "local",
			Platform:     runtime.GOOS + "/" + runtime.GOARCH,
			InputsDigest: digest(name, option, target),
		},
		Operations: []plan.Operation{{
			ID:        "install",
			Operation: "install",
			Target:    target,
			Action:    "install",
			Provider:  "catalog",
			Params:    map[string]string{"option": option},
		}},
	}
	if err := doc.Validate(); err != nil || store.Save(doc) != nil {
		return "", diag.DaemonPlanFailed
	}
	return id, ""
}

// Apply runs a saved plan id. Requests that carry anything besides the id
// are refused before Exec.
func (p Planner) Apply(planID, name, option, target, soul, harness, session string) string {
	if name != "" || option != "" || target != "" {
		return diag.DaemonUndeclared
	}
	if planID == "" {
		return diag.DaemonNotInCatalog
	}
	store, err := plan.Open(p.StateDir, p.now)
	if err != nil {
		return diag.DaemonPlanFailed
	}
	defer store.Close()
	doc, err := store.Load(planID)
	if err != nil {
		return diag.DaemonNotInCatalog
	}
	if doc.State != plan.Saved {
		return diag.DaemonReplay
	}
	if doc.Bindings.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		return diag.DaemonPlanFailed
	}
	item, ok := p.catalog().get(doc.Software)
	if !ok {
		return diag.DaemonNotInCatalog
	}
	savedOption := ""
	if len(doc.Operations) > 0 {
		savedOption = doc.Operations[0].Params["option"]
	}
	if id := item.allows(savedOption, doc.Target); id != "" {
		return id
	}
	if err := store.Move(doc, plan.Executing); err != nil {
		return diag.DaemonPlanFailed
	}
	execErr := error(nil)
	if p.Exec != nil {
		execErr = p.Exec(item, savedOption, doc.Target)
	}
	outcome := plan.OutcomeApplied
	next := plan.Applied
	if execErr != nil {
		outcome = plan.OutcomeFailed
		next = plan.Failed
	}
	if err := store.Append(plan.Record{
		Plan:      doc.Plan,
		Operation: "install",
		Action:    plan.ActionReceipt,
		Outcome:   outcome,
		At:        p.now().UTC().Format(time.RFC3339),
		Account:   p.Account,
		Soul:      soul,
		Harness:   harness,
		Session:   session,
	}); err != nil {
		return diag.DaemonPlanFailed
	}
	if err := store.Move(doc, next); err != nil {
		return diag.DaemonPlanFailed
	}
	if errors.Is(execErr, provider.ErrMissing) {
		return diag.DaemonProviderMissing
	}
	if execErr != nil {
		return diag.DaemonPlanFailed
	}
	return ""
}

// Update reapplies one declared flavor. An empty name selects default.
// A row that needs elevation refuses the whole flavor before any process.
func (p Planner) Update(name, soul, harness, session string) (string, string) {
	if name == "" {
		name = "default"
	}
	var items []string
	for _, flavor := range p.catalog().Flavors {
		if flavor.Name == name {
			items = append([]string(nil), flavor.Items...)
			break
		}
	}
	if len(items) == 0 {
		return "", diag.DaemonNotInCatalog
	}
	for _, itemName := range items {
		item, ok := p.catalog().get(itemName)
		if !ok {
			return "", diag.DaemonNotInCatalog
		}
		if item.PrivilegeRequired {
			return "", diag.DaemonPrivilegeRequired
		}
	}
	var ids []string
	for _, itemName := range items {
		item, _ := p.catalog().get(itemName)
		option, target := "", ""
		if len(item.Options) > 0 {
			option = item.Options[0]
		}
		if len(item.Targets) > 0 {
			target = item.Targets[0]
		}
		id, diagID := p.Plan(itemName, option, target)
		if diagID != "" {
			return "", diagID
		}
		if diagID = p.Apply(id, "", "", "", soul, harness, session); diagID != "" {
			return "", diagID
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, ","), ""
}

func digest(name, option, target string) string {
	sum := sha256.Sum256([]byte(name + "\n" + option + "\n" + target))
	return hex.EncodeToString(sum[:])
}

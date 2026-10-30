package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/qwts/managed-machine/internal/plan"
)

// Item is one pinned catalog row the daemon loaded itself.
type Item struct {
	Name              string   `json:"name"`
	Options           []string `json:"options"`
	Targets           []string `json:"targets"`
	Provider          string   `json:"provider,omitempty"`
	PrivilegeRequired bool     `json:"privilege_required,omitempty"`
}

// Catalog is the installable set. A name that is not in it does not exist.
type Catalog struct {
	Items []Item `json:"items"`
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
		return "MM_DAEMON_PRIVILEGE_REQUIRED"
	}
	if option != "" && !contains(item.Options, option) {
		return "MM_DAEMON_UNDECLARED"
	}
	if !contains(item.Targets, target) {
		return "MM_DAEMON_UNDECLARED"
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
type Planner struct {
	Catalog  Catalog
	StateDir string
	Account  string
	Exec     func(item Item, option, target string) error
	Now      func() time.Time
}

func (p Planner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Plan records a saved plan for a declared selection. It never starts a process.
func (p Planner) Plan(name, option, target string) (string, string) {
	item, ok := p.Catalog.get(name)
	if !ok {
		return "", "MM_DAEMON_NOT_IN_CATALOG"
	}
	if id := item.allows(option, target); id != "" {
		return "", id
	}
	store, err := plan.Open(p.StateDir, p.now)
	if err != nil {
		return "", "MM_DAEMON_PLAN_FAILED"
	}
	defer store.Close()
	id, err := plan.NewID(p.now())
	if err != nil {
		return "", "MM_DAEMON_PLAN_FAILED"
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
			Platform:     "darwin",
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
		return "", "MM_DAEMON_PLAN_FAILED"
	}
	return id, ""
}

// Apply runs a saved plan id. Requests that carry anything besides the id
// are refused before Exec.
func (p Planner) Apply(planID, name, option, target string) string {
	if name != "" || option != "" || target != "" {
		return "MM_DAEMON_UNDECLARED"
	}
	if planID == "" {
		return "MM_DAEMON_NOT_IN_CATALOG"
	}
	store, err := plan.Open(p.StateDir, p.now)
	if err != nil {
		return "MM_DAEMON_PLAN_FAILED"
	}
	defer store.Close()
	doc, err := store.Load(planID)
	if err != nil {
		return "MM_DAEMON_NOT_IN_CATALOG"
	}
	item, ok := p.Catalog.get(doc.Software)
	if !ok {
		return "MM_DAEMON_NOT_IN_CATALOG"
	}
	savedOption := ""
	if len(doc.Operations) > 0 {
		savedOption = doc.Operations[0].Params["option"]
	}
	if id := item.allows(savedOption, doc.Target); id != "" {
		return id
	}
	if err := store.Move(doc, plan.Executing); err != nil {
		return "MM_DAEMON_PLAN_FAILED"
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
	}); err != nil {
		return "MM_DAEMON_PLAN_FAILED"
	}
	if err := store.Move(doc, next); err != nil {
		return "MM_DAEMON_PLAN_FAILED"
	}
	if execErr != nil {
		return "MM_DAEMON_PLAN_FAILED"
	}
	return ""
}

func digest(name, option, target string) string {
	sum := sha256.Sum256([]byte(name + "\n" + option + "\n" + target))
	return hex.EncodeToString(sum[:])
}

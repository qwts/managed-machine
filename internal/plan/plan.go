// Package plan holds the saved artifacts of the shared execution model
// (ADR-0012, schema version 1): the plan document, the results journal and
// its receipt, and the store that saves, supersedes, selects and recovers
// them in the platform state directory.
package plan

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SchemaVersion is the integer schema version of plans and journals.
const SchemaVersion = 1

// State is a plan's lifecycle state. Only a saved plan is eligible to run;
// every other state is final except executing, which ends in applied or
// failed (or in failed with an interrupted receipt when recovered).
type State string

const (
	Saved      State = "saved"
	Executing  State = "executing"
	Applied    State = "applied"
	Failed     State = "failed"
	Rejected   State = "rejected"
	Superseded State = "superseded"
)

var transitions = map[State][]State{
	Saved:     {Executing, Rejected, Superseded},
	Executing: {Applied, Failed},
}

// CanMove reports whether a plan may move from one state to another.
func CanMove(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Document is a saved plan. Field order is the serialized key order.
// Software is set for software plans, Config for configuration plans, and
// Fleet for fleet authorized-keys plans.
type Document struct {
	Plan           string         `json:"plan"`
	SchemaVersion  int            `json:"schema_version"`
	Command        string         `json:"command"`
	Software       string         `json:"software,omitempty"`
	Config         string         `json:"config,omitempty"`
	Fleet          string         `json:"fleet,omitempty"`
	Target         string         `json:"target"`
	Account        string         `json:"account"`
	SourceRevision string         `json:"source_revision"`
	CreatedAt      string         `json:"created_at"`
	State          State          `json:"state"`
	Bindings       Bindings       `json:"bindings"`
	Preconditions  []Precondition `json:"preconditions"`
	Operations     []Operation    `json:"operations"`
}

// Bindings tie a plan to the machine and the resolved inputs. Apply
// re-derives them and rejects the plan when any differs.
type Bindings struct {
	Machine      string     `json:"machine"`
	Platform     string     `json:"platform"`
	InputsDigest string     `json:"inputs_digest"`
	Assets       []Asset    `json:"assets"`
	Providers    []Provider `json:"providers"`
}

// Asset is a pinned input file: a definition or a downloaded artifact.
type Asset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Provider records the provider version a plan was resolved with.
type Provider struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Precondition is one requirement checked before the first operation.
// Kind is software, privilege, platform or target.
type Precondition struct {
	Kind        string `json:"kind"`
	Subject     string `json:"subject"`
	Requirement string `json:"requirement"`
	Observed    string `json:"observed"`
}

// Operation is one step, run in plan order exactly as saved. The first four
// fields are the `config --plan` operation record; Params carries what the
// provider needs to perform it, fixed at plan time.
type Operation struct {
	ID        string            `json:"id"`
	Operation string            `json:"operation"`
	Target    string            `json:"target"`
	Action    string            `json:"action"`
	Provider  string            `json:"provider"`
	Params    map[string]string `json:"params"`
}

var idPattern = regexp.MustCompile(`^plan-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}$`)

// ValidID reports whether s is a plan id: plan-<UTC second, ISO 8601 basic>-<4 hex>; the id is a file name, so it has no colon (Windows).
func ValidID(s string) bool { return idPattern.MatchString(s) }

// NewID returns a plan id for the creation time t.
func NewID(t time.Time) (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "plan-" + t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

// Validate checks a document before it is saved or after it is read.
func (d *Document) Validate() error {
	var missing []string
	for name, v := range map[string]string{"plan": d.Plan, "command": d.Command, "target": d.Target, "account": d.Account,
		"source_revision": d.SourceRevision, "created_at": d.CreatedAt, "bindings.machine": d.Bindings.Machine,
		"bindings.platform": d.Bindings.Platform, "bindings.inputs_digest": d.Bindings.InputsDigest} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	resources := 0
	for _, r := range []string{d.Software, d.Config, d.Fleet} {
		if r != "" {
			resources++
		}
	}
	switch {
	case len(missing) > 0:
		return fmt.Errorf("plan is missing %v", missing)
	case d.SchemaVersion != SchemaVersion:
		return fmt.Errorf("plan schema_version %d is not %d", d.SchemaVersion, SchemaVersion)
	case !ValidID(d.Plan):
		return fmt.Errorf("plan id %q is malformed", d.Plan)
	case resources != 1:
		return errors.New("plan names none or more than one of software, config and fleet")
	}
	if _, err := time.Parse(time.RFC3339, d.CreatedAt); err != nil {
		return fmt.Errorf("created_at: %v", err)
	}
	switch d.State {
	case Saved, Executing, Applied, Failed, Rejected, Superseded:
	default:
		return fmt.Errorf("unknown plan state %q", d.State)
	}
	seen := map[string]bool{}
	for _, o := range d.Operations {
		if o.ID == "" || seen[o.ID] {
			return fmt.Errorf("operation id %q is empty or repeated", o.ID)
		}
		seen[o.ID] = true
	}
	return nil
}

// Name is the resource the plan acts on.
func (d *Document) Name() string {
	switch {
	case d.Software != "":
		return d.Software
	case d.Config != "":
		return d.Config
	default:
		return d.Fleet
	}
}

// Record is one results-journal line. Field order is the serialized key
// order. State is set only on the receipt, the final record of a plan.
type Record struct {
	Plan      string         `json:"plan"`
	Operation string         `json:"operation"`
	Action    string         `json:"action"`
	Outcome   string         `json:"outcome"`
	At        string         `json:"at"`
	Account   string         `json:"account,omitempty"`
	Soul      string         `json:"soul,omitempty"`
	Harness   string         `json:"harness,omitempty"`
	Evidence  map[string]any `json:"evidence"`
	State     State          `json:"state,omitempty"`
}

// Journal outcomes. A receipt's outcome is applied, failed, rejected or
// interrupted; an operation's is started, ok or failed.
const (
	OutcomeStarted     = "started"
	OutcomeOK          = "ok"
	OutcomeFailed      = "failed"
	OutcomeApplied     = "applied"
	OutcomeRejected    = "rejected"
	OutcomeInterrupted = "interrupted"
	ActionReceipt      = "receipt"
	OperationPrecheck  = "preconditions"
	OperationBindings  = "bindings"
)

// Package diag writes the accepted stderr diagnostics, one JSON object per
// line, and maps every accepted identifier to its exit code.
package diag

import (
	"encoding/json"
	"io"
)

// Exit codes accepted in the v1 specification.
const (
	ExitOK         = 0 // success
	ExitFailure    = 1 // runtime failure or refused apply
	ExitUsage      = 2 // usage or validation error
	ExitFindings   = 3 // completed evaluation with findings
	ExitIncomplete = 4 // incomplete evaluation
)

// Identifiers from the accepted registry (v1 specification, "Diagnostic
// identifiers"). New identifiers are added here only with the documentation.
const (
	Usage                     = "MM_USAGE"
	SoftwareUnknownName       = "MM_SOFTWARE_UNKNOWN_NAME"
	SoftwareUnknownProvider   = "MM_SOFTWARE_UNKNOWN_PROVIDER"
	SoftwareUnknownRecipe     = "MM_SOFTWARE_UNKNOWN_RECIPE"
	SoftwareUnsupportedCombo  = "MM_SOFTWARE_UNSUPPORTED_COMBINATION"
	SoftwareAmbiguousProvider = "MM_SOFTWARE_AMBIGUOUS_PROVIDER"
	SoftwareAmbiguousRecipe   = "MM_SOFTWARE_AMBIGUOUS_RECIPE"
	SoftwareInstallFailed     = "MM_SOFTWARE_INSTALL_FAILED"
	ConfigUnknownName         = "MM_CONFIG_UNKNOWN_NAME"
	ConfigConflict            = "MM_CONFIG_CONFLICT"
	ConfigMissingPrerequisite = "MM_CONFIG_MISSING_PREREQUISITE"
	ConfigNoPlan              = "MM_CONFIG_NO_PLAN"
	ConfigPlanMismatch        = "MM_CONFIG_PLAN_MISMATCH"
	ConfigPlanRejected        = "MM_CONFIG_PLAN_REJECTED"
	ConfigDriftFound          = "MM_CONFIG_DRIFT_FOUND"
	ConfigCheckIncomplete     = "MM_CONFIG_CHECK_INCOMPLETE"
	StatusPartial             = "MM_STATUS_PARTIAL"
	SkillUnknownName          = "MM_SKILL_UNKNOWN_NAME"
	SkillUnknownKind          = "MM_SKILL_UNKNOWN_KIND"
	SkillUnknownBundle        = "MM_SKILL_UNKNOWN_BUNDLE"
	SkillUnknownVersion       = "MM_SKILL_UNKNOWN_VERSION"
	SkillUnknownHandle        = "MM_SKILL_UNKNOWN_HANDLE"
	SkillCursorMismatch       = "MM_SKILL_CURSOR_MISMATCH"
	SkillContentOversized     = "MM_SKILL_CONTENT_OVERSIZED"
	SkillUnsupportedKind      = "MM_SKILL_UNSUPPORTED_KIND"
	SkillBundleUnreadable     = "MM_SKILL_BUNDLE_UNREADABLE"
	// Added with the F03 boundary (ADR-0017): the host is outside the five
	// supported targets, or the native-architecture probe failed.
	PlatformUnsupported = "MM_PLATFORM_UNSUPPORTED"
	PlatformProbeFailed = "MM_PLATFORM_PROBE_FAILED"
)

var registry = map[string]int{
	Usage:                     ExitUsage,
	SoftwareUnknownName:       ExitUsage,
	SoftwareUnknownProvider:   ExitUsage,
	SoftwareUnknownRecipe:     ExitUsage,
	SoftwareUnsupportedCombo:  ExitUsage,
	SoftwareAmbiguousProvider: ExitUsage,
	SoftwareAmbiguousRecipe:   ExitUsage,
	SoftwareInstallFailed:     ExitFailure,
	ConfigUnknownName:         ExitUsage,
	ConfigConflict:            ExitFailure,
	ConfigMissingPrerequisite: ExitFailure,
	ConfigNoPlan:              ExitUsage,
	ConfigPlanMismatch:        ExitUsage,
	ConfigPlanRejected:        ExitFailure,
	ConfigDriftFound:          ExitFindings,
	ConfigCheckIncomplete:     ExitIncomplete,
	StatusPartial:             ExitIncomplete,
	SkillUnknownName:          ExitUsage,
	SkillUnknownKind:          ExitUsage,
	SkillUnknownBundle:        ExitUsage,
	SkillUnknownVersion:       ExitUsage,
	SkillUnknownHandle:        ExitUsage,
	SkillCursorMismatch:       ExitUsage,
	SkillContentOversized:     ExitFailure,
	SkillUnsupportedKind:      ExitUsage,
	SkillBundleUnreadable:     ExitFailure,
	PlatformUnsupported:       ExitFailure,
	PlatformProbeFailed:       ExitFailure,
}

// Diagnostic is one stderr line: the identifier and a concise message.
type Diagnostic struct {
	ID      string `json:"diagnostic"`
	Message string `json:"message"`
}

// New builds a diagnostic for an identifier in the registry. It panics on an
// unregistered identifier, so a typo fails the first test that uses it rather
// than reaching a user.
func New(id, message string) Diagnostic {
	if _, ok := registry[id]; !ok {
		panic("diag: unregistered identifier " + id)
	}
	return Diagnostic{ID: id, Message: message}
}

// ExitCode returns the accepted exit code for the identifier.
func (d Diagnostic) ExitCode() int {
	return registry[d.ID]
}

// Known reports whether the identifier is in the accepted registry.
func Known(id string) bool {
	_, ok := registry[id]
	return ok
}

// Write emits the diagnostic as one JSON object followed by a newline, with
// no HTML escaping, so the bytes are the shortest faithful encoding.
func (d Diagnostic) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(d)
}

// Human renders the diagnostic for --human mode: one stderr line of the form
// "MM_<ID>: <message>", so the identifier stays greppable without JSON in a
// human stream (delegate-selected 2026-10-21).
func (d Diagnostic) Human() string {
	return d.ID + ": " + d.Message + "\n"
}

// Registry returns a copy of the identifier table for documentation checks.
func Registry() map[string]int {
	out := make(map[string]int, len(registry))
	for k, v := range registry {
		out[k] = v
	}
	return out
}

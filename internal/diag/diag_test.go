package diag

import (
	"bytes"
	"testing"
)

func TestEveryIdentifierHasAnAcceptedExitCode(t *testing.T) {
	for id, code := range Registry() {
		if code < ExitFailure || code > ExitIncomplete {
			t.Errorf("%s maps to exit %d, outside 1..4", id, code)
		}
	}
	// Spot checks against the v1 specification table.
	want := map[string]int{
		Usage:                       ExitUsage,
		SoftwareInstallFailed:       ExitFailure,
		ConfigDriftFound:            ExitFindings,
		ConfigCheckIncomplete:       ExitIncomplete,
		StatusPartial:               ExitIncomplete,
		SkillContentOversized:       ExitFailure,
		PlatformUnsupported:         ExitFailure,
		PlatformProbeFailed:         ExitFailure,
		LegacyRouteUnsupported:      ExitUsage,
		SoftwareSourceInvalid:       ExitUsage,
		SoftwareSourceUnavailable:   ExitUsage,
		SoftwareSourceFetchFailed:   ExitFailure,
		SoftwareCaskRefused:         ExitFailure,
		SoftwareArtifactMismatch:    ExitFailure,
		SoftwareSignatureRejected:   ExitFailure,
		SoftwareHostUnlisted:        ExitFailure,
		SoftwareDestinationOccupied: ExitFailure,
		ConfigSourceInvalid:         ExitUsage,
		ConfigDesiredInvalid:        ExitUsage,
		ConfigUnsupportedCombo:      ExitUsage,
		ConfigUnknownName:           ExitUsage,
		ConfigConflict:              ExitFailure,
		ConfigMissingPrerequisite:   ExitFailure,
		ConfigNoPlan:                ExitUsage,
		ConfigPlanMismatch:          ExitUsage,
		ConfigPlanRejected:          ExitFailure,
		ConfigSecretUnresolved:      ExitFailure,
		ConfigPrivilegeRequired:     ExitFailure,
		ConfigApplyFailed:           ExitFailure,
		ConfigBusy:                  ExitFailure,
		FleetPolicyUnavailable:      ExitUsage,
		FleetPolicyInvalid:          ExitUsage,
		FleetDirectoryUnavailable:   ExitUsage,
		FleetDirectoryInvalid:       ExitUsage,
		FleetDirectoryFetchFailed:   ExitFailure,
		FleetKeyInvalid:             ExitUsage,
		FleetKeyUnknown:             ExitUsage,
		FleetNotCutOver:             ExitFailure,
		FleetConflict:               ExitFailure,
		FleetNoPlan:                 ExitUsage,
		FleetPlanMismatch:           ExitUsage,
		FleetPlanRejected:           ExitFailure,
		FleetApplyFailed:            ExitFailure,
		FleetBusy:                   ExitFailure,
		FleetPrivilegeRequired:      ExitFailure,
		FleetPublishFailed:          ExitFailure,
		FleetDriftFound:             ExitFindings,
		FleetCheckIncomplete:        ExitIncomplete,
	}
	for id, code := range want {
		if got := New(id, "").ExitCode(); got != code {
			t.Errorf("%s exit = %d, want %d", id, got, code)
		}
	}
}

func TestWriteIsOneCompactLineWithFixedKeyOrder(t *testing.T) {
	var b bytes.Buffer
	if err := New(Usage, `--json and --human are <mutually> exclusive & "quoted"`).Write(&b); err != nil {
		t.Fatal(err)
	}
	want := `{"diagnostic":"MM_USAGE","message":"--json and --human are <mutually> exclusive & \"quoted\""}` + "\n"
	if b.String() != want {
		t.Errorf("Write =\n%s\nwant\n%s", b.String(), want)
	}
}

func TestHumanForm(t *testing.T) {
	if got := New(StatusPartial, "two observers failed").Human(); got != "MM_STATUS_PARTIAL: two observers failed\n" {
		t.Errorf("Human = %q", got)
	}
}

func TestUnregisteredIdentifierPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New accepted an identifier outside the registry")
		}
	}()
	New("MM_TYPO", "")
}

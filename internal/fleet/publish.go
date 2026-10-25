package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/output"
)

// EnrollmentSchemaVersion is the format version of <state>/fleet/enrollment.json (D5).
const EnrollmentSchemaVersion = 1

// Publication and enrollment outcomes (D5).
const (
	OutcomeWritten = "written"
	OutcomeStaged  = "staged"
)

const maxPublicKeyFileBytes = 64 * 1024

// Enrollment is the local-file adapter record written to
// <state>/fleet/enrollment.json by --publish and --unpublish (D3, D5).
// Field order is the fixed JSON key order of the serialized record.
type Enrollment struct {
	SchemaVersion int    `json:"schema_version"`
	MachineID     string `json:"machine_id"`
	Owner         string `json:"owner"`
	Fingerprint   string `json:"fingerprint"`
	KeyType       string `json:"key_type,omitempty"`
	Directory     string `json:"directory"`
	Outcome       string `json:"outcome"`
	At            string `json:"at"`
}

func keyInvalidErr(format string, args ...any) *diag.Diagnostic {
	d := diag.New(diag.FleetKeyInvalid, fmt.Sprintf(format, args...))
	return &d
}

func keyUnknownErr(format string, args ...any) *diag.Diagnostic {
	d := diag.New(diag.FleetKeyUnknown, fmt.Sprintf(format, args...))
	return &d
}

func publishFailedErr(format string, args ...any) *diag.Diagnostic {
	d := diag.New(diag.FleetPublishFailed, fmt.Sprintf(format, args...))
	return &d
}

func publishRun(d Deps, goos string, f flags) cli.Result {
	if f.ownerFlag == "" {
		return cli.Result{Diagnostic: command.UsageErr("--publish requires --owner <id>")}
	}
	if !policy.ValidOwnerID(f.ownerFlag) {
		return cli.Result{Diagnostic: command.UsageErr("invalid owner id %q", f.ownerFlag)}
	}
	if f.keyPath == "" {
		return cli.Result{Diagnostic: command.UsageErr("--publish requires --key <public-key-path>")}
	}
	if f.label != "" {
		if err := directory.ValidateLabel(f.label); err != nil {
			return cli.Result{Diagnostic: command.UsageErr("invalid --label: %v", err)}
		}
	}

	pk, keyLine, dg := readPublicKeyFile(f.keyPath)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}

	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	dir, dg := d.loadDirectory(goos, pol.KeyDirectory)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}

	now := d.NowRFC3339()
	updatedEntry := applyPublish(dir, f.ownerFlag, pk, keyLine, f.label, now)
	return commitPublication(d, goos, pol.KeyDirectory, dir, updatedEntry, pk.Fingerprint, pk.KeyType, pk.MachineID, now)
}

func unpublishRun(d Deps, goos string, f flags) cli.Result {
	if f.ownerFlag != "" && !policy.ValidOwnerID(f.ownerFlag) {
		return cli.Result{Diagnostic: command.UsageErr("invalid owner id %q", f.ownerFlag)}
	}

	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	dir, dg := d.loadDirectory(goos, pol.KeyDirectory)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}

	updatedEntry, removedKey, found := applyUnpublish(dir, f.ownerFlag, f.unpublish)
	if !found {
		return cli.Result{Diagnostic: keyUnknownErr("fingerprint %q is not published in the key directory", f.unpublish)}
	}

	machineID := removedKey.MachineID
	if machineID == "" {
		if mid, err := directory.MachineIDFromFingerprint(removedKey.Fingerprint); err == nil {
			machineID = mid
		}
	}

	now := d.NowRFC3339()
	return commitPublication(d, goos, pol.KeyDirectory, dir, updatedEntry, removedKey.Fingerprint, removedKey.KeyType, machineID, now)
}

// readPublicKeyFile reads and validates the OpenSSH public-key file at
// keyPath. On any failure it returns MM_FLEET_KEY_INVALID without echoing
// any bytes from the file (D3, D5).
func readPublicKeyFile(keyPath string) (directory.PublicKey, string, *diag.Diagnostic) {
	fi, err := os.Stat(keyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return directory.PublicKey{}, "", keyInvalidErr("public key file %q does not exist", keyPath)
		}
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q is not readable", keyPath)
	}
	if !fi.Mode().IsRegular() {
		return directory.PublicKey{}, "", keyInvalidErr("public key path %q is not a regular file", keyPath)
	}
	if fi.Size() == 0 {
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q is empty", keyPath)
	}
	if fi.Size() > maxPublicKeyFileBytes {
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q exceeds %d bytes", keyPath, maxPublicKeyFileBytes)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q is not readable", keyPath)
	}
	trimmed := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q is empty", keyPath)
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 || !directory.KnownKeyType(fields[0]) {
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q does not begin with a supported OpenSSH public key type", keyPath)
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q must contain a single OpenSSH public key line", keyPath)
	}
	trimmed = strings.TrimRight(trimmed, " \t")
	pk, err := directory.ParsePublicKey(trimmed)
	if err != nil {
		return directory.PublicKey{}, "", keyInvalidErr("public key file %q is not a valid OpenSSH public key", keyPath)
	}
	keyLine := pk.KeyType + " " + pk.Base64Blob
	if pk.Comment != "" {
		keyLine += " " + pk.Comment
	}
	return pk, keyLine, nil
}

func applyPublish(dir directory.Directory, owner string, pk directory.PublicKey, keyLine, label, now string) directory.Entry {
	for _, entry := range dir.Entries {
		if entry.ID != owner {
			continue
		}
		keys := make([]directory.Key, len(entry.Keys))
		copy(keys, entry.Keys)
		highestRev := 0
		for i, k := range keys {
			if k.Revision > highestRev {
				highestRev = k.Revision
			}
			if k.Fingerprint == pk.Fingerprint && (k.RecordedFingerprint == "" || k.RecordedFingerprint == pk.Fingerprint) {
				keys[i].PublishedAt = now
				if label != "" {
					keys[i].Label = label
				}
				return directory.Entry{
					SchemaVersion: directory.SchemaVersion,
					Kind:          directory.KindKeyDirectory,
					ID:            owner,
					Keys:          keys,
				}
			}
		}
		keys = append(keys, directory.Key{
			Fingerprint: pk.Fingerprint,
			Key:         keyLine,
			KeyType:     pk.KeyType,
			Blob:        pk.Base64Blob,
			Label:       label,
			PublishedAt: now,
			Revision:    highestRev + 1,
			MachineID:   pk.MachineID,
			State:       directory.StateValid,
		})
		return directory.Entry{
			SchemaVersion: directory.SchemaVersion,
			Kind:          directory.KindKeyDirectory,
			ID:            owner,
			Keys:          keys,
		}
	}

	return directory.Entry{
		SchemaVersion: directory.SchemaVersion,
		Kind:          directory.KindKeyDirectory,
		ID:            owner,
		Keys: []directory.Key{{
			Fingerprint: pk.Fingerprint,
			Key:         keyLine,
			KeyType:     pk.KeyType,
			Blob:        pk.Base64Blob,
			Label:       label,
			PublishedAt: now,
			Revision:    1,
			MachineID:   pk.MachineID,
			State:       directory.StateValid,
		}},
	}
}

func applyUnpublish(dir directory.Directory, ownerFilter, fp string) (directory.Entry, directory.Key, bool) {
	for _, entry := range dir.Entries {
		if ownerFilter != "" && entry.ID != ownerFilter {
			continue
		}
		matchIdx := -1
		var matchedKey directory.Key
		for i, k := range entry.Keys {
			if k.Fingerprint == fp || k.RecordedFingerprint == fp {
				matchIdx = i
				matchedKey = k
				break
			}
		}
		if matchIdx < 0 {
			continue
		}
		remaining := make([]directory.Key, 0, len(entry.Keys)-1)
		for i, k := range entry.Keys {
			if i == matchIdx {
				continue
			}
			remaining = append(remaining, k)
		}
		return directory.Entry{
			SchemaVersion: directory.SchemaVersion,
			Kind:          directory.KindKeyDirectory,
			ID:            entry.ID,
			Keys:          remaining,
		}, matchedKey, true
	}
	return directory.Entry{}, directory.Key{}, false
}

func commitPublication(
	d Deps,
	goos string,
	kd policy.KeyDirectorySource,
	dir directory.Directory,
	updatedEntry directory.Entry,
	fingerprint, keyType, machineID, now string,
) cli.Result {
	entryBytes, err := directory.FormatEntry(updatedEntry)
	if err != nil {
		return cli.Result{Diagnostic: publishFailedErr("format %s.yaml: %v", updatedEntry.ID, err)}
	}
	stateDir, err := d.ResolveStateDir(goos)
	if err != nil {
		return cli.Result{Diagnostic: publishFailedErr("locate state directory: %v", err)}
	}
	enrollDest := filepath.Join(stateDir, "fleet", "enrollment.json")

	if kd.Path != "" {
		updatedEntries := replaceOrInsertEntry(dir.Entries, updatedEntry)
		updatedDir, err := directory.FromEntries(updatedEntries)
		if err != nil {
			return cli.Result{Diagnostic: publishFailedErr("validate updated key directory: %v", err)}
		}
		enrollBytes, err := marshalEnrollment(Enrollment{
			SchemaVersion: EnrollmentSchemaVersion,
			MachineID:     machineID,
			Owner:         updatedEntry.ID,
			Fingerprint:   fingerprint,
			KeyType:       keyType,
			Directory:     updatedDir.Identity,
			Outcome:       OutcomeWritten,
			At:            now,
		})
		if err != nil {
			return cli.Result{Diagnostic: publishFailedErr("encode enrollment record: %v", err)}
		}

		entryDest := filepath.Join(kd.Path, "keys", updatedEntry.ID+".yaml")
		entryTmp, err := stageTempFile(filepath.Dir(entryDest), entryBytes)
		if err != nil {
			return cli.Result{Diagnostic: publishFailedErr("write %s: %v", entryDest, err)}
		}
		enrollTmp, err := stageTempFile(filepath.Dir(enrollDest), enrollBytes)
		if err != nil {
			_ = os.Remove(entryTmp)
			return cli.Result{Diagnostic: publishFailedErr("write %s: %v", enrollDest, err)}
		}
		if err := os.Rename(entryTmp, entryDest); err != nil {
			_ = os.Remove(entryTmp)
			_ = os.Remove(enrollTmp)
			return cli.Result{Diagnostic: publishFailedErr("commit %s: %v", entryDest, err)}
		}
		loadedDir, err := directory.LoadPath(kd.Path)
		if err != nil {
			_ = os.Remove(enrollTmp)
			return cli.Result{Diagnostic: publishFailedErr("verify %s: %v", entryDest, err)}
		}
		if err := os.Rename(enrollTmp, enrollDest); err != nil {
			_ = os.Remove(enrollTmp)
			return cli.Result{Diagnostic: publishFailedErr("commit %s: %v", enrollDest, err)}
		}

		rec := output.New().
			Set("owner", updatedEntry.ID).
			Set("fingerprint", fingerprint).
			Set("directory", loadedDir.Identity).
			SetOptional("staged", "").
			SetOptional("next_step", "")
		return cli.Result{Records: []*output.Record{rec}}
	}

	stagedPath := filepath.Join(stateDir, "fleet", "staged", updatedEntry.ID+".yaml")
	nextStep := fmt.Sprintf("gh gist edit %s -f %s.yaml %s", kd.Gist, updatedEntry.ID, stagedPath)
	enrollBytes, err := marshalEnrollment(Enrollment{
		SchemaVersion: EnrollmentSchemaVersion,
		MachineID:     machineID,
		Owner:         updatedEntry.ID,
		Fingerprint:   fingerprint,
		KeyType:       keyType,
		Directory:     dir.Identity,
		Outcome:       OutcomeStaged,
		At:            now,
	})
	if err != nil {
		return cli.Result{Diagnostic: publishFailedErr("encode enrollment record: %v", err)}
	}

	stagedTmp, err := stageTempFile(filepath.Dir(stagedPath), entryBytes)
	if err != nil {
		return cli.Result{Diagnostic: publishFailedErr("write %s: %v", stagedPath, err)}
	}
	enrollTmp, err := stageTempFile(filepath.Dir(enrollDest), enrollBytes)
	if err != nil {
		_ = os.Remove(stagedTmp)
		return cli.Result{Diagnostic: publishFailedErr("write %s: %v", enrollDest, err)}
	}
	if err := os.Rename(stagedTmp, stagedPath); err != nil {
		_ = os.Remove(stagedTmp)
		_ = os.Remove(enrollTmp)
		return cli.Result{Diagnostic: publishFailedErr("commit %s: %v", stagedPath, err)}
	}
	if err := os.Rename(enrollTmp, enrollDest); err != nil {
		_ = os.Remove(enrollTmp)
		return cli.Result{Diagnostic: publishFailedErr("commit %s: %v", enrollDest, err)}
	}

	rec := output.New().
		Set("owner", updatedEntry.ID).
		Set("fingerprint", fingerprint).
		Set("directory", dir.Identity).
		SetOptional("staged", stagedPath).
		SetOptional("next_step", nextStep)
	return cli.Result{Records: []*output.Record{rec}}
}

func replaceOrInsertEntry(entries []directory.Entry, updated directory.Entry) []directory.Entry {
	out := make([]directory.Entry, 0, len(entries)+1)
	replaced := false
	for _, e := range entries {
		if e.ID == updated.ID {
			out = append(out, updated)
			replaced = true
		} else {
			out = append(out, e)
		}
	}
	if !replaced {
		out = append(out, updated)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func marshalEnrollment(e Enrollment) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func stageTempFile(dir string, content []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".mm-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	_ = os.Chmod(tmpName, 0o644)
	return tmpName, nil
}

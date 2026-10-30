package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/status"
)

// statusBody is the daemon status reply. It uses the read-only status
// command with empty catalogs so a missing config checkout is an empty
// report, not a fetch. Command output is not included.
func statusBody() (string, []byte, error) {
	res := status.Command(status.Deps{
		Deps:            command.Deps{Getenv: os.Getenv, GOOS: hostOS()},
		SoftwareCatalog: func() ([]source.Entry, *registry.Registry, error) { return nil, nil, nil },
		ConfigCatalog:   func() ([]config.Entry, error) { return nil, nil },
	}).Run(cli.Context{Getenv: os.Getenv}, nil)
	if res.Diagnostic != nil {
		return "", nil, errStatus(res.Diagnostic.Message)
	}
	body, err := recordsJSON(res.Records)
	if err != nil {
		return "", nil, err
	}
	return "reported", body, nil
}

type statusError string

func (e statusError) Error() string { return string(e) }

func errStatus(msg string) error { return statusError(msg) }

func recordsJSON(recs []*output.Record) ([]byte, error) {
	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		row := map[string]any{}
		for _, f := range rec.Fields() {
			row[f.Key] = f.Value
		}
		out = append(out, row)
	}
	return json.Marshal(out)
}

// receiptsBody lists saved plans without journal evidence, which can carry
// command output. A missing directory is an empty list.
func receiptsBody(dir string) ([]byte, error) {
	names, err := filepath.Glob(filepath.Join(dir, "plans", "plan-*.json"))
	if err != nil {
		return nil, err
	}
	type row struct {
		Plan    string `json:"plan"`
		Command string `json:"command,omitempty"`
		State   string `json:"state,omitempty"`
		Outcome string `json:"outcome,omitempty"`
		At      string `json:"at,omitempty"`
	}
	out := []row{}
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Plan    string `json:"plan"`
			Command string `json:"command"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			continue
		}
		item := row{Plan: doc.Plan, Command: doc.Command, State: doc.State}
		if rec, err := lastReceipt(filepath.Join(dir, "journal", doc.Plan+".jsonl")); err == nil {
			item.Outcome = rec.Outcome
			item.At = rec.At
		}
		out = append(out, item)
	}
	return json.Marshal(out)
}

type receiptHead struct {
	Action  string `json:"action"`
	Outcome string `json:"outcome"`
	At      string `json:"at"`
}

func lastReceipt(path string) (receiptHead, error) {
	f, err := os.Open(path)
	if err != nil {
		return receiptHead{}, err
	}
	defer f.Close()
	var last receiptHead
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row receiptHead
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.Action == "receipt" {
			last = row
		}
	}
	if last.Action == "" {
		return receiptHead{}, os.ErrNotExist
	}
	return last, sc.Err()
}

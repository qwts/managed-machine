// Command mm-catalog-report classifies a legacy managed-machine-config
// apps.json against the F04 software model (migration story F05; S7 story N05).
// It is a migration tool, not part of the managed-machine command contract: it
// reads one file, writes a report, optionally writes candidate definitions with
// --emit <dir> or checks converted definitions with --converted <dir>, and
// never fetches, refreshes or writes the catalog.
//
//	mm-catalog-report [--json | --human] [--emit <dir>] [--converted <dir>] <apps.json>
//
// Default output is one JSON Lines record per row; a one-line summary goes
// to stderr. Exit 0 on a completed report, 2 on usage errors, 1 when the
// catalog cannot be read.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/registry"
)

const usage = "usage: mm-catalog-report [--json | --human] [--emit <dir>] [--converted <dir>] <apps.json>"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(stdout, usage)
			return 0
		}
	}
	var (
		jsonFlag, humanFlag         bool
		emitDir, convertedDir, file string
		emitSet, convertedSet       bool
	)
	if dg := command.Parse(args, command.Spec{
		Positional:     &file,
		PositionalName: "catalog file",
		Bools:          map[string]*bool{"--json": &jsonFlag, "--human": &humanFlag},
		Values: map[string]command.Flag{
			"--emit":      {Value: &emitDir, Set: &emitSet},
			"--converted": {Value: &convertedDir, Set: &convertedSet},
		},
	}); dg != nil {
		fmt.Fprintf(stderr, "mm-catalog-report: %s\n", dg.Message)
		return 2
	}
	mode, err := output.SelectMode(jsonFlag, humanFlag)
	if err != nil || file == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "mm-catalog-report: %v\n", err)
		return 1
	}
	reg := registry.Builtin()
	rep, err := legacy.Classify(data, reg)
	if err != nil {
		fmt.Fprintf(stderr, "mm-catalog-report: %v\n", err)
		return 1
	}
	if emitSet {
		if err := legacy.Emit(rep, emitDir); err != nil {
			fmt.Fprintf(stderr, "mm-catalog-report: %v\n", err)
			return 1
		}
	}
	if convertedSet {
		rep, err = legacy.CheckConverted(rep, convertedDir, reg)
		if err != nil {
			fmt.Fprintf(stderr, "mm-catalog-report: %v\n", err)
			return 1
		}
	}
	recs := Records(rep)
	if mode == output.Human {
		recs = HumanRecords(rep)
	}
	if err := output.Write(stdout, mode, recs); err != nil {
		fmt.Fprintf(stderr, "mm-catalog-report: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, rep.Summary())
	return 0
}

// Records renders one record per row; collisions are attached to the rows
// they involve so every record has the same keys.
func Records(rep legacy.Report) []*output.Record {
	byRow := map[string][]string{}
	for _, c := range rep.Collisions {
		for _, r := range c.Rows {
			byRow[r] = append(byRow[r], c.Alias)
		}
	}
	var out []*output.Record
	for _, r := range rep.Rows {
		rec := output.New().Set("name", r.Name).Set("kind", r.Kind).Set("verdict", r.Verdict)
		if r.Software != nil {
			rec.Set("candidate", r.Software)
		}
		cols := byRow[r.Name]
		if cols == nil {
			cols = []string{}
		}
		rec.Set("aliases", r.Aliases).Set("collisions", cols).Set("auto", r.Auto).Set("desired", r.Desired).Set("items", r.Items)
		out = append(out, rec)
	}
	return out
}

// HumanRecords is the readable table: one line per row with the provider
// of its candidate and the fields that block it. The JSON modes carry the
// full candidate and items.
func HumanRecords(rep legacy.Report) []*output.Record {
	var out []*output.Record
	for _, r := range rep.Rows {
		provider := "-"
		if r.Software != nil {
			provider = r.Software.Recipes[0].Provider
		}
		var blocked []string
		for _, it := range r.Items {
			if it.Action == legacy.Blocked {
				blocked = append(blocked, it.Field)
			}
		}
		if blocked == nil {
			blocked = []string{"-"}
		}
		out = append(out, output.New().Set("name", r.Name).Set("kind", r.Kind).Set("verdict", r.Verdict).
			Set("provider", provider).Set("auto", r.Auto).Set("blocked", strings.Join(blocked, ",")))
	}
	return out
}

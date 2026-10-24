package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflowDoc struct {
	Name        string                 `yaml:"name"`
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Name        string            `yaml:"name"`
	Uses        string            `yaml:"uses"`
	Needs       any               `yaml:"needs"`
	If          string            `yaml:"if"`
	RunsOn      string            `yaml:"runs-on"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

func TestWorkflowsContractAndPermissions(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	wfDir := filepath.Join(repoRoot, ".github", "workflows")
	ents, err := os.ReadDir(wfDir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}

	// No workflow file may reference pull_request_target or secrets.*
	for _, e := range ents {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		p := filepath.Join(wfDir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		text := string(raw)
		if strings.Contains(text, "pull_request_target") {
			t.Errorf("%s contains forbidden pull_request_target trigger", e.Name())
		}
		if strings.Contains(text, "secrets.") {
			t.Errorf("%s contains forbidden secrets.* reference (only github.token is permitted)", e.Name())
		}
	}

	// Parse and check go-core.yml.
	goCoreRaw, err := os.ReadFile(filepath.Join(wfDir, "go-core.yml"))
	if err != nil {
		t.Fatalf("read go-core.yml: %v", err)
	}
	var goCore workflowDoc
	if err := yaml.Unmarshal(goCoreRaw, &goCore); err != nil {
		t.Fatalf("unmarshal go-core.yml: %v", err)
	}
	if _, ok := goCore.On["workflow_call"]; !ok {
		t.Errorf("go-core.yml missing on.workflow_call trigger")
	}
	if len(goCore.Permissions) != 1 || goCore.Permissions["contents"] != "read" {
		t.Errorf("go-core.yml top-level permissions = %v, want {contents: read}", goCore.Permissions)
	}
	for jobID, job := range goCore.Jobs {
		if len(job.Permissions) != 0 {
			t.Errorf("go-core.yml job %s has unexpected permissions %v", jobID, job.Permissions)
		}
		for i, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "actions/checkout@") {
				if v, ok := step.With["persist-credentials"]; !ok || v != false {
					t.Errorf("go-core.yml job %s step %d checkout persist-credentials = %v, want false", jobID, i+1, v)
				}
			}
		}
	}
	rgJob, ok := goCore.Jobs["release-gate"]
	if !ok {
		t.Fatalf("go-core.yml missing release-gate job")
	}
	if rgJob.Name != "release-gate (${{ matrix.target }})" {
		t.Errorf("release-gate.name = %q, want %q", rgJob.Name, "release-gate (${{ matrix.target }})")
	}
	if rgJob.Needs != "build" {
		t.Errorf("release-gate.needs = %v, want build", rgJob.Needs)
	}
	var sawDistDL, sawRGUpload bool
	for _, step := range rgJob.Steps {
		if strings.HasPrefix(step.Uses, "actions/download-artifact@") && step.With["name"] == "dist" {
			sawDistDL = true
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && step.With["name"] == "release-gate-${{ matrix.os }}-${{ matrix.arch }}" {
			sawRGUpload = true
		}
	}
	if !sawDistDL || !sawRGUpload {
		t.Errorf("release-gate artifacts: dist download=%v, release-gate upload=%v", sawDistDL, sawRGUpload)
	}
	s6Job, ok := goCore.Jobs["s6-gate"]
	if !ok {
		t.Fatalf("go-core.yml missing s6-gate job")
	}
	if s6Job.Name != "S6 release gate (R09)" {
		t.Errorf("s6-gate.name = %q, want %q", s6Job.Name, "S6 release gate (R09)")
	}
	var sawSMUpload, sawAggregateRun bool
	for _, step := range s6Job.Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && step.With["name"] == "support-matrix" {
			sawSMUpload = true
		}
		if strings.Contains(step.Run, "release-aggregate") && strings.Contains(step.Run, "-checksums dist/SHA256SUMS") && strings.Contains(step.Run, "-out support-matrix.md") {
			sawAggregateRun = true
		}
	}
	if !sawSMUpload || !sawAggregateRun {
		t.Errorf("s6-gate steps: support-matrix upload=%v, release-aggregate run=%v", sawSMUpload, sawAggregateRun)
	}

	// Parse and check release.yml.
	relRaw, err := os.ReadFile(filepath.Join(wfDir, "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	var rel workflowDoc
	if err := yaml.Unmarshal(relRaw, &rel); err != nil {
		t.Fatalf("unmarshal release.yml: %v", err)
	}

	// Top-level permissions: contents: read.
	if len(rel.Permissions) != 1 || rel.Permissions["contents"] != "read" {
		t.Errorf("release.yml top-level permissions = %v, want {contents: read}", rel.Permissions)
	}

	// Triggers: push.tags: ['v*'] and workflow_dispatch with inputs tag (required) and publish (boolean, default false).
	pushCfg, ok := rel.On["push"].(map[string]any)
	if !ok {
		t.Fatalf("release.yml missing on.push map: %T", rel.On["push"])
	}
	tagsList, ok := pushCfg["tags"].([]any)
	if !ok || len(tagsList) != 1 || tagsList[0] != "v*" {
		t.Errorf("release.yml on.push.tags = %v, want [v*]", pushCfg["tags"])
	}

	wdCfg, ok := rel.On["workflow_dispatch"].(map[string]any)
	if !ok {
		t.Fatalf("release.yml missing on.workflow_dispatch map")
	}
	inputsCfg, ok := wdCfg["inputs"].(map[string]any)
	if !ok {
		t.Fatalf("release.yml missing on.workflow_dispatch.inputs map")
	}
	tagInput, ok := inputsCfg["tag"].(map[string]any)
	if !ok || tagInput["required"] != true {
		t.Errorf("release.yml workflow_dispatch.inputs.tag = %v, want required: true", inputsCfg["tag"])
	}
	pubInput, ok := inputsCfg["publish"].(map[string]any)
	if !ok || pubInput["type"] != "boolean" || pubInput["default"] != false {
		t.Errorf("release.yml workflow_dispatch.inputs.publish = %v, want type: boolean, default: false", inputsCfg["publish"])
	}

	// Job gates calls ./.github/workflows/go-core.yml with no elevated permissions.
	gatesJob, ok := rel.Jobs["gates"]
	if !ok {
		t.Fatalf("release.yml missing gates job")
	}
	if gatesJob.Uses != "./.github/workflows/go-core.yml" {
		t.Errorf("gates.uses = %q, want ./.github/workflows/go-core.yml", gatesJob.Uses)
	}
	if gatesJob.If != "" {
		t.Errorf("gates.if = %q, want empty so gates always run on workflow_dispatch", gatesJob.If)
	}
	if len(gatesJob.Permissions) != 0 {
		t.Errorf("gates.permissions = %v, want empty (inherits contents: read)", gatesJob.Permissions)
	}

	// Job publish is the only job with permissions: contents: write.
	pubJob, ok := rel.Jobs["publish"]
	if !ok {
		t.Fatalf("release.yml missing publish job")
	}
	if pubJob.Needs != "gates" {
		t.Errorf("publish.needs = %v, want gates", pubJob.Needs)
	}
	if pubJob.RunsOn != "ubuntu-24.04" {
		t.Errorf("publish.runs-on = %q, want ubuntu-24.04", pubJob.RunsOn)
	}
	if len(pubJob.Permissions) != 1 || pubJob.Permissions["contents"] != "write" {
		t.Errorf("publish.permissions = %v, want {contents: write}", pubJob.Permissions)
	}
	if len(rel.Jobs) != 2 {
		t.Errorf("release.yml has %d jobs, want 2 (gates, publish)", len(rel.Jobs))
	}

	var (
		sawCheckout      bool
		sawDistDownload  bool
		sawGateDownload  bool
		sawMatrix        bool
		sawTagVerifyStep bool
		sawGHReleaseStep bool
	)
	const wantGHRelease = `gh release create "$TAG" dist/* support-matrix.md release-gate-*.json --verify-tag --title "$TAG" --generate-notes`
	for i, step := range pubJob.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			sawCheckout = true
			if v, ok := step.With["persist-credentials"]; !ok || v != false {
				t.Errorf("publish step %d checkout persist-credentials = %v, want false", i+1, v)
			}
		}
		if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
			switch {
			case step.With["name"] == "dist":
				sawDistDownload = true
			case step.With["pattern"] == "release-gate-*":
				sawGateDownload = true
			case step.With["name"] == "support-matrix":
				sawMatrix = true
			}
		}
		if strings.Contains(step.Run, "git tag --points-at HEAD") &&
			strings.Contains(step.Run, "v$(cat VERSION)") &&
			strings.Contains(step.Run, "mm-release verify") &&
			strings.Contains(step.Run, "-require-pin") {
			sawTagVerifyStep = true
		}
		if strings.Contains(step.Run, wantGHRelease) {
			sawGHReleaseStep = true
			if step.Env["GH_TOKEN"] != "${{ github.token }}" {
				t.Errorf("publish gh release step GH_TOKEN = %q, want ${{ github.token }}", step.Env["GH_TOKEN"])
			}
		}
	}
	if !sawCheckout {
		t.Errorf("publish job missing actions/checkout step")
	}
	if !sawDistDownload || !sawGateDownload || !sawMatrix {
		t.Errorf("publish job artifact downloads: dist=%v release-gate=%v support-matrix=%v",
			sawDistDownload, sawGateDownload, sawMatrix)
	}
	if !sawTagVerifyStep {
		t.Errorf("publish job missing tag + mm-release verify -require-pin step")
	}
	if !sawGHReleaseStep {
		t.Errorf("publish job missing exact %q invocation", wantGHRelease)
	}
}

type workflowContext struct {
	EventName     string
	Ref           string
	InputsPublish bool
}

func TestReleasePublishJobIfExpression(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	relRaw, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var rel workflowDoc
	if err := yaml.Unmarshal(relRaw, &rel); err != nil {
		t.Fatal(err)
	}
	ifExpr := strings.TrimSpace(rel.Jobs["publish"].If)
	const wantExpr = "(github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')) || (github.event_name == 'workflow_dispatch' && inputs.publish)"
	if ifExpr != wantExpr {
		t.Fatalf("publish.if = %q, want %q", ifExpr, wantExpr)
	}

	cases := []struct {
		name string
		ctx  workflowContext
		want bool
	}{
		{
			name: "workflow_dispatch with publish=false on a branch runs gates and skips publish",
			ctx:  workflowContext{EventName: "workflow_dispatch", Ref: "refs/heads/main", InputsPublish: false},
			want: false,
		},
		{
			name: "workflow_dispatch with publish=false on release branch skips publish",
			ctx:  workflowContext{EventName: "workflow_dispatch", Ref: "refs/heads/release/v0.9.0", InputsPublish: false},
			want: false,
		},
		{
			name: "workflow_dispatch with publish=false on a tag ref still skips publish (dry run)",
			ctx:  workflowContext{EventName: "workflow_dispatch", Ref: "refs/tags/v0.9.0", InputsPublish: false},
			want: false,
		},
		{
			name: "workflow_dispatch with publish=true runs publish",
			ctx:  workflowContext{EventName: "workflow_dispatch", Ref: "refs/tags/v0.9.0", InputsPublish: true},
			want: true,
		},
		{
			name: "push of v* tag runs publish",
			ctx:  workflowContext{EventName: "push", Ref: "refs/tags/v0.9.0", InputsPublish: false},
			want: true,
		},
		{
			name: "push of branch skips publish",
			ctx:  workflowContext{EventName: "push", Ref: "refs/heads/main", InputsPublish: false},
			want: false,
		},
		{
			name: "push of non-v tag skips publish",
			ctx:  workflowContext{EventName: "push", Ref: "refs/tags/test-1", InputsPublish: false},
			want: false,
		},
	}
	for _, tc := range cases {
		got := evalPublishIf(ifExpr, tc.ctx)
		if got != tc.want {
			t.Errorf("%s: eval(%q, %+v) = %v, want %v", tc.name, ifExpr, tc.ctx, got, tc.want)
		}
	}
}

// evalPublishIf evaluates the boolean workflow `if:` expression grammar used by
// jobs.publish (`||`, `&&`, parenthesized groups, `==` string comparisons,
// `startsWith(a, b)`, and boolean context `inputs.publish`).
func evalPublishIf(expr string, ctx workflowContext) bool {
	expr = strings.TrimSpace(expr)
	if parts := splitTopLevel(expr, "||"); len(parts) > 1 {
		for _, p := range parts {
			if evalPublishIf(p, ctx) {
				return true
			}
		}
		return false
	}
	if parts := splitTopLevel(expr, "&&"); len(parts) > 1 {
		for _, p := range parts {
			if !evalPublishIf(p, ctx) {
				return false
			}
		}
		return true
	}
	if strings.HasPrefix(expr, "(") && strings.HasSuffix(expr, ")") && matchingOuterParens(expr) {
		return evalPublishIf(expr[1:len(expr)-1], ctx)
	}
	if strings.HasPrefix(expr, "startsWith(") && strings.HasSuffix(expr, ")") {
		inner := expr[len("startsWith/") : len(expr)-1]
		args := splitTopLevel(inner, ",")
		if len(args) == 2 {
			return strings.HasPrefix(evalStringAtom(args[0], ctx), evalStringAtom(args[1], ctx))
		}
	}
	if lhs, rhs, ok := strings.Cut(expr, "=="); ok {
		return evalStringAtom(lhs, ctx) == evalStringAtom(rhs, ctx)
	}
	if expr == "inputs.publish" {
		return ctx.InputsPublish
	}
	return false
}

func evalStringAtom(atom string, ctx workflowContext) string {
	atom = strings.TrimSpace(atom)
	if len(atom) >= 2 && atom[0] == '\'' && atom[len(atom)-1] == '\'' {
		return atom[1 : len(atom)-1]
	}
	switch atom {
	case "github.event_name":
		return ctx.EventName
	case "github.ref":
		return ctx.Ref
	}
	return atom
}

func matchingOuterParens(s string) bool {
	depth := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
				if depth == 0 && i < len(s)-1 {
					return false
				}
			}
		}
	}
	return depth == 0
}

func splitTopLevel(s, sep string) []string {
	var parts []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
			}
		default:
			if !inQuote && depth == 0 && strings.HasPrefix(s[i:], sep) {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				i += len(sep) - 1
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}

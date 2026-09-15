package workflow

// compile_taint_call_edges_test.go — M6.4 conformance (CRI-170): the
// compiler's taint pass treats adapter call edges as NON-data-flow.
//
// The differential the ticket demands: a workflow whose step input derives
// from a secret and whose tools list references another adapter must produce
// exactly the same taint diagnostics as the same workflow without the tools
// list — a call edge is permission metadata (callee surface grant), never a
// compile-time data-flow channel. If compile_taint ever started walking
// tools[] references as data-flow, the with-tools variant would gain a
// diagnostic and this suite would fail.
//
// The with-tools arm is non-vacuous: the clean variants assert the compiled
// graph actually recorded the call edge (AdapterCallEdges / StepNode.Tools),
// so a regression that stops parsing the tools list is caught too.

import (
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// taintCallEdgesSchemas mirrors the call-edges fixture: the caller carries
// the adapter_tools capability; the callee declares its tool surface and —
// deliberately — a Sensitive output, so the differential also proves a tools
// list referencing an adapter with sensitive outputs contributes no taint.
func taintCallEdgesSchemas() map[string]AdapterInfo {
	return map[string]AdapterInfo{
		"caller.default": {
			InputSchema:  map[string]ConfigField{},
			OutputSchema: map[string]ConfigField{},
			Capabilities: []string{"adapter_tools"},
		},
		"callee.default": {
			InputSchema: map[string]ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]ConfigField{
				"report": {CtyType: cty.String, Sensitive: true},
			},
		},
	}
}

// taintCallEdgesSrc renders the fixture with the caller step's tools list
// toggled: withTools adds a cross-adapter tools list, withoutTools leaves the
// step otherwise byte-identical.
func taintCallEdgesSrc(withTools bool) string {
	tools := ""
	if withTools {
		tools = `  tools = [adapter.callee.default.tools.helper_task]` + "\n"
	}
	return `
workflow {
  name          = "taint_call_edges"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "sk_live_taint_call_edges"
}

adapter "callee" "default" {
  tool "helper_task" {}
}

adapter "caller" "default" {}

step "call" {
  target = adapter.caller.default
  input {
    task = var.api_key
  }
` + tools + `  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`
}

func compileTaintCallEdges(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("taint_call_edges.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	return Compile(spec, taintCallEdgesSchemas())
}

// diagnosticSummaries returns the sorted, joined summaries of the error-level
// diagnostics so two variants can be compared structurally (subject ranges
// necessarily differ between the two fixtures).
func diagnosticSummaries(diags hcl.Diagnostics) []string {
	summaries := []string{}
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			summaries = append(summaries, d.Summary)
		}
	}
	sort.Strings(summaries)
	return summaries
}

// TestTaintPass_CallEdgeToolsNoNewTaintDiagnostic is the M6.4 differential
// with an error baseline: the caller step's input derives from a secret in a
// non-secret channel, so the compile already carries exactly one taint error
// (D65). Adding the cross-adapter tools list must yield byte-for-byte the
// same set of error summaries — no new taint diagnostic from the call edge.
func TestTaintPass_CallEdgeToolsNoNewTaintDiagnostic(t *testing.T) {
	_, withoutTools := compileTaintCallEdges(t, taintCallEdgesSrc(false))
	_, withTools := compileTaintCallEdges(t, taintCallEdgesSrc(true))

	base := diagnosticSummaries(withoutTools)
	edge := diagnosticSummaries(withTools)

	if len(base) == 0 || !strings.Contains(base[0], "tainted value") {
		t.Fatalf("baseline fixture must carry the D65 taint error for the secret in input{}, got %q", base)
	}
	if len(base) != len(edge) {
		t.Fatalf("with-tools variant gained %d error diagnostics (call edges must be non-data-flow): baseline=%q with-tools=%q",
			len(edge)-len(base), base, edge)
	}
	for i := range base {
		if base[i] != edge[i] {
			t.Fatalf("with-tools variant changed error %d: baseline=%q with-tools=%q", i, base[i], edge[i])
		}
	}
	for i, d := range withTools {
		if strings.Contains(d.Summary, "tool") || strings.Contains(d.Detail, "tool") {
			t.Errorf("with-tools diagnostic %d references the tools list (no call-edge diagnostic may exist): %q / %q",
				i, d.Summary, d.Detail)
		}
	}
}

// TestTaintPass_CallEdgeToolsNonDataFlow is the clean-channel differential:
// with the secret routed through the secret_input channel both variants
// compile cleanly and the step is marked Tainted in both; the with-tools
// variant additionally records the callee call edge, proving the differential
// actually exercised the tools list.
func TestTaintPass_CallEdgeToolsNonDataFlow(t *testing.T) {
	src := `
workflow {
  name          = "taint_call_edges"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "sk_live_taint_call_edges"
}

adapter "callee" "default" {
  tool "helper_task" {}
}

adapter "caller" "default" {}

step "call" {
  target = adapter.caller.default
  secret_input {
    token = var.api_key
  }
%s  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`

	compile := func(withTools bool) (*FSMGraph, hcl.Diagnostics) {
		t.Helper()
		tools := ""
		if withTools {
			tools = `  tools = [adapter.callee.default.tools.helper_task]` + "\n"
		}
		spec, diags := Parse("taint_call_edges.hcl", []byte(strings.ReplaceAll(src, "%s", tools)))
		if diags.HasErrors() {
			t.Fatalf("parse (withTools=%v): %s", withTools, diags.Error())
		}
		return Compile(spec, taintCallEdgesSchemas())
	}

	baseline, diags := compile(false)
	if diags.HasErrors() {
		t.Fatalf("baseline compile: %s", diags.Error())
	}
	withTools, diags := compile(true)
	if diags.HasErrors() {
		t.Fatalf("with-tools compile: %s", diags.Error())
	}

	for name, g := range map[string]*FSMGraph{"baseline": baseline, "with-tools": withTools} {
		step := g.Steps["call"]
		if step == nil {
			t.Fatalf("%s: step %q not found", name, "call")
		}
		if !step.Tainted {
			t.Errorf("%s: step %q must be Tainted (secret_input is a secret channel, not a taint exemption)", name, step.Name)
		}
	}

	if edges := baseline.AdapterCallEdges; len(edges) != 0 {
		t.Errorf("baseline must record no call edges, got %v", edges)
	}
	if steps := baseline.Steps["call"].Tools; len(steps) != 0 {
		t.Errorf("baseline must record no tool grants, got %v", steps)
	}

	// The with-tools arm must be real: the graph records the callee edge and
	// the step's granted tool, otherwise the differential would pass trivially.
	edges := withTools.AdapterCallEdges
	if len(edges) != 1 {
		t.Fatalf("with-tools graph must record the callee call edge, got %v", edges)
	}
	edge := edges[0]
	if edge.CallerAdapterRef != "caller.default" || edge.CalleeAdapterRef != "callee.default" ||
		edge.Tool != "helper_task" || edge.StepName != "call" {
		t.Errorf("unexpected call edge: %+v", edge)
	}
	tools := withTools.Steps["call"].Tools
	if len(tools) != 1 || tools[0].CalleeRef != "callee.default" || tools[0].Tool != "helper_task" || tools[0].CallerIsSelf {
		t.Errorf("unexpected tool grant on the caller step: %+v", tools)
	}
}

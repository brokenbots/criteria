package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --- Shared fixtures ---

const callEdgesToolHCL = `
workflow {
  name          = "tool_calls"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}
adapter "mcp" "registry" {
  tool "search" {}
  tool "fetch" {}
}
adapter "shell" "worker" {
  tool "git_status" {}
}
step "start" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools, adapter.shell.worker.tools.git_status]
  outcome "success" { next = step.audit }
}
step "audit" {
  target = adapter.shell.worker
  tools  = [adapter.shell.worker.tools.git_status]
  outcome "success" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`

const callEdgesPlainHCL = `
workflow {
  name          = "no_tool_calls"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`

func compileCallEdgesJSON(t *testing.T, hcl string) string {
	t.Helper()
	path := writeWorkflowFile(t, hcl)
	out, err := compileWorkflowOutput(context.Background(), path, "", "json", nil, false, false)
	if err != nil {
		t.Fatalf("compile json: %v", err)
	}
	return string(out)
}

// --- JSON: adapter_call_edges ---

// TestCompileJSON_AdapterCallEdges verifies the compiled JSON renders one
// adapter_call_edges entry per recorded call edge, with the bare `.tools`
// grant rendered as tool "*".
func TestCompileJSON_AdapterCallEdges(t *testing.T) {
	out := compileCallEdgesJSON(t, callEdgesToolHCL)

	var decoded struct {
		AdapterCallEdges []struct {
			Caller string `json:"caller"`
			Callee string `json:"callee"`
			Tool   string `json:"tool"`
			Step   string `json:"step"`
		} `json:"adapter_call_edges"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode compiled json: %v", err)
	}

	want := []struct{ caller, callee, tool, step string }{
		{"mcp.registry", "mcp.registry", "*", "start"},
		{"mcp.registry", "shell.worker", "git_status", "start"},
		{"shell.worker", "shell.worker", "git_status", "audit"},
	}
	if len(decoded.AdapterCallEdges) != len(want) {
		t.Fatalf("adapter_call_edges = %+v, want %d entries", decoded.AdapterCallEdges, len(want))
	}
	for i, w := range want {
		got := decoded.AdapterCallEdges[i]
		if got.Caller != w.caller || got.Callee != w.callee || got.Tool != w.tool || got.Step != w.step {
			t.Errorf("edge[%d] = %+v, want {caller:%s callee:%s tool:%s step:%s}", i, got, w.caller, w.callee, w.tool, w.step)
		}
	}
}

// TestCompileJSON_NoCallEdgesOmitsArray verifies that a workflow without tool
// refs keeps its previous output: the adapter_call_edges key must be absent.
func TestCompileJSON_NoCallEdgesOmitsArray(t *testing.T) {
	out := compileCallEdgesJSON(t, callEdgesPlainHCL)

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode compiled json: %v", err)
	}
	if raw, ok := decoded["adapter_call_edges"]; ok {
		t.Errorf("adapter_call_edges must be absent for workflows without tool refs, got: %s", raw)
	}
}

// --- dot: dashed call edges ---

// TestDOT_CallEdgesRender verifies the tool-call visualization: dashed edges
// (distinct from the solid FSM outcome edges) labeled with the granted tool
// name, colored with the caller type's palette color, and one tool-surface
// node per distinct callee labeled with its granted surface ("tools: *" for
// a bare `.tools` grant).
func TestDOT_CallEdgesRender(t *testing.T) {
	dot := compileDOTFromHCL(t, callEdgesToolHCL)

	// Dashed edges with the caller type's palette color.
	wantEdges := []string{
		`"start" -> "adapter:mcp.registry" [style=dashed, color="` + dotAdapterPalette[0] + `"];`,
		`"start" -> "adapter:shell.worker" [style=dashed, color="` + dotAdapterPalette[0] + `", label="git_status"];`,
		`"audit" -> "adapter:shell.worker" [style=dashed, color="` + dotAdapterPalette[1] + `", label="git_status"];`,
	}
	for _, want := range wantEdges {
		if !strings.Contains(dot, want) {
			t.Errorf("dot output missing call edge %q; got:\n%s", want, dot)
		}
	}

	// Tool-surface nodes with palette fill and granted-surface labels.
	if !strings.Contains(dot, `"adapter:mcp.registry" [shape=note, style="filled", fillcolor="`+dotAdapterPalette[0]+`", label="mcp.registry\ntools: *"];`) {
		t.Errorf("dot output missing mcp.registry tool-surface node (bare grant renders tools: *); got:\n%s", dot)
	}
	if !strings.Contains(dot, `"adapter:shell.worker" [shape=note, style="filled", fillcolor="`+dotAdapterPalette[1]+`", label="shell.worker\ntools: git_status"];`) {
		t.Errorf("dot output missing shell.worker tool-surface node; got:\n%s", dot)
	}
}

// TestDOT_NoCallEdgesNoToolNodes verifies that a workflow without tool refs
// renders no tool-surface nodes and no dashed call edges.
func TestDOT_NoCallEdgesNoToolNodes(t *testing.T) {
	dot := compileDOTFromHCL(t, callEdgesPlainHCL)
	if strings.Contains(dot, `"adapter:`) {
		t.Errorf("plain workflow must not render tool-surface nodes; got:\n%s", dot)
	}
	for _, line := range strings.Split(dot, "\n") {
		if strings.Contains(line, "->") && strings.Contains(line, "style=dashed") {
			t.Errorf("plain workflow must not render dashed call edges; got line: %s", line)
		}
	}
}

// --- plan: step-level tools line ---

// TestPlan_CallEdgesToolsLine verifies the plan output renders the step-level
// tools list on its own line directly after allow_tools, in the HCL reference
// form.
func TestPlan_CallEdgesToolsLine(t *testing.T) {
	path := writeWorkflowFile(t, callEdgesToolHCL)
	out, err := renderPlanOutput(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("renderPlanOutput: %v", err)
	}
	if !strings.Contains(out, "    tools: mcp.registry.tools, shell.worker.tools.git_status\n") {
		t.Errorf("plan output missing step tools line; got:\n%s", out)
	}
	if !strings.Contains(out, "    tools: shell.worker.tools.git_status\n") {
		t.Errorf("plan output missing audit step tools line; got:\n%s", out)
	}
	// The tools line must sit directly below the allow_tools line it qualifies.
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "    tools: ") {
			continue
		}
		if i == 0 || !strings.HasPrefix(lines[i-1], "    allow_tools:") {
			t.Errorf("tools line must directly follow the allow_tools line; got lines %d-%d:\n%s\n%s", i-1, i, lines[i-1], line)
		}
	}
}

// TestPlan_NoToolsNoLine verifies that a workflow without tool refs keeps its
// previous plan output: no step-level tools line at all.
func TestPlan_NoToolsNoLine(t *testing.T) {
	path := writeWorkflowFile(t, callEdgesPlainHCL)
	out, err := renderPlanOutput(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("renderPlanOutput: %v", err)
	}
	if strings.Contains(out, "    tools:") {
		t.Errorf("plain workflow must not render a step tools line; got:\n%s", out)
	}
}

package workflow

// compile_call_edges_test.go — tests for the CRI-157 adapter call-edge pass:
// edge construction (bare `.tools` and named tools), per-step tool lists,
// call-cycle warnings (kept distinct from FSM back-edge warnings), and
// policy.max_tool_depth wiring into graph.Policy.
//
// The equivalence tests pin the scope guard: nodeTargets, reachability, and
// FSM transitions must be identical with and without tool refs present.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// callEdgesSrc renders a workflow from adapter and step declaration text.
// policyBlock is inserted inside the workflow header ("" for none). The
// initial state is "start".
func callEdgesSrc(policyBlock, adapterDecls, stepDecls string) string {
	return callEdgesSrcNamed("start", policyBlock, adapterDecls, stepDecls)
}

// callEdgesSrcNamed is callEdgesSrc with an explicit initial_state.
func callEdgesSrcNamed(initialState, policyBlock, adapterDecls, stepDecls string) string {
	var sb strings.Builder
	sb.WriteString(`
workflow {
  name          = "call-edges"
  version       = "0.1"
  initial_state = "` + initialState + `"
  target_state  = "done"
`)
	if policyBlock != "" {
		sb.WriteString("\n  " + policyBlock + "\n")
	}
	sb.WriteString("}\n\n")
	sb.WriteString(adapterDecls)
	sb.WriteString("\n\n")
	sb.WriteString(stepDecls)
	sb.WriteString("\n\nstate \"done\" { terminal = true }\n")
	return sb.String()
}

// callEdgesSchemas registers the given adapter types with an empty input
// schema and the adapter_tools capability, so granting steps compile without
// the pointless-caller warning.
func callEdgesSchemas(types ...string) map[string]AdapterInfo {
	out := make(map[string]AdapterInfo, len(types))
	for _, ty := range types {
		out[ty] = AdapterInfo{InputSchema: map[string]ConfigField{}, Capabilities: []string{"adapter_tools"}}
	}
	return out
}

// compileCallEdges parses and compiles a workflow source, failing the test on
// parse errors.
func compileCallEdges(t *testing.T, src string, schemas map[string]AdapterInfo) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	return Compile(spec, schemas)
}

// fsmShape renders nodeTargets for every node plus the reachable set from the
// initial state as one deterministic string. Two graphs with identical FSM
// routing must render identical shapes regardless of adapter call edges.
func fsmShape(g *FSMGraph) string {
	var sb strings.Builder
	names := make([]string, 0, len(g.Steps)+len(g.States)+len(g.Waits)+len(g.Approvals)+len(g.Switches))
	for name := range g.Steps {
		names = append(names, name)
	}
	for name := range g.States {
		names = append(names, name)
	}
	for name := range g.Waits {
		names = append(names, name)
	}
	for name := range g.Approvals {
		names = append(names, name)
	}
	for name := range g.Switches {
		names = append(names, name)
	}
	sort.Strings(names)
	sb.WriteString("targets:\n")
	for _, name := range names {
		targets := nodeTargets(name, g)
		sort.Strings(targets)
		sb.WriteString("  " + name + " -> [" + strings.Join(targets, ",") + "]\n")
	}
	reachable := collectReachableNodes(g, g.InitialState)
	reachList := make([]string, 0, len(reachable))
	for name := range reachable {
		reachList = append(reachList, name)
	}
	sort.Strings(reachList)
	sb.WriteString("reachable: [" + strings.Join(reachList, ",") + "]\n")
	sb.WriteString("transitions:\n")
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		outcomes := make([]string, 0, len(step.Outcomes))
		for outName, co := range step.Outcomes {
			outcomes = append(outcomes, outName+"->"+co.Next)
		}
		sort.Strings(outcomes)
		sb.WriteString("  " + name + " [" + strings.Join(outcomes, ",") + "]\n")
	}
	return sb.String()
}

// TestCallEdges_BareAndNamed pins edge construction for both grant forms on
// one step: a bare `.tools` grant (empty Tool) and a named tool grant, in
// declaration order, with the originating step as provenance. The same grants
// must also appear on the step's Tools list.
func TestCallEdges_BareAndNamed(t *testing.T) {
	src := callEdgesSrc("",
		`adapter "copilot" "worker" {
  tool "plan" {}
}

adapter "shell" "worker" {
  tool "git_status" {}
}`,
		`step "start" {
  target = adapter.copilot.worker
  tools  = [adapter.shell.worker.tools, adapter.shell.worker.tools.git_status]
  outcome "success" { next = state.done }
}`)
	g, diags := compileCallEdges(t, src, callEdgesSchemas("copilot", "shell"))
	if len(diags) != 0 {
		t.Fatalf("expected clean compile, got: %s", diags.Error())
	}
	wantEdges := []AdapterCallEdge{
		{CallerAdapterRef: "copilot.worker", CalleeAdapterRef: "shell.worker", Tool: "", StepName: "start"},
		{CallerAdapterRef: "copilot.worker", CalleeAdapterRef: "shell.worker", Tool: "git_status", StepName: "start"},
	}
	if !reflect.DeepEqual(g.AdapterCallEdges, wantEdges) {
		t.Errorf("AdapterCallEdges = %+v, want %+v", g.AdapterCallEdges, wantEdges)
	}
	wantTools := []AdapterToolRef{
		{CallerIsSelf: false, CalleeRef: "shell.worker", Tool: ""},
		{CallerIsSelf: false, CalleeRef: "shell.worker", Tool: "git_status"},
	}
	if !reflect.DeepEqual(g.Steps["start"].Tools, wantTools) {
		t.Errorf("StepNode.Tools = %+v, want %+v", g.Steps["start"].Tools, wantTools)
	}
}

// TestCallEdges_CallerIsSelf pins the self-grant form: the callee is the
// step's own adapter. The edge set records it and Tools carries
// CallerIsSelf=true, but no cycle warning may fire — a self-call is a
// runtime-rejected call per ADR-0004 §10, not a tool-call cycle.
func TestCallEdges_CallerIsSelf(t *testing.T) {
	src := callEdgesSrc("",
		`adapter "mcp" "registry" {
  tool "search" {}
}`,
		`step "start" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools.search]
  outcome "success" { next = state.done }
}`)
	g, diags := compileCallEdges(t, src, callEdgesSchemas("mcp"))
	if len(diags) != 0 {
		t.Fatalf("self-grant must compile clean, got: %s", diags.Error())
	}
	wantEdges := []AdapterCallEdge{
		{CallerAdapterRef: "mcp.registry", CalleeAdapterRef: "mcp.registry", Tool: "search", StepName: "start"},
	}
	if !reflect.DeepEqual(g.AdapterCallEdges, wantEdges) {
		t.Errorf("AdapterCallEdges = %+v, want %+v", g.AdapterCallEdges, wantEdges)
	}
	wantTools := []AdapterToolRef{{CallerIsSelf: true, CalleeRef: "mcp.registry", Tool: "search"}}
	if !reflect.DeepEqual(g.Steps["start"].Tools, wantTools) {
		t.Errorf("StepNode.Tools = %+v, want %+v", g.Steps["start"].Tools, wantTools)
	}
}

// TestCallEdges_NoToolRefsNoEdges pins the no-op behavior: a workflow without
// any tools attribute produces no edges, no per-step tool lists, and no
// cycle warnings.
func TestCallEdges_NoToolRefsNoEdges(t *testing.T) {
	src := callEdgesSrc("",
		`adapter "copilot" "worker" {
  tool "plan" {}
}

adapter "shell" "worker" {
  tool "git_status" {}
}`,
		`step "start" {
  target = adapter.copilot.worker
  outcome "success" { next = state.done }
}`)
	g, diags := compileCallEdges(t, src, callEdgesSchemas("copilot", "shell"))
	if len(diags) != 0 {
		t.Fatalf("expected clean compile, got: %s", diags.Error())
	}
	if len(g.AdapterCallEdges) != 0 {
		t.Errorf("AdapterCallEdges = %+v, want none", g.AdapterCallEdges)
	}
	if g.Steps["start"].Tools != nil {
		t.Errorf("StepNode.Tools = %+v, want nil", g.Steps["start"].Tools)
	}
}

// TestCallEdges_IteratingStep verifies the pass covers iterating steps
// (for_each) the same way as plain adapter steps.
func TestCallEdges_IteratingStep(t *testing.T) {
	src := callEdgesSrc("",
		`adapter "copilot" "worker" {
  tool "plan" {}
}

adapter "shell" "worker" {
  tool "git_status" {}
}`,
		`step "start" {
  target   = adapter.copilot.worker
  for_each = ["alpha", "beta"]
  tools    = [adapter.shell.worker.tools.git_status]
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.done }
}`)
	g, diags := compileCallEdges(t, src, callEdgesSchemas("copilot", "shell"))
	if len(diags) != 0 {
		t.Fatalf("expected clean compile, got: %s", diags.Error())
	}
	wantEdges := []AdapterCallEdge{
		{CallerAdapterRef: "copilot.worker", CalleeAdapterRef: "shell.worker", Tool: "git_status", StepName: "start"},
	}
	if !reflect.DeepEqual(g.AdapterCallEdges, wantEdges) {
		t.Errorf("AdapterCallEdges = %+v, want %+v", g.AdapterCallEdges, wantEdges)
	}
}

// TestCallEdges_SubworkflowStepNoEdge verifies a subworkflow-targeted step
// records its tools list (CallerIsSelf always false; there is no caller
// adapter) but produces no AdapterCallEdge.
func TestCallEdges_SubworkflowStepNoEdge(t *testing.T) {
	dir := t.TempDir()
	childDir := filepath.Join(dir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	childSrc := `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

state "done" { terminal = true }
`
	if err := os.WriteFile(filepath.Join(childDir, "child.hcl"), []byte(childSrc), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	src := callEdgesSrcNamed("run", "",
		`adapter "mcp" "registry" {
  tool "search" {}
}`,
		`subworkflow "child" { source = "./child" }

step "run" {
  target = subworkflow.child
  tools  = [adapter.mcp.registry.tools.search]
  outcome "success" { next = state.done }
}`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{AllowedRoots: []string{dir}},
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if len(diags) != 0 {
		t.Fatalf("expected clean compile, got: %s", diags.Error())
	}
	if len(g.AdapterCallEdges) != 0 {
		t.Errorf("subworkflow step must produce no AdapterCallEdge, got %+v", g.AdapterCallEdges)
	}
	wantTools := []AdapterToolRef{{CallerIsSelf: false, CalleeRef: "mcp.registry", Tool: "search"}}
	if !reflect.DeepEqual(g.Steps["run"].Tools, wantTools) {
		t.Errorf("StepNode.Tools = %+v, want %+v", g.Steps["run"].Tools, wantTools)
	}
}

// TestToolRefSubworkflowStepHandshakeSurface verifies a subworkflow-targeted
// step's named tools ref is checked against the handshake's InfoResponse.tools
// surface (CRI-173): it must compile clean when the name is reported, fail
// with the mode 9 diagnostic when absent, and fall back to the strict default
// with an accurate "no handshake available" detail when no schemas were
// supplied (standalone compile).
func TestToolRefSubworkflowStepHandshakeSurface(t *testing.T) {
	dir := t.TempDir()
	childDir := filepath.Join(dir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	childSrc := `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

state "done" { terminal = true }
`
	if err := os.WriteFile(filepath.Join(childDir, "child.hcl"), []byte(childSrc), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	src := callEdgesSrcNamed("run", "",
		`adapter "mcp" "registry" {}`,
		`subworkflow "child" { source = "./child" }

step "run" {
  target = subworkflow.child
  tools  = [adapter.mcp.registry.tools.search]
  outcome "success" { next = state.done }
}`)
	opts := func(schemas map[string]AdapterInfo) CompileOpts {
		return CompileOpts{
			WorkflowDir:         dir,
			Schemas:             schemas,
			SubWorkflowResolver: &LocalSubWorkflowResolver{AllowedRoots: []string{dir}},
		}
	}
	handshakeSchemas := func(runtimeTools ...string) map[string]AdapterInfo {
		return map[string]AdapterInfo{
			"mcp": {InputSchema: map[string]ConfigField{}, RuntimeTools: runtimeTools},
		}
	}

	t.Run("reported name compiles clean", func(t *testing.T) {
		spec, diags := Parse("t.hcl", []byte(src))
		if diags.HasErrors() {
			t.Fatalf("parse: %s", diags.Error())
		}
		_, diags = CompileWithOpts(spec, nil, opts(handshakeSchemas("search")))
		if len(diags) != 0 {
			t.Fatalf("expected clean compile, got %d diagnostic(s): %s", len(diags), diags.Error())
		}
	})

	t.Run("unreported name fails the mode 9 check", func(t *testing.T) {
		spec, diags := Parse("t.hcl", []byte(src))
		if diags.HasErrors() {
			t.Fatalf("parse: %s", diags.Error())
		}
		_, diags = CompileWithOpts(spec, nil, opts(handshakeSchemas("fetch")))
		expectToolDiag(t, diags, hcl.DiagError, `tools entry references unknown tool "adapter.mcp.registry.tools.search"`, src, "adapter.mcp.registry.tools.search")
		if len(diags) != 1 {
			t.Fatalf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
		}
		if !strings.Contains(diags[0].Detail, "InfoResponse.tools: fetch") {
			t.Errorf("detail %q must name the reported runtime surface", diags[0].Detail)
		}
	})

	t.Run("nil schemas fall back to the strict default with an accurate detail", func(t *testing.T) {
		spec, diags := Parse("t.hcl", []byte(src))
		if diags.HasErrors() {
			t.Fatalf("parse: %s", diags.Error())
		}
		_, diags = CompileWithOpts(spec, nil, opts(nil))
		expectToolDiag(t, diags, hcl.DiagError, `callee "mcp.registry" presents no tool surface`, src, "adapter.mcp.registry.tools.search")
		if len(diags) != 1 {
			t.Fatalf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
		}
		if !strings.Contains(diags[0].Detail, "no adapter handshake was available to the compiler") {
			t.Errorf("detail %q must state that no handshake was available", diags[0].Detail)
		}
		if strings.Contains(diags[0].Detail, "exposes no tools") {
			t.Errorf("detail %q must not claim a handshake was consulted", diags[0].Detail)
		}
	})
}

// TestCallEdges_CycleWarning pins the ADR-0004 §6 cycle warning: a two-adapter
// tool-call cycle is a compile WARNING (not an error), reported once, with the
// call path in the summary and the runtime cap named in the detail.
func TestCallEdges_CycleWarning(t *testing.T) {
	src := callEdgesSrcNamed("s1", "",
		`adapter "a" "one" {
  tool "x" {}
}

adapter "b" "two" {
  tool "y" {}
}`,
		`step "s1" {
  target = adapter.a.one
  tools  = [adapter.b.two.tools]
  outcome "success" { next = step.s2 }
}

step "s2" {
  target = adapter.b.two
  tools  = [adapter.a.one.tools]
  outcome "success" { next = state.done }
}`)
	g, diags := compileCallEdges(t, src, callEdgesSchemas("a", "b"))
	if diags.HasErrors() {
		t.Fatalf("a call cycle must not be a compile error: %s", diags.Error())
	}
	if g == nil {
		t.Fatal("expected a graph despite the cycle warning")
	}
	if len(diags) != 1 {
		t.Fatalf("expected exactly one warning, got %d: %s", len(diags), diags.Error())
	}
	d := diags[0]
	if d.Severity != hcl.DiagWarning {
		t.Errorf("severity = %v, want warning", d.Severity)
	}
	wantSummary := "adapter tool-call cycle detected: a.one -> b.two -> a.one"
	if d.Summary != wantSummary {
		t.Errorf("summary = %q, want %q", d.Summary, wantSummary)
	}
	for _, want := range []string{"policy.max_tool_depth", "currently 8", "allowed"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q missing %q", d.Detail, want)
		}
	}
	wantEdges := []AdapterCallEdge{
		{CallerAdapterRef: "a.one", CalleeAdapterRef: "b.two", Tool: "", StepName: "s1"},
		{CallerAdapterRef: "b.two", CalleeAdapterRef: "a.one", Tool: "", StepName: "s2"},
	}
	if !reflect.DeepEqual(g.AdapterCallEdges, wantEdges) {
		t.Errorf("AdapterCallEdges = %+v, want %+v", g.AdapterCallEdges, wantEdges)
	}
}

// TestCallEdges_CycleThreeAdapters verifies transitive cycle detection:
// a -> b -> c -> a closes through three hops and warns once.
func TestCallEdges_CycleThreeAdapters(t *testing.T) {
	src := callEdgesSrcNamed("s1", "",
		`adapter "a" "one" {
  tool "x" {}
}

adapter "b" "two" {
  tool "y" {}
}

adapter "c" "three" {
  tool "z" {}
}`,
		`step "s1" {
  target = adapter.a.one
  tools  = [adapter.b.two.tools]
  outcome "success" { next = step.s2 }
}

step "s2" {
  target = adapter.b.two
  tools  = [adapter.c.three.tools]
  outcome "success" { next = step.s3 }
}

step "s3" {
  target = adapter.c.three
  tools  = [adapter.a.one.tools]
  outcome "success" { next = state.done }
}`)
	_, diags := compileCallEdges(t, src, callEdgesSchemas("a", "b", "c"))
	if diags.HasErrors() {
		t.Fatalf("a transitive call cycle must not be a compile error: %s", diags.Error())
	}
	if len(diags) != 1 {
		t.Fatalf("expected exactly one cycle warning, got %d: %s", len(diags), diags.Error())
	}
	wantSummary := "adapter tool-call cycle detected: a.one -> b.two -> c.three -> a.one"
	if diags[0].Summary != wantSummary {
		t.Errorf("summary = %q, want %q", diags[0].Summary, wantSummary)
	}
}

// TestCallEdges_CycleDedupePerPair verifies that two steps producing the same
// caller/callee pair (with different tools) still yield a single cycle warning.
func TestCallEdges_CycleDedupePerPair(t *testing.T) {
	src := callEdgesSrcNamed("s1", "",
		`adapter "a" "one" {
  tool "x" {}
}

adapter "b" "two" {
  tool "y" {}
}`,
		`step "s1" {
  target = adapter.a.one
  tools  = [adapter.b.two.tools]
  outcome "success" { next = step.s2 }
}

step "s2" {
  target = adapter.a.one
  tools  = [adapter.b.two.tools.y]
  outcome "success" { next = step.s3 }
}

step "s3" {
  target = adapter.b.two
  tools  = [adapter.a.one.tools]
  outcome "success" { next = state.done }
}`)
	g, diags := compileCallEdges(t, src, callEdgesSchemas("a", "b"))
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if len(g.AdapterCallEdges) != 3 {
		t.Errorf("AdapterCallEdges = %+v, want 3 edges (provenance per step)", g.AdapterCallEdges)
	}
	if len(diags) != 1 {
		t.Fatalf("expected exactly one cycle warning, got %d: %s", len(diags), diags.Error())
	}
}

// TestCallEdges_CycleDistinctFromBackEdgeWarnings pins the separation between
// the two cycle mechanisms: an FSM back-edge workflow produces the back-edge
// warning and no call-cycle warning, and a tool-call-cycle workflow produces
// the call-cycle warning and no back-edge warning.
func TestCallEdges_CycleDistinctFromBackEdgeWarnings(t *testing.T) {
	t.Run("FSM back-edge produces no call-cycle warning", func(t *testing.T) {
		src := callEdgesSrc("policy { max_total_steps = 300 }",
			`adapter "shell" "worker" {
  tool "git_status" {}
}`,
			`step "start" {
  target = adapter.shell.worker
  outcome "success" { next = step.start }
}`)
		g, diags := compileCallEdges(t, src, callEdgesSchemas("shell"))
		if diags.HasErrors() {
			t.Fatalf("compile: %s", diags.Error())
		}
		expectToolDiag(t, diags, hcl.DiagWarning, "appears in a loop with max_total_steps=300", src, "")
		for _, d := range diags {
			if strings.Contains(d.Summary, "tool-call cycle") {
				t.Errorf("FSM back-edge must not produce a call-cycle warning: %s", d.Summary)
			}
		}
		if len(g.AdapterCallEdges) != 0 {
			t.Errorf("no tools declared, got edges %+v", g.AdapterCallEdges)
		}
	})
	t.Run("tool-call cycle produces no back-edge warning", func(t *testing.T) {
		src := callEdgesSrcNamed("s1", "",
			`adapter "a" "one" {
  tool "x" {}
}

adapter "b" "two" {
  tool "y" {}
}`,
			`step "s1" {
  target = adapter.a.one
  tools  = [adapter.b.two.tools]
  outcome "success" { next = step.s2 }
}

step "s2" {
  target = adapter.b.two
  tools  = [adapter.a.one.tools]
  outcome "success" { next = state.done }
}`)
		_, diags := compileCallEdges(t, src, callEdgesSchemas("a", "b"))
		if diags.HasErrors() {
			t.Fatalf("compile: %s", diags.Error())
		}
		if len(diags) != 1 {
			t.Fatalf("expected exactly the cycle warning, got %d: %s", len(diags), diags.Error())
		}
		if strings.Contains(diags[0].Summary, "back-edge") || strings.Contains(diags[0].Summary, "max_visits") {
			t.Errorf("call-cycle warning must be distinct from back-edge wording: %s", diags[0].Summary)
		}
	})
}

// TestCallEdges_PolicyMaxToolDepth pins graph.Policy.MaxToolDepth wiring:
// unset keeps DefaultPolicy's engine default of 8, a declared positive value
// overrides it, and declared values < 1 are parse errors that block compile.
func TestCallEdges_PolicyMaxToolDepth(t *testing.T) {
	if DefaultPolicy.MaxToolDepth != 8 {
		t.Fatalf("DefaultPolicy.MaxToolDepth = %d, want 8", DefaultPolicy.MaxToolDepth)
	}
	t.Run("unset defaults to 8", func(t *testing.T) {
		src := callEdgesSrc("",
			`adapter "shell" "worker" {
  tool "git_status" {}
}`,
			`step "start" {
  target = adapter.shell.worker
  outcome "success" { next = state.done }
}`)
		g, diags := compileCallEdges(t, src, callEdgesSchemas("shell"))
		if len(diags) != 0 {
			t.Fatalf("compile: %s", diags.Error())
		}
		if g.Policy.MaxToolDepth != 8 {
			t.Errorf("unset max_tool_depth = %d, want 8", g.Policy.MaxToolDepth)
		}
	})
	t.Run("declared value overrides", func(t *testing.T) {
		src := callEdgesSrc("policy { max_tool_depth = 3 }",
			`adapter "shell" "worker" {
  tool "git_status" {}
}`,
			`step "start" {
  target = adapter.shell.worker
  outcome "success" { next = state.done }
}`)
		g, diags := compileCallEdges(t, src, callEdgesSchemas("shell"))
		if len(diags) != 0 {
			t.Fatalf("compile: %s", diags.Error())
		}
		if g.Policy.MaxToolDepth != 3 {
			t.Errorf("max_tool_depth = %d, want 3", g.Policy.MaxToolDepth)
		}
	})
	for _, value := range []string{"0", "-1"} {
		t.Run("rejected "+value, func(t *testing.T) {
			src := callEdgesSrc("policy { max_tool_depth = "+value+" }",
				`adapter "shell" "worker" {
  tool "git_status" {}
}`,
				`step "start" {
  target = adapter.shell.worker
  outcome "success" { next = state.done }
}`)
			_, diags := Parse("t.hcl", []byte(src))
			if !diags.HasErrors() {
				t.Fatalf("max_tool_depth = %s: want parse error, got none", value)
			}
			if !strings.Contains(diags.Error(), "max_tool_depth must be an integer >= 1") {
				t.Errorf("unexpected diagnostic: %s", diags.Error())
			}
		})
	}
}

// TestCallEdges_ReachabilityAndRoutingUnchanged pins the scope guard: adding
// tool refs to a workflow must not change nodeTargets, reachability, or FSM
// transitions — only the new edge set appears.
func TestCallEdges_ReachabilityAndRoutingUnchanged(t *testing.T) {
	adapters := `adapter "a" "one" {
  tool "x" {}
}

adapter "b" "two" {
  tool "y" {}
}`
	stepsWithout := `step "s1" {
  target = adapter.a.one
  outcome "success" { next = step.s2 }
}

step "s2" {
  target = adapter.b.two
  outcome "success" { next = step.s3 }
  outcome "failure"    { next = state.done }
}

step "s3" {
  target = adapter.a.one
  outcome "success" { next = state.done }
}`
	stepsWith := strings.NewReplacer(
		"target = adapter.a.one\n  outcome \"success\" { next = step.s2 }",
		"target = adapter.a.one\n  tools  = [adapter.b.two.tools.y]\n  outcome \"success\" { next = step.s2 }",
		"target = adapter.a.one\n  outcome \"success\" { next = state.done }",
		"target = adapter.a.one\n  tools  = [adapter.b.two.tools]\n  outcome \"success\" { next = state.done }",
	).Replace(stepsWithout)
	srcWithout := callEdgesSrcNamed("s1", "", adapters, stepsWithout)
	srcWith := callEdgesSrcNamed("s1", "", adapters, stepsWith)

	gWithout, diags := compileCallEdges(t, srcWithout, callEdgesSchemas("a", "b"))
	if len(diags) != 0 {
		t.Fatalf("without tools: %s", diags.Error())
	}
	gWith, diags := compileCallEdges(t, srcWith, callEdgesSchemas("a", "b"))
	if len(diags) != 0 {
		t.Fatalf("with tools: %s", diags.Error())
	}

	if shapeWithout, shapeWith := fsmShape(gWithout), fsmShape(gWith); shapeWithout != shapeWith {
		t.Errorf("nodeTargets/reachability/transitions changed by tool refs:\nwithout:\n%s\nwith:\n%s", shapeWithout, shapeWith)
	}
	if len(gWith.AdapterCallEdges) == 0 {
		t.Error("expected call edges on the with-tools graph")
	}
	for _, name := range []string{"s1", "s3"} {
		if len(gWith.Steps[name].Tools) == 0 {
			t.Errorf("step %q: expected a recorded tools list", name)
		}
	}
	if len(gWith.Steps["s2"].Tools) != 0 {
		t.Errorf("step \"s2\": unexpected tools list: %+v", gWith.Steps["s2"].Tools)
	}
	if gWithout.Steps["s1"].Tools != nil {
		t.Errorf("without-tools graph must have no Tools, got %+v", gWithout.Steps["s1"].Tools)
	}
}

// TestCallEdges_DiagnosticsUnchangedForAcyclicTools pins that a workflow with
// tool refs but no call cycle compiles with exactly the same diagnostics as
// the same workflow without them.
func TestCallEdges_DiagnosticsUnchangedForAcyclicTools(t *testing.T) {
	adapters := `adapter "a" "one" {
  tool "x" {}
}

adapter "b" "two" {
  tool "y" {}
}`
	stepsWithout := `step "s1" {
  target = adapter.a.one
  outcome "success" { next = state.done }
}`
	stepsWith := `step "s1" {
  target = adapter.a.one
  tools  = [adapter.b.two.tools.y]
  outcome "success" { next = state.done }
}`
	_, diagsWithout := compileCallEdges(t, callEdgesSrcNamed("s1", "", adapters, stepsWithout), callEdgesSchemas("a", "b"))
	gWith, diagsWith := compileCallEdges(t, callEdgesSrcNamed("s1", "", adapters, stepsWith), callEdgesSchemas("a", "b"))
	if len(diagsWithout) != 0 || len(diagsWith) != 0 {
		t.Fatalf("acyclic tool refs must not add diagnostics: without=%d (%s) with=%d (%s)",
			len(diagsWithout), diagsWithout.Error(), len(diagsWith), diagsWith.Error())
	}
	if len(gWith.AdapterCallEdges) != 1 {
		t.Errorf("AdapterCallEdges = %+v, want exactly 1 edge", gWith.AdapterCallEdges)
	}
}

// TestAdapterNode_CarriesToolSurface verifies that the compiled graph's
// AdapterNodes expose the declared tool surface (CRI-159): tool-block names
// in declaration order for static adapters and the dynamic_tools flag for
// dynamic ones. The runtime seam (internal/adapterhost) reads these fields
// when validating callee adapters and static tools.
func TestAdapterNode_CarriesToolSurface(t *testing.T) {
	src := callEdgesSrc("",
		`adapter "shell" "runner" {
  tool "git_status" {}
  tool "run" {}
}

adapter "mcp" "fs" {
  dynamic_tools = true
}`,
		`step "start" {
  target = adapter.shell.runner
  outcome "success" { next = state.done }
}`)
	g, diags := compileCallEdges(t, src, callEdgesSchemas("shell", "mcp"))
	if len(diags) != 0 {
		t.Fatalf("expected clean compile, got: %s", diags.Error())
	}

	shell := g.Adapters["shell.runner"]
	if shell == nil {
		t.Fatal("expected adapter shell.runner in graph")
	}
	if want := []string{"git_status", "run"}; !reflect.DeepEqual(shell.StaticTools, want) {
		t.Errorf("StaticTools = %+v, want %+v", shell.StaticTools, want)
	}
	if shell.DynamicTools {
		t.Error("expected DynamicTools=false for tool-block adapter")
	}

	fs := g.Adapters["mcp.fs"]
	if fs == nil {
		t.Fatal("expected adapter mcp.fs in graph")
	}
	if !fs.DynamicTools {
		t.Error("expected DynamicTools=true for dynamic_tools adapter")
	}
	if len(fs.StaticTools) != 0 {
		t.Errorf("StaticTools = %+v, want empty", fs.StaticTools)
	}
}

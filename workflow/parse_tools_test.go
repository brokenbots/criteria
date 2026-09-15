package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// TestFixtures_ToolParse walks every fixture module under testdata/tools
// (one workflow per subdirectory) and asserts the M3 tool constructs decode
// into the specs as raw fields. This is the CRI-155 parse pass only: no
// validation, no reference resolution, and no graph changes.
func TestFixtures_ToolParse(t *testing.T) {
	fixtureDir := filepath.Join("testdata", "tools")
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixtureDir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("no fixtures under %s", fixtureDir)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			dir := filepath.Join(fixtureDir, entry.Name())
			spec, diags := ParseDir(dir)
			if diags.HasErrors() {
				t.Fatalf("parse %s: %s", dir, diags.Error())
			}
			assertToolFixture(t, entry.Name(), spec)
			assertNoGraphChanges(t, dir)
		})
	}
}

// assertToolFixture asserts the decoded spec fields for one fixture, keyed by
// fixture filename.
func assertToolFixture(t *testing.T, name string, spec *Spec) {
	t.Helper()
	switch name {
	case "adapter_tool_blocks":
		if len(spec.Adapters) != 1 {
			t.Fatalf("got %d adapters, want 1", len(spec.Adapters))
		}
		tools := spec.Adapters[0].Tools
		want := []string{"search", "fetch", "reserved"}
		if len(tools) != len(want) {
			t.Fatalf("adapter Tools = %v, want names %v", toolNames(tools), want)
		}
		for i, wantName := range want {
			if tools[i].Name != wantName {
				t.Errorf("Tools[%d].Name = %q, want %q", i, tools[i].Name, wantName)
			}
		}
		// The reserved tool block body (unknown attributes and blocks) must be
		// decoded and ignored without error; Remain captures it for CRI-156.
		if tools[2].Remain == nil {
			t.Error("reserved tool body must be captured in ToolDeclSpec.Remain")
		}
		if spec.Adapters[0].DynamicTools {
			t.Error("DynamicTools must default to false")
		}
		if spec.Header == nil || spec.Header.Policy == nil {
			t.Fatalf("policy block missing from fixture %s", name)
		}
		if got := spec.Header.Policy.MaxToolDepth; got != 12 {
			t.Errorf("policy MaxToolDepth = %d, want 12", got)
		}
		if len(spec.Steps) != 1 || len(spec.Steps[0].Tools) != 0 {
			t.Errorf("no step-level tools expected in %s, got %v", name, spec.Steps[0].Tools)
		}
	case "adapter_dynamic_tools":
		if len(spec.Adapters) != 1 {
			t.Fatalf("got %d adapters, want 1", len(spec.Adapters))
		}
		if !spec.Adapters[0].DynamicTools {
			t.Error("DynamicTools = false, want true")
		}
		if len(spec.Adapters[0].Tools) != 0 {
			t.Errorf("adapter Tools = %v, want none", toolNames(spec.Adapters[0].Tools))
		}
	case "step_tools_bare_ref":
		assertStepTools(t, spec, "adapter.mcp.registry.tools")
	case "step_tools_named_ref":
		assertStepTools(t, spec, "adapter.mcp.registry.tools.search")
	case "step_tools_list":
		assertStepTools(t, spec,
			"adapter.mcp.registry.tools",
			"adapter.mcp.registry.tools.search",
			"adapter.shell.worker.tools.git_status",
		)
	default:
		t.Fatalf("unknown fixture %q: add an assertion branch for it", name)
	}
}

// assertStepTools asserts that the single step in spec decoded the given raw
// traversal grants, with no reference resolution performed.
func assertStepTools(t *testing.T, spec *Spec, want ...string) {
	t.Helper()
	if len(spec.Steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(spec.Steps))
	}
	tools := spec.Steps[0].Tools
	if len(tools) != len(want) {
		t.Fatalf("StepSpec.Tools = %v, want %v", toolRefStrings(t, tools), want)
	}
	for i, wantRef := range want {
		tr := tools[i]
		if got := toolTraversalString(t, tr); got != wantRef {
			t.Errorf("StepSpec.Tools[%d] = %q, want %q", i, got, wantRef)
		}
		// Entries are raw traversals rooted at the adapter keyword; positions
		// stay attached so CRI-156 can diagnose invalid shapes later.
		if got := tr[0].(hcl.TraverseRoot); got.Name != "adapter" {
			t.Errorf("StepSpec.Tools[%d] root = %q, want \"adapter\"", i, got.Name)
		}
		if filename := tr[0].SourceRange().Filename; filename == "" {
			t.Errorf("StepSpec.Tools[%d] lost its source range", i)
		}
	}
}

// assertNoGraphChanges compiles the fixture and compares the resulting graph
// against the same workflow with the step-level `tools` attribute removed. The
// tool constructs must not alter the compiled graph in CRI-155 (no reference
// resolution, no graph wiring).
func assertNoGraphChanges(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var hclName string
	for _, entry := range entries {
		if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".hcl") || strings.HasSuffix(entry.Name(), ".chcl")) {
			hclName = entry.Name()
			break
		}
	}
	if hclName == "" {
		t.Fatalf("no .hcl file in fixture dir %s", dir)
	}
	path := filepath.Join(dir, hclName)
	srcBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(srcBytes), "\n")
	baseline := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "tools") {
			continue
		}
		baseline = append(baseline, line)
	}
	baselineSrc := []byte(strings.Join(baseline, "\n"))

	withTools, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("parse %s: %s", dir, diags.Error())
	}
	withoutTools, diags := Parse(path, baselineSrc)
	if diags.HasErrors() {
		t.Fatalf("parse baseline for %s: %s", path, diags.Error())
	}

	gWith, diags := Compile(withTools, nil)
	if diags.HasErrors() {
		t.Fatalf("compile %s: %s", path, diags.Error())
	}
	gWithout, diags := Compile(withoutTools, nil)
	if diags.HasErrors() {
		t.Fatalf("compile baseline for %s: %s", path, diags.Error())
	}

	want := graphShape(t, gWithout)
	got := graphShape(t, gWith)
	if got != want {
		t.Errorf("tools constructs changed the compiled graph:\n with tools:    %s\n without tools: %s", got, want)
	}
}

// graphShape renders the position-independent, tool-irrelevant structure of a
// compiled graph so two graphs can be compared for equality.
func graphShape(t *testing.T, g *FSMGraph) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("name=" + g.Name)
	sb.WriteString(" initial=" + g.InitialState)
	sb.WriteString(" target=" + g.TargetState)
	sb.WriteString(" defaultEnv=" + g.DefaultEnvironment)
	sb.WriteString(" adapters=[")
	for _, key := range g.AdapterOrder {
		ad := g.Adapters[key]
		sb.WriteString(key + "{type=" + ad.Type + ",name=" + ad.Name + ",source=" + ad.Source + ",env=" + ad.Environment + ",on_crash=" + ad.OnCrash + "},")
	}
	sb.WriteString("] steps=[")
	for _, name := range g.StepOrder() {
		st := g.Steps[name]
		sb.WriteString(fmt.Sprintf("%s{kind=%d,target=%s%s,on_crash=%s,on_failure=%s},",
			name, st.TargetKind, st.AdapterRef, st.SubworkflowRef, st.OnCrash, st.OnFailure))
	}
	sb.WriteString("] states=[")
	stateNames := make([]string, 0, len(g.States))
	for name := range g.States {
		stateNames = append(stateNames, name)
	}
	sort.Strings(stateNames)
	sb.WriteString(strings.Join(stateNames, ",") + "]")
	sb.WriteString(fmt.Sprintf(" policy=%+v", g.Policy))
	return sb.String()
}

// toolNames renders decoded tool declarations as their name labels.
func toolNames(tools []ToolDeclSpec) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}

// toolRefStrings renders raw tool traversals as dotted target strings.
func toolRefStrings(t *testing.T, tools []hcl.Traversal) []string {
	t.Helper()
	refs := make([]string, len(tools))
	for i, tr := range tools {
		refs[i] = toolTraversalString(t, tr)
	}
	return refs
}

// toolTraversalString renders an attribute-step traversal (root and .attr
// names) as its dotted target string, e.g. adapter.mcp.registry.tools.search.
func toolTraversalString(t *testing.T, tr hcl.Traversal) string {
	t.Helper()
	var sb strings.Builder
	for _, step := range tr {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			sb.WriteString(s.Name)
		case hcl.TraverseAttr:
			sb.WriteString("." + s.Name)
		default:
			t.Fatalf("unexpected traverser %T in tools traversal", step)
		}
	}
	return sb.String()
}

// TestParseTools_LegacyWorkflowLeavesToolFieldsZero asserts that legacy
// workflows without tool constructs keep parsing with zero-valued tool fields.
func TestParseTools_LegacyWorkflowLeavesToolFieldsZero(t *testing.T) {
	src := []byte(`
workflow {
  name          = "legacy"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

adapter "shell" "worker" {}

step "start" {
  target = adapter.shell.worker
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
	spec, diags := Parse("legacy.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse legacy workflow: %s", diags.Error())
	}
	if len(spec.Adapters) != 1 || len(spec.Steps) != 1 {
		t.Fatalf("unexpected legacy shape: %d adapters, %d steps", len(spec.Adapters), len(spec.Steps))
	}
	if len(spec.Adapters[0].Tools) != 0 {
		t.Errorf("adapter Tools = %v, want none", toolNames(spec.Adapters[0].Tools))
	}
	if spec.Adapters[0].DynamicTools {
		t.Error("DynamicTools = true, want false")
	}
	if len(spec.Steps[0].Tools) != 0 {
		t.Errorf("StepSpec.Tools = %v, want none", toolRefStrings(t, spec.Steps[0].Tools))
	}
	if spec.Header == nil || spec.Header.Policy != nil {
		t.Fatalf("legacy workflow without policy must leave Header.Policy nil")
	}
}

// TestParseTools_MaxToolDepthRangeCheck pins the CRI-155 placement decision:
// the >= 1 range check on policy.max_tool_depth is a plain parse-time decode
// diagnostic. Unset is valid (engine default of 8); declared values below 1
// are rejected with a diagnostic. CRI-157 owns graph-level wiring only.
func TestParseTools_MaxToolDepthRangeCheck(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "unset", value: "", wantErr: false},
		{name: "one", value: "1", wantErr: false},
		{name: "large", value: "64", wantErr: false},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var src strings.Builder
			src.WriteString(`
workflow {
  name          = "max-tool-depth"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
`)
			if tt.value != "" {
				src.WriteString("\n  policy {\n    max_tool_depth = " + tt.value + "\n  }\n")
			}
			src.WriteString(`}

adapter "shell" "worker" {}

step "start" {
  target = adapter.shell.worker
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
			spec, diags := Parse("max_tool_depth.hcl", []byte(src.String()))
			if tt.wantErr {
				if !diags.HasErrors() {
					t.Fatalf("max_tool_depth = %s: want parse error, got none", tt.value)
				}
				if !strings.Contains(diags.Error(), "max_tool_depth must be an integer >= 1") {
					t.Errorf("unexpected diagnostic: %s", diags.Error())
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("max_tool_depth = %s: unexpected parse error: %s", tt.value, diags.Error())
			}
			if tt.value != "" {
				want, ok := map[string]int{"1": 1, "64": 64}[tt.value]
				if !ok {
					t.Fatalf("missing expectation for %q", tt.value)
				}
				if got := spec.Header.Policy.MaxToolDepth; got != want {
					t.Errorf("MaxToolDepth = %d, want %d", got, want)
				}
			}
		})
	}
}

// TestParseTools_NonTraversalEntriesDeferred pins the CRI-155 "no validation"
// boundary: entries in a step `tools` list that are not bare traversals (and
// non-list values) parse without error and yield no grants. Shape diagnostics
// with positions land in CRI-156, which re-walks the step Remain bodies.
func TestParseTools_NonTraversalEntriesDeferred(t *testing.T) {
	tests := []struct {
		name  string
		tools string
	}{
		{name: "string literal entry", tools: `tools = ["adapter.mcp.registry.tools"]`},
		{name: "function call entry", tools: `tools = [lower("adapter.mcp.registry.tools")]`},
		{name: "non-list value", tools: `tools = "adapter.mcp.registry.tools"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(`
workflow {
  name          = "non-traversal-tools"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

adapter "mcp" "registry" {}

step "start" {
  target = adapter.mcp.registry
  ` + tt.tools + `
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
			spec, diags := Parse("non_traversal_tools.hcl", src)
			if diags.HasErrors() {
				t.Fatalf("parse: %s", diags.Error())
			}
			if len(spec.Steps) != 1 {
				t.Fatalf("got %d steps, want 1", len(spec.Steps))
			}
			if len(spec.Steps[0].Tools) != 0 {
				t.Errorf("StepSpec.Tools = %v, want none (deferred to CRI-156)", toolRefStrings(t, spec.Steps[0].Tools))
			}
		})
	}
}

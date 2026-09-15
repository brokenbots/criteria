package workflow

// compile_steps_tools_test.go — golden diagnostics for compile-time tool-ref
// validation (CRI-156). One test per failure mode, each asserting the exact
// diagnostic count, severity, summary/detail wording, and source position.
//
// Severity note: hcl v2 exposes only DiagError and DiagWarning. The ticket's
// "info" severity for the bare-tools advisory (mode 7) is realized as
// DiagWarning with advisory wording (see compile_steps_tools.go); the tests
// below pin that realization so a future severity model change is deliberate.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// toolsWorkflowSrc renders a minimal workflow around one adapter declaration
// and one adapter-targeted step. adapterDecl is the adapter body ("" for a
// surface-less adapter); toolsAttr is the step's `tools  = [...]` attribute
// text ("" for none).
func toolsWorkflowSrc(adapterDecl, toolsAttr string) string {
	var sb strings.Builder
	sb.WriteString(`
workflow {
  name          = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "mcp" "registry" {
`)
	if adapterDecl != "" {
		sb.WriteString(adapterDecl + "\n")
	}
	sb.WriteString(`}

step "run" {
  target = adapter.mcp.registry
`)
	if toolsAttr != "" {
		sb.WriteString(toolsAttr + "\n")
	}
	sb.WriteString(`  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`)
	return sb.String()
}

// toolsCallerSchemas returns schemas registering a "bot" adapter type, with
// the adapter_tools capability only when withCapability is true. The input
// schema is empty so steps without input blocks compile cleanly.
func toolsCallerSchemas(withCapability bool) map[string]AdapterInfo {
	caps := []string{}
	if withCapability {
		caps = []string{"adapter_tools"}
	}
	return map[string]AdapterInfo{
		"bot": {InputSchema: map[string]ConfigField{}, Capabilities: caps},
	}
}

// compileToolsWorkflow parses and compiles a workflow source with the given
// schemas, failing the test on parse errors.
func compileToolsWorkflow(t *testing.T, src string, schemas map[string]AdapterInfo) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, schemas)
	return g, diags
}

// lineOf returns the 1-based line number of the first source line containing
// needle, failing the test when absent.
func lineOf(t *testing.T, src, needle string) int {
	t.Helper()
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	t.Fatalf("marker %q not found in source", needle)
	return 0
}

// expectToolDiag asserts the diagnostics contain exactly one diagnostic with
// the given severity and a summary containing summarySub; it also asserts the
// subject's line matches the line containing marker (when marker is
// non-empty) and that the diagnostic carries a source position.
func expectToolDiag(t *testing.T, diags hcl.Diagnostics, severity hcl.DiagnosticSeverity, summarySub, src, marker string) {
	t.Helper()
	var found *hcl.Diagnostic
	for i := range diags {
		d := diags[i]
		if strings.Contains(d.Summary, summarySub) {
			if found != nil {
				t.Fatalf("expected exactly one %q diagnostic, got multiple: %s", summarySub, diags.Error())
			}
			found = d
		}
	}
	if found == nil {
		t.Fatalf("expected a diagnostic with summary containing %q, got: %s", summarySub, diags.Error())
	}
	if found.Severity != severity {
		t.Errorf("severity = %v, want %v (diags: %s)", found.Severity, severity, diags.Error())
	}
	if marker != "" {
		if found.Subject == nil {
			t.Fatalf("diagnostic %q has no source position", summarySub)
		}
		want := lineOf(t, src, marker)
		if found.Subject.Start.Line != want {
			t.Errorf("diagnostic %q at line %d, want line %d (entry %q)", summarySub, found.Subject.Start.Line, want, marker)
		}
	}
}

// TestToolRefMode1_UnknownAdapter verifies a tools entry naming an adapter not
// declared in the workflow produces exactly one positioned error.
func TestToolRefMode1_UnknownAdapter(t *testing.T) {
	src := toolsWorkflowSrc(`tool "search" {}`, `tools  = [adapter.ghost.missing.tools]`)
	_, diags := compileToolsWorkflow(t, src, nil)
	expectToolDiag(t, diags, hcl.DiagError, `references adapter "ghost.missing" which is not declared`, src, "adapter.ghost.missing.tools")
	if len(diags) != 1 {
		t.Errorf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
	}
	// Representative position check: the diagnostic column points at the
	// offending entry's first label.
	line := strings.Split(src, "\n")[lineOf(t, src, "adapter.ghost.missing.tools")-1]
	wantCol := strings.Index(line, "adapter.ghost.missing.tools") + 1
	for _, d := range diags {
		if d.Subject != nil && d.Subject.Start.Column != wantCol {
			t.Errorf("column = %d, want %d", d.Subject.Start.Column, wantCol)
		}
	}
}

// TestToolRefMode1_MalformedShape verifies entries that are not the shape
// adapter.<type>.<name>.tools[.<tool>] produce exactly one malformed-entry
// error each, at the entry's position.
func TestToolRefMode1_MalformedShape(t *testing.T) {
	cases := map[string]struct {
		toolsAttr string
		summary   string
	}{
		"three labels":        {`tools  = [adapter.mcp.registry]`, "malformed tools entry"},
		"six labels":          {`tools  = [adapter.mcp.registry.tools.search.extra]`, "malformed tools entry"},
		"first label":         {`tools  = [target.mcp.registry.tools]`, "malformed tools entry"},
		"fourth label":        {`tools  = [adapter.mcp.registry.wat]`, "malformed tools entry"},
		"string literal":      {`tools  = ["git_status"]`, "tools entry is not a bare traversal"},
		"index expression":    {`tools  = [adapter.mcp.registry.tools[0]]`, "malformed tools entry"},
		"tools not a list":    {`tools  = adapter.mcp.registry.tools`, "tools must be a list of tool references"},
		"three labels reword": {`tools  = [tools.mcp.registry]`, "malformed tools entry"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src := toolsWorkflowSrc(`tool "search" {}`, tc.toolsAttr)
			_, diags := compileToolsWorkflow(t, src, nil)
			expectToolDiag(t, diags, hcl.DiagError, tc.summary, src, tc.toolsAttr)
			if len(diags) != 1 {
				t.Errorf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
			}
		})
	}
}

// TestToolRefMode2_UnknownStaticTool verifies a named ref against a callee
// with static tool blocks that does not declare the tool produces exactly one
// error whose detail lists the declared tools.
func TestToolRefMode2_UnknownStaticTool(t *testing.T) {
	src := toolsWorkflowSrc(`tool "search" {}`, `tools  = [adapter.mcp.registry.tools.git]`)
	_, diags := compileToolsWorkflow(t, src, nil)
	expectToolDiag(t, diags, hcl.DiagError, `references unknown tool "adapter.mcp.registry.tools.git"`, src, "adapter.mcp.registry.tools.git")
	if len(diags) != 1 {
		t.Errorf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
	}
	for _, d := range diags {
		if !strings.Contains(d.Detail, "search") {
			t.Errorf("expected detail to list declared tools, got: %s", d.Detail)
		}
	}
}

// TestToolRefMode2_BareRefSkipsNameCheck verifies a bare .tools ref on a
// callee with static tool blocks compiles with no diagnostics (all-tools
// surface).
func TestToolRefMode2_BareRefSkipsNameCheck(t *testing.T) {
	src := toolsWorkflowSrc(`tool "search" {}`, `tools  = [adapter.mcp.registry.tools]`)
	_, diags := compileToolsWorkflow(t, src, nil)
	if len(diags) != 0 {
		t.Errorf("expected clean compile, got: %s", diags.Error())
	}
}

// TestToolRefMode3_DynamicToolsLenient verifies adapters declaring
// dynamic_tools = true accept any tool name and emit nothing.
func TestToolRefMode3_DynamicToolsLenient(t *testing.T) {
	for name, toolsAttr := range map[string]string{
		"named unknown tool": `tools  = [adapter.mcp.registry.tools.anything]`,
		"bare ref":           `tools  = [adapter.mcp.registry.tools]`,
	} {
		t.Run(name, func(t *testing.T) {
			src := toolsWorkflowSrc(`dynamic_tools = true`, toolsAttr)
			_, diags := compileToolsWorkflow(t, src, nil)
			if len(diags) != 0 {
				t.Errorf("expected no diagnostics for dynamic_tools adapter, got: %s", diags.Error())
			}
		})
	}
}

// TestToolRefMode4_NoToolSurface verifies a named ref against an adapter that
// declares neither tool blocks nor dynamic_tools produces exactly one
// "presents no tool surface" error.
func TestToolRefMode4_NoToolSurface(t *testing.T) {
	src := toolsWorkflowSrc(``, `tools  = [adapter.mcp.registry.tools.search]`)
	_, diags := compileToolsWorkflow(t, src, nil)
	expectToolDiag(t, diags, hcl.DiagError, `callee "mcp.registry" presents no tool surface`, src, "adapter.mcp.registry.tools.search")
	if len(diags) != 1 {
		t.Errorf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
	}
}

// addCallerAdapter inserts a "bot" adapter declaration into a workflow source
// and repoints the step's target at it, so the step's caller adapter type is
// resolved from schemas instead of the declared callee.
func addCallerAdapter(t *testing.T, src string) string {
	t.Helper()
	if !strings.Contains(src, `adapter "bot" "default" {}`) {
		src = strings.Replace(src, "step \"run\" {", "adapter \"bot\" \"default\" {}\n\nstep \"run\" {", 1)
	}
	return strings.Replace(src, `target = adapter.mcp.registry`, `target = adapter.bot.default`, 1)
}

// TestToolRefMode5_PointlessCaller verifies a step whose caller adapter lacks
// the adapter_tools capability gets exactly one pointless-tools warning and
// compilation proceeds; a caller with the capability gets none.
func TestToolRefMode5_PointlessCaller(t *testing.T) {
	t.Run("no capability", func(t *testing.T) {
		src := addCallerAdapter(t, toolsWorkflowSrc(`tool "search" {}`, `tools  = [adapter.mcp.registry.tools.search]`))
		g, diags := compileToolsWorkflow(t, src, toolsCallerSchemas(false))
		expectToolDiag(t, diags, hcl.DiagWarning, "passing tools is pointless", src, "tools  = [")
		if diags.HasErrors() {
			t.Errorf("pointless-caller warning must not fail compilation: %s", diags.Error())
		}
		if _, ok := g.Steps["run"]; !ok {
			t.Error("step must be registered despite the pointless-caller warning")
		}
	})
	t.Run("with capability", func(t *testing.T) {
		src := addCallerAdapter(t, toolsWorkflowSrc(`tool "search" {}`, `tools  = [adapter.mcp.registry.tools.search]`))
		_, diags := compileToolsWorkflow(t, src, toolsCallerSchemas(true))
		if len(diags) != 0 {
			t.Errorf("expected no diagnostics for capable caller, got: %s", diags.Error())
		}
	})
	t.Run("no tools list", func(t *testing.T) {
		src := addCallerAdapter(t, toolsWorkflowSrc(`tool "search" {}`, ""))
		_, diags := compileToolsWorkflow(t, src, toolsCallerSchemas(false))
		if len(diags) != 0 {
			t.Errorf("expected no diagnostics without a tools list, got: %s", diags.Error())
		}
	})
}

// TestToolRefMode6_DuplicateEntry verifies a target repeated in one list
// produces exactly one duplicate-entry warning at the second occurrence.
func TestToolRefMode6_DuplicateEntry(t *testing.T) {
	entry := "adapter.mcp.registry.tools.search"
	src := toolsWorkflowSrc(`tool "search" {}`, "tools  = [\n    "+entry+",\n    "+entry+",\n  ]")
	_, diags := compileToolsWorkflow(t, src, nil)
	expectToolDiag(t, diags, hcl.DiagWarning, `duplicate tools entry "adapter.mcp.registry.tools.search"`, src, "")
	if len(diags) != 1 {
		t.Errorf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
	}
	if diags.HasErrors() {
		t.Errorf("duplicate entry must be a warning, not an error: %s", diags.Error())
	}
	// The duplicate's diagnostic points at the second occurrence: the line
	// immediately after the first (both occurrences carry the same text).
	wantLine := lineOf(t, src, entry) + 1
	if diags[0].Subject == nil || diags[0].Subject.Start.Line != wantLine {
		t.Errorf("duplicate diagnostic line = %v, want %d", diags[0].Subject, wantLine)
	}
}

// TestToolRefMode7_BareRefAdvisory verifies a bare .tools ref on an adapter
// with no declared surface produces exactly one advisory diagnostic and
// compilation proceeds. The advisory ("info") posture is realized as
// hcl.DiagWarning because hcl has no info severity; this test pins that
// realization.
func TestToolRefMode7_BareRefAdvisory(t *testing.T) {
	src := toolsWorkflowSrc(``, `tools  = [adapter.mcp.registry.tools]`)
	_, diags := compileToolsWorkflow(t, src, nil)
	expectToolDiag(t, diags, hcl.DiagWarning, `bare tools reference "adapter.mcp.registry.tools" on an adapter with no declared tool surface`, src, "adapter.mcp.registry.tools")
	if len(diags) != 1 {
		t.Errorf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
	}
	if diags.HasErrors() {
		t.Errorf("bare-ref advisory must not fail compilation: %s", diags.Error())
	}
	for _, d := range diags {
		if !strings.Contains(d.Detail, "validated at runtime") {
			t.Errorf("expected advisory detail to carry the runtime-validation posture, got: %s", d.Detail)
		}
	}
}

// TestToolRefMode8_AllowToolsInterplay verifies tool refs compile without a
// matching allow_tools entry and that an allow_tools entry shaped like a tool
// target still gets the ordinary vocabulary warning, not a special case.
func TestToolRefMode8_AllowToolsInterplay(t *testing.T) {
	t.Run("no allow_tools required", func(t *testing.T) {
		src := toolsWorkflowSrc(`tool "search" {}`, `tools  = [adapter.mcp.registry.tools.search]`)
		_, diags := compileToolsWorkflow(t, src, nil)
		if len(diags) != 0 {
			t.Errorf("tool refs must not require a matching allow_tools entry, got: %s", diags.Error())
		}
	})
	t.Run("vocabulary warning unchanged", func(t *testing.T) {
		schemas := map[string]AdapterInfo{
			"mcp": {
				InputSchema: map[string]ConfigField{},
				Permissions: []string{"search"},
			},
		}
		src := toolsWorkflowSrc(`tool "search" {}`, `allow_tools = ["adapter.mcp.registry.tools"]`)
		_, diags := compileToolsWorkflow(t, src, schemas)
		// The vocabulary warning is the pre-existing check's output; it carries
		// no source position today, so no position is asserted here.
		expectToolDiag(t, diags, hcl.DiagWarning, "not declared in the adapter's permissions vocabulary", src, "")
		if len(diags) != 1 {
			t.Errorf("expected exactly the vocabulary warning, got %d: %s", len(diags), diags.Error())
		}
		if diags.HasErrors() {
			t.Errorf("tool-shaped allow_tools entry must not be special-cased into an error: %s", diags.Error())
		}
	})
}

// TestToolRefIteratingStep verifies tool refs are validated on iterating steps
// (for_each): a bad entry errors and a capability-less caller is warned.
func TestToolRefIteratingStep(t *testing.T) {
	base := `
workflow {
  name          = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "mcp" "registry" {
  tool "search" {}
}

step "run" {
  target = adapter.bot.default
  for_each = ["alpha", "beta"]
` + "%s" + `  outcome "all_succeeded" { next = step.done }
  outcome "any_failed"    { next = step.done }
}

state "done" { terminal = true }
`
	t.Run("unknown tool errors", func(t *testing.T) {
		src := strings.Replace(base, "%s", `tools  = [adapter.mcp.registry.tools.git]`+"\n", 1)
		src = strings.Replace(src, `target = adapter.bot.default`, `target = adapter.mcp.registry`, 1)
		_, diags := compileToolsWorkflow(t, src, nil)
		expectToolDiag(t, diags, hcl.DiagError, `references unknown tool "adapter.mcp.registry.tools.git"`, src, "adapter.mcp.registry.tools.git")
	})
	t.Run("pointless caller warns", func(t *testing.T) {
		src := addCallerAdapter(t, strings.Replace(base, "%s", `tools  = [adapter.mcp.registry.tools.search]`+"\n", 1))
		_, diags := compileToolsWorkflow(t, src, toolsCallerSchemas(false))
		expectToolDiag(t, diags, hcl.DiagWarning, "passing tools is pointless", src, "tools  = [")
		if diags.HasErrors() {
			t.Errorf("pointless-caller warning must not fail compilation: %s", diags.Error())
		}
	})
}

// TestToolRefSubworkflowStep verifies tool refs are validated on
// subworkflow-targeted steps (shape + resolution) without the pointless-caller
// warning, which does not apply to steps with no caller adapter.
func TestToolRefSubworkflowStep(t *testing.T) {
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
	base := `
workflow {
  name          = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "mcp" "registry" {
  tool "search" {}
}

subworkflow "child" { source = "./child" }

step "run" {
  target = subworkflow.child
` + "%s" + `  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`
	compileSub := func(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
		t.Helper()
		spec, diags := Parse("t.hcl", []byte(src))
		if diags.HasErrors() {
			t.Fatalf("parse: %s", diags.Error())
		}
		return CompileWithOpts(spec, nil, CompileOpts{
			WorkflowDir:         dir,
			SubWorkflowResolver: &LocalSubWorkflowResolver{AllowedRoots: []string{dir}},
		})
	}
	t.Run("valid entry clean", func(t *testing.T) {
		src := strings.Replace(base, "%s", `tools  = [adapter.mcp.registry.tools.search]`+"\n", 1)
		_, diags := compileSub(t, src)
		if len(diags) != 0 {
			t.Errorf("expected no diagnostics for valid subworkflow-step tools, got: %s", diags.Error())
		}
	})
	t.Run("unknown adapter errors", func(t *testing.T) {
		src := strings.Replace(base, "%s", `tools  = [adapter.ghost.missing.tools]`+"\n", 1)
		_, diags := compileSub(t, src)
		expectToolDiag(t, diags, hcl.DiagError, `references adapter "ghost.missing" which is not declared`, src, "adapter.ghost.missing.tools")
	})
}

// TestToolRefFixturesCompileClean walks every CRI-155 fixture under
// testdata/tools and asserts the compile pass emits no diagnostics: all
// fixtures are valid under the tool-ref validator.
func TestToolRefFixturesCompileClean(t *testing.T) {
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
			spec, diags := ParseDir(filepath.Join(fixtureDir, entry.Name()))
			if diags.HasErrors() {
				t.Fatalf("parse: %s", diags.Error())
			}
			_, diags = Compile(spec, nil)
			if len(diags) != 0 {
				t.Errorf("expected a clean compile for fixture %s, got: %s", entry.Name(), diags.Error())
			}
		})
	}
}

// TestLegacyWorkflowWithoutToolsNoDiagnostics verifies workflows without tool
// refs compile with no diagnostics at all — the diagnostic set for
// tool-ref-free workflows is byte-identical to before this change.
func TestLegacyWorkflowWithoutToolsNoToolDiags(t *testing.T) {
	src := toolsWorkflowSrc(`tool "search" {}`, "")
	_, diags := compileToolsWorkflow(t, src, nil)
	if len(diags) != 0 {
		t.Errorf("workflow without tools must compile without diagnostics, got: %s", diags.Error())
	}
}

// toolSourceSchemas returns schemas registering the "mcp" adapter type with
// the given runtime tool names (the handshake's InfoResponse.tools) and the
// adapter_tools capability, so a self-targeted step does not trip the
// pointless-caller warning. Used by the CRI-173 tool-source precedence tests.
func toolSourceSchemas(runtimeTools ...string) map[string]AdapterInfo {
	return map[string]AdapterInfo{
		"mcp": {
			InputSchema:  map[string]ConfigField{},
			Capabilities: []string{adapterToolsCapability},
			RuntimeTools: runtimeTools,
		},
	}
}

// TestToolRefToolSourcePrecedence pins the CRI-173 tool-source precedence for
// all four tool-source combinations: static tool blocks, dynamic_tools = true,
// InfoResponse.tools-exposed runtime tools, and neither. The ladder is
//
//	static tool blocks > dynamic_tools (skip check) >
//	InfoResponse.tools (check when present) > neither (reject refs)
//
// and each rung's diagnostics are asserted exactly (golden).
func TestToolRefToolSourcePrecedence(t *testing.T) {
	// compileCase compiles one tools attribute against one adapter
	// declaration with the given runtime tools, and asserts either a clean
	// compile or exactly one diagnostic matching severity + summary/detail
	// substrings.
	compileCase := func(t *testing.T, adapterDecl, toolsAttr string, runtimeTools []string, wantClean bool, severity hcl.DiagnosticSeverity, summarySub, detailSub string) {
		t.Helper()
		src := toolsWorkflowSrc(adapterDecl, toolsAttr)
		_, diags := compileToolsWorkflow(t, src, toolSourceSchemas(runtimeTools...))
		if wantClean {
			if len(diags) != 0 {
				t.Fatalf("expected clean compile, got %d diagnostic(s): %s", len(diags), diags.Error())
			}
			return
		}
		expectToolDiag(t, diags, severity, summarySub, src, toolsAttr)
		if len(diags) != 1 {
			t.Fatalf("expected exactly one diagnostic, got %d: %s", len(diags), diags.Error())
		}
		if detailSub != "" && !strings.Contains(diags[0].Detail, detailSub) {
			t.Errorf("diagnostic detail %q does not contain %q", diags[0].Detail, detailSub)
		}
	}

	t.Run("static tool blocks", func(t *testing.T) {
		t.Run("declared name compiles", func(t *testing.T) {
			compileCase(t, `tool "search" {}`, `tools  = [adapter.mcp.registry.tools.search]`, []string{"fetch"}, true, 0, "", "")
		})
		t.Run("unknown name errors even when handshake reports tools", func(t *testing.T) {
			// Static blocks outrank InfoResponse.tools: the name must resolve
			// against the static blocks, and the runtime-only "fetch" is not
			// consulted.
			compileCase(t, `tool "search" {}`, `tools  = [adapter.mcp.registry.tools.fetch]`, []string{"fetch"},
				false, hcl.DiagError, `references unknown tool "adapter.mcp.registry.tools.fetch"`, "declares tools search")
		})
	})

	t.Run("dynamic_tools skips the check", func(t *testing.T) {
		t.Run("unknown name compiles even when handshake reports tools", func(t *testing.T) {
			compileCase(t, `dynamic_tools = true`, `tools  = [adapter.mcp.registry.tools.anything]`, []string{"search"}, true, 0, "", "")
		})
		t.Run("bare ref compiles", func(t *testing.T) {
			compileCase(t, `dynamic_tools = true`, `tools  = [adapter.mcp.registry.tools]`, []string{"search"}, true, 0, "", "")
		})
		t.Run("static blocks win over dynamic flag", func(t *testing.T) {
			compileCase(t, "tool \"search\" {}\n  dynamic_tools = true", `tools  = [adapter.mcp.registry.tools.search]`, []string{"other"}, true, 0, "", "")
			compileCase(t, "tool \"search\" {}\n  dynamic_tools = true", `tools  = [adapter.mcp.registry.tools.runtime_only]`, []string{"runtime_only"},
				false, hcl.DiagError, `references unknown tool "adapter.mcp.registry.tools.runtime_only"`, "declares tools search")
		})
	})

	t.Run("InfoResponse.tools runtime surface", func(t *testing.T) {
		t.Run("reported name compiles", func(t *testing.T) {
			compileCase(t, ``, `tools  = [adapter.mcp.registry.tools.search]`, []string{"search", "fetch"}, true, 0, "", "")
		})
		t.Run("unreported name fails the check", func(t *testing.T) {
			compileCase(t, ``, `tools  = [adapter.mcp.registry.tools.git]`, []string{"search", "fetch"},
				false, hcl.DiagError, `references unknown tool "adapter.mcp.registry.tools.git"`, "InfoResponse.tools: search, fetch")
		})
		t.Run("bare ref grants the known runtime surface", func(t *testing.T) {
			compileCase(t, ``, `tools  = [adapter.mcp.registry.tools]`, []string{"search"}, true, 0, "", "")
		})
	})

	t.Run("neither source rejects refs", func(t *testing.T) {
		t.Run("named ref errors when handshake exposes no tools", func(t *testing.T) {
			compileCase(t, ``, `tools  = [adapter.mcp.registry.tools.search]`, nil,
				false, hcl.DiagError, `callee "mcp.registry" presents no tool surface`, "")
		})
		t.Run("named ref errors when the adapter type is registered but exposes no tools", func(t *testing.T) {
			compileCase(t, ``, `tools  = [adapter.mcp.registry.tools.search]`, []string{},
				false, hcl.DiagError, `callee "mcp.registry" presents no tool surface`, "adapter handshake exposes no tools")
		})
		t.Run("bare ref stays advisory when the adapter type is registered but exposes no tools", func(t *testing.T) {
			compileCase(t, ``, `tools  = [adapter.mcp.registry.tools]`, []string{},
				false, hcl.DiagWarning, `bare tools reference "adapter.mcp.registry.tools" on an adapter with no declared tool surface`, "")
		})
	})
}

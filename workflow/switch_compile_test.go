package workflow_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/workflow"
)

// injectDefaultAdapters automatically adds adapter declarations for any bare adapter type
// references in the HCL, and updates all references to use the dotted "<type>.default" form.
// This is a test helper to reduce boilerplate since most tests use simple single-adapter workflows.
func injectDefaultAdapters(src string) string {
	// Collect unique adapter type names from target = adapter.... references
	adapters := make(map[string]bool)
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		// Match "target = adapter.<type>" (bare, 2-segment reference without instance name)
		if strings.HasPrefix(trimmed, `target`) && strings.Contains(trimmed, `=`) && strings.Contains(trimmed, `adapter.`) {
			afterEq := strings.TrimSpace(trimmed[strings.Index(trimmed, "=")+1:])
			if strings.HasPrefix(afterEq, "adapter.") {
				parts := strings.FieldsFunc(strings.TrimPrefix(afterEq, "adapter."), func(c rune) bool {
					return c == '.' || c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '#'
				})
				if len(parts) == 1 {
					// Bare type: target = adapter.fake (no instance name)
					adapters[parts[0]] = true
				}
			}
		}
	}

	if len(adapters) == 0 {
		return src
	}

	// Build adapter declarations to inject
	var injected strings.Builder
	for adapterType := range adapters {
		//nolint:gocritic // sprintfQuotedString: Sprintf needed to build HCL with literal quotes
		injected.WriteString(fmt.Sprintf("  adapter \"%s\" \"default\" {}\n", adapterType))
	}

	// Insert the adapters after the workflow header
	workflowStart := strings.Index(src, "workflow \"")
	if workflowStart == -1 {
		return src
	}
	headerEnd := strings.Index(src[workflowStart:], "\n") + workflowStart + 1

	result := src[:headerEnd] + "\n" + injected.String() + src[headerEnd:]

	// Replace all bare adapter references with dotted references
	for adapterType := range adapters {
		pattern := regexp.MustCompile(fmt.Sprintf(`target\s*=\s*adapter\.%s\b`, regexp.QuoteMeta(adapterType)))
		replacement := fmt.Sprintf(`target = adapter.%s.default`, adapterType)
		result = pattern.ReplaceAllString(result, replacement)
	}

	return result
}

// parseAndCompile is a test helper that parses src and compiles it.
func parseAndCompile(t *testing.T, src string) (*workflow.FSMGraph, error) {
	t.Helper()
	src = injectDefaultAdapters(src)
	spec, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		return nil, diags
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		return nil, diags
	}
	return g, nil
}

// mustParseAndCompile fails the test if parsing or compilation produces errors.
func mustParseAndCompile(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	g, err := parseAndCompile(t, src)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	return g
}

// compileExpectError asserts that compilation of src produces an error
// containing the given substring.
func compileExpectError(t *testing.T, src, want string) {
	t.Helper()
	src = injectDefaultAdapters(src)
	spec, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		// Parse errors are fine for this helper if they mention want.
		if !strings.Contains(diags.Error(), want) {
			t.Fatalf("parse error %q does not contain %q", diags.Error(), want)
		}
		return
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatalf("expected compile error containing %q, got none", want)
	}
	if !strings.Contains(diags.Error(), want) {
		t.Fatalf("compile error %q does not contain %q", diags.Error(), want)
	}
}

func TestSwitchCompile_HappyPath(t *testing.T) {
	// Note: "prev" is unreachable from "check" (initial_state), but "check" is
	// the initial state itself. Build a reachable workflow.
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

variable "env" {
  type    = "string"
  default = "staging"
}

switch "check" {
  condition {
    match = var.env == "prod"
    next  = state.deploy
  }
  condition {
    match = var.env == "staging"
    next  = state.deploy_staging
  }
  default {
    next = state.done
  }
}

state "deploy"         { terminal = true }
state "deploy_staging" { terminal = true }
state "done"           { terminal = true }
`
	g := mustParseAndCompile(t, src)
	sw, ok := g.Switches["check"]
	if !ok {
		t.Fatal("switch node 'check' missing from compiled graph")
	}
	if len(sw.Conditions) != 2 {
		t.Errorf("expected 2 conditions, got %d", len(sw.Conditions))
	}
	if sw.DefaultNext != "done" {
		t.Errorf("expected default target 'done', got %q", sw.DefaultNext)
	}
}

func TestSwitchCompile_MissingDefault(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match = var.env == "prod"
    next  = state.done
  }
}

variable "env" {
  type = "string"
}

state "done" { terminal = true }
`
	src = injectDefaultAdapters(src)
	spec, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	_, diags = workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("expected warning only, got error: %v", diags)
	}
	hasWarn := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "default block is required") {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Errorf("expected warning about missing default block; got diags: %v", diags)
	}
}

func TestSwitchCompile_UnknownArmTarget(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match = true
    next  = "nonexistent"
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	compileExpectError(t, src, `unknown target "nonexistent"`)
}

func TestSwitchCompile_UnknownDefaultTarget(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match = true
    next  = state.done
  }
  default {
    next = "missing"
  }
}

state "done" { terminal = true }
`
	compileExpectError(t, src, `unknown target "missing"`)
}

func TestSwitchCompile_UndeclaredVariable(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match = var.undeclared == "x"
    next  = state.done
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	compileExpectError(t, src, `undefined variable "undeclared"`)
}

func TestSwitchCompile_UnknownStepReference(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match = steps.ghoststep.exit_code == "0"
    next  = state.done
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	compileExpectError(t, src, `unknown step "ghoststep"`)
}

func TestSwitchCompile_SelfReferenceRejected(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

adapter "noop" "default" {}

switch "check" {
  condition {
    match = steps.check.exit_code == "0"
    next  = state.done
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	compileExpectError(t, src, `self-reference steps.check is always empty at match time`)
}

func TestSwitchCompile_UnreachableSwitchWarns(t *testing.T) {
	// The switch node is not reachable from initial_state (a step is initial).
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "default" {}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

switch "orphan" {
  condition {
    match = true
    next  = state.done
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	spec, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	_, diags = workflow.Compile(spec, nil)
	// Should produce a warning, not an error.
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %v", diags)
	}
	hasWarn := false
	for _, d := range diags {
		if strings.Contains(d.Summary, "unreachable") && strings.Contains(d.Summary, "orphan") {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Error("expected unreachability warning for 'orphan' switch, got none")
	}
}

func TestSwitchCompile_DuplicateSwitch(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  default { next = state.done }
}

switch "check" {
  default { next = state.done }
}

state "done" { terminal = true }
`
	compileExpectError(t, src, `duplicate switch "check"`)
}

func TestSwitchCompile_FixtureFile(t *testing.T) {
	spec, diags := workflow.ParseFile("testdata/switch_basic.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse fixture: %v", diags)
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile fixture: %v", diags)
	}
	if _, ok := g.Switches["decide"]; !ok {
		t.Error("switch 'decide' missing from compiled fixture graph")
	}
}

// TestSwitchCompile_LegacyBranchBlock_HardError ensures the old "branch" block
// is hard-rejected at parse time.
func TestSwitchCompile_LegacyBranchBlock_HardError(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

branch "check" {
  arm {
    when          = true
    transition_to = "done"
  }
  default {
    transition_to = "done"
  }
}

state "done" { terminal = true }
`
	_, diags := workflow.Parse("test.hcl", []byte(src))
	if !diags.HasErrors() {
		t.Fatal("expected parse error for legacy 'branch' block, got none")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, "branch") || strings.Contains(d.Detail, "branch") ||
			strings.Contains(d.Detail, "switch") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error mentioning 'branch' or 'switch', got: %v", diags)
	}
}

// TestCompileSwitch_NextIsReturn verifies that next = "return" is accepted by
// the compiler and stored as the condition's Next target.
func TestCompileSwitch_NextIsReturn(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match = true
    next  = "return"
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	g := mustParseAndCompile(t, src)
	sw, ok := g.Switches["check"]
	if !ok {
		t.Fatal("switch node 'check' missing from compiled graph")
	}
	if len(sw.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(sw.Conditions))
	}
	if sw.Conditions[0].Next != "return" {
		t.Errorf("Conditions[0].Next = %q, want \"return\"", sw.Conditions[0].Next)
	}
}

// TestCompileSwitch_LegacyTransitionToOnArm_HardError ensures that using
// the old "transition_to" attribute inside a condition block is rejected.
func TestCompileSwitch_LegacyTransitionToOnArm_HardError(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match         = true
    next          = state.done
    transition_to = "done"
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	compileExpectError(t, src, `unknown attribute`)
}

// TestCompileSwitch_OutputExprFolds verifies compile-time output expression
// validation: a bare string is rejected (not an object), and an object literal
// is accepted.
func TestCompileSwitch_OutputExprFolds(t *testing.T) {
	bad := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match  = true
    next   = state.done
    output = "oops"
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	compileExpectError(t, bad, `output must be an object literal`)

	good := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

switch "check" {
  condition {
    match  = true
    next   = state.done
    output = { tier = "prod" }
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	mustParseAndCompile(t, good)
}

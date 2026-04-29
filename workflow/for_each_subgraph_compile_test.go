package workflow_test

// for_each_subgraph_compile_test.go — compile-time tests for the W08 iteration
// subgraph feature. Tests 1–8 from the workstream specification.

import (
	"os"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// parseFile reads a testdata fixture file and compiles it.
func parseFileAndCompile(t *testing.T, path string) (*workflow.FSMGraph, error) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	spec, diags := workflow.Parse(path, src)
	if diags.HasErrors() {
		return nil, diags
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		return nil, diags
	}
	return g, nil
}

// mustParseFile is like mustParseAndCompile but reads from a fixture file.
func mustParseFile(t *testing.T, path string) *workflow.FSMGraph {
	t.Helper()
	g, err := parseFileAndCompile(t, path)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	return g
}

// fileCompileExpectError asserts that compiling a fixture file produces an
// error containing want.
func fileCompileExpectError(t *testing.T, path, want string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	spec, diags := workflow.Parse(path, src)
	if diags.HasErrors() {
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

// --- Test 1: single-step subgraph ---

func TestSubgraph_SingleStep_CompilesAndHasCorrectIterationSteps(t *testing.T) {
	g := mustParseFile(t, "testdata/for_each/single_step.hcl")
	fe, ok := g.ForEachs["loop"]
	if !ok {
		t.Fatal("for_each 'loop' not found in compiled graph")
	}
	if _, ok := fe.IterationSteps["execute"]; !ok {
		t.Errorf("IterationSteps missing 'execute'; got %v", fe.IterationSteps)
	}
	if len(fe.IterationSteps) != 1 {
		t.Errorf("IterationSteps len = %d, want 1; got %v", len(fe.IterationSteps), fe.IterationSteps)
	}
	if g.Steps["execute"].IterationOwner != "loop" {
		t.Errorf("execute.IterationOwner = %q, want %q", g.Steps["execute"].IterationOwner, "loop")
	}
}

// --- Test 2: multi-step subgraph ---

func TestSubgraph_MultiStep_IterationStepsContainsAllThree(t *testing.T) {
	g := mustParseFile(t, "testdata/for_each/multi_step.hcl")
	fe := g.ForEachs["loop"]
	want := map[string]struct{}{"execute": {}, "review": {}, "cleanup": {}}
	for name := range want {
		if _, ok := fe.IterationSteps[name]; !ok {
			t.Errorf("IterationSteps missing %q; got %v", name, fe.IterationSteps)
		}
	}
	if len(fe.IterationSteps) != 3 {
		t.Errorf("IterationSteps len = %d, want 3; got %v", len(fe.IterationSteps), fe.IterationSteps)
	}
	for name := range want {
		if g.Steps[name].IterationOwner != "loop" {
			t.Errorf("step %q IterationOwner = %q, want 'loop'", name, g.Steps[name].IterationOwner)
		}
	}
}

// --- Test 3: branching subgraph ---

func TestSubgraph_Branching_AllThreeStepsInSubgraph(t *testing.T) {
	g := mustParseFile(t, "testdata/for_each/branching.hcl")
	fe := g.ForEachs["loop"]
	for _, name := range []string{"execute", "review", "cleanup"} {
		if _, ok := fe.IterationSteps[name]; !ok {
			t.Errorf("IterationSteps missing %q; got %v", name, fe.IterationSteps)
		}
	}
	if len(fe.IterationSteps) != 3 {
		t.Errorf("IterationSteps len = %d, want 3; got %v", len(fe.IterationSteps), fe.IterationSteps)
	}
}

// --- Test 4: state-only exit (no _continue path) ---

func TestSubgraph_StateOnlyExit_FailsCompile(t *testing.T) {
	fileCompileExpectError(t, "testdata/for_each/state_only_exit.hcl", "no outcome path that reaches")
}

// --- Test 5: overlapping subgraphs ---

func TestSubgraph_Overlap_FailsCompile(t *testing.T) {
	fileCompileExpectError(t, "testdata/for_each/overlapping_subgraphs.hcl", "steps cannot be shared between distinct for_each subgraphs")
}

// --- Test 6: each.* reference outside subgraph ---

func TestSubgraph_EachScopeLeak_FailsCompile(t *testing.T) {
	fileCompileExpectError(t, "testdata/for_each/each_scope_leak.hcl", "references each.*")
}

// --- Test 7: cycle without _continue exit ---

func TestSubgraph_CycleNoExit_FailsCompile(t *testing.T) {
	fileCompileExpectError(t, "testdata/for_each/cycle_no_exit.hcl", "no outcome path that reaches")
}

// --- Test 8: cycle with _continue exit ---

func TestSubgraph_CycleWithExit_Compiles(t *testing.T) {
	g := mustParseFile(t, "testdata/for_each/cycle_with_exit.hcl")
	fe := g.ForEachs["loop"]
	for _, name := range []string{"execute", "review"} {
		if _, ok := fe.IterationSteps[name]; !ok {
			t.Errorf("IterationSteps missing %q; got %v", name, fe.IterationSteps)
		}
	}
	if len(fe.IterationSteps) != 2 {
		t.Errorf("IterationSteps len = %d, want 2; got %v", len(fe.IterationSteps), fe.IterationSteps)
	}
}

// --- Additional inline tests for edge cases ---

// Each.value in a step that IS in the subgraph should compile fine.
func TestSubgraph_EachValueInSubgraph_Compiles(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"

  for_each "loop" {
    items = ["a", "b"]
    do    = "execute"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "execute" {
    adapter = "noop"
    input {
      value = each.value
    }
    outcome "success" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	mustParseAndCompile(t, src)
}

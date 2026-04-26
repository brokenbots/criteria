package workflow_test

import (
	"os"
	"testing"
)

// base for_each workflow template — valid on its own.
const forEachBaseWorkflow = `
workflow "w" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = ["a", "b", "c"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "failed" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`

func TestForEachCompile_HappyPath(t *testing.T) {
	g := mustParseAndCompile(t, forEachBaseWorkflow)
	if _, ok := g.ForEachs["each_item"]; !ok {
		t.Fatal("expected for_each 'each_item' in compiled graph")
	}
	fe := g.ForEachs["each_item"]
	if fe.Do != "process" {
		t.Errorf("ForEachNode.Do = %q, want 'process'", fe.Do)
	}
	if _, ok := fe.Outcomes["all_succeeded"]; !ok {
		t.Error("expected 'all_succeeded' outcome")
	}
	if _, ok := fe.Outcomes["any_failed"]; !ok {
		t.Error("expected 'any_failed' outcome")
	}
}

func TestForEachCompile_LookupKind(t *testing.T) {
	g := mustParseAndCompile(t, forEachBaseWorkflow)
	kind, ok := g.Lookup("each_item")
	if !ok {
		t.Fatal("Lookup returned false for 'each_item'")
	}
	if kind != "for_each" {
		t.Errorf("Lookup kind = %q, want 'for_each'", kind)
	}
}

func TestForEachCompile_DoReferencesUnknownStep(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = ["x"]
    do    = "nonexistent"
    outcome "all_succeeded" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	compileExpectError(t, src, `do = "nonexistent" does not reference a known step`)
}

func TestForEachCompile_MissingAllSucceeded(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = ["x"]
    do    = "process"
    outcome "any_failed" { transition_to = "failed" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`
	compileExpectError(t, src, `outcome "all_succeeded" is required`)
}

func TestForEachCompile_ContinueReservedAsState(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "_continue"

  for_each "each_item" {
    items = ["x"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "_continue" }
    outcome "any_failed"    { transition_to = "_continue" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  state "_continue" {
    terminal = true
    success  = true
  }
}
`
	compileExpectError(t, src, `"_continue" is a reserved engine-internal target`)
}

func TestForEachCompile_ContinueReservedAsStep(t *testing.T) {
	// A plain step named "_continue" with no for_each — avoids spurious warnings.
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "_continue"
  target_state  = "done"

  step "_continue" {
    adapter = "noop"
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	compileExpectError(t, src, `"_continue" is a reserved engine-internal target`)
}

func TestForEachCompile_ContinueReservedAsForEach(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "_continue"
  target_state  = "done"

  for_each "_continue" {
    items = ["x"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	compileExpectError(t, src, `"_continue" is a reserved engine-internal target`)
}

func TestForEachCompile_ClashWithStep(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "done" }
  }

  for_each "process" {
    items = ["x"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	compileExpectError(t, src, `clashes with step`)
}

func TestForEachCompile_DuplicateForEach(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "each_item"
  target_state  = "done"

  for_each "each_item" {
    items = ["x"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  for_each "each_item" {
    items = ["y"]
    do    = "process"
    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "process" {
    adapter = "noop"
    outcome "success" { transition_to = "_continue" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	compileExpectError(t, src, `duplicate for_each`)
}

func TestForEachCompile_Testdata(t *testing.T) {
	src, err := os.ReadFile("testdata/for_each_basic.hcl")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	g := mustParseAndCompile(t, string(src))
	if _, ok := g.ForEachs["deploy_all"]; !ok {
		t.Fatal("expected for_each 'deploy_all' in compiled graph")
	}
}

package workflow_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

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

func TestBranchCompile_HappyPath(t *testing.T) {
	// Note: "prev" is unreachable from "check" (initial_state), but "check" is
	// the initial state itself. Build a reachable workflow.
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  variable "env" {
    type    = "string"
    default = "staging"
  }

  branch "check" {
    arm {
      when          = var.env == "prod"
      transition_to = "deploy"
    }
    arm {
      when          = var.env == "staging"
      transition_to = "deploy_staging"
    }
    default {
      transition_to = "done"
    }
  }

  state "deploy"         { terminal = true }
  state "deploy_staging" { terminal = true }
  state "done"           { terminal = true }
}
`
	g := mustParseAndCompile(t, src)
	br, ok := g.Branches["check"]
	if !ok {
		t.Fatal("branch node 'check' missing from compiled graph")
	}
	if len(br.Arms) != 2 {
		t.Errorf("expected 2 arms, got %d", len(br.Arms))
	}
	if br.DefaultTarget != "done" {
		t.Errorf("expected default target 'done', got %q", br.DefaultTarget)
	}
}

func TestBranchCompile_MissingDefault(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  branch "check" {
    arm {
      when          = true
      transition_to = "done"
    }
  }

  state "done" { terminal = true }
}
`
	compileExpectError(t, src, `default block is required`)
}

func TestBranchCompile_UnknownArmTarget(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  branch "check" {
    arm {
      when          = true
      transition_to = "nonexistent"
    }
    default {
      transition_to = "done"
    }
  }

  state "done" { terminal = true }
}
`
	compileExpectError(t, src, `unknown target "nonexistent"`)
}

func TestBranchCompile_UnknownDefaultTarget(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  branch "check" {
    arm {
      when          = true
      transition_to = "done"
    }
    default {
      transition_to = "missing"
    }
  }

  state "done" { terminal = true }
}
`
	compileExpectError(t, src, `unknown target "missing"`)
}

func TestBranchCompile_UndeclaredVariable(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  branch "check" {
    arm {
      when          = var.undeclared == "x"
      transition_to = "done"
    }
    default {
      transition_to = "done"
    }
  }

  state "done" { terminal = true }
}
`
	compileExpectError(t, src, `undefined variable "undeclared"`)
}

func TestBranchCompile_UnknownStepReference(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  branch "check" {
    arm {
      when          = steps.ghoststep.exit_code == "0"
      transition_to = "done"
    }
    default {
      transition_to = "done"
    }
  }

  state "done" { terminal = true }
}
`
	compileExpectError(t, src, `unknown step "ghoststep"`)
}

func TestBranchCompile_UnreachableBranchWarns(t *testing.T) {
	// The branch node is not reachable from initial_state (a step is initial).
	src := `
workflow "w" {
  adapter "noop" "default" {}
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"

  step "start" {
    target = adapter.noop.default
    outcome "success" { next = "done" }
  }

  branch "orphan" {
    arm {
      when          = true
      transition_to = "done"
    }
    default {
      transition_to = "done"
    }
  }

  state "done" { terminal = true }
}
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
		t.Error("expected unreachability warning for 'orphan' branch, got none")
	}
}

func TestBranchCompile_DuplicateBranch(t *testing.T) {
	src := `
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  branch "check" {
    default { transition_to = "done" }
  }

  branch "check" {
    default { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`
	compileExpectError(t, src, `duplicate branch "check"`)
}

func TestBranchCompile_FixtureFile(t *testing.T) {
	spec, diags := workflow.ParseFile("testdata/branch_basic.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse fixture: %v", diags)
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile fixture: %v", diags)
	}
	if _, ok := g.Branches["decide"]; !ok {
		t.Error("branch 'decide' missing from compiled fixture graph")
	}
}

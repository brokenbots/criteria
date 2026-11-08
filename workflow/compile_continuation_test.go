package workflow

// compile_continuation_test.go — the session-fate (continuation) compile
// validation matrix: the value domain on adapter blocks, and the re-entry
// graph rule (an instance re-declared by both a parent graph and its callee
// must declare one agreeing fate).

import (
	"testing"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/workflow/workflowtest"
)

// continuationCalleeHCL builds a callee workflow HCL declaring the given
// adapter (with the given continuation) plus a terminal state.
func continuationCalleeHCL(name, adapterType, adapterInstance, continuation string) string {
	b := workflowtest.New().
		WithHeader(name, "1", "done", "done").
		WithState("done", true, boolPtr(true))
	if continuation == "" {
		b.WithAdapter(adapterType, adapterInstance)
	} else {
		b.WithAdapterFate(adapterType, adapterInstance, continuation)
	}
	return b.Build()
}

func boolPtr(b bool) *bool { return &b }

// continuationParentHCL builds a parent workflow HCL declaring subworkflow
// swName (source) plus its own adapter and terminal state.
func continuationParentHCL(adapterType, adapterInstance, continuation, swName, source string) string {
	b := workflowtest.New().
		WithHeader("parent", "1", "done", "done").
		WithState("done", true, boolPtr(true))
	if continuation == "" {
		b.WithAdapter(adapterType, adapterInstance)
	} else {
		b.WithAdapterFate(adapterType, adapterInstance, continuation)
	}
	b.WithSubworkflow(swName, source)
	return b.Build()
}

// compileWithAdapters parses and compiles a parent HCL whose subworkflow is
// resolved from disk.
func compileWithAdapters(t *testing.T, parentHCL, parentDir string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	graph, diags := compileParentSpec(t, parentHCL, parentDir)
	return graph, diags.(hcl.Diagnostics)
}

// TestCompileContinuation_ValueDomain pins the continuation value domain on
// adapter declarations: only "reuse" and "fresh" are valid.
func TestCompileContinuation_ValueDomain(t *testing.T) {
	for _, tc := range []struct {
		decl        string
		wantSummary string
	}{
		{`adapter "noop" "default" {}`, ""},
		{`adapter "noop" "default" { continuation = "reuse" }`, ""},
		{`adapter "noop" "default" { continuation = "fresh" }`, ""},
		{`adapter "noop" "default" { continuation = "sometimes" }`,
			`adapter "noop.default": invalid continuation "sometimes"`},
		{`adapter "noop" "default" { continuation = "REUSE" }`,
			`adapter "noop.default": invalid continuation "REUSE"`},
	} {
		b := workflowtest.New().
			WithHeader("fate", "1", "done", "done").
			WithState("done", true, boolPtr(true)).
			WithRaw(tc.decl)
		spec, parseDiags := Parse("fate.hcl", []byte(b.Build()))
		if parseDiags.HasErrors() {
			t.Fatalf("parse failed for %q: %s", tc.decl, parseDiags.Error())
		}
		graph, diags := CompileWithOpts(spec, nil, CompileOpts{})
		if tc.wantSummary == "" {
			if diags.HasErrors() {
				t.Fatalf("decl %q: expected no errors, got %s", tc.decl, diags.Error())
			}
			if graph == nil {
				t.Fatal("expected non-nil graph")
			}
			continue
		}
		requireExactErrorSummary(t, diags, tc.wantSummary)
	}
}

// TestCompileContinuation_AdapterNodeContinuation pins the compiled
// continuation value landing on the AdapterNode.
func TestCompileContinuation_AdapterNodeContinuation(t *testing.T) {
	b := workflowtest.New().
		WithHeader("fate", "1", "done", "done").
		WithState("done", true, boolPtr(true)).
		WithAdapterFate("noop", "default", "reuse").
		WithAdapterFate("shell", "scoped", "fresh")
	spec, parseDiags := Parse("fate.hcl", []byte(b.Build()))
	if parseDiags.HasErrors() {
		t.Fatalf("parse failed: %s", parseDiags.Error())
	}
	graph, diags := CompileWithOpts(spec, nil, CompileOpts{})
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got %s", diags.Error())
	}
	if got := graph.Adapters["noop.default"].Continuation; got != "reuse" {
		t.Errorf("noop.default continuation = %q, want %q", got, "reuse")
	}
	if got := graph.Adapters["shell.scoped"].Continuation; got != "fresh" {
		t.Errorf("shell.scoped continuation = %q, want %q", got, "fresh")
	}
}

// TestCompileContinuation_ReentryValidation is the re-entry graph matrix:
// same adapter instance declared in BOTH parent and callee graph.
func TestCompileContinuation_ReentryValidation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		parentCont  string
		calleeCont  string
		wantSummary string
	}{
		{
			name:        "both undeclared is a hard error",
			parentCont:  "",
			calleeCont:  "",
			wantSummary: `subworkflow "inner_task" re-declares parent adapter "noop.default": both graphs must declare continuation (one of "reuse" or "fresh"); the parent declares "(undeclared)", the callee declares "(undeclared)"`,
		},
		{
			name:        "parent undeclared, callee declared is a hard error",
			parentCont:  "",
			calleeCont:  "reuse",
			wantSummary: `subworkflow "inner_task" re-declares parent adapter "noop.default": both graphs must declare continuation (one of "reuse" or "fresh"); the parent declares "(undeclared)", the callee declares "reuse"`,
		},
		{
			name:        "parent declared, callee undeclared is a hard error",
			parentCont:  "reuse",
			calleeCont:  "",
			wantSummary: `subworkflow "inner_task" re-declares parent adapter "noop.default": both graphs must declare continuation (one of "reuse" or "fresh"); the parent declares "reuse", the callee declares "(undeclared)"`,
		},
		{
			name:        "conflicting declarations are a hard error",
			parentCont:  "reuse",
			calleeCont:  "fresh",
			wantSummary: `subworkflow "inner_task" re-declares parent adapter "noop.default" with conflicting continuation: the parent declares "reuse", the callee declares "fresh"; one instance, one fate`,
		},
		{
			name:       "both reuse compiles",
			parentCont: "reuse",
			calleeCont: "reuse",
		},
		{
			name:       "both fresh compiles",
			parentCont: "fresh",
			calleeCont: "fresh",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			calleeDir := writeSubworkflowDir(t, tmpDir, "inner", continuationCalleeHCL("inner", "noop", "default", tc.calleeCont))
			parentHCL := continuationParentHCL("noop", "default", tc.parentCont, "inner_task", "./inner")
			graph, compileDiags := compileWithAdapters(t, parentHCL, tmpDir)
			if tc.wantSummary != "" {
				requireExactErrorSummary(t, compileDiags, tc.wantSummary)
				return
			}
			if compileDiags.HasErrors() {
				t.Fatalf("expected no errors, got %s", compileDiags.Error())
			}
			calleeGraph := graph.Subworkflows["inner_task"].Body
			if calleeGraph == nil {
				t.Fatal("expected callee graph")
			}
			if got, want := calleeGraph.Adapters["noop.default"].Continuation, tc.calleeCont; got != want {
				t.Errorf("callee continuation = %q, want %q (dir %s)", got, want, calleeDir)
			}
		})
	}
}

// TestCompileContinuation_CalleeOnlyInstanceUnconstrained pins that an
// adapter instance declared only by the callee (no parent re-declaration)
// needs no continuation declaration: an undeclared instance keeps KB-257's
// detach-at-body-exit semantics, and only a DECLARED fresh releases at the
// boundary (the fate policy is declaration-gated, not defaulted).
func TestCompileContinuation_CalleeOnlyInstanceUnconstrained(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", continuationCalleeHCL("inner", "noop", "only_there", ""))
	parentHCL := continuationParentHCL("noop", "default", "", "inner_task", "./inner")
	graph, diags := compileWithAdapters(t, parentHCL, tmpDir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got %s", diags.Error())
	}
	callee := graph.Subworkflows["inner_task"].Body
	if got := callee.Adapters["noop.only_there"].Continuation; got != "" {
		t.Errorf("callee-only instance continuation = %q, want undeclared", got)
	}
}

// TestCompileContinuation_UnknownParentAdapterNotOverlapping pins that the
// re-entry rule keys on the re-declared instance only: a parent adapter with
// a different name than the callee's does not trigger the rule.
func TestCompileContinuation_UnknownParentAdapterNotOverlapping(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", continuationCalleeHCL("inner", "noop", "callee_instance", ""))
	parentHCL := continuationParentHCL("noop", "parent_instance", "reuse", "inner_task", "./inner")
	if _, diags := compileWithAdapters(t, parentHCL, tmpDir); diags.HasErrors() {
		t.Fatalf("expected no errors, got %s", diags.Error())
	}
}

// TestCompileContinuation_DeclaredFateNoOverlapNotRequired pins that a
// re-declaration-free parent adapter keeps its declared value untouched (the
// declaration is honored, not re-validated into an error).
func TestCompileContinuation_DeclaredFateNoOverlapNotRequired(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", continuationCalleeHCL("inner", "noop", "callee_instance", ""))
	parentHCL := continuationParentHCL("noop", "default", "fresh", "inner_task", "./inner")
	graph, diags := compileWithAdapters(t, parentHCL, tmpDir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got %s", diags.Error())
	}
	if got := graph.Adapters["noop.default"].Continuation; got != "fresh" {
		t.Errorf("parent continuation = %q, want %q", got, "fresh")
	}
}

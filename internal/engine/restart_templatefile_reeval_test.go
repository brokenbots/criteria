package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

// collectInventoryTemplateFile returns the rendered template fixture used to
// exercise templatefile()-rendered step inputs across a restart. Line 21
// pipelines a directly-bound string variable into shellquote (the shape that
// panics at restart when the variable context is not restored); line 24
// pipelines jsonencode(var.repos) through shellquote so both input channels
// are covered.
func collectInventoryTemplateFile() []byte {
	return []byte(`#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

# collect_inventory.sh — sweep repo inventory ("criteria": sweeping).
# Rendered by the collect step via templatefile().
# Inputs: sweep_key (string, launch-provided), repos_json (jsonencode(var.repos)).
# The sweep key is shell-quoted so the report line is injection-safe.

mode="${MODE:-inventory}"
out="${OUT:-inventory.json}"
workdir="${WORKDIR:-.}"

echo "repo criteria: sweeping"

cleanup() {
  rm -f "${workdir}/.sweep.lock"
}
trap cleanup EXIT

key="$( {{ .sweep_key | shellquote }} )"
echo "sweep key: ${key}"

repos=( {{ .repos_json | shellquote }} )
for r in "${repos[@]}"; do
  echo "repo: ${r}"
done

{
  echo "mode: ${mode}"
  echo "out: ${out}"
} > "${out}"

echo "wrote ${out}"
`)
}

// templatefileRecorderAdapter is a fixed-outcome adapter that records the
// resolved command input of every Execute call, so a restart re-evaluation of
// the same step input can be compared byte-for-byte against the first render.
type templatefileRecorderAdapter struct {
	fakeAdapter

	mu       sync.Mutex
	commands []string
}

func (a *templatefileRecorderAdapter) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	if step != nil && step.Input != nil {
		a.mu.Lock()
		a.commands = append(a.commands, step.Input["command"])
		a.mu.Unlock()
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (a *templatefileRecorderAdapter) rendered() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.commands...)
}

// kb411TemplateFileGraph compiles the workflow under test: two launch-provided
// variables (a list and a string, both declared without defaults so only the
// run-time override supplies values) and a collect step whose command input is
// rendered from the template fixture.
func kb411TemplateFileGraph(t *testing.T, dir string) *workflow.FSMGraph {
	t.Helper()

	b := workflowtest.New().
		WithHeader("kb411template", "0.1", "collect", "done").
		WithAdapterFate("noop", "default", "reuse").
		WithVariable("repos", "list(string)", "").
		WithVariable("sweep_key", "string", "").
		WithStep("collect", workflowtest.Target("adapter.noop.default"),
			workflowtest.StepRaw(`input {
  command = templatefile("collect_inventory.sh.tftpl", {
    sweep_key  = var.sweep_key
    repos_json = jsonencode(var.repos)
  })
}`),
			workflowtest.Outcome("success", "wait.hold"),
			workflowtest.Outcome("failure", "state.failed")).
		WithWait("hold", "resume", workflowtest.WaitOutcome("resumed", "state.done")).
		WithState("done", true, boolPtr(true)).
		WithState("failed", true, boolPtr(false))

	scriptPath := filepath.Join(dir, "collect_inventory.sh.tftpl")
	if err := os.WriteFile(scriptPath, collectInventoryTemplateFile(), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}

	return compileKb411Graph(t, b.Build(), dir)
}

// compileKb411Graph compiles source with the workflow directory configured so
// compile-time folding can resolve templatefile() against the template fixture.
func compileKb411Graph(t *testing.T, src, dir string) *workflow.FSMGraph {
	t.Helper()
	src = injectDefaultAdapters(src)
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.CompileWithOpts(spec, nil, workflow.CompileOpts{WorkflowDir: dir})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// TestRestart_TemplateFileInputReevaluationByteIdentical drives the restart
// machinery at a step boundary: a templatefile-rendered input renders once on
// the original engine, the variable scope is persisted at the pause boundary
// and restored into a new engine that re-enters the same step, and the second
// render must be byte-identical to the first. A list-typed variable supplied
// only by the launch-time override and a string variable piped straight into
// shellquote both keep their exact values across restart.
func TestRestart_TemplateFileInputReevaluationByteIdentical(t *testing.T) {
	dir := t.TempDir()
	g := kb411TemplateFileGraph(t, dir)

	recorderA := &templatefileRecorderAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loaderA := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": recorderA}}
	engA := NewTestEngine(g, loaderA, &fakeSink{},
		WithVarOverrides(map[string]cty.Value{
			"repos":     cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")}),
			"sweep_key": cty.StringVal("nightly"),
		}),
		WithWorkflowDir(dir))

	if err := engA.Run(context.Background()); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	commandsA := recorderA.rendered()
	if len(commandsA) != 1 {
		t.Fatalf("initial run recorded %d renders, want 1", len(commandsA))
	}
	cmdA := commandsA[0]
	for _, want := range []string{`key="$( 'nightly' )"`, `repos=( '["infra","tools"]' )`} {
		if !strings.Contains(cmdA, want) {
			t.Errorf("initial render missing %q; command:\n%s", want, cmdA)
		}
	}

	// criteria_restart: serialize the variable scope at the step boundary
	// (the server stores this blob verbatim) and restore it into a fresh
	// engine that re-enters the interrupted step at attempt 1.
	scopeJSON, err := workflow.SerializeVarScope(engA.VarScope())
	if err != nil {
		t.Fatalf("SerializeVarScope: %v", err)
	}
	restoredVars, restoredIter, err := workflow.RestoreVarScope(scopeJSON, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	recorderB := &templatefileRecorderAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loaderB := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": recorderB}}
	engB := NewTestEngine(g, loaderB, &fakeSink{},
		WithResumedVars(restoredVars),
		WithResumedIter(restoredIter),
		WithWorkflowDir(dir))

	if err := engB.RunFrom(context.Background(), "collect", 1); err != nil {
		t.Fatalf("restarted run: %v", err)
	}

	commandsB := recorderB.rendered()
	if len(commandsB) != 1 {
		t.Fatalf("restarted run recorded %d renders, want 1", len(commandsB))
	}
	cmdB := commandsB[0]
	if cmdB != cmdA {
		t.Errorf("restart re-evaluation is not byte-identical:\n--- first render ---\n%s\n--- restart render ---\n%s", cmdA, cmdB)
	}
	for _, want := range []string{`key="$( 'nightly' )"`, `repos=( '["infra","tools"]' )`} {
		if !strings.Contains(cmdB, want) {
			t.Errorf("restart render missing %q; command:\n%s", want, cmdB)
		}
	}
}

// TestRestart_TemplateFileReeval_ServerBlobLegacyShape reproduces the server
// reattach shape that lost typed variable values: the orchestrator assembles
// the wire VariableScope from string-rendered events, so the restored blob
// carries only the lossy legacy "var" map plus step outputs — never the
// vars_typed pair. The resumed engine must re-apply the launch-time typed
// overrides on top of the restored scope, so the re-entered collect step
// re-evaluates its templatefile() input with the same typed values as the
// pre-restart render (jsonencode over a list(string) stays a list and
// shellquote accepts it); without that re-application the rendered variables
// fall back to declared defaults and shellquote rejects the value.
func TestRestart_TemplateFileReeval_ServerBlobLegacyShape(t *testing.T) {
	dir := t.TempDir()
	g := kb411TemplateFileGraph(t, dir)

	overrides := map[string]cty.Value{
		"repos":     cty.ListVal([]cty.Value{cty.StringVal("infra"), cty.StringVal("tools")}),
		"sweep_key": cty.StringVal("nightly"),
	}

	recorderA := &templatefileRecorderAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loaderA := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": recorderA}}
	engA := NewTestEngine(g, loaderA, &fakeSink{},
		WithVarOverrides(overrides),
		WithWorkflowDir(dir))

	if err := engA.Run(context.Background()); err != nil {
		t.Fatalf("initial run: %v", err)
	}
	commandsA := recorderA.rendered()
	if len(commandsA) != 1 {
		t.Fatalf("initial run recorded %d renders, want 1", len(commandsA))
	}
	cmdA := commandsA[0]
	for _, want := range []string{`key="$( 'nightly' )"`, `repos=( '["infra","tools"]' )`} {
		if !strings.Contains(cmdA, want) {
			t.Errorf("initial render missing %q; command:\n%s", want, cmdA)
		}
	}

	// Server-shaped legacy blob: legacy lossy string "var" map plus the
	// collect step's captured outputs; no vars_typed pair (the wire channel
	// cannot carry one). Modelled on the blob a ReattachRun response delivers.
	legacyBlob := `{"var":{"repos":"[\"infra\",\"tools\"]"},"steps":{"collect":{"stdout":"artifact-42.bin","exit_code":"0"}}}`
	restoredVars, restoredIter, err := workflow.RestoreVarScope(legacyBlob, g)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	recorderB := &templatefileRecorderAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loaderB := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": recorderB}}
	engB := NewTestEngine(g, loaderB, &fakeSink{},
		WithResumedVars(restoredVars),
		WithResumedIter(restoredIter),
		WithVarOverrides(overrides),
		WithWorkflowDir(dir))

	if err := engB.RunFrom(context.Background(), "collect", 1); err != nil {
		t.Fatalf("restarted run: %v", err)
	}

	commandsB := recorderB.rendered()
	if len(commandsB) != 1 {
		t.Fatalf("restarted run recorded %d renders, want 1", len(commandsB))
	}
	cmdB := commandsB[0]
	if cmdB != cmdA {
		t.Errorf("restart re-evaluation over the server-shaped legacy blob is not byte-identical:\n--- first render ---\n%s\n--- restart render ---\n%s", cmdA, cmdB)
	}
	for _, want := range []string{`key="$( 'nightly' )"`, `repos=( '["infra","tools"]' )`} {
		if !strings.Contains(cmdB, want) {
			t.Errorf("restart render missing %q; command:\n%s", want, cmdB)
		}
	}
}

func boolPtr(v bool) *bool { return &v }

package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/workflow/workflowtest"
)

func allDiagMessages(diags hcl.Diagnostics) string {
	var sb strings.Builder
	for _, d := range diags {
		sb.WriteString(d.Summary)
		if d.Detail != "" {
			sb.WriteString(": ")
			sb.WriteString(d.Detail)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func compileTestWorkflow(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, parseDiags := Parse("test.hcl", []byte(src))
	if parseDiags.HasErrors() {
		t.Fatalf("parse failed:\n%s\nHCL:\n%s", allDiagMessages(parseDiags), src)
	}
	return Compile(spec, nil)
}

func TestGraphVerify_UnreachableNodes(t *testing.T) {
	falseVal := false
	tests := []struct {
		name         string
		orphanNode   string
		wantErr      bool
		wantSeverity hcl.DiagnosticSeverity
		wantSubstr   string
	}{
		{
			name:         "unreachable step is error",
			orphanNode:   workflowtest.Step("orphan", workflowtest.Next("state.done")),
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `step "orphan" is unreachable from initial_state`,
		},
		{
			name:         "unreachable switch is error",
			orphanNode:   workflowtest.Switch("orphan_sw", workflowtest.DefaultNext("state.done")),
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `switch "orphan_sw" is unreachable from initial_state`,
		},
		{
			name:         "unreachable wait is error",
			orphanNode:   workflowtest.Wait("orphan_wait", "go", workflowtest.WaitOutcome("signaled", "state.done")),
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `wait "orphan_wait" is unreachable from initial_state`,
		},
		{
			name: "unreachable approval is error",
			orphanNode: workflowtest.Approval("orphan_appr", []string{"alice"}, "approve?",
				workflowtest.ApprovalOutcome("approved", "state.done"),
				workflowtest.ApprovalOutcome("rejected", "state.done"),
			),
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `approval "orphan_appr" is unreachable from initial_state`,
		},
		{
			name:         "unreachable non-target terminal state is warning",
			orphanNode:   workflowtest.State("unused_failed", true, false),
			wantErr:      false,
			wantSeverity: hcl.DiagWarning,
			wantSubstr:   `state "unused_failed" is unreachable from initial_state`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := workflowtest.New().
				WithHeader("t", "0.1", "start", "done").
				WithAdapter("noop", "default").
				WithStep("start", workflowtest.Next("state.done")).
				WithState("done", true, nil).
				WithRaw(tc.orphanNode)

			if tc.name == "unreachable non-target terminal state is warning" {
				// The test specifically asserts on unused_failed; done has no success attr
				b = workflowtest.New().
					WithHeader("t", "0.1", "start", "done").
					WithAdapter("noop", "default").
					WithStep("start", workflowtest.Next("state.done")).
					WithState("done", true, nil).
					WithState("unused_failed", true, &falseVal)
			}

			src := b.Build()
			_, diags := compileTestWorkflow(t, src)
			if diags.HasErrors() != tc.wantErr {
				t.Fatalf("HasErrors() = %v, want %v; diags: %s", diags.HasErrors(), tc.wantErr, allDiagMessages(diags))
			}
			found := false
			for _, d := range diags {
				if d.Severity == tc.wantSeverity && strings.Contains(d.Summary, tc.wantSubstr) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected diagnostic (%v) containing %q, got:\n%s", tc.wantSeverity, tc.wantSubstr, allDiagMessages(diags))
			}
		})
	}
}

func TestGraphVerify_Livelocks(t *testing.T) {
	falseVal := false
	tests := []struct {
		name       string
		builder    *workflowtest.WorkflowBuilder
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "self-loop livelock",
			builder: workflowtest.New().
				WithHeader("t", "0.1", "spin", "done").
				WithAdapter("noop", "default").
				WithStep("spin", workflowtest.Next("step.spin")).
				WithState("done", true, nil),
			wantErr:    true,
			wantSubstr: `livelock detected: step "spin" enters cycle [spin -> spin] with no exit path to any terminal state`,
		},
		{
			name: "two-node cycle livelock with exact trace",
			builder: workflowtest.New().
				WithHeader("t", "0.1", "retry_loop", "done").
				WithAdapter("noop", "default").
				WithStep("retry_loop", workflowtest.Next("step.check_status")).
				WithStep("check_status", workflowtest.Next("step.retry_loop")).
				WithState("done", true, nil),
			wantErr:    true,
			wantSubstr: `livelock detected: step "retry_loop" enters cycle [retry_loop -> check_status -> retry_loop] with no exit path to any terminal state`,
		},
		{
			name: "entry stem leading into inescapable cycle",
			builder: workflowtest.New().
				WithHeader("t", "0.1", "setup", "done").
				WithAdapter("noop", "default").
				WithStep("setup",
					workflowtest.Outcome("success", "state.done"),
					workflowtest.Outcome("failure", "step.enter_trap"),
				).
				WithStep("enter_trap", workflowtest.Next("step.retry_loop")).
				WithStep("retry_loop", workflowtest.Next("step.check_status")).
				WithStep("check_status", workflowtest.Next("step.retry_loop")).
				WithState("done", true, nil),
			wantErr:    true,
			wantSubstr: `livelock detected: step "enter_trap" enters cycle [enter_trap -> retry_loop -> check_status -> retry_loop] with no exit path to any terminal state`,
		},
		{
			name: "cycle mediated by switch with no exit",
			builder: workflowtest.New().
				WithHeader("t", "0.1", "retry_loop", "done").
				WithAdapter("noop", "default").
				WithStep("retry_loop", workflowtest.Next("switch.decide")).
				WithSwitch("decide", workflowtest.DefaultNext("step.retry_loop")).
				WithState("done", true, nil),
			wantErr:    true,
			wantSubstr: `livelock detected: step "retry_loop" enters cycle [retry_loop -> decide -> retry_loop] with no exit path to any terminal state`,
		},
		{
			name: "non-terminal state dead-end trap",
			builder: workflowtest.New().
				WithHeader("t", "0.1", "start", "done").
				WithAdapter("noop", "default").
				WithStep("start",
					workflowtest.Outcome("success", "state.done"),
					workflowtest.Outcome("failure", "step.cleanup"),
				).
				WithStep("cleanup", workflowtest.Next("state.limbo")).
				WithState("limbo", false, nil).
				WithState("done", true, nil),
			wantErr:    true,
			wantSubstr: `livelock detected: step "cleanup" reaches dead-end [cleanup -> limbo] with no exit path to any terminal state`,
		},
		{
			name: "cycle with exit to terminal state is valid",
			builder: workflowtest.NewStandard().
				WithHeader("t", "0.1", "retry_loop", "done").
				WithStep("retry_loop",
					workflowtest.Outcome("success", "step.check_status"),
					workflowtest.Outcome("failure", "state.failed"),
				).
				WithStep("check_status",
					workflowtest.Outcome("success", "state.done"),
					workflowtest.Outcome("failure", "step.retry_loop"),
				).
				WithState("done", true, nil).
				WithState("failed", true, &falseVal),
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.builder.Build()
			_, diags := compileTestWorkflow(t, src)
			if diags.HasErrors() != tc.wantErr {
				t.Fatalf("HasErrors() = %v, want %v; diags:\n%s", diags.HasErrors(), tc.wantErr, allDiagMessages(diags))
			}
			if tc.wantSubstr != "" && !strings.Contains(allDiagMessages(diags), tc.wantSubstr) {
				t.Errorf("expected error containing %q, got:\n%s", tc.wantSubstr, allDiagMessages(diags))
			}
		})
	}
}

func TestGraphVerify_OutcomeCompleteness(t *testing.T) {
	tests := []struct {
		name       string
		stepOpts   []workflowtest.StepOpt
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "step with only success outcome fails completeness",
			stepOpts:   []workflowtest.StepOpt{workflowtest.Outcome("success", "state.done")},
			wantErr:    true,
			wantSubstr: `step "run": outcome completeness requires handling adapter failure/error paths`,
		},
		{
			name: "schema-bearing success arm without failure lanes fails completeness",
			// StepRaw places the schema attribute on the arm so the step
			// enters contract mode; a contract-mode success arm without any
			// failure/error lane is still rejected — the verifier is
			// absolute and has no contract-mode or adapter-kind opt-out.
			stepOpts: []workflowtest.StepOpt{workflowtest.StepRaw(`outcome "success" {
  schema = object({ reason = string })
  next   = state.done
}`)},
			wantErr:    true,
			wantSubstr: `step "run": outcome completeness requires handling adapter failure/error paths`,
		},
		{
			name: "schema-bearing success arm plus declared fallback compiles",
			// The recomposed never-finalize probe shape: a schema-bearing
			// success arm declared alongside a non-failure-class fallback
			// arm satisfies the absolute verifier, so the fallback synthesis
			// lane stays compile-legal.
			stepOpts: []workflowtest.StepOpt{
				workflowtest.StepRaw(`outcome "success" {
  schema = object({ reason = string })
  next   = state.done
}`),
				workflowtest.FallbackOutcome("finalize_exhausted", "state.failed"),
			},
			wantErr: false,
		},
		{
			name:       "step with only failure outcome fails completeness",
			stepOpts:   []workflowtest.StepOpt{workflowtest.Outcome("failure", "state.done")},
			wantErr:    true,
			wantSubstr: `step "run": outcome completeness requires handling both success and failure/error paths`,
		},
		{
			name: "step with success and failure compiles",
			stepOpts: []workflowtest.StepOpt{
				workflowtest.Outcome("success", "state.done"),
				workflowtest.Outcome("failure", "state.failed"),
			},
			wantErr: false,
		},
		{
			name: "step with success and error compiles",
			stepOpts: []workflowtest.StepOpt{
				workflowtest.Outcome("success", "state.done"),
				workflowtest.Outcome("error", "state.failed"),
			},
			wantErr: false,
		},
		{
			name: "step with success and default outcome compiles",
			stepOpts: []workflowtest.StepOpt{
				workflowtest.Outcome("success", "state.done"),
				workflowtest.Outcome("default", "state.failed"),
			},
			wantErr: false,
		},
		{
			name: "step with success and fallback outcome compiles",
			stepOpts: []workflowtest.StepOpt{
				workflowtest.Outcome("success", "state.done"),
				workflowtest.FallbackOutcome("unexpected", "state.failed"),
			},
			wantErr: false,
		},
		{
			name: "step with duplicate outcomes fails",
			stepOpts: []workflowtest.StepOpt{
				workflowtest.Outcome("success", "state.done"),
				workflowtest.Outcome("success", "state.done"),
				workflowtest.Outcome("failure", "state.done"),
			},
			wantErr:    true,
			wantSubstr: `step "run": duplicate outcome "success"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := workflowtest.NewStandard().
				WithInitial("run").
				WithStep("run", tc.stepOpts...).
				Build()
			_, diags := compileTestWorkflow(t, src)
			if diags.HasErrors() != tc.wantErr {
				t.Fatalf("HasErrors() = %v, want %v; diags:\n%s", diags.HasErrors(), tc.wantErr, allDiagMessages(diags))
			}
			if tc.wantSubstr != "" && !strings.Contains(allDiagMessages(diags), tc.wantSubstr) {
				t.Errorf("expected error containing %q, got:\n%s", tc.wantSubstr, allDiagMessages(diags))
			}
		})
	}
}

func TestGraphVerify_SwitchDeterminism(t *testing.T) {
	tests := []struct {
		name       string
		switchOpts []workflowtest.SwitchOpt
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "duplicate match condition is rejected",
			switchOpts: []workflowtest.SwitchOpt{
				workflowtest.Match(`steps.s1.outcome == "success"`, "state.done"),
				workflowtest.Match(`steps.s1.outcome == "success"`, "state.failed"),
				workflowtest.DefaultNext("state.failed"),
			},
			wantErr:    true,
			wantSubstr: `switch "route" match[1]: duplicate match condition "steps.s1.outcome == \"success\"" (already declared at match[0])`,
		},
		{
			name: "match arm shadowed by earlier constant true match arm is rejected",
			switchOpts: []workflowtest.SwitchOpt{
				workflowtest.Match("true", "state.done"),
				workflowtest.Match(`steps.s1.outcome == "success"`, "state.failed"),
				workflowtest.DefaultNext("state.failed"),
			},
			wantErr:    true,
			wantSubstr: `switch "route" match[1]: unreachable match condition shadowed by unconditional match[0] (condition = true)`,
		},
		{
			name: "constant false match condition is rejected",
			switchOpts: []workflowtest.SwitchOpt{
				workflowtest.Match("false", "state.done"),
				workflowtest.DefaultNext("state.done"),
			},
			wantErr:    true,
			wantSubstr: `switch "route" match[0]: match condition evaluates to constant false and can never be taken`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := workflowtest.NewStandard().
				WithInitial("s1").
				WithStep("s1",
					workflowtest.Outcome("success", "switch.route"),
					workflowtest.Outcome("failure", "state.failed"),
				).
				WithSwitch("route", tc.switchOpts...).
				Build()
			_, diags := compileTestWorkflow(t, src)
			if diags.HasErrors() != tc.wantErr {
				t.Fatalf("HasErrors() = %v, want %v; diags:\n%s", diags.HasErrors(), tc.wantErr, allDiagMessages(diags))
			}
			if tc.wantSubstr != "" && !strings.Contains(allDiagMessages(diags), tc.wantSubstr) {
				t.Errorf("expected error containing %q, got:\n%s", tc.wantSubstr, allDiagMessages(diags))
			}
		})
	}
}

func TestGraphVerify_TargetStateReachability(t *testing.T) {
	trueVal := true
	falseVal := false
	src := workflowtest.New().
		WithHeader("t", "0.1", "build", "verified").
		WithAdapter("noop", "default").
		WithStep("build",
			workflowtest.Outcome("success", "step.test"),
			workflowtest.Outcome("failure", "state.failed"),
		).
		WithStep("test",
			workflowtest.Outcome("success", "state.failed"),
			workflowtest.Outcome("failure", "state.failed"),
		).
		WithState("verified", true, &trueVal).
		WithState("failed", true, &falseVal).
		Build()

	_, diags := compileTestWorkflow(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected error when target_state is unreachable")
	}
	msgs := allDiagMessages(diags)
	wantSummary := `target_state "verified" is unreachable from initial_state "build": no happy path to target_state exists`
	if !strings.Contains(msgs, wantSummary) {
		t.Errorf("expected summary %q, got:\n%s", wantSummary, msgs)
	}
	if !strings.Contains(msgs, `[build -> failed]`) {
		t.Errorf("expected detail to include reachable failure path [build -> failed], got:\n%s", msgs)
	}
}

func TestGraphVerify_BudgetAndVisitFeasibility(t *testing.T) {
	tests := []struct {
		name       string
		builder    *workflowtest.WorkflowBuilder
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "shortest path to target_state requires 3 steps but budget is 2",
			builder: workflowtest.Chain("s1", "s2", "s3").
				WithMaxTotalSteps(2),
			wantErr:    true,
			wantSubstr: `unsatisfiable step_budget: shortest path from "s1" to "done" [s1 -> s2 -> s3 -> done] requires 3 steps, but budget is 2`,
		},
		{
			name: "branching graph with one fast path within budget compiles",
			builder: workflowtest.NewStandard().
				WithInitial("s1").
				WithMaxTotalSteps(2).
				WithStep("s1",
					workflowtest.Outcome("fast", "step.s2"),
					workflowtest.Outcome("slow", "step.slow1"),
					workflowtest.Outcome("failure", "state.failed"),
				).
				WithStep("s2",
					workflowtest.Outcome("success", "state.done"),
					workflowtest.Outcome("failure", "state.failed"),
				).
				WithStep("slow1",
					workflowtest.Outcome("success", "step.slow2"),
					workflowtest.Outcome("failure", "state.failed"),
				).
				WithStep("slow2",
					workflowtest.Outcome("success", "step.s2"),
					workflowtest.Outcome("failure", "state.failed"),
				),
			wantErr: false,
		},
		{
			name: "switch nodes cost zero steps in budget proof",
			builder: workflowtest.NewStandard().
				WithInitial("s1").
				WithMaxTotalSteps(1).
				WithStep("s1",
					workflowtest.Outcome("success", "switch.gate"),
					workflowtest.Outcome("failure", "state.failed"),
				).
				WithSwitch("gate", workflowtest.DefaultNext("step.s2")).
				WithStep("s2",
					workflowtest.Outcome("success", "state.done"),
					workflowtest.Outcome("failure", "state.failed"),
				),
			wantErr:    true,
			wantSubstr: `unsatisfiable step_budget: shortest path from "s1" to "done" [s1 -> gate -> s2 -> done] requires 2 steps, but budget is 1`,
		},
		{
			name: "static count exceeds step_budget",
			builder: workflowtest.NewStandard().
				WithInitial("batch").
				WithMaxTotalSteps(3).
				WithStep("batch",
					workflowtest.Count(5),
					workflowtest.Outcome("all_succeeded", "state.done"),
					workflowtest.Outcome("any_failed", "state.failed"),
				),
			wantErr:    true,
			wantSubstr: `unsatisfiable step_budget: shortest path from "batch" to "done" [batch -> done] requires 5 steps, but budget is 3`,
		},
		{
			name: "static for_each exceeds max_visits",
			builder: workflowtest.NewStandard().
				WithInitial("fanout").
				WithStep("fanout",
					workflowtest.ForEach("a", "b", "c", "d"),
					workflowtest.MaxVisits(2),
					workflowtest.Outcome("all_succeeded", "state.done"),
					workflowtest.Outcome("any_failed", "state.failed"),
				),
			wantErr:    true,
			wantSubstr: `unsatisfiable max_visits: shortest path from "fanout" to "done" [fanout -> done] requires visiting step "fanout" 4 times, but max_visits is 2`,
		},
		{
			name: "static parallel exceeds max_visits",
			builder: workflowtest.NewStandard().
				WithInitial("par").
				WithStep("par",
					workflowtest.Parallel("a", "b", "c"),
					workflowtest.MaxVisits(2),
					workflowtest.Outcome("all_succeeded", "state.done"),
					workflowtest.Outcome("any_failed", "state.failed"),
				),
			wantErr:    true,
			wantSubstr: `unsatisfiable max_visits: shortest path from "par" to "done" [par -> done] requires visiting step "par" 3 times, but max_visits is 2`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.builder.Build()
			_, diags := compileTestWorkflow(t, src)
			if diags.HasErrors() != tc.wantErr {
				t.Fatalf("HasErrors() = %v, want %v; diags:\n%s", diags.HasErrors(), tc.wantErr, allDiagMessages(diags))
			}
			if tc.wantSubstr != "" && !strings.Contains(allDiagMessages(diags), tc.wantSubstr) {
				t.Errorf("expected error containing %q, got:\n%s", tc.wantSubstr, allDiagMessages(diags))
			}
		})
	}
}

func TestGraphVerify_SubworkflowTopologyAndBudget(t *testing.T) {
	t.Run("recursive subworkflow compilation enforces topology and budget invariants", func(t *testing.T) {
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "inner")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}

		falseVal := false
		calleeHCL := workflowtest.New().
			WithHeader("inner", "0.1", "sub1", "sub_done").
			WithMaxTotalSteps(1).
			WithAdapter("noop", "default").
			WithStep("sub1",
				workflowtest.Outcome("success", "step.sub2"),
				workflowtest.Outcome("failure", "state.sub_failed"),
			).
			WithStep("sub2",
				workflowtest.Outcome("success", "state.sub_done"),
				workflowtest.Outcome("failure", "state.sub_failed"),
			).
			WithState("sub_done", true, nil).
			WithState("sub_failed", true, &falseVal).
			Build()

		if err := os.WriteFile(filepath.Join(subDir, "workflow.hcl"), []byte(calleeHCL), 0o644); err != nil {
			t.Fatal(err)
		}

		parentHCL := workflowtest.NewStandard().
			WithHeader("parent", "0.1", "call_sub", "done").
			WithSubworkflow("inner", "./inner").
			WithStep("call_sub",
				workflowtest.Target("subworkflow.inner"),
				workflowtest.Outcome("success", "step.finish"),
				workflowtest.Outcome("failure", "state.failed"),
			).
			WithStep("finish", workflowtest.Next("state.done")).
			Build()

		spec, parseDiags := Parse("parent.hcl", []byte(parentHCL))
		if parseDiags.HasErrors() {
			t.Fatalf("parse: %s", parseDiags.Error())
		}
		_, diags := CompileWithOpts(spec, nil, CompileOpts{
			WorkflowDir:         tmpDir,
			SubWorkflowResolver: fixedPinResolver(subDir, nil),
		})
		if !diags.HasErrors() {
			t.Fatal("expected budget feasibility error from inner subworkflow")
		}
		want := `unsatisfiable step_budget: shortest path from "sub1" to "sub_done" [sub1 -> sub2 -> sub_done] requires 2 steps, but budget is 1`
		if !strings.Contains(allDiagMessages(diags), want) {
			t.Errorf("expected error containing %q, got:\n%s", want, allDiagMessages(diags))
		}
	})
}

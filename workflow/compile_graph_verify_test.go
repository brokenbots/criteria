package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
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

func TestGraphVerify_UnreachableNodes(t *testing.T) {
	tests := []struct {
		name         string
		hcl          string
		wantErr      bool
		wantSeverity hcl.DiagnosticSeverity
		wantSubstr   string
	}{
		{
			name: "unreachable step is error",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
step "orphan" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
state "done" { terminal = true }
`,
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `step "orphan" is unreachable from initial_state`,
		},
		{
			name: "unreachable switch is error",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
switch "orphan_sw" {
  default { next = state.done }
}
state "done" { terminal = true }
`,
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `switch "orphan_sw" is unreachable from initial_state`,
		},
		{
			name: "unreachable wait is error",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
wait "orphan_wait" {
  signal = "go"
  outcome "signaled" { next = state.done }
}
state "done" { terminal = true }
`,
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `wait "orphan_wait" is unreachable from initial_state`,
		},
		{
			name: "unreachable approval is error",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
approval "orphan_appr" {
  approvers = ["alice"]
  reason    = "approve?"
  outcome "approved" { next = state.done }
  outcome "rejected" { next = state.done }
}
state "done" { terminal = true }
`,
			wantErr:      true,
			wantSeverity: hcl.DiagError,
			wantSubstr:   `approval "orphan_appr" is unreachable from initial_state`,
		},
		{
			name: "unreachable non-target terminal state is warning",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
state "done" { terminal = true }
state "unused_failed" {
  terminal = true
  success  = false
}
`,
			wantErr:      false,
			wantSeverity: hcl.DiagWarning,
			wantSubstr:   `state "unused_failed" is unreachable from initial_state`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, parseDiags := Parse("test.hcl", []byte(tc.hcl))
			if parseDiags.HasErrors() {
				t.Fatalf("parse: %s", parseDiags.Error())
			}
			_, diags := Compile(spec, nil)
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
	tests := []struct {
		name       string
		hcl        string
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "self-loop livelock",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "spin"
  target_state  = "done"
}
adapter "noop" "default" {}
step "spin" {
  target = adapter.noop.default
  outcome "success" { next = step.spin }
  outcome "failure" { next = step.spin }
}
state "done" { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `livelock detected: step "spin" enters cycle [spin -> spin] with no exit path to any terminal state`,
		},
		{
			name: "two-node cycle livelock with exact trace",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "retry_loop"
  target_state  = "done"
}
adapter "noop" "default" {}
step "retry_loop" {
  target = adapter.noop.default
  outcome "success" { next = step.check_status }
  outcome "failure" { next = step.check_status }
}
step "check_status" {
  target = adapter.noop.default
  outcome "success" { next = step.retry_loop }
  outcome "failure" { next = step.retry_loop }
}
state "done" { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `livelock detected: step "retry_loop" enters cycle [retry_loop -> check_status -> retry_loop] with no exit path to any terminal state`,
		},
		{
			name: "entry stem leading into inescapable cycle",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "setup"
  target_state  = "done"
}
adapter "noop" "default" {}
step "setup" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = step.enter_trap }
}
step "enter_trap" {
  target = adapter.noop.default
  outcome "success" { next = step.retry_loop }
  outcome "failure" { next = step.retry_loop }
}
step "retry_loop" {
  target = adapter.noop.default
  outcome "success" { next = step.check_status }
  outcome "failure" { next = step.check_status }
}
step "check_status" {
  target = adapter.noop.default
  outcome "success" { next = step.retry_loop }
  outcome "failure" { next = step.retry_loop }
}
state "done" { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `livelock detected: step "enter_trap" enters cycle [enter_trap -> retry_loop -> check_status -> retry_loop] with no exit path to any terminal state`,
		},
		{
			name: "cycle mediated by switch with no exit",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "retry_loop"
  target_state  = "done"
}
adapter "noop" "default" {}
step "retry_loop" {
  target = adapter.noop.default
  outcome "success" { next = switch.decide }
  outcome "failure" { next = switch.decide }
}
switch "decide" {
  default { next = step.retry_loop }
}
state "done" { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `livelock detected: step "retry_loop" enters cycle [retry_loop -> decide -> retry_loop] with no exit path to any terminal state`,
		},
		{
			name: "non-terminal state dead-end trap",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = step.cleanup }
}
step "cleanup" {
  target = adapter.noop.default
  outcome "success" { next = state.limbo }
  outcome "failure" { next = state.limbo }
}
state "limbo" { terminal = false }
state "done"  { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `livelock detected: step "cleanup" reaches dead-end [cleanup -> limbo] with no exit path to any terminal state`,
		},
		{
			name: "cycle with exit to terminal state is valid",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "retry_loop"
  target_state  = "done"
}
adapter "noop" "default" {}
step "retry_loop" {
  target = adapter.noop.default
  outcome "success" { next = step.check_status }
  outcome "failure" { next = state.failed }
}
step "check_status" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = step.retry_loop }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, parseDiags := Parse("test.hcl", []byte(tc.hcl))
			if parseDiags.HasErrors() {
				t.Fatalf("parse: %s", parseDiags.Error())
			}
			_, diags := Compile(spec, nil)
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
		hcl        string
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "step with only success outcome fails completeness",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `step "run": outcome completeness requires handling adapter failure/error paths`,
		},
		{
			name: "step with only failure outcome fails completeness",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "failure" { next = state.done }
}
state "done" { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `step "run": outcome completeness requires handling both success and failure/error paths`,
		},
		{
			name: "step with success and failure compiles",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr: false,
		},
		{
			name: "step with success and error compiles",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "error"   { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr: false,
		},
		{
			name: "step with success and default outcome compiles",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "default" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr: false,
		},
		{
			name: "step with success and fallback outcome compiles",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "unexpected" {
    fallback = true
    next     = state.failed
  }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr: false,
		},
		{
			name: "step with duplicate outcomes fails",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
state "done" { terminal = true }
`,
			wantErr:    true,
			wantSubstr: `step "run": duplicate outcome "success"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, parseDiags := Parse("test.hcl", []byte(tc.hcl))
			if parseDiags.HasErrors() {
				t.Fatalf("parse: %s", parseDiags.Error())
			}
			_, diags := Compile(spec, nil)
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
		hcl        string
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "duplicate match condition is rejected",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "s1"
  target_state  = "done"
}
adapter "noop" "default" {}
step "s1" {
  target = adapter.noop.default
  outcome "success" { next = switch.route }
  outcome "failure" { next = state.failed }
}
switch "route" {
  match {
    condition = steps.s1.outcome == "success"
    next      = state.done
  }
  match {
    condition = steps.s1.outcome == "success"
    next      = state.failed
  }
  default {
    next = state.failed
  }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `switch "route" match[1]: duplicate match condition "steps.s1.outcome == \"success\"" (already declared at match[0])`,
		},
		{
			name: "match arm shadowed by earlier constant true match arm is rejected",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "s1"
  target_state  = "done"
}
adapter "noop" "default" {}
step "s1" {
  target = adapter.noop.default
  outcome "success" { next = switch.route }
  outcome "failure" { next = state.failed }
}
switch "route" {
  match {
    condition = true
    next      = state.done
  }
  match {
    condition = steps.s1.outcome == "success"
    next      = state.failed
  }
  default {
    next = state.failed
  }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `switch "route" match[1]: unreachable match condition shadowed by unconditional match[0] (condition = true)`,
		},
		{
			name: "constant false match condition is rejected",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "s1"
  target_state  = "done"
}
adapter "noop" "default" {}
step "s1" {
  target = adapter.noop.default
  outcome "success" { next = switch.route }
  outcome "failure" { next = state.failed }
}
switch "route" {
  match {
    condition = false
    next      = state.done
  }
  default {
    next = state.done
  }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `switch "route" match[0]: match condition evaluates to constant false and can never be taken`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, parseDiags := Parse("test.hcl", []byte(tc.hcl))
			if parseDiags.HasErrors() {
				t.Fatalf("parse: %s", parseDiags.Error())
			}
			_, diags := Compile(spec, nil)
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
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "build"
  target_state  = "verified"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" { next = step.test }
  outcome "failure" { next = state.failed }
}
step "test" {
  target = adapter.noop.default
  outcome "success" { next = state.failed }
  outcome "failure" { next = state.failed }
}
state "verified" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`
	spec, parseDiags := Parse("test.hcl", []byte(src))
	if parseDiags.HasErrors() {
		t.Fatalf("parse: %s", parseDiags.Error())
	}
	_, diags := Compile(spec, nil)
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
		hcl        string
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "shortest path to target_state requires 3 steps but budget is 2",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "s1"
  target_state  = "done"
  policy {
    max_total_steps = 2
  }
}
adapter "noop" "default" {}
step "s1" {
  target = adapter.noop.default
  outcome "success" { next = step.s2 }
  outcome "failure" { next = state.failed }
}
step "s2" {
  target = adapter.noop.default
  outcome "success" { next = step.s3 }
  outcome "failure" { next = state.failed }
}
step "s3" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `unsatisfiable step_budget: shortest path from "s1" to "done" [s1 -> s2 -> s3 -> done] requires 3 steps, but budget is 2`,
		},
		{
			name: "branching graph with one fast path within budget compiles",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "s1"
  target_state  = "done"
  policy {
    max_total_steps = 2
  }
}
adapter "noop" "default" {}
step "s1" {
  target = adapter.noop.default
  outcome "fast"    { next = step.s2 }
  outcome "slow"    { next = step.slow1 }
  outcome "failure" { next = state.failed }
}
step "s2" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
step "slow1" {
  target = adapter.noop.default
  outcome "success" { next = step.slow2 }
  outcome "failure" { next = state.failed }
}
step "slow2" {
  target = adapter.noop.default
  outcome "success" { next = step.s2 }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr: false,
		},
		{
			name: "switch nodes cost zero steps in budget proof",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "s1"
  target_state  = "done"
  policy {
    max_total_steps = 1
  }
}
adapter "noop" "default" {}
step "s1" {
  target = adapter.noop.default
  outcome "success" { next = switch.gate }
  outcome "failure" { next = state.failed }
}
switch "gate" {
  default { next = step.s2 }
}
step "s2" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `unsatisfiable step_budget: shortest path from "s1" to "done" [s1 -> gate -> s2 -> done] requires 2 steps, but budget is 1`,
		},
		{
			name: "static count exceeds step_budget",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "batch"
  target_state  = "done"
  policy {
    max_total_steps = 3
  }
}
adapter "noop" "default" {}
step "batch" {
  target = adapter.noop.default
  count  = 5
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `unsatisfiable step_budget: shortest path from "batch" to "done" [batch -> done] requires 5 steps, but budget is 3`,
		},
		{
			name: "static for_each exceeds max_visits",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "fanout"
  target_state  = "done"
}
adapter "noop" "default" {}
step "fanout" {
  target     = adapter.noop.default
  for_each   = ["a", "b", "c", "d"]
  max_visits = 2
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `unsatisfiable max_visits: shortest path from "fanout" to "done" [fanout -> done] requires visiting step "fanout" 4 times, but max_visits is 2`,
		},
		{
			name: "static parallel exceeds max_visits",
			hcl: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "par"
  target_state  = "done"
}
adapter "noop" "default" {}
step "par" {
  target     = adapter.noop.default
  parallel   = ["a", "b", "c"]
  max_visits = 2
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`,
			wantErr:    true,
			wantSubstr: `unsatisfiable max_visits: shortest path from "par" to "done" [par -> done] requires visiting step "par" 3 times, but max_visits is 2`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, parseDiags := Parse("test.hcl", []byte(tc.hcl))
			if parseDiags.HasErrors() {
				t.Fatalf("parse: %s", parseDiags.Error())
			}
			_, diags := Compile(spec, nil)
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
		calleeHCL := `
workflow {
  name          = "inner"
  version       = "0.1"
  initial_state = "sub1"
  target_state  = "sub_done"
  policy {
    max_total_steps = 1
  }
}
adapter "noop" "default" {}
step "sub1" {
  target = adapter.noop.default
  outcome "success" { next = step.sub2 }
  outcome "failure" { next = state.sub_failed }
}
step "sub2" {
  target = adapter.noop.default
  outcome "success" { next = state.sub_done }
  outcome "failure" { next = state.sub_failed }
}
state "sub_done" { terminal = true }
state "sub_failed" {
  terminal = true
  success  = false
}
`
		if err := os.WriteFile(filepath.Join(subDir, "workflow.hcl"), []byte(calleeHCL), 0o644); err != nil {
			t.Fatal(err)
		}
		parentHCL := `
workflow {
  name          = "parent"
  version       = "0.1"
  initial_state = "call_sub"
  target_state  = "done"
}
adapter "noop" "default" {}
subworkflow "inner" {
  source = "./inner"
}
step "call_sub" {
  target = subworkflow.inner
  outcome "success" { next = step.finish }
  outcome "failure" { next = state.failed }
}
step "finish" {
  target = adapter.noop.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`
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

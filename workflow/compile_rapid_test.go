package workflow

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

type synthStep struct {
	name        string
	maxVisits   int
	successNext string // "step.<x>" or "state.<x>" or "switch.<x>"
	retryNext   string // optional extra outcome target ("step.<x>")
	failureNext string // "state.failed" or "step.<x>"
}

type synthSwitch struct {
	name        string
	condStep    string
	matchNext   string
	defaultNext string
}

type synthWorkflow struct {
	steps         []synthStep
	switches      []synthSwitch
	maxTotalSteps int
}

func (w synthWorkflow) RenderHCL() string {
	var sb strings.Builder
	sb.WriteString("workflow {\n")
	sb.WriteString("  name          = \"rapid_synth\"\n")
	sb.WriteString("  version       = \"1\"\n")
	sb.WriteString(fmt.Sprintf("  initial_state = %q\n", w.steps[0].name))
	sb.WriteString("  target_state  = \"done\"\n")
	if w.maxTotalSteps > 0 {
		sb.WriteString("  policy {\n")
		sb.WriteString(fmt.Sprintf("    max_total_steps = %d\n", w.maxTotalSteps))
		sb.WriteString("  }\n")
	}
	sb.WriteString("}\n\n")
	sb.WriteString("adapter \"noop\" \"default\" {}\n\n")

	for _, st := range w.steps {
		sb.WriteString(fmt.Sprintf("step %q {\n", st.name))
		sb.WriteString("  target = adapter.noop.default\n")
		if st.maxVisits > 0 {
			sb.WriteString(fmt.Sprintf("  max_visits = %d\n", st.maxVisits))
		}
		if st.successNext != "" {
			sb.WriteString(fmt.Sprintf("  outcome \"success\" { next = %s }\n", st.successNext))
		}
		if st.retryNext != "" {
			sb.WriteString(fmt.Sprintf("  outcome \"retry\"   { next = %s }\n", st.retryNext))
		}
		if st.failureNext != "" {
			sb.WriteString(fmt.Sprintf("  outcome \"failure\" { next = %s }\n", st.failureNext))
		}
		sb.WriteString("}\n\n")
	}

	for _, sw := range w.switches {
		sb.WriteString(fmt.Sprintf("switch %q {\n", sw.name))
		sb.WriteString("  match {\n")
		sb.WriteString(fmt.Sprintf("    condition = steps.%s.outcome == \"success\"\n", sw.condStep))
		sb.WriteString(fmt.Sprintf("    next      = %s\n", sw.matchNext))
		sb.WriteString("  }\n")
		sb.WriteString("  default {\n")
		sb.WriteString(fmt.Sprintf("    next = %s\n", sw.defaultNext))
		sb.WriteString("  }\n")
		sb.WriteString("}\n\n")
	}

	sb.WriteString("state \"done\" {\n  terminal = true\n  success  = true\n}\n\n")
	sb.WriteString("state \"failed\" {\n  terminal = true\n  success  = false\n}\n")
	return sb.String()
}

func genValidWorkflow() *rapid.Generator[synthWorkflow] {
	return rapid.Custom(func(t *rapid.T) synthWorkflow {
		n := rapid.IntRange(2, 7).Draw(t, "numSteps")
		useSwitch := rapid.Bool().Draw(t, "useSwitch")

		steps := make([]synthStep, n)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("step_%d", i)
			maxVisits := rapid.SampledFrom([]int{0, 2, 3, 5}).Draw(t, fmt.Sprintf("maxVisits_%d", i))
			nextRef := "state.done"
			if i < n-1 {
				nextRef = fmt.Sprintf("step.step_%d", i+1)
			}
			// Optional cyclic retry back-edge to an earlier or same step.
			retryRef := ""
			if rapid.Bool().Draw(t, fmt.Sprintf("hasRetry_%d", i)) {
				backIdx := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("retryTarget_%d", i))
				retryRef = fmt.Sprintf("step.step_%d", backIdx)
			}
			failRef := "state.failed"
			if i < n-1 && rapid.Bool().Draw(t, fmt.Sprintf("failToNext_%d", i)) {
				failRef = fmt.Sprintf("step.step_%d", i+1)
			}
			steps[i] = synthStep{
				name:        name,
				maxVisits:   maxVisits,
				successNext: nextRef,
				retryNext:   retryRef,
				failureNext: failRef,
			}
		}

		var switches []synthSwitch
		if useSwitch && n >= 2 {
			// Route the last step's success through a switch to state.done / state.failed.
			last := n - 1
			steps[last].successNext = "switch.final_gate"
			switches = append(switches, synthSwitch{
				name:        "final_gate",
				condStep:    steps[last].name,
				matchNext:   "state.done",
				defaultNext: "state.failed",
			})
		}

		// Shortest path along the spine takes n steps; pick budget >= n.
		extraBudget := rapid.IntRange(0, 30).Draw(t, "extraBudget")
		return synthWorkflow{
			steps:         steps,
			switches:      switches,
			maxTotalSteps: n + extraBudget,
		}
	})
}

type mutatedCase struct {
	kind           string
	hcl            string
	expectedSubstr string
}

func genMutatedInvalidWorkflow() *rapid.Generator[mutatedCase] {
	return rapid.Custom(func(t *rapid.T) mutatedCase {
		base := genValidWorkflow().Draw(t, "base")
		mutation := rapid.SampledFrom([]string{
			"orphan_step",
			"black_hole_cycle",
			"unreachable_target_state",
			"impossible_step_budget",
			"incomplete_outcomes",
			"duplicate_switch_condition",
		}).Draw(t, "mutation")

		switch mutation {
		case "orphan_step":
			base.steps = append(base.steps, synthStep{
				name:        "orphan_node",
				successNext: "state.done",
				failureNext: "state.failed",
			})
			return mutatedCase{
				kind:           mutation,
				hcl:            base.RenderHCL(),
				expectedSubstr: `step "orphan_node" is unreachable from initial_state`,
			}

		case "black_hole_cycle":
			// Append a 2-step inescapable cycle reachable from step_0.retry.
			base.steps[0].retryNext = "step.trap_a"
			base.steps = append(base.steps,
				synthStep{
					name:        "trap_a",
					successNext: "step.trap_b",
					failureNext: "step.trap_b",
				},
				synthStep{
					name:        "trap_b",
					successNext: "step.trap_a",
					failureNext: "step.trap_a",
				},
			)
			return mutatedCase{
				kind:           mutation,
				hcl:            base.RenderHCL(),
				expectedSubstr: `livelock detected: step "trap_a" enters cycle [trap_a -> trap_b -> trap_a] with no exit path to any terminal state`,
			}

		case "unreachable_target_state":
			// Redirect all edges targeting state.done to state.failed.
			for i := range base.steps {
				if base.steps[i].successNext == "state.done" {
					base.steps[i].successNext = "state.failed"
				}
				if base.steps[i].failureNext == "state.done" {
					base.steps[i].failureNext = "state.failed"
				}
			}
			for i := range base.switches {
				if base.switches[i].matchNext == "state.done" {
					base.switches[i].matchNext = "state.failed"
				}
				if base.switches[i].defaultNext == "state.done" {
					base.switches[i].defaultNext = "state.failed"
				}
			}
			return mutatedCase{
				kind:           mutation,
				hcl:            base.RenderHCL(),
				expectedSubstr: `target_state "done" is unreachable from initial_state "step_0": no happy path to target_state exists`,
			}

		case "impossible_step_budget":
			// Force strict linear chain step_0 -> step_1 -> ... -> step_{n-1} -> done
			// so shortest path is exactly len(base.steps), and set budget < len(base.steps).
			n := len(base.steps)
			for i := 0; i < n; i++ {
				if i < n-1 {
					base.steps[i].successNext = fmt.Sprintf("step.step_%d", i+1)
				} else {
					base.steps[i].successNext = "state.done"
				}
				base.steps[i].retryNext = ""
				base.steps[i].failureNext = "state.failed"
			}
			base.switches = nil
			base.maxTotalSteps = rapid.IntRange(1, n-1).Draw(t, "impossibleBudget")
			return mutatedCase{
				kind:           mutation,
				hcl:            base.RenderHCL(),
				expectedSubstr: fmt.Sprintf("requires %d steps, but budget is %d", n, base.maxTotalSteps),
			}

		case "incomplete_outcomes":
			idx := rapid.IntRange(0, len(base.steps)-1).Draw(t, "incompleteStepIdx")
			base.steps[idx].failureNext = ""
			return mutatedCase{
				kind:           mutation,
				hcl:            base.RenderHCL(),
				expectedSubstr: fmt.Sprintf("step %q: outcome completeness requires handling adapter failure/error paths", base.steps[idx].name),
			}

		default: // duplicate_switch_condition
			last := len(base.steps) - 1
			base.steps[last].successNext = "switch.dup_switch"
			base.switches = nil
			hcl := base.RenderHCL() + fmt.Sprintf(`
switch "dup_switch" {
  match {
    condition = steps.%s.outcome == "success"
    next      = state.done
  }
  match {
    condition = steps.%s.outcome == "success"
    next      = state.failed
  }
  default {
    next = state.failed
  }
}
`, base.steps[last].name, base.steps[last].name)
			return mutatedCase{
				kind:           mutation,
				hcl:            hcl,
				expectedSubstr: `switch "dup_switch" match[1]: duplicate match condition`,
			}
		}
	})
}

func TestRapid_CompilerAcceptsValidWorkflows(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		wf := genValidWorkflow().Draw(t, "workflow")
		src := wf.RenderHCL()
		spec, parseDiags := Parse("synth.hcl", []byte(src))
		if parseDiags.HasErrors() {
			t.Fatalf("parse failed on valid workflow:\n%s\nHCL:\n%s", allDiagMessages(parseDiags), src)
		}
		_, compileDiags := Compile(spec, nil)
		if compileDiags.HasErrors() {
			t.Fatalf("compile failed on valid workflow:\n%s\nHCL:\n%s", allDiagMessages(compileDiags), src)
		}
	})
}

func TestRapid_CompilerRejectsMutatedWorkflows(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		mc := genMutatedInvalidWorkflow().Draw(t, "mutated")
		spec, parseDiags := Parse("mutated.hcl", []byte(mc.hcl))
		if parseDiags.HasErrors() {
			t.Fatalf("unexpected parse failure for %s:\n%s", mc.kind, allDiagMessages(parseDiags))
		}
		_, compileDiags := Compile(spec, nil)
		if !compileDiags.HasErrors() {
			t.Fatalf("expected compile error for mutation %q, but compilation succeeded!\nHCL:\n%s", mc.kind, mc.hcl)
		}
		msgs := allDiagMessages(compileDiags)
		if !strings.Contains(msgs, mc.expectedSubstr) {
			t.Fatalf("mutation %q: expected diagnostic containing %q, got:\n%s\nHCL:\n%s", mc.kind, mc.expectedSubstr, msgs, mc.hcl)
		}
	})
}

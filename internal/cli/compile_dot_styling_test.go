package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// --- Unit tests for buildAdapterColorMap ---

// makeAdapterGraph builds a minimal FSMGraph with AdapterOrder and Adapters
// populated from the provided list of "<type>.<name>" refs.
func makeAdapterGraph(refs []string) *workflow.FSMGraph {
	g := &workflow.FSMGraph{
		Adapters:     make(map[string]*workflow.AdapterNode, len(refs)),
		AdapterOrder: refs,
	}
	for _, ref := range refs {
		parts := strings.SplitN(ref, ".", 2)
		typeName, name := parts[0], ""
		if len(parts) == 2 {
			name = parts[1]
		}
		g.Adapters[ref] = &workflow.AdapterNode{Type: typeName, Name: name}
	}
	return g
}

// TestBuildAdapterColorMap_AssignsPaletteInOrder verifies that two distinct
// adapter types receive palette entries in declaration order.
func TestBuildAdapterColorMap_AssignsPaletteInOrder(t *testing.T) {
	g := makeAdapterGraph([]string{"shell.default", "noop.default"})
	colors := buildAdapterColorMap(g)

	if c := colors["shell"]; c != dotAdapterPalette[0] {
		t.Errorf("shell: got %q, want %q", c, dotAdapterPalette[0])
	}
	if c := colors["noop"]; c != dotAdapterPalette[1] {
		t.Errorf("noop: got %q, want %q", c, dotAdapterPalette[1])
	}
	if colors["shell"] == colors["noop"] {
		t.Errorf("two distinct adapter types must receive different colors; both got %q", colors["shell"])
	}
}

// TestBuildAdapterColorMap_WrapsAtPaletteEnd verifies that when more distinct
// adapter types exist than palette entries, colors wrap (modulo palette length).
func TestBuildAdapterColorMap_WrapsAtPaletteEnd(t *testing.T) {
	// Build one more type than the palette has entries.
	n := len(dotAdapterPalette) + 1
	refs := make([]string, n)
	for i := range refs {
		refs[i] = fmt.Sprintf("type%d.inst", i)
	}
	g := makeAdapterGraph(refs)
	colors := buildAdapterColorMap(g)

	// The (n-1)th type (index len(palette)) must wrap to palette[0].
	lastType := fmt.Sprintf("type%d", len(dotAdapterPalette))
	if c := colors[lastType]; c != dotAdapterPalette[0] {
		t.Errorf("wrap: type at index %d got %q, want %q", len(dotAdapterPalette), c, dotAdapterPalette[0])
	}
}

// TestBuildAdapterColorMap_SameTypeMultipleInstances verifies that two adapters
// of the same type share the same color and only consume one palette slot.
func TestBuildAdapterColorMap_SameTypeMultipleInstances(t *testing.T) {
	// shell.default and shell.alt are two instances of "shell".
	g := makeAdapterGraph([]string{"shell.default", "shell.alt", "noop.default"})
	colors := buildAdapterColorMap(g)

	if len(colors) != 2 {
		t.Errorf("expected 2 distinct color entries (shell + noop), got %d: %v", len(colors), colors)
	}
	if colors["shell"] == colors["noop"] {
		t.Errorf("shell and noop must have different colors; both got %q", colors["shell"])
	}
	// shell gets palette[0], noop gets palette[1] — only one slot used for shell.
	if c := colors["shell"]; c != dotAdapterPalette[0] {
		t.Errorf("shell: got %q, want %q (palette[0])", c, dotAdapterPalette[0])
	}
	if c := colors["noop"]; c != dotAdapterPalette[1] {
		t.Errorf("noop: got %q, want %q (palette[1])", c, dotAdapterPalette[1])
	}
}

// --- Render tests using compileWorkflowOutput ---

var hexColorRE = regexp.MustCompile(`#[0-9A-Fa-f]{6}`)

// TestDOT_StepHasFillColor compiles a single-adapter workflow and verifies the
// step node line contains style=filled and a valid hex fillcolor.
func TestDOT_StepHasFillColor(t *testing.T) {
	const hcl = `
workflow "styled" {
  version       = "1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	if !strings.Contains(dot, `style="filled"`) {
		t.Errorf("step node must have style=filled; got:\n%s", dot)
	}
	if !strings.Contains(dot, "fillcolor=") {
		t.Errorf("step node must have a fillcolor attribute; got:\n%s", dot)
	}
	for _, line := range strings.Split(dot, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `"work" [`) {
			if !hexColorRE.MatchString(line) {
				t.Errorf("fillcolor value must be a hex color like #RRGGBB; got line: %s", line)
			}
		}
	}
}

// TestDOT_TwoAdapterTypesDifferentColors compiles a workflow with two steps
// targeting two different adapter types and verifies they receive distinct fill colors.
func TestDOT_TwoAdapterTypesDifferentColors(t *testing.T) {
	const hcl = `
workflow "two_adapters" {
  version       = "1"
  initial_state = "step_a"
  target_state  = "done"
}
adapter "noop" "a" {}
adapter "shell" "b" {
  config { }
}
step "step_a" {
  target = adapter.noop.a
  outcome "success" { next = "step_b" }
}
step "step_b" {
  target = adapter.shell.b
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)

	colorOf := func(nodeName string) string {
		for _, line := range strings.Split(dot, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), `"`+nodeName+`" [`) {
				m := hexColorRE.FindString(line)
				return m
			}
		}
		return ""
	}

	colorA := colorOf("step_a")
	colorB := colorOf("step_b")
	if colorA == "" {
		t.Fatalf("could not find fillcolor for step_a in:\n%s", dot)
	}
	if colorB == "" {
		t.Fatalf("could not find fillcolor for step_b in:\n%s", dot)
	}
	if colorA == colorB {
		t.Errorf("steps with different adapter types must have different fill colors; both got %q\n%s", colorA, dot)
	}
}

// TestDOT_SubworkflowStepColor verifies that the fallback shape=component node
// for a subworkflow step without a compiled body uses the fixed semantic fill color.
func TestDOT_SubworkflowStepColor(t *testing.T) {
	st := &workflow.StepNode{Name: "delegate", SubworkflowRef: "inner"}
	got := dotStepAttrs("delegate", st, nil)
	if !strings.Contains(got, "shape=component") {
		t.Errorf("subworkflow fallback node must use shape=component; got: %s", got)
	}
	want := fmt.Sprintf(`fillcolor=%q`, dotSubworkflowFill)
	if !strings.Contains(got, want) {
		t.Errorf("subworkflow fallback node must use fixed fill %q; got: %s", dotSubworkflowFill, got)
	}
}

// TestDOT_ForEachStepDashedBorder verifies that a for_each step renders with
// style="filled,dashed".
func TestDOT_ForEachStepDashedBorder(t *testing.T) {
	const hcl = `
workflow "dashed" {
  version       = "1"
  initial_state = "fan"
  target_state  = "done"
}
adapter "noop" "default" {}
step "fan" {
  target   = adapter.noop.default
  for_each = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	if !strings.Contains(dot, `style="filled,dashed"`) {
		t.Errorf("for_each step must have style=filled,dashed; got:\n%s", dot)
	}
}

// TestDOT_ParallelStepDoublePeripheries verifies that a parallel step renders
// with peripheries=2.
func TestDOT_ParallelStepDoublePeripheries(t *testing.T) {
	const hcl = `
workflow "double_border" {
  version       = "1"
  initial_state = "concurrent"
  target_state  = "done"
}
adapter "noop" "default" {}
step "concurrent" {
  target   = adapter.noop.default
  parallel = ["x", "y"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	if !strings.Contains(dot, "peripheries=2") {
		t.Errorf("parallel step must have peripheries=2; got:\n%s", dot)
	}
}

// TestDOT_SwitchFillColor verifies that switch nodes are rendered with the fixed
// semantic fill color.
func TestDOT_SwitchFillColor(t *testing.T) {
	const hcl = `
workflow "switched" {
  version       = "1"
  initial_state = "build"
  target_state  = "done"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" { next = "decide" }
}
switch "decide" {
  condition {
    match = true
    next  = state.done
  }
  default { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	wantFill := fmt.Sprintf(`fillcolor=%q`, dotSwitchFill)
	if !strings.Contains(dot, wantFill) {
		t.Errorf("switch node must have fillcolor=%q; got:\n%s", dotSwitchFill, dot)
	}
	if !strings.Contains(dot, "shape=diamond") {
		t.Errorf("switch node must still use shape=diamond; got:\n%s", dot)
	}
}

// TestDOT_TerminalSuccessStateFill verifies that a terminal success state is
// rendered with the fixed green fill color.
func TestDOT_TerminalSuccessStateFill(t *testing.T) {
	const hcl = `
workflow "success_fill" {
  version       = "1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	dot := compileDOTFromHCL(t, hcl)
	wantFill := fmt.Sprintf(`fillcolor=%q`, dotSuccessFill)
	for _, line := range strings.Split(dot, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `"done" [`) {
			if !strings.Contains(line, wantFill) {
				t.Errorf("terminal success state must have fillcolor=%q; got line: %s", dotSuccessFill, line)
			}
			if !strings.Contains(line, "shape=doublecircle") {
				t.Errorf("terminal success state must have shape=doublecircle; got line: %s", line)
			}
		}
	}
}

// TestDOT_TerminalFailureStateFill verifies that a terminal failure state is
// rendered with the fixed pink fill color.
func TestDOT_TerminalFailureStateFill(t *testing.T) {
	const hcl = `
workflow "failure_fill" {
  version       = "1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}
state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`
	dot := compileDOTFromHCL(t, hcl)
	wantFill := fmt.Sprintf(`fillcolor=%q`, dotFailureFill)
	for _, line := range strings.Split(dot, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `"failed" [`) {
			if !strings.Contains(line, wantFill) {
				t.Errorf("terminal failure state must have fillcolor=%q; got line: %s", dotFailureFill, line)
			}
			if !strings.Contains(line, "shape=doublecircle") {
				t.Errorf("terminal failure state must have shape=doublecircle; got line: %s", line)
			}
		}
	}
}

// TestDOT_NonTerminalStateNoFill verifies that a non-terminal state is rendered
// without a fillcolor attribute.
func TestDOT_NonTerminalStateNoFill(t *testing.T) {
	// Build a workflow with a non-terminal state used as an intermediate state.
	// We need a switch to route through a non-terminal state without it being a step.
	// The simplest approach: use a switch whose target is a non-terminal state,
	// which then transitions via another step.
	// Actually, "state" blocks in HCL that lack terminal=true are non-terminal.
	// We can use initial_state pointing to a non-terminal state which is the start.
	// But a non-terminal state needs a next step... Let's use a simple workflow:
	// initial_state is a non-terminal state called "waiting", target_state is "done".
	// We need at least one step for the workflow to be valid, so we'll use a step
	// that transitions out of a non-terminal state.
	const hcl = `
workflow "non_terminal" {
  version       = "1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" {
  terminal = true
  success  = true
}
`
	// "done" is terminal; the "work" step is not a state. We need a non-terminal state.
	// The simplest non-terminal state that can appear is via a requires= state or
	// by having a step outcome point to a non-terminal state. Let's add one.
	const hcl2 = `
workflow "non_terminal_state" {
  version       = "1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
  outcome "retry"   { next = "pending" }
}
state "pending" {}
state "done" {
  terminal = true
  success  = true
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(hcl2), 0o644); err != nil {
		t.Fatalf("write hcl: %v", err)
	}
	out, err := compileWorkflowOutput(context.Background(), dir, "dot", nil)
	if err != nil {
		t.Fatalf("compile dot: %v", err)
	}
	dot := string(out)
	for _, line := range strings.Split(dot, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `"pending" [`) {
			if strings.Contains(line, "fillcolor=") {
				t.Errorf("non-terminal state must not have fillcolor; got line: %s", line)
			}
			if strings.Contains(line, "style=") {
				t.Errorf("non-terminal state must not have style; got line: %s", line)
			}
		}
	}
}

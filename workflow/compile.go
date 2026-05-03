// Package workflow compiles HCL workflow definitions into an executable FSMGraph.
package workflow

// compile.go — Compile entry point and graph-level validation passes
// (transition resolution and reachability analysis).

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

const (
	onCrashFail     = "fail"
	onCrashRespawn  = "respawn"
	onCrashAbortRun = "abort_run"

	lifecycleOpen  = "open"
	lifecycleClose = "close"
)

// CompileOpts carries optional configuration for the Compile pass.
type CompileOpts struct {
	// WorkflowDir is the directory containing the HCL file being compiled.
	// When set, compile-time validation of constant file() arguments is
	// enabled: missing files produce HCL diagnostics with source ranges.
	WorkflowDir string
	// LoadDepth tracks the current inline sub-workflow nesting depth. The
	// compiler increments this for each recursive CompileWithOpts call when
	// compiling a workflow-type step body. Maximum depth is 4.
	LoadDepth int
	// LoadedFiles tracks file paths already in the load chain for
	// workflow_file cycle detection. It is populated automatically by the
	// compiler when SubWorkflowResolver is set.
	LoadedFiles []string
	// SubWorkflowResolver is an optional callback used to load an external
	// workflow file referenced by workflow_file = "...". When nil, any step
	// using workflow_file is rejected with a compile error. The resolver
	// receives the file path and the WorkflowDir; it must return a parsed
	// *Spec or an error.
	SubWorkflowResolver func(filePath, workflowDir string) (*Spec, error)
}

// Compile validates a Spec and returns an executable FSMGraph. It is a
// convenience wrapper around CompileWithOpts with empty options (no
// compile-time file() validation).
func Compile(spec *Spec, schemas map[string]AdapterInfo) (*FSMGraph, hcl.Diagnostics) {
	return CompileWithOpts(spec, schemas, CompileOpts{})
}

// CompileWithOpts validates a Spec and returns an executable FSMGraph. All
// errors are returned as HCL diagnostics so callers can surface file/line
// context. schemas maps adapter name to its declared AdapterInfo for
// compile-time validation of agent config and step input blocks. Pass nil to
// skip schema validation (permissive mode: any keys accepted).
//
// When opts.WorkflowDir is set, constant file() arguments in step input
// expressions are validated at compile time (path existence + confinement).
func CompileWithOpts(spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) (*FSMGraph, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	if spec.Version == "" {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "workflow.version is required"})
	}
	if spec.InitialState == "" {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "workflow.initial_state is required"})
	}
	if spec.TargetState == "" {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "workflow.target_state is required"})
	}
	if spec.Policy != nil && spec.Policy.MaxVisitsWarnThreshold != nil && *spec.Policy.MaxVisitsWarnThreshold < 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "policy.max_visits_warn_threshold must be >= 0 (use 0 to disable warnings, omit to use the default of 200)"})
	}

	g := newFSMGraph(spec)
	diags = append(diags, compileVariables(g, spec)...)
	diags = append(diags, compileLocals(g, spec, opts)...)
	diags = append(diags, compileOutputs(g, spec, opts)...)
	diags = append(diags, compileAgents(g, spec, schemas, opts)...)
	diags = append(diags, compileStates(g, spec)...)
	diags = append(diags, compileSteps(g, spec, schemas, opts)...)
	diags = append(diags, compileWaits(g, spec)...)
	diags = append(diags, compileApprovals(g, spec)...)
	diags = append(diags, compileBranches(g, spec, opts)...)
	// Warn after all nodes are compiled so branch/wait/approval targets are
	// available for the back-edge walk (W07).
	diags = append(diags, warnBackEdges(g)...)
	// Reserved-name checks only apply to user-authored top-level workflows.
	// Sub-workflow bodies (LoadDepth > 0) are synthetic and intentionally use
	// the "_continue" name as a terminal state.
	if opts.LoadDepth == 0 {
		diags = append(diags, checkReservedNames(spec)...)
	}
	diags = append(diags, resolveTransitions(g)...)
	if g.InitialState != "" && !diags.HasErrors() {
		diags = append(diags, checkReachability(g)...)
	}

	if diags.HasErrors() {
		return nil, diags
	}
	return g, diags
}

// newFSMGraph allocates a fresh FSMGraph seeded from spec's top-level fields.
func newFSMGraph(spec *Spec) *FSMGraph {
	g := &FSMGraph{
		Name:         spec.Name,
		InitialState: spec.InitialState,
		TargetState:  spec.TargetState,
		Variables:    map[string]*VariableNode{},
		Locals:       map[string]*LocalNode{},
		Outputs:      map[string]*OutputNode{},
		OutputOrder:  []string{},
		Agents:       map[string]*AgentNode{},
		Steps:        map[string]*StepNode{},
		States:       map[string]*StateNode{},
		Waits:        map[string]*WaitNode{},
		Approvals:    map[string]*ApprovalNode{},
		Branches:     map[string]*BranchNode{},
		Policy:       DefaultPolicy,
	}
	if spec.Policy != nil {
		if spec.Policy.MaxTotalSteps > 0 {
			g.Policy.MaxTotalSteps = spec.Policy.MaxTotalSteps
		}
		if spec.Policy.MaxStepRetries > 0 {
			g.Policy.MaxStepRetries = spec.Policy.MaxStepRetries
		}
		// MaxVisitsWarnThreshold: nil means "not set" (keep default of 200);
		// 0 explicitly disables the warning; positive values override the default.
		// Negative values are rejected at compile time before this point.
		if spec.Policy.MaxVisitsWarnThreshold != nil {
			g.Policy.MaxVisitsWarnThreshold = *spec.Policy.MaxVisitsWarnThreshold
		}
	}
	return g
}

// resolveTransitions verifies that initial_state, target_state, and all
// outcome targets refer to declared graph nodes.
func resolveTransitions(g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if g.InitialState != "" {
		if _, ok := g.Lookup(g.InitialState); !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("initial_state %q does not refer to a declared step or state", g.InitialState)})
		}
	}
	if g.TargetState != "" {
		kind, ok := g.Lookup(g.TargetState)
		if !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("target_state %q does not refer to a declared step or state", g.TargetState)})
		} else if kind == "state" && !g.States[g.TargetState].Terminal {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("target_state %q must be terminal", g.TargetState)})
		}
	}
	for _, step := range g.Steps {
		for outcome, target := range step.Outcomes {
			if target == "_continue" {
				// _continue is a synthetic engine-internal target, not a graph node.
				// It is only valid inside for_each do-steps; reachability validation
				// is deferred to runtime.
				continue
			}
			if _, ok := g.Lookup(target); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("step %q outcome %q -> unknown target %q", step.Name, outcome, target),
				})
			}
		}
	}
	for _, wait := range g.Waits {
		for outcome, target := range wait.Outcomes {
			if _, ok := g.Lookup(target); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("wait %q outcome %q -> unknown target %q", wait.Name, outcome, target),
				})
			}
		}
	}
	for _, appr := range g.Approvals {
		for outcome, target := range appr.Outcomes {
			if _, ok := g.Lookup(target); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("approval %q outcome %q -> unknown target %q", appr.Name, outcome, target),
				})
			}
		}
	}
	for _, br := range g.Branches {
		for i, arm := range br.Arms {
			if _, ok := g.Lookup(arm.Target); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("branch %q arm[%d] -> unknown target %q", br.Name, i, arm.Target),
				})
			}
		}
		if _, ok := g.Lookup(br.DefaultTarget); !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("branch %q default -> unknown target %q", br.Name, br.DefaultTarget),
			})
		}
	}
	return diags
}

// checkReachability performs a reachability walk from g.InitialState and
// emits diagnostics for unreachable nodes.
func checkReachability(g *FSMGraph) hcl.Diagnostics {
	var diags hcl.Diagnostics
	reachable := map[string]bool{g.InitialState: true}
	var walk func(name string)
	walk = func(name string) {
		if step, isStep := g.Steps[name]; isStep {
			for _, target := range step.Outcomes {
				if target == "_continue" {
					// _continue is synthetic; the for_each's outgoing edges drive reachability.
					continue
				}
				if !reachable[target] {
					reachable[target] = true
					walk(target)
				}
			}
			return
		}
		if wait, isWait := g.Waits[name]; isWait {
			for _, target := range wait.Outcomes {
				if !reachable[target] {
					reachable[target] = true
					walk(target)
				}
			}
			return
		}
		if appr, isAppr := g.Approvals[name]; isAppr {
			for _, target := range appr.Outcomes {
				if !reachable[target] {
					reachable[target] = true
					walk(target)
				}
			}
			return
		}
		if br, isBranch := g.Branches[name]; isBranch {
			for _, arm := range br.Arms {
				if !reachable[arm.Target] {
					reachable[arm.Target] = true
					walk(arm.Target)
				}
			}
			if !reachable[br.DefaultTarget] {
				reachable[br.DefaultTarget] = true
				walk(br.DefaultTarget)
			}
			return
		}
	}
	walk(g.InitialState)
	for _, name := range g.stepOrder {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q is unreachable from initial_state", name)})
		}
	}
	for name := range g.Waits {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: fmt.Sprintf("wait %q is unreachable from initial_state", name)})
		}
	}
	for name := range g.Approvals {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: fmt.Sprintf("approval %q is unreachable from initial_state", name)})
		}
	}
	for name := range g.Branches {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: fmt.Sprintf("branch %q is unreachable from initial_state", name)})
		}
	}
	for name := range g.States {
		if strings.HasPrefix(name, "_") {
			// Synthetic states (e.g. _continue) are internal loop targets;
			// skipping them here avoids spurious "unreachable" warnings.
			continue
		}
		if !reachable[name] {
			// Unreachable terminal states are a warning — they may be intentional placeholders.
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: fmt.Sprintf("state %q is unreachable from initial_state", name)})
		}
	}
	return diags
}

// compileStates compiles all state blocks from spec into g.States.
func compileStates(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, st := range spec.States {
		name := st.Name
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate state %q", name)})
			continue
		}
		node := &StateNode{Name: name, Terminal: st.Terminal, Requires: st.Requires}
		if st.Success != nil {
			node.Success = *st.Success
		} else {
			node.Success = st.Terminal // default: terminal states are successful unless overridden
		}
		g.States[name] = node
	}
	return diags
}

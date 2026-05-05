// Package workflow compiles HCL workflow definitions into an executable FSMGraph.
package workflow

// compile.go — Compile entry point and graph-level validation passes
// (transition resolution and reachability analysis).

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

const (
	onCrashFail     = "fail"
	onCrashRespawn  = "respawn"
	onCrashAbortRun = "abort_run"
)

// SubWorkflowResolver resolves subworkflow source directories.
// ResolveSource resolves a source string ("./path" or "scheme://...")
// to a directory containing one or more .hcl files.
// callerDir is the directory containing the parent workflow (used to resolve relative paths).
// For local paths, the returned dir is the absolute path; for remote sources,
// the resolver fetches into a cache dir.
type SubWorkflowResolver interface {
	ResolveSource(ctx context.Context, callerDir, source string) (dir string, err error)
}

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
	// SubworkflowChain tracks resolved source paths in the current call stack
	// for cycle detection when compiling subworkflows.
	SubworkflowChain []string
	// SubWorkflowResolver is an optional callback used to load an external
	// subworkflow directory referenced by subworkflow.source = "...".
	// When nil, any subworkflow is rejected with a compile error.
	SubWorkflowResolver SubWorkflowResolver
	// Schemas is the adapter schema map propagated into recursive subworkflow
	// compiles so callee adapter config and step input are fully validated.
	// Set by the CLI compile path; nil when compiling standalone without adapters.
	Schemas map[string]AdapterInfo
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
	diags = append(diags, compileEnvironments(g, spec, opts)...)
	diags = append(diags, compileSubworkflows(g, spec, opts)...)
	diags = append(diags, compileOutputs(g, spec, opts)...)
	diags = append(diags, compileAdapters(g, spec, schemas, opts)...)
	diags = append(diags, compileStates(g, spec)...)
	diags = append(diags, compileSteps(g, spec, schemas, opts)...)
	diags = append(diags, compileWaits(g, spec)...)
	diags = append(diags, compileApprovals(g, spec)...)
	diags = append(diags, compileSwitches(g, spec, opts)...)
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
		Environments: map[string]*EnvironmentNode{},
		Outputs:      map[string]*OutputNode{},
		OutputOrder:  []string{},
		Adapters:     map[string]*AdapterNode{},
		AdapterOrder: []string{},
		Subworkflows: map[string]*SubworkflowNode{},
		Steps:        map[string]*StepNode{},
		States:       map[string]*StateNode{},
		Waits:        map[string]*WaitNode{},
		Approvals:    map[string]*ApprovalNode{},
		Switches:     map[string]*SwitchNode{},
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
		for outcome, co := range step.Outcomes {
			if co.Next == "_continue" || co.Next == ReturnSentinel {
				// _continue is a synthetic engine-internal target, not a graph node.
				// "return" is a reserved routing sentinel, not a declared node.
				continue
			}
			if _, ok := g.Lookup(co.Next); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("step %q outcome %q -> unknown target %q", step.Name, outcome, co.Next),
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
	for _, sw := range g.Switches {
		for i, cond := range sw.Conditions {
			if cond.Next == ReturnSentinel {
				continue
			}
			if _, ok := g.Lookup(cond.Next); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("switch %q condition[%d] -> unknown target %q", sw.Name, i, cond.Next),
				})
			}
		}
		if sw.DefaultNext != ReturnSentinel && sw.DefaultNext != "" {
			if _, ok := g.Lookup(sw.DefaultNext); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("switch %q default -> unknown target %q", sw.Name, sw.DefaultNext),
				})
			}
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
			for _, co := range step.Outcomes {
				if co.Next == "_continue" || co.Next == ReturnSentinel {
					// _continue is synthetic; the for_each's outgoing edges drive reachability.
					// "return" is not a real node.
					continue
				}
				if !reachable[co.Next] {
					reachable[co.Next] = true
					walk(co.Next)
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
		if sw, isSwitch := g.Switches[name]; isSwitch {
			for _, cond := range sw.Conditions {
				if cond.Next == ReturnSentinel {
					continue
				}
				if !reachable[cond.Next] {
					reachable[cond.Next] = true
					walk(cond.Next)
				}
			}
			if sw.DefaultNext != ReturnSentinel && sw.DefaultNext != "" && !reachable[sw.DefaultNext] {
				reachable[sw.DefaultNext] = true
				walk(sw.DefaultNext)
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
	for name := range g.Switches {
		if !reachable[name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: fmt.Sprintf("switch %q is unreachable from initial_state", name)})
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

package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
)

const (
	onCrashFail     = "fail"
	onCrashRespawn  = "respawn"
	onCrashAbortRun = "abort_run"

	lifecycleOpen  = "open"
	lifecycleClose = "close"
)

// Compile validates a Spec and returns an executable FSMGraph. All errors are
// returned as HCL diagnostics so callers can surface file/line context.
func Compile(spec *Spec) (*FSMGraph, hcl.Diagnostics) {
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

	g := &FSMGraph{
		Name:         spec.Name,
		InitialState: spec.InitialState,
		TargetState:  spec.TargetState,
		Agents:       map[string]*AgentNode{},
		Steps:        map[string]*StepNode{},
		States:       map[string]*StateNode{},
		Policy:       DefaultPolicy,
	}
	if spec.Policy != nil {
		if spec.Policy.MaxTotalSteps > 0 {
			g.Policy.MaxTotalSteps = spec.Policy.MaxTotalSteps
		}
		if spec.Policy.MaxStepRetries > 0 {
			g.Policy.MaxStepRetries = spec.Policy.MaxStepRetries
		}
	}

	// First pass: declare agents and states, detect duplicates.
	for _, ag := range spec.Agents {
		name := ag.Name
		if _, dup := g.Agents[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate agent %q", name)})
			continue
		}
		if !isValidAdapterName(ag.Adapter) {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("agent %q: invalid adapter %q", name, ag.Adapter)})
		}
		effectiveOnCrash := ag.OnCrash
		if effectiveOnCrash == "" {
			effectiveOnCrash = onCrashFail
		} else if !isValidOnCrash(effectiveOnCrash) {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("agent %q: invalid on_crash %q", name, ag.OnCrash)})
		}
		g.Agents[name] = &AgentNode{
			Name:    name,
			Adapter: ag.Adapter,
			OnCrash: effectiveOnCrash,
		}
	}

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
	for _, sp := range spec.Steps {
		if _, dup := g.Steps[sp.Name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate step %q", sp.Name)})
			continue
		}
		if _, clash := g.States[sp.Name]; clash {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q clashes with state of the same name", sp.Name)})
			continue
		}
		hasAdapter := sp.Adapter != ""
		hasAgent := sp.Agent != ""
		if hasAdapter == hasAgent {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: exactly one of adapter or agent must be set", sp.Name)})
		}
		if hasAdapter && !isValidAdapterName(sp.Adapter) {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid adapter %q", sp.Name, sp.Adapter)})
		}
		if hasAgent {
			if _, ok := g.Agents[sp.Agent]; !ok {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: unknown agent %q", sp.Name, sp.Agent)})
			}
		}
		if sp.Lifecycle != "" {
			// Compile validates lifecycle syntax only. Runtime is responsible for
			// enforcing use-before-open/double-open and other session state rules.
			if !isValidLifecycle(sp.Lifecycle) {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid lifecycle %q", sp.Name, sp.Lifecycle)})
			}
			if !hasAgent {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: lifecycle requires agent", sp.Name)})
			}
			if sp.Lifecycle == lifecycleClose && len(sp.Config) > 0 {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: lifecycle \"close\" must not include config", sp.Name)})
			}
		}
		effectiveOnCrash := sp.OnCrash
		if effectiveOnCrash != "" && !isValidOnCrash(effectiveOnCrash) {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid on_crash %q", sp.Name, sp.OnCrash)})
		}
		if effectiveOnCrash == "" {
			if hasAgent {
				if agent, ok := g.Agents[sp.Agent]; ok {
					effectiveOnCrash = agent.OnCrash
				} else {
					effectiveOnCrash = onCrashFail
				}
			} else {
				effectiveOnCrash = onCrashFail
			}
		}
		var timeout time.Duration
		if sp.Timeout != "" {
			d, err := time.ParseDuration(sp.Timeout)
			if err != nil {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid timeout %q: %v", sp.Name, sp.Timeout, err)})
			}
			timeout = d
		}
		node := &StepNode{
			Name:      sp.Name,
			Adapter:   sp.Adapter,
			Agent:     sp.Agent,
			Lifecycle: sp.Lifecycle,
			OnCrash:   effectiveOnCrash,
			Config:    sp.Config,
			Timeout:   timeout,
			Outcomes:  map[string]string{},
		}
		seenOutcome := map[string]bool{}
		for _, o := range sp.Outcomes {
			if seenOutcome[o.Name] {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: duplicate outcome %q", sp.Name, o.Name)})
				continue
			}
			seenOutcome[o.Name] = true
			if o.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q outcome %q: transition_to required", sp.Name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.TransitionTo
		}
		if len(node.Outcomes) == 0 {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
		}
		g.Steps[sp.Name] = node
		g.stepOrder = append(g.stepOrder, sp.Name)
	}

	// Second pass: resolve transitions, ensure initial/target exist and reachable.
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
			if _, ok := g.Lookup(target); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("step %q outcome %q -> unknown target %q", step.Name, outcome, target),
				})
			}
		}
	}

	// Reachability from initial state.
	if g.InitialState != "" && !diags.HasErrors() {
		reachable := map[string]bool{g.InitialState: true}
		var walk func(name string)
		walk = func(name string) {
			step, isStep := g.Steps[name]
			if !isStep {
				return
			}
			for _, target := range step.Outcomes {
				if !reachable[target] {
					reachable[target] = true
					walk(target)
				}
			}
		}
		walk(g.InitialState)
		for _, name := range g.stepOrder {
			if !reachable[name] {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q is unreachable from initial_state", name)})
			}
		}
		for name := range g.States {
			if !reachable[name] {
				// Unreachable terminal states are a warning, not an error — they may be intentional placeholders.
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: fmt.Sprintf("state %q is unreachable from initial_state", name)})
			}
		}
	}

	if diags.HasErrors() {
		return nil, diags
	}
	return g, diags
}

func isValidOnCrash(v string) bool {
	switch v {
	case onCrashFail, onCrashRespawn, onCrashAbortRun:
		return true
	default:
		return false
	}
}

func isValidLifecycle(v string) bool {
	switch v {
	case lifecycleOpen, lifecycleClose:
		return true
	default:
		return false
	}
}

func isValidAdapterName(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

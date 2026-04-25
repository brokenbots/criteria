// Package workflow defines the HCL workflow schema, parser, and the compiled
// FSM graph that the Overseer engine executes.
package workflow

import "time"

// Spec is the parsed (but unvalidated) HCL workflow document.
type Spec struct {
	Name         string      `hcl:"name,label"`
	Version      string      `hcl:"version"`
	InitialState string      `hcl:"initial_state"`
	TargetState  string      `hcl:"target_state"`
	Agents       []AgentSpec `hcl:"agent,block"`
	Steps        []StepSpec  `hcl:"step,block"`
	States       []StateSpec `hcl:"state,block"`
	Policy       *PolicySpec `hcl:"policy,block"`
}

// HCL extensions for session-aware workflows:
// - Top-level `agent "name" { adapter = "..." }` declarations bind names to adapters.
// - Steps use `agent = "name"` to route work to an agent-backed session.
// - Steps with `lifecycle = "open"|"close"` explicitly manage session lifetime.
//   `open` may carry config, while `close` must not include config.
// AgentSpec declares a named long-lived adapter session target.
type AgentSpec struct {
	Name    string `hcl:"name,label"`
	Adapter string `hcl:"adapter"`
	OnCrash string `hcl:"on_crash,optional"`
}

// StepSpec describes a single step in the workflow.
type StepSpec struct {
	Name      string            `hcl:"name,label"`
	Adapter   string            `hcl:"adapter,optional"`
	Agent     string            `hcl:"agent,optional"`
	Lifecycle string            `hcl:"lifecycle,optional"`
	OnCrash   string            `hcl:"on_crash,optional"`
	Config    map[string]string `hcl:"config,optional"`
	Timeout   string            `hcl:"timeout,optional"`
	Outcomes  []OutcomeSpec     `hcl:"outcome,block"`
}

// OutcomeSpec maps an adapter outcome name to a transition target.
type OutcomeSpec struct {
	Name         string `hcl:"name,label"`
	TransitionTo string `hcl:"transition_to"`
}

// StateSpec declares a non-step state (typically terminal or human-gated).
type StateSpec struct {
	Name     string `hcl:"name,label"`
	Terminal bool   `hcl:"terminal,optional"`
	Success  *bool  `hcl:"success,optional"`
	Requires string `hcl:"requires,optional"`
}

// PolicySpec defines global execution guards.
type PolicySpec struct {
	MaxTotalSteps  int `hcl:"max_total_steps,optional"`
	MaxStepRetries int `hcl:"max_step_retries,optional"`
}

// FSMGraph is the validated, executable representation of a workflow.
type FSMGraph struct {
	Name         string
	InitialState string
	TargetState  string
	Agents       map[string]*AgentNode
	Steps        map[string]*StepNode  // by step name
	States       map[string]*StateNode // by state name (terminal etc.)
	Policy       Policy
	// Order of step declarations (stable for diagnostics).
	stepOrder []string
}

// AgentNode is a compiled long-lived adapter declaration.
type AgentNode struct {
	Name    string
	Adapter string
	OnCrash string
}

// StepNode is a compiled step with resolved transitions.
type StepNode struct {
	Name      string
	Adapter   string
	Agent     string
	Lifecycle string
	OnCrash   string
	Config    map[string]string
	Timeout   time.Duration     // zero = no timeout
	Outcomes  map[string]string // outcome name -> target node name (step or state)
}

// StateNode is a compiled (non-step) state.
type StateNode struct {
	Name     string
	Terminal bool
	Success  bool // only meaningful when Terminal
	Requires string
}

// Policy holds resolved engine guards. Defaults are applied during compile.
type Policy struct {
	MaxTotalSteps  int
	MaxStepRetries int
}

// DefaultPolicy is applied when a workflow omits a policy block.
var DefaultPolicy = Policy{
	MaxTotalSteps:  100,
	MaxStepRetries: 0,
}

// IsTerminal reports whether the named node is a terminal state.
func (g *FSMGraph) IsTerminal(name string) bool {
	if s, ok := g.States[name]; ok {
		return s.Terminal
	}
	return false
}

// Lookup returns ("step"|"state", true) if name exists in the graph.
func (g *FSMGraph) Lookup(name string) (kind string, ok bool) {
	if _, ok := g.Steps[name]; ok {
		return "step", true
	}
	if _, ok := g.States[name]; ok {
		return "state", true
	}
	return "", false
}

// StepOrder returns step names in declaration order.
func (g *FSMGraph) StepOrder() []string {
	out := make([]string, len(g.stepOrder))
	copy(out, g.stepOrder)
	return out
}

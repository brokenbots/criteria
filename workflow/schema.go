// Package workflow defines the HCL workflow schema, parser, and the compiled
// FSM graph that the Overseer engine executes.
package workflow

import (
	"time"

	"github.com/hashicorp/hcl/v2"
)

// Spec is the parsed (but unvalidated) HCL workflow document.
type Spec struct {
	Name         string           `hcl:"name,label"`
	Version      string           `hcl:"version"`
	InitialState string           `hcl:"initial_state"`
	TargetState  string           `hcl:"target_state"`
	Agents       []AgentSpec      `hcl:"agent,block"`
	Steps        []StepSpec       `hcl:"step,block"`
	States       []StateSpec      `hcl:"state,block"`
	Policy       *PolicySpec      `hcl:"policy,block"`
	Permissions  *PermissionsSpec `hcl:"permissions,block"`
}

// ConfigSpec holds the raw HCL body of an `agent.config { ... }` block.
// Attributes are decoded into string values by the compiler.
// W04 will upgrade to expression-aware decoding (var.<name>, each.value).
type ConfigSpec struct {
	Remain hcl.Body `hcl:",remain"`
}

// InputSpec holds the raw HCL body of a `step.input { ... }` block.
// Attributes are decoded into string values by the compiler.
// W04 will upgrade to expression-aware decoding (var.<name>, each.value).
// TODO(W04): replace Remain decode with hcl.EvalContext for expression interpolation.
type InputSpec struct {
	Remain hcl.Body `hcl:",remain"`
}

// HCL extensions for session-aware workflows:
//   - Top-level `agent "name" { adapter = "..." }` declarations bind names to adapters.
//   - Steps use `agent = "name"` to route work to an agent-backed session.
//   - Steps with `lifecycle = "open"|"close"` explicitly manage session lifetime.
//     `open` and `close` must not include `input { }`.
//   - Agent-level `config { }` block carries session-open config (replaces open-step config).
//
// AgentSpec declares a named long-lived adapter session target.
type AgentSpec struct {
	Name    string      `hcl:"name,label"`
	Adapter string      `hcl:"adapter"`
	OnCrash string      `hcl:"on_crash,optional"`
	Config  *ConfigSpec `hcl:"config,block"`
}

// StepSpec describes a single step in the workflow.
type StepSpec struct {
	Name      string `hcl:"name,label"`
	Adapter   string `hcl:"adapter,optional"`
	Agent     string `hcl:"agent,optional"`
	Lifecycle string `hcl:"lifecycle,optional"`
	OnCrash   string `hcl:"on_crash,optional"`
	// Config is the legacy map attribute; retained for parse-time detection so the
	// compiler can emit a helpful "use input { } block" diagnostic.
	Config     map[string]string `hcl:"config,optional"`
	Input      *InputSpec        `hcl:"input,block"`
	Timeout    string            `hcl:"timeout,optional"`
	AllowTools []string          `hcl:"allow_tools,optional"`
	Outcomes   []OutcomeSpec     `hcl:"outcome,block"`
	// LegacyConfigRange, when set by Parse, points at the source range for a
	// legacy config = { ... } attribute so compile diagnostics can include
	// file/line context.
	LegacyConfigRange *hcl.Range
}

// ConfigFieldType enumerates the types a config or input field may carry.
type ConfigFieldType int

const (
	ConfigFieldString     ConfigFieldType = iota // "string"
	ConfigFieldNumber                            // "number"
	ConfigFieldBool                              // "bool"
	ConfigFieldListString                        // "list_string"
)

// ConfigField describes a single field in an adapter's config or input schema.
type ConfigField struct {
	Required bool
	Type     ConfigFieldType
	Doc      string
}

// AdapterInfo describes an adapter's declared configuration schema.
// It is used during workflow compilation to validate agent config blocks and
// step input blocks against the adapter's declared requirements.
// An empty (zero-value) AdapterInfo means "any keys accepted" (permissive).
type AdapterInfo struct {
	ConfigSchema map[string]ConfigField // schema for agent-level `config { }` blocks
	InputSchema  map[string]ConfigField // schema for per-step `input { }` blocks
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

// PermissionsSpec defines workflow-level permission allowlists applied to all steps.
type PermissionsSpec struct {
	// AllowTools is the workflow-wide list of glob patterns for permitted tool
	// invocations. Step-level allow_tools is unioned with this list.
	// See StepSpec.AllowTools for matching semantics.
	AllowTools []string `hcl:"allow_tools,optional"`
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
	Config  map[string]string // session-open config from agent.config { }
}

// StepNode is a compiled step with resolved transitions.
type StepNode struct {
	Name      string
	Adapter   string
	Agent     string
	Lifecycle string
	OnCrash   string
	// Input holds the per-step adapter input from the `input { }` block.
	// Wire name on ExecuteRequest proto remains "config" to avoid breaking changes;
	// only the Go-side field is renamed here. W04 will upgrade to map[string]cty.Value.
	Input    map[string]string
	Timeout  time.Duration     // zero = no timeout
	Outcomes map[string]string // outcome name -> target node name (step or state)
	// AllowTools is the union of step-level and workflow-level allow_tools glob
	// patterns. An empty slice means deny-all (default). Only valid on
	// execute-shape steps (Lifecycle == "").
	AllowTools []string
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

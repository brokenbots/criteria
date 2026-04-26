// Package workflow defines the HCL workflow schema, parser, and the compiled
// FSM graph that the Overseer engine executes.
package workflow

import (
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// Spec is the parsed (but unvalidated) HCL workflow document.
type Spec struct {
	Name         string           `hcl:"name,label"`
	Version      string           `hcl:"version"`
	InitialState string           `hcl:"initial_state"`
	TargetState  string           `hcl:"target_state"`
	Variables    []VariableSpec   `hcl:"variable,block"`
	Agents       []AgentSpec      `hcl:"agent,block"`
	Steps        []StepSpec       `hcl:"step,block"`
	States       []StateSpec      `hcl:"state,block"`
	Waits        []WaitSpec       `hcl:"wait,block"`
	Approvals    []ApprovalSpec   `hcl:"approval,block"`
	Branches     []BranchSpec     `hcl:"branch,block"`
	ForEachs     []ForEachSpec    `hcl:"for_each,block"`
	Policy       *PolicySpec      `hcl:"policy,block"`
	Permissions  *PermissionsSpec `hcl:"permissions,block"`
	// SourceBytes holds the raw HCL source that was parsed to produce this Spec.
	// Populated by Parse/ParseFile; used by the compiler to extract expression
	// source text (e.g. for BranchEvaluated.Condition).
	SourceBytes []byte
}

// VariableSpec is the parsed (but unvalidated) variable declaration.
// The `type` and `default` attributes are decoded by the compiler.
type VariableSpec struct {
	Name        string   `hcl:"name,label"`
	TypeStr     string   `hcl:"type,optional"`
	Description string   `hcl:"description,optional"`
	Remain      hcl.Body `hcl:",remain"` // captures the "default" expression
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
	OutputSchema map[string]ConfigField // declared outputs the adapter promises to populate (W04)
}

// OutcomeSpec maps an adapter outcome name to a transition target.
type OutcomeSpec struct {
	Name         string `hcl:"name,label"`
	TransitionTo string `hcl:"transition_to"`
}

// WaitSpec declares a wait node. Exactly one of duration or signal must be set.
type WaitSpec struct {
	Name     string        `hcl:"name,label"`
	Duration string        `hcl:"duration,optional"`
	Signal   string        `hcl:"signal,optional"`
	Outcomes []OutcomeSpec `hcl:"outcome,block"`
}

// ApprovalSpec declares an approval node. Must have both "approved" and
// "rejected" outcomes.
type ApprovalSpec struct {
	Name      string        `hcl:"name,label"`
	Approvers []string      `hcl:"approvers"`
	Reason    string        `hcl:"reason"`
	Outcomes  []OutcomeSpec `hcl:"outcome,block"`
}

// StateSpec declares a non-step state (typically terminal or human-gated).
type StateSpec struct {
	Name     string `hcl:"name,label"`
	Terminal bool   `hcl:"terminal,optional"`
	Success  *bool  `hcl:"success,optional"`
	Requires string `hcl:"requires,optional"`
}

// BranchSpec declares a branch node. Arms are evaluated in declaration order;
// the first truthy arm wins. Default is required.
type BranchSpec struct {
	Name    string          `hcl:"name,label"`
	Arms    []ArmSpec       `hcl:"arm,block"`
	Default *DefaultArmSpec `hcl:"default,block"`
}

// ArmSpec holds a single conditional arm inside a branch block.
// The `when` expression is captured via Remain and extracted by the compiler.
type ArmSpec struct {
	TransitionTo string   `hcl:"transition_to"`
	Remain       hcl.Body `hcl:",remain"` // captures the "when" expression
}

// DefaultArmSpec holds the fallback transition for a branch block.
type DefaultArmSpec struct {
	TransitionTo string `hcl:"transition_to"`
}

// ForEachSpec declares a for_each node. It iterates the `items` expression
// (which must evaluate to a list or tuple at runtime) and runs the `do` step
// once per item. The `items` attribute and any other attributes not explicitly
// decoded are captured via the Remain body and extracted by the compiler.
type ForEachSpec struct {
	Name     string        `hcl:"name,label"`
	Do       string        `hcl:"do"`
	Outcomes []OutcomeSpec `hcl:"outcome,block"`
	// Remain captures the `items` expression attribute (and any unknown attrs)
	// for lazy extraction by the compiler. gohcl does not support hcl.Expression
	// as a direct decode target, so the remain pattern is used instead.
	Remain hcl.Body `hcl:",remain"`
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
	Variables    map[string]*VariableNode // compiled variable declarations (W04)
	Agents       map[string]*AgentNode
	Steps        map[string]*StepNode     // by step name
	States       map[string]*StateNode    // by state name (terminal etc.)
	Waits        map[string]*WaitNode     // by wait node name (W05)
	Approvals    map[string]*ApprovalNode // by approval node name (W05)
	Branches     map[string]*BranchNode   // by branch node name (W06)
	ForEachs     map[string]*ForEachNode  // by for_each node name (W07)
	Policy       Policy
	// Order of step declarations (stable for diagnostics).
	stepOrder []string
}

// VariableNode is a compiled variable declaration.
// Variables are read-only in W04; write support is tracked as future work.
type VariableNode struct {
	Name        string
	Type        cty.Type
	Default     cty.Value // cty.NilVal when no default was declared
	Description string
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
	// only the Go-side field is renamed here.
	// For steps with variable expressions, Input may contain empty strings for
	// expression-valued attributes; the engine evaluates InputExprs at step entry.
	Input map[string]string
	// InputExprs holds the raw HCL attribute expressions from the input{} block.
	// The engine evaluates these at step entry via BuildEvalContext(rs.Vars) to
	// produce the effective input map passed to the adapter. If nil, Input is
	// used directly (static-only inputs, e.g. lifecycle steps).
	InputExprs map[string]hcl.Expression
	Timeout    time.Duration     // zero = no timeout
	Outcomes   map[string]string // outcome name -> target node name (step or state)
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

// WaitNode is a compiled wait node. Exactly one of Duration or Signal is set.
// Duration form resumes automatically after the specified time.
// Signal form pauses until an external Resume RPC fires.
type WaitNode struct {
	Name     string
	Duration time.Duration     // zero means signal mode
	Signal   string            // empty means duration mode
	Outcomes map[string]string // outcome name -> target node name
}

// ApprovalNode is a compiled approval node. It pauses until a Resume RPC
// delivers a decision of "approved" or "rejected".
type ApprovalNode struct {
	Name      string
	Approvers []string
	Reason    string
	Outcomes  map[string]string // "approved" -> target, "rejected" -> target
}

// BranchNode is a compiled branch node. Arms are evaluated in declaration
// order; the first truthy arm selects the transition target. If no arm
// matches, DefaultTarget is used.
type BranchNode struct {
	Name          string
	Arms          []BranchArm
	DefaultTarget string
}

// BranchArm holds a single conditional arm in a BranchNode.
type BranchArm struct {
	Condition hcl.Expression // evaluated at runtime against BuildEvalContext(rs.Vars)
	// ConditionSrc is the source text of the condition expression, extracted from
	// Spec.SourceBytes during compilation. It is populated only when the Spec was
	// produced by Parse or ParseFile (i.e. SourceBytes is non-nil). Callers that
	// construct a Spec programmatically (e.g. unit tests) will see an empty string.
	ConditionSrc string
	Target       string // transition_to target node name
}

// ForEachNode is a compiled for_each loop node (W07).
// It iterates Items (evaluated at runtime from the Items expression) and
// invokes the Do step once per item. The aggregate outcome is determined by
// whether any iteration produced a non-success (AnyFailed) result.
type ForEachNode struct {
	// Name is the node identifier used in the FSM.
	Name string
	// Items is the raw HCL expression that must evaluate to a list or tuple.
	// Evaluated at runtime inside the engine using BuildEvalContext(rs.Vars).
	Items hcl.Expression
	// Do is the name of the step to invoke for each item.
	Do string
	// Outcomes maps aggregate outcome names to target node names.
	// "all_succeeded" is required; "any_failed" is recommended.
	Outcomes map[string]string
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

// Lookup returns ("step"|"state"|"wait"|"approval"|"branch"|"for_each", true) if name exists in the graph.
func (g *FSMGraph) Lookup(name string) (kind string, ok bool) {
	if _, ok := g.Steps[name]; ok {
		return "step", true
	}
	if _, ok := g.States[name]; ok {
		return "state", true
	}
	if _, ok := g.Waits[name]; ok {
		return "wait", true
	}
	if _, ok := g.Approvals[name]; ok {
		return "approval", true
	}
	if _, ok := g.Branches[name]; ok {
		return "branch", true
	}
	if _, ok := g.ForEachs[name]; ok {
		return "for_each", true
	}
	return "", false
}

// StepOrder returns step names in declaration order.
func (g *FSMGraph) StepOrder() []string {
	out := make([]string, len(g.stepOrder))
	copy(out, g.stepOrder)
	return out
}

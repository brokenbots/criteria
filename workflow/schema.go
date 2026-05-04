// Package workflow defines the HCL workflow schema, parser, and the compiled
// FSM graph that the Criteria engine executes.
package workflow

import (
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// LocalSpec is the parsed (but unvalidated) local value declaration.
// The "value" expression is decoded by the compiler via the Remain body.
type LocalSpec struct {
	Name        string   `hcl:"name,label"`
	Description string   `hcl:"description,optional"`
	Remain      hcl.Body `hcl:",remain"` // captures the "value" expression
}

// LocalNode is a compiled local declaration with its fully-resolved value.
type LocalNode struct {
	Name        string
	Type        cty.Type  // inferred from the folded value
	Value       cty.Value // fully resolved at compile
	Description string
}

// EnvironmentSpec declares a typed execution environment in HCL.
// The HCL form has two labels: type then name.
//
//	environment "shell" "default" { variables = {...}, config = {...} }
type EnvironmentSpec struct {
	Type   string   `hcl:"type,label"`
	Name   string   `hcl:"name,label"`
	Remain hcl.Body `hcl:",remain"` // captures variables and config attributes
}

// EnvironmentNode is a compiled environment declaration.
type EnvironmentNode struct {
	Type      string
	Name      string
	Variables map[string]string    // resolved env vars (compile-folded)
	Config    map[string]cty.Value // type-specific config (compile-folded; shape unenforced for v0.3.0)
}

// OutputNode is a compiled output declaration. The value expression is
// evaluated at runtime when the run reaches a terminal state.
type OutputNode struct {
	Name         string
	Description  string
	DeclaredType cty.Type       // cty.NilType if no explicit type was declared
	Value        hcl.Expression // evaluated at runtime
}

// Spec is the parsed (but unvalidated) HCL workflow document.
type Spec struct {
	Name               string            `hcl:"name,label"`
	Version            string            `hcl:"version"`
	InitialState       string            `hcl:"initial_state"`
	TargetState        string            `hcl:"target_state"`
	DefaultEnvironment string            `hcl:"environment,optional"` // "<type>.<name>" reference to the workflow's default environment
	Variables          []VariableSpec    `hcl:"variable,block"`
	Locals             []LocalSpec       `hcl:"local,block"`
	Environments       []EnvironmentSpec `hcl:"environment,block"`
	Outputs            []OutputSpec      `hcl:"output,block"`
	Adapters           []AdapterDeclSpec `hcl:"adapter,block"`
	Subworkflows       []SubworkflowSpec `hcl:"subworkflow,block"`
	Steps              []StepSpec        `hcl:"step,block"`
	States             []StateSpec       `hcl:"state,block"`
	Waits              []WaitSpec        `hcl:"wait,block"`
	Approvals          []ApprovalSpec    `hcl:"approval,block"`
	Branches           []BranchSpec      `hcl:"branch,block"`
	Policy             *PolicySpec       `hcl:"policy,block"`
	Permissions        *PermissionsSpec  `hcl:"permissions,block"`
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

// ConfigSpec holds the raw HCL body of an `adapter.config { ... }` block.
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

// AdapterDeclSpec declares a named long-lived adapter session target in HCL form.
// This is the HCL schema for the `adapter "<type>" "<name>"` block.
// Note: This is distinct from AdapterInfo, which describes an adapter's schema.
type AdapterDeclSpec struct {
	Type        string      `hcl:"type,label"`           // first label: adapter type
	Name        string      `hcl:"name,label"`           // second label: instance name
	Environment string      `hcl:"environment,optional"` // "<env_type>.<env_name>" reference
	OnCrash     string      `hcl:"on_crash,optional"`
	Config      *ConfigSpec `hcl:"config,block"`
}

// StepSpec describes a single step in the workflow.
type StepSpec struct {
	Name    string `hcl:"name,label"`
	OnCrash string `hcl:"on_crash,optional"`
	// OnFailure controls iteration failure behaviour: "continue" (default),
	// "abort" (stop on first failure), or "ignore" (treat all as success).
	OnFailure string `hcl:"on_failure,optional"`
	// MaxVisits limits how many times this step may be evaluated in a single run.
	// 0 (default) means unlimited. Negative values are rejected at compile time.
	MaxVisits int `hcl:"max_visits,optional"`
	// Config is the legacy map attribute; retained for parse-time detection so the
	// compiler can emit a helpful "use input { } block" diagnostic.
	Config     map[string]string `hcl:"config,optional"`
	Input      *InputSpec        `hcl:"input,block"`
	Timeout    string            `hcl:"timeout,optional"`
	AllowTools []string          `hcl:"allow_tools,optional"`
	Outcomes   []OutcomeSpec     `hcl:"outcome,block"`
	// Remain captures adapter attribute and other expressions (for_each, count, etc.)
	// for lazy extraction by the compiler. The adapter attribute, if present, must be
	// an HCL traversal expression (not a string literal), e.g., adapter.shell.default.
	Remain hcl.Body `hcl:",remain"`
	// LegacyConfigRange, when set by Parse, points at the source range for a
	// legacy config = { ... } attribute so compile diagnostics can include
	// file/line context.
	LegacyConfigRange *hcl.Range
}

// SpecContent holds the workflow content fields shared between Spec and BodySpec.
// It is the gohcl decode target for the body of an inline workflow { ... } block
// and acts as a single source of truth for all content block types. Adding a new
// workflow-scope block type here automatically makes it available in both
// top-level Spec contexts and inline body contexts.
//
// Note: gohcl does not support anonymous embedded struct field promotion, so
// this struct is decoded separately by compileWorkflowBodyInline rather than
// embedded directly in BodySpec.
type SpecContent struct {
	Variables    []VariableSpec    `hcl:"variable,block"`
	Locals       []LocalSpec       `hcl:"local,block"`
	Environments []EnvironmentSpec `hcl:"environment,block"`
	Adapters     []AdapterDeclSpec `hcl:"adapter,block"`
	Steps        []StepSpec        `hcl:"step,block"`
	States       []StateSpec       `hcl:"state,block"`
	Waits        []WaitSpec        `hcl:"wait,block"`
	Approvals    []ApprovalSpec    `hcl:"approval,block"`
	Branches     []BranchSpec      `hcl:"branch,block"`
	Policy       *PolicySpec       `hcl:"policy,block"`
	Permissions  *PermissionsSpec  `hcl:"permissions,block"`
}

// BodySpec is the thin parsed header for an inline `workflow { ... }` block
// inside a step. Unlike Spec it needs no label; all header fields are optional.
// Content blocks (steps, variables, locals, etc.) are captured in Remain and
// decoded by compileWorkflowBodyInline into a SpecContent, eliminating
// field duplication between BodySpec and Spec.
type BodySpec struct {
	// Name and Version are optional user-supplied labels; they default to
	// "<step>:body" and "1" respectively during compilation.
	Name    string `hcl:"name,optional"`
	Version string `hcl:"version,optional"`
	// InitialState selects the starting state (lower priority than Entry).
	InitialState string `hcl:"initial_state,optional"`
	// Entry is the explicit initial step name. When empty the compiler uses
	// InitialState (if set) or the first declared step.
	Entry   string       `hcl:"entry,optional"`
	Outputs []OutputSpec `hcl:"output,block"`
	// Remain captures all content blocks (steps, variables, locals, adapters,
	// states, waits, approvals, branches, policy, permissions) for later
	// decoding into SpecContent by compileWorkflowBodyInline.
	Remain hcl.Body `hcl:",remain"`
}

// OutputSpec declares a named output value exposed by a workflow or workflow-step body.
// The value expression is extracted from Remain by the compiler.
type OutputSpec struct {
	Name        string   `hcl:"name,label"`
	Description string   `hcl:"description,optional"`
	TypeStr     string   `hcl:"type,optional"`
	Remain      hcl.Body `hcl:",remain"` // captures the "value" expression
}

// SubworkflowSpec declares a reusable sub-workflow to be resolved and compiled.
// The name is a single label; source and input are attributes.
// The Remain body captures any additional attributes like the "input" block.
type SubworkflowSpec struct {
	Name        string   `hcl:"name,label"`
	Source      string   `hcl:"source"`               // directory path; local or remote
	Environment string   `hcl:"environment,optional"` // "<env_type>.<env_name>" reference
	Remain      hcl.Body `hcl:",remain"`              // captures the "input" block
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
// It is used during workflow compilation to validate adapter config blocks and
// step input blocks against the adapter's declared requirements.
// An empty (zero-value) AdapterInfo means "any keys accepted" (permissive).
type AdapterInfo struct {
	ConfigSchema map[string]ConfigField // schema for adapter-level config { }` blocks
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

// PolicySpec defines global execution guards.
type PolicySpec struct {
	MaxTotalSteps  int `hcl:"max_total_steps,optional"`
	MaxStepRetries int `hcl:"max_step_retries,optional"`
	// MaxVisitsWarnThreshold controls when the engine emits a warning for
	// excessive revisits while executing a workflow.
	//
	// Semantics:
	//   - nil: use the default threshold (200 visits)
	//   - 0: disable revisit warnings
	//   - >0: use the provided threshold value
	//   - <0: invalid (validation error)
	//
	// This warning threshold is independent from MaxTotalSteps (hard stop), but
	// should typically be <= MaxTotalSteps when a max is configured so warnings
	// can be emitted before execution is terminated.
	MaxVisitsWarnThreshold *int `hcl:"max_visits_warn_threshold,optional"`
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
	Name               string
	InitialState       string
	TargetState        string
	Variables          map[string]*VariableNode    // compiled variable declarations (W04)
	Locals             map[string]*LocalNode       // compiled local declarations (W07)
	Environments       map[string]*EnvironmentNode // compiled environment declarations; keyed by "<type>.<name>"
	DefaultEnvironment string                      // optional; set if exactly one env is declared or explicitly set on workflow header
	Outputs            map[string]*OutputNode      // compiled output declarations (W09)
	OutputOrder        []string                    // declaration order for stable iteration
	Adapters           map[string]*AdapterNode     // compiled adapter declarations; keyed by "<type>.<name>"
	AdapterOrder       []string                    // declaration order for stable iteration
	Subworkflows       map[string]*SubworkflowNode // compiled subworkflow declarations; keyed by subworkflow name
	SubworkflowOrder   []string                    // declaration order for stable iteration
	Steps              map[string]*StepNode        // by step name
	States             map[string]*StateNode       // by state name (terminal etc.)
	Waits              map[string]*WaitNode        // by wait node name (W05)
	Approvals          map[string]*ApprovalNode    // by approval node name (W05)
	Branches           map[string]*BranchNode      // by branch node name (W06)
	Policy             Policy
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

// IsRequired returns true when the variable has no declared default.
// Used by the body input validation logic to detect unbound required vars.
func (v *VariableNode) IsRequired() bool { return v.Default == cty.NilVal }

// AdapterNode is a compiled adapter declaration with resolved type and configuration.
// The key in FSMGraph.Adapters is "<type>.<name>" (both labels).
type AdapterNode struct {
	Type        string            // adapter type (first label)
	Name        string            // instance name (second label)
	Environment string            // optional "<env_type>.<env_name>" reference; resolved to default at scope start if not set
	OnCrash     string            // "fail" (default) or "continue"
	Config      map[string]string // compile-folded config from adapter.config { }
}

// StepNode is a compiled step with resolved transitions.
type StepNode struct {
	Name    string
	Adapter string // "<type>.<name>" reference to a declared adapter
	OnCrash string
	// Type is the step kind: "" (default adapter) or "workflow" (sub-workflow body).
	Type string
	// OnFailure controls iteration behaviour when an iteration produces a
	// non-success outcome. Values: "continue" (default), "abort", "ignore".
	OnFailure string
	// MaxVisits limits how many times this step may be evaluated in a single run.
	// 0 means unlimited. Enforced by the engine before each evaluation (W07).
	MaxVisits int
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
	// ForEach is the raw HCL expression for step-level iteration over a list or
	// map. Evaluated at runtime on first step entry. Mutually exclusive with Count.
	ForEach hcl.Expression
	// Count is the raw HCL expression for step-level iteration by count.
	// Evaluates to an integer N; iteration runs N times with each.value = 0..N-1.
	// Mutually exclusive with ForEach.
	Count hcl.Expression
	// Body is the compiled FSMGraph for workflow-type steps. Nil for non-workflow steps.
	Body *FSMGraph
	// BodyEntry is the initial state name for the workflow body. Derived from
	// the first declared step in the body when not explicitly set.
	BodyEntry string
	// BodyInputExpr is the optional `input = { ... }` expression on the parent
	// step. When non-nil the engine evaluates it at iteration entry to build the
	// child scope's var.* bindings. When nil the body's variable defaults are
	// used directly.
	BodyInputExpr hcl.Expression
	// Outputs maps output block names to their value HCL expressions. Evaluated
	// after each body iteration completes to populate indexed step outputs.
	Outputs map[string]hcl.Expression
}

// SubworkflowNode is a compiled subworkflow declaration with resolved source,
// body, and input bindings.
type SubworkflowNode struct {
	Name         string                    // subworkflow name (the label)
	SourcePath   string                    // resolved absolute path to subworkflow directory
	Body         *FSMGraph                 // deep-compiled callee workflow
	BodyEntry    string                    // initial state name for the subworkflow body
	Environment  string                    // resolved "<env_type>.<env_name>" reference (optional)
	Inputs       map[string]hcl.Expression // parent-scope input expressions (name -> HCL expression)
	DeclaredVars map[string]*VariableNode  // callee's declared variables (name -> VariableNode)
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

// Policy holds resolved engine guards. Defaults are applied during compile.
type Policy struct {
	MaxTotalSteps  int
	MaxStepRetries int
	// MaxVisitsWarnThreshold is the threshold value that max_total_steps is
	// compared against to determine whether to emit a warning when a step with a
	// back-edge has no max_visits set (W07). 0 disables the warning. Default is 200.
	MaxVisitsWarnThreshold int
}

// DefaultPolicy is applied when a workflow omits a policy block.
var DefaultPolicy = Policy{
	MaxTotalSteps:          100,
	MaxStepRetries:         0,
	MaxVisitsWarnThreshold: 200,
}

// IsTerminal reports whether the named node is a terminal state.
func (g *FSMGraph) IsTerminal(name string) bool {
	if s, ok := g.States[name]; ok {
		return s.Terminal
	}
	return false
}

// Lookup returns ("step"|"state"|"wait"|"approval"|"branch", true) if name exists in the graph.
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
	return "", false
}

// StepOrder returns step names in declaration order.
func (g *FSMGraph) StepOrder() []string {
	out := make([]string, len(g.stepOrder))
	copy(out, g.stepOrder)
	return out
}

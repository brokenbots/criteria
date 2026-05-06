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

// SharedVariableSpec is the parsed (but unvalidated) shared_variable declaration.
// shared_variable blocks declare runtime-mutable, workflow-scoped values with
// engine-managed locking. Unlike variable blocks (compile-time defaults, read-only
// after run start) and local blocks (compile-time constants), shared_variables
// are read-write throughout the run.
//
// The optional "value" initial expression is decoded by the compiler via Remain.
type SharedVariableSpec struct {
	Name        string   `hcl:"name,label"`
	Description string   `hcl:"description,optional"`
	TypeStr     string   `hcl:"type,optional"`
	Remain      hcl.Body `hcl:",remain"` // captures the optional "value" expression
}

// SharedVariableNode is a compiled shared_variable declaration.
type SharedVariableNode struct {
	Name         string
	Type         cty.Type  // explicit (parsed from TypeStr)
	InitialValue cty.Value // compile-folded; cty.NullVal(Type) if not declared
	Description  string
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

// WorkflowHeaderSpec carries the workflow identity and routing fields declared
// in the `workflow "<name>" { ... }` header block. In a directory module, exactly
// one .hcl file must contain this block; across multiple files, exactly one
// WorkflowHeaderSpec may be non-nil after merging.
type WorkflowHeaderSpec struct {
	Name               string `hcl:"name,label"`
	Version            string `hcl:"version,optional"`
	InitialState       string `hcl:"initial_state,optional"`
	TargetState        string `hcl:"target_state,optional"`
	DefaultEnvironment string `hcl:"environment,optional"` // "<type>.<name>" reference to the workflow's default environment
}

// Spec is the parsed (but unvalidated) HCL workflow document. After workstream
// 17, the `workflow "<name>" { ... }` block is header-only; all content blocks
// (step, state, adapter, etc.) live at the top level of the HCL file.
type Spec struct {
	Header          *WorkflowHeaderSpec  `hcl:"workflow,block"`
	Variables       []VariableSpec       `hcl:"variable,block"`
	Locals          []LocalSpec          `hcl:"local,block"`
	SharedVariables []SharedVariableSpec `hcl:"shared_variable,block"`
	Environments    []EnvironmentSpec    `hcl:"environment,block"`
	Outputs         []OutputSpec         `hcl:"output,block"`
	Adapters        []AdapterDeclSpec    `hcl:"adapter,block"`
	Subworkflows    []SubworkflowSpec    `hcl:"subworkflow,block"`
	Steps           []StepSpec           `hcl:"step,block"`
	States          []StateSpec          `hcl:"state,block"`
	Waits           []WaitSpec           `hcl:"wait,block"`
	Approvals       []ApprovalSpec       `hcl:"approval,block"`
	Switches        []SwitchSpec         `hcl:"switch,block"`
	Policy          *PolicySpec          `hcl:"policy,block"`
	Permissions     *PermissionsSpec     `hcl:"permissions,block"`
	// SourceBytes holds the raw HCL source that was parsed to produce this Spec.
	// Populated by Parse/ParseFile; used by the compiler to extract expression
	// source text (e.g. for SwitchEvaluated.Condition).
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
	// DefaultOutcome, when set, is the fallback outcome name used when an adapter
	// returns an outcome name not in the declared set. Must refer to a declared
	// outcome; validated at compile time.
	DefaultOutcome string `hcl:"default_outcome,optional"`
	// Outcomes lists the declared outcome blocks for this step.
	// Environment (e.g. shell.ci) is not decoded as a struct field; it is a bare
	// traversal captured from Remain by resolveStepEnvironmentOverride. A
	// quoted-string form causes a compile error with a migration hint.
	Outcomes []OutcomeSpec `hcl:"outcome,block"`
	// Remain captures the target attribute and other expressions (for_each, count,
	// etc.) for lazy extraction by the compiler. The target attribute, if present,
	// must be an HCL traversal expression of the form adapter.<type>.<name> or
	// subworkflow.<name>.
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
	Variables       []VariableSpec       `hcl:"variable,block"`
	Locals          []LocalSpec          `hcl:"local,block"`
	SharedVariables []SharedVariableSpec `hcl:"shared_variable,block"`
	Environments    []EnvironmentSpec    `hcl:"environment,block"`
	Adapters        []AdapterDeclSpec    `hcl:"adapter,block"`
	Steps           []StepSpec           `hcl:"step,block"`
	States          []StateSpec          `hcl:"state,block"`
	Waits           []WaitSpec           `hcl:"wait,block"`
	Approvals       []ApprovalSpec       `hcl:"approval,block"`
	Switches        []SwitchSpec         `hcl:"switch,block"`
	Policy          *PolicySpec          `hcl:"policy,block"`
	Permissions     *PermissionsSpec     `hcl:"permissions,block"`
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

// OutcomeSpec maps an adapter outcome name to the next node.
// The Next attribute replaces the removed transition_to attribute (v0.3.0).
// An optional "output" expression may appear in the Remain body to project
// a custom output map instead of passing the step's full output downstream.
type OutcomeSpec struct {
	Name   string   `hcl:"name,label"`
	Next   string   `hcl:"next"`
	Remain hcl.Body `hcl:",remain"` // captures the optional "output" expression
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

// SwitchSpec declares a switch node. Conditions are evaluated in declaration order;
// the first truthy condition wins. Default is recommended; absence produces a
// compile warning when no condition is provably exhaustive at compile time, and
// a runtime error when no condition matches.
type SwitchSpec struct {
	Name       string             `hcl:"name,label"`
	Conditions []ConditionSpec    `hcl:"condition,block"`
	Default    *SwitchDefaultSpec `hcl:"default,block"`
}

// ConditionSpec holds a single conditional arm inside a switch block.
// The `match` (required), `next` (required), and `output` (optional) attributes
// are captured via Remain and extracted by the compiler.
type ConditionSpec struct {
	Remain hcl.Body `hcl:",remain"` // captures: match (required), next (required), output (optional)
}

// SwitchDefaultSpec holds the fallback transition for a switch block.
// The `next` (required) and `output` (optional) attributes are captured via Remain.
type SwitchDefaultSpec struct {
	Remain hcl.Body `hcl:",remain"` // captures: next (required), output (optional)
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
	Name                string
	InitialState        string
	TargetState         string
	Variables           map[string]*VariableNode       // compiled variable declarations (W04)
	Locals              map[string]*LocalNode          // compiled local declarations (W07)
	SharedVariables     map[string]*SharedVariableNode // compiled shared_variable declarations (W18)
	SharedVariableOrder []string                       // declaration order for stable iteration (W18)
	Environments        map[string]*EnvironmentNode    // compiled environment declarations; keyed by "<type>.<name>"
	DefaultEnvironment  string                         // optional; set if exactly one env is declared or explicitly set on workflow header
	Outputs             map[string]*OutputNode         // compiled output declarations (W09)
	OutputOrder         []string                       // declaration order for stable iteration
	Adapters            map[string]*AdapterNode        // compiled adapter declarations; keyed by "<type>.<name>"
	AdapterOrder        []string                       // declaration order for stable iteration
	Subworkflows        map[string]*SubworkflowNode    // compiled subworkflow declarations; keyed by subworkflow name
	SubworkflowOrder    []string                       // declaration order for stable iteration
	Steps               map[string]*StepNode           // by step name
	States              map[string]*StateNode          // by state name (terminal etc.)
	Waits               map[string]*WaitNode           // by wait node name (W05)
	Approvals           map[string]*ApprovalNode       // by approval node name (W05)
	Switches            map[string]*SwitchNode         // by switch node name (W16)
	Policy              Policy
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

// StepTargetKind enumerates the kinds of compiled step targets.
type StepTargetKind int

const (
	// StepTargetAdapter targets a named adapter declaration: target = adapter.<type>.<name>.
	StepTargetAdapter StepTargetKind = iota
	// StepTargetSubworkflow targets a named subworkflow declaration: target = subworkflow.<name>.
	StepTargetSubworkflow
)

// CompiledOutcome is a compiled step outcome with resolved transition target
// and an optional output projection expression.
type CompiledOutcome struct {
	// Name is the outcome name declared in the workflow.
	Name string
	// Next is the resolved target node name or the reserved sentinel "return".
	// When "return", the engine halts the current scope and propagates the
	// projected output upward (or treats the run as terminal-success at
	// the top level).
	Next string
	// OutputExpr, when non-nil, is evaluated at runtime against the current
	// run scope to produce the projected output map. When nil, the step's
	// full adapter output is passed downstream unchanged.
	OutputExpr hcl.Expression
	// SharedWrites maps shared_variable names to output keys. After output
	// projection, the engine applies these writes atomically to the scope's
	// SharedVarStore. The key is the shared_variable name; the value is the
	// key from the outcome's projected output (or step raw output when no
	// output projection is declared).
	//
	// HCL form: shared_writes = { var_name = "output_key" }
	SharedWrites map[string]string
}

// ReturnSentinel is the reserved next value that signals scope-exit.
const ReturnSentinel = "return"

// StepNode is a compiled step with resolved transitions.
type StepNode struct {
	Name string
	// TargetKind identifies what this step executes: an adapter or a subworkflow.
	TargetKind StepTargetKind
	// AdapterRef is the resolved "<type>.<name>" adapter reference when TargetKind == StepTargetAdapter.
	AdapterRef string
	// SubworkflowRef is the resolved subworkflow name when TargetKind == StepTargetSubworkflow.
	SubworkflowRef string
	OnCrash        string
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
	Timeout    time.Duration               // zero = no timeout
	Outcomes   map[string]*CompiledOutcome // outcome name -> compiled outcome
	// DefaultOutcome, when non-empty, is applied when the adapter returns an
	// outcome name not present in Outcomes. The unknown name is silently mapped
	// to this outcome. When empty, an unknown outcome is a runtime error.
	DefaultOutcome string
	// AllowTools is the union of step-level and workflow-level allow_tools glob
	// patterns. An empty slice means deny-all (default). Only valid for adapter steps.
	AllowTools []string
	// ForEach is the raw HCL expression for step-level iteration over a list or
	// map. Evaluated at runtime on first step entry. Mutually exclusive with Count.
	ForEach hcl.Expression
	// Count is the raw HCL expression for step-level iteration by count.
	// Evaluates to an integer N; iteration runs N times with each.value = 0..N-1.
	// Mutually exclusive with ForEach.
	Count hcl.Expression
	// Parallel is the raw HCL expression for step-level parallel execution.
	// Evaluates to a list or tuple; the step body runs concurrently for every item.
	// Mutually exclusive with ForEach and Count.
	Parallel hcl.Expression
	// ParallelMax is the maximum number of concurrent goroutines for a parallel step.
	// Populated from the compile-time parallel_max attribute; default is
	// runtime.GOMAXPROCS(0) when the attribute is absent. Never 0 at runtime.
	ParallelMax int
	// Environment is an optional per-step override for the execution environment,
	// in the form "<env_type>.<env_name>". When set, it overrides the adapter
	// block's environment and the workflow-level default for this step only.
	// Applies env-var injection only; does not create a new adapter session.
	Environment string
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

// SwitchNode is a compiled switch node. Conditions are evaluated in declaration
// order; the first truthy condition selects the transition target. If no
// condition matches, DefaultNext is used.
type SwitchNode struct {
	Name          string
	Conditions    []SwitchCondition
	DefaultNext   string
	DefaultOutput hcl.Expression // nil if not declared
}

// SwitchCondition holds a single conditional arm in a SwitchNode.
type SwitchCondition struct {
	Match hcl.Expression // evaluated at runtime against BuildEvalContext(rs.Vars)
	// MatchSrc is the source text of the match expression, extracted from
	// Spec.SourceBytes during compilation. Empty when Spec was constructed
	// programmatically (e.g. unit tests).
	MatchSrc   string
	Next       string         // resolved target node name or ReturnSentinel
	OutputExpr hcl.Expression // nil if not declared
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

// Lookup returns ("step"|"state"|"wait"|"approval"|"switch", true) if name exists in the graph.
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
	if _, ok := g.Switches[name]; ok {
		return "switch", true
	}
	return "", false
}

// StepOrder returns step names in declaration order.
func (g *FSMGraph) StepOrder() []string {
	out := make([]string, len(g.stepOrder))
	copy(out, g.stepOrder)
	return out
}

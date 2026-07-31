// Package workflow defines the HCL workflow schema, parser, and the compiled
// FSM graph that the Criteria engine executes.
package workflow

import (
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// LocalSpec is the parsed (but unvalidated) local value declaration.
// The "value" expression is decoded by the compiler via the Remain body.
type LocalSpec struct {
	Name        string   `hcl:"name,label"`
	Description string   `hcl:"description,optional"`
	Remain      hcl.Body `hcl:",remain"` // captures the "value" expression
}

// DataSpec is the WS02 form: data "<kind>" "<name>" { ... }.
// Only kind = "internal" is currently supported.
type DataSpec struct {
	Kind        string         `hcl:"kind,label"` // first label, e.g. "internal"
	Name        string         `hcl:"name,label"` // second label
	Description string         `hcl:"description,optional"`
	Type        hcl.Expression `hcl:"type"`    // required; WS01-style type expression
	Remain      hcl.Body       `hcl:",remain"` // captures the optional "value" expression
}

// DataNode is a compiled data block declaration.
type DataNode struct {
	Kind         string
	Name         string
	Type         cty.Type           // explicit (parsed from type expression)
	TypeDefaults *typeexpr.Defaults // nil when type has no optional defaults
	InitialValue cty.Value          // compile-folded; cty.NullVal(Type) if not declared
	Description  string
	Secret       bool
}

// DataRef identifies a data block by kind and name for stable ordering.
type DataRef struct {
	Kind string
	Name string
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
	Type string `hcl:"type,label"`
	Name string `hcl:"name,label"`
	// Captures: variables (optional, map of string env-vars), config (optional, type-specific config map).
	Remain hcl.Body `hcl:",remain"`
}

// FilesystemPolicy controls filesystem access for sandboxed/container environments.
type FilesystemPolicy struct{ ReadOnly bool }

// NetworkPolicy controls network access for sandboxed/container environments.
type NetworkPolicy struct{ AllowEgress bool }

// SecretsPolicy controls secret resolution for an environment.
type SecretsPolicy struct {
	Provider string   `json:"provider"`
	Fallback []string `json:"fallback,omitempty"`
}

// ResourcesPolicy caps compute resources for an environment.
type ResourcesPolicy struct{ MaxMemory string }

// PolicyHints provides default values for environment policy fields that an
// adapter manifest may declare. These hints are consumed by the three-rule
// field resolver when policy_mode = "permissive".
type PolicyHints struct {
	PolicyMode   string
	OS           string
	Filesystem   *FilesystemPolicy
	Network      *NetworkPolicy
	Secrets      *SecretsPolicy
	Resources    *ResourcesPolicy
	TypeSpecific map[string]cty.Value
}

// ResolvedPolicy is the result of resolving an environment block against an
// adapter's manifest hints and the three D37 rules.
type ResolvedPolicy struct {
	PolicyMode   string
	OS           string
	Filesystem   *FilesystemPolicy
	Network      *NetworkPolicy
	Secrets      *SecretsPolicy
	Resources    *ResourcesPolicy
	TypeSpecific map[string]cty.Value
}

// EnvironmentNode is a compiled environment declaration.
type EnvironmentNode struct {
	Type      string
	Name      string
	Variables map[string]string    // resolved env vars (compile-folded)
	Config    map[string]cty.Value // type-specific config (compile-folded; shape unenforced for v0.3.0)
	// PolicyMode is "permissive" (default) or "strict".
	PolicyMode string
	// OS is "" (any) or a specific GOOS value like "linux" or "darwin".
	OS string
	// WorkingDirectoryExpr is the environment's optional working_directory
	// attribute, kept as an unevaluated expression and resolved at runtime when
	// the adapter session is initialized (against the run's var + local + input
	// closure). Deferring evaluation lets the workflow set the adapter launch cwd
	// dynamically (e.g. working_directory = var.worktree supplied via --var at
	// run time). The resolved value becomes the adapter process launch cwd so
	// shell/copilot adapters operate in that directory by default. Supported by
	// shell, sandbox, and remote environments; container environments reject it
	// (they isolate paths rather than relocate the process cwd).
	WorkingDirectoryExpr hcl.Expression
	Filesystem           *FilesystemPolicy
	Network              *NetworkPolicy
	Secrets              *SecretsPolicy
	Resources            *ResourcesPolicy
	TypeSpecific         map[string]cty.Value // e.g. runtime="docker"
	// RawBody preserves the original HCL body so runtime handlers can parse
	// type-specific blocks (e.g. mtls { ... }) that the generic compiler does
	// not decode into typed fields.
	RawBody hcl.Body
}

// OutputNode is a compiled output declaration. The value expression is
// evaluated at runtime when the run reaches a terminal state.
type OutputNode struct {
	Name         string
	Description  string
	DeclaredType cty.Type           // cty.NilType if no explicit type was declared
	TypeDefaults *typeexpr.Defaults // nil when type has no optional defaults
	Value        hcl.Expression     // evaluated at runtime
}

// WorkflowHeaderSpec carries the workflow identity and routing fields declared
// in the `workflow { ... }` header block. In a directory module, exactly
// one .chcl or .hcl file must contain this block; across multiple files, exactly
// one WorkflowHeaderSpec may be non-nil after merging.
type WorkflowHeaderSpec struct {
	Name string `hcl:"name"`
	// Version is the HCL schema version string. Use "1".
	//
	// spec:required
	Version string `hcl:"version,optional"`
	// InitialState names the step or state where workflow execution begins.
	//
	// spec:required
	InitialState       string         `hcl:"initial_state,optional"`
	TargetState        string         `hcl:"target_state,optional"`
	DefaultEnvironment hcl.Expression `hcl:"environment,optional"` // bare traversal reference to the workflow's default environment (e.g. shell.default)
	Policy             *PolicySpec    `hcl:"policy,block"`
	// Verification is the workflow-level signature-verification posture for OCI
	// adapters: off|warn|strict. Empty means the CLI transition default applies.
	// The CLI override (--allow-unsigned / CRITERIA_ALLOW_UNSIGNED) takes
	// precedence over this attribute.
	Verification string `hcl:"verification,optional"`
}

// Spec is the parsed (but unvalidated) HCL workflow document. After workstream
// 17, the `workflow { ... }` block is header-only; all content blocks
// (step, state, adapter, etc.) live at the top level of the HCL file.
type Spec struct {
	Header       *WorkflowHeaderSpec `hcl:"workflow,block"`
	Variables    []VariableSpec      `hcl:"variable,block"`
	Locals       []LocalSpec         `hcl:"local,block"`
	Data         []DataSpec          `hcl:"data,block"`
	Environments []EnvironmentSpec   `hcl:"environment,block"`
	Outputs      []OutputSpec        `hcl:"output,block"`
	Adapters     []AdapterDeclSpec   `hcl:"adapter,block"`
	Subworkflows []SubworkflowSpec   `hcl:"subworkflow,block"`
	Steps        []StepSpec          `hcl:"step,block"`
	States       []StateSpec         `hcl:"state,block"`
	Waits        []WaitSpec          `hcl:"wait,block"`
	Approvals    []ApprovalSpec      `hcl:"approval,block"`
	Switches     []SwitchSpec        `hcl:"switch,block"`
	Permissions  *PermissionsSpec    `hcl:"permissions,block"`
	// SourceBytes holds the raw HCL source that was parsed to produce this Spec.
	// Populated by Parse/ParseFile; used by the compiler to extract expression
	// source text (e.g. for SwitchEvaluated.Condition).
	SourceBytes []byte
}

// VariableSpec is the parsed (but unvalidated) variable declaration.
// The `type` and `default` attributes are decoded by the compiler.
type VariableSpec struct {
	Name        string         `hcl:"name,label"`
	Type        hcl.Expression `hcl:"type,optional"`
	Description string         `hcl:"description,optional"`
	Remain      hcl.Body       `hcl:",remain"` // captures the "default" expression
}

// ConfigSpec holds the raw HCL body of an `adapter.config { ... }` block.
// Attributes are decoded into string values by the compiler.
// W04 will upgrade to expression-aware decoding (var.<name>, each.value).
type ConfigSpec struct {
	Remain hcl.Body `hcl:",remain"`
}

// InputSpec holds the raw HCL body of a `step.input { ... }` block.
// Attribute expressions are decoded by the compiler into a string map
// (compile-time) and parallel hcl.Expression map (runtime).
// Runtime evaluation uses ResolveInputExprs / ResolveInputExprsAsCty
// in workflow/eval.go, which builds an hcl.EvalContext with var.*,
// steps.*, local.*, data.*, and each.* namespaces.
type InputSpec struct {
	Remain hcl.Body `hcl:",remain"`
}

// AdapterDeclSpec declares a named long-lived adapter session target in HCL form.
// This is the HCL schema for the `adapter "<type>" "<name>"` block.
// Note: This is distinct from AdapterInfo, which describes an adapter's schema.
type AdapterDeclSpec struct {
	Type string `hcl:"type,label"` // first label: adapter type
	Name string `hcl:"name,label"` // second label: instance name
	// Source is the adapter's OCI location (a registry/repo path or a registry
	// alias), decoupled from the version. Required for OCI-backed adapters.
	Source string `hcl:"source,optional"`
	// Version is the semver constraint resolved at lock time: exact ("1.2.3"),
	// caret ("^1.2"), tilde ("~1.2.0"), wildcard ("1.x"), or "latest". The
	// lockfile pins the resolved digest for run-to-run reproducibility.
	Version     string         `hcl:"version,optional"`
	Environment hcl.Expression `hcl:"environment,optional"` // bare traversal reference (e.g. shell.default)
	OnCrash     string         `hcl:"on_crash,optional"`
	Config      *ConfigSpec    `hcl:"config,block"`
	Secrets     *ConfigSpec    `hcl:"secrets,block"` // optional secrets block
}

// StepSpec describes a single step in the workflow.
type StepSpec struct {
	Name    string `hcl:"name,label"`
	OnCrash string `hcl:"on_crash,optional"`
	// OnFailure controls iteration failure behaviour: "continue" (default for
	// sequential for_each/count steps), "abort" (stop on first failure; default
	// for parallel steps), or "ignore" (treat all as success).
	OnFailure string `hcl:"on_failure,optional"`
	// MaxVisits limits how many times this step may be evaluated in a single run.
	// 0 (default) means unlimited. Negative values are rejected at compile time.
	MaxVisits int `hcl:"max_visits,optional"`
	// Config is the legacy map attribute; retained for parse-time detection so the
	// compiler can emit a helpful "use input { } block" diagnostic.
	Config      map[string]string `hcl:"config,optional"`
	Input       *InputSpec        `hcl:"input,block"`
	SecretInput *InputSpec        `hcl:"secret_input,block"`
	Timeout     string            `hcl:"timeout,optional"`
	AllowTools  []string          `hcl:"allow_tools,optional"`
	// Outcomes lists the declared outcome blocks for this step.
	// Environment (e.g. shell.ci) is not decoded as a struct field; it is a bare
	// traversal captured from Remain by resolveStepEnvironmentOverride. A
	// quoted-string form causes a compile error with a migration hint.
	Outcomes []OutcomeSpec `hcl:"outcome,block"`
	// Captures: target (required — adapter traversal e.g. adapter.copilot.main, or subworkflow.<name>);
	// for_each, count, parallel, while (optional iteration controls); environment (optional, bare traversal e.g. shell.ci).
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
	Data         []DataSpec        `hcl:"data,block"`
	Environments []EnvironmentSpec `hcl:"environment,block"`
	Adapters     []AdapterDeclSpec `hcl:"adapter,block"`
	Steps        []StepSpec        `hcl:"step,block"`
	States       []StateSpec       `hcl:"state,block"`
	Waits        []WaitSpec        `hcl:"wait,block"`
	Approvals    []ApprovalSpec    `hcl:"approval,block"`
	Switches     []SwitchSpec      `hcl:"switch,block"`
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
	Name        string         `hcl:"name,label"`
	Description string         `hcl:"description,optional"`
	Type        hcl.Expression `hcl:"type,optional"`
	Remain      hcl.Body       `hcl:",remain"` // captures the "value" expression
}

// SubworkflowSpec declares a reusable sub-workflow to be resolved and compiled.
// The name is a single label; source and input are attributes.
// The Remain body captures any additional attributes like the "input" block.
type SubworkflowSpec struct {
	Name        string         `hcl:"name,label"`
	Source      string         `hcl:"source"`               // directory path; local or remote
	Environment hcl.Expression `hcl:"environment,optional"` // bare traversal reference (e.g. shell.default)
	Remain      hcl.Body       `hcl:",remain"`              // captures the "input" block
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
	Required  bool
	Type      ConfigFieldType
	Doc       string
	Sensitive bool // marks the field as a taint-source for downstream tools
	// CtyType is the full declared cty type for the field. For OutputSchema it is
	// authoritative and drives typed coercion of step outputs (object/array/number/
	// bool/string). cty.NilType means "no declared type" (permissive — value is
	// preserved as a raw string). Config/input validation still uses Type; CtyType
	// is the richer model needed for structured outputs.
	CtyType cty.Type
}

// AdapterInfo describes an adapter's declared configuration schema.
// It is used during workflow compilation to validate adapter config blocks and
// step input blocks against the adapter's declared requirements.
// An empty (zero-value) AdapterInfo means "any keys accepted" (permissive).
type AdapterInfo struct {
	ConfigSchema           map[string]ConfigField // schema for adapter-level `config { }` blocks
	InputSchema            map[string]ConfigField // schema for per-step `input { }` blocks
	OutputSchema           map[string]ConfigField // declared outputs the adapter promises to populate (W04)
	Capabilities           []string               // well-known capability strings (e.g. "parallel_safe")
	CompatibleEnvironments []string               // nil/empty means any (default)
	PolicyHints            *PolicyHints           // D36 manifest hints for environment policy fields
	// SupportedFeatures lists optional capabilities this adapter implements.
	// Well-known values: "pause", "resume", "snapshot", "restore", "inspect".
	// The host gates UI and behavior on this list; unknown values are ignored.
	SupportedFeatures []string // NEW v2 (D76)
}

// OutcomeSpec maps an adapter outcome name to the next node.
// The Next attribute replaces the removed transition_to attribute (v0.3.0).
// It is an hcl.Expression decoded by the compiler (traversal form: step.foo,
// state.done, return, continue).
// An optional "output" expression may appear in the Remain body to project
// a custom output map instead of passing the step's full output downstream.
// Zero or more write { target = ..., value = ... } blocks declare data writes.
type OutcomeSpec struct {
	Name   string         `hcl:"name,label"`
	Next   hcl.Expression `hcl:"next"`
	Writes []WriteSpec    `hcl:"write,block"`
	Remain hcl.Body       `hcl:",remain"` // captures the optional "output" expression
}

// WriteSpec is a single data write declaration inside an outcome block.
type WriteSpec struct {
	Target hcl.Expression `hcl:"target"` // traversal: data.<kind>.<name>.value
	Value  hcl.Expression `hcl:"value"`  // runtime-evaluated expression
}

// CompiledWrite is a compiled data write with resolved kind/name and the
// value expression to evaluate at runtime.
type CompiledWrite struct {
	DataKind  string         // resolved from target traversal
	DataName  string         // resolved from target traversal
	ValueExpr hcl.Expression // runtime-evaluated against the step's output scope
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

// SwitchSpec declares a switch node. Matches are evaluated in declaration order;
// the first truthy match wins. Default is recommended; absence produces a
// compile warning when no match is provably exhaustive at compile time, and
// a runtime error when no match matches.
type SwitchSpec struct {
	Name    string             `hcl:"name,label"`
	Matches []MatchSpec        `hcl:"match,block"`
	Default *SwitchDefaultSpec `hcl:"default,block"`
}

// MatchSpec holds a single match arm inside a switch block.
// The `condition` (required), `next` (required), and `output` (optional) attributes
// are captured via Remain and extracted by the compiler.
type MatchSpec struct {
	Remain hcl.Body `hcl:",remain"` // captures: condition (required), next (required), output (optional)
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
	Name         string
	InitialState string
	TargetState  string
	// Verification is the workflow-level signature-verification posture
	// (off|warn|strict) copied from the header; empty means the CLI transition
	// default applies. Consumed by the runtime adapter-pin enforcement path.
	Verification       string
	Variables          map[string]*VariableNode        // compiled variable declarations (W04)
	Locals             map[string]*LocalNode           // compiled local declarations (W07)
	Data               map[string]map[string]*DataNode // compiled data declarations; keyed by kind then name (W02)
	DataOrder          []DataRef                       // declaration order for stable iteration (W02)
	Environments       map[string]*EnvironmentNode     // compiled environment declarations; keyed by "<type>.<name>"
	DefaultEnvironment string                          // optional; set if exactly one env is declared or explicitly set on workflow header
	Outputs            map[string]*OutputNode          // compiled output declarations (W09)
	OutputOrder        []string                        // declaration order for stable iteration
	Adapters           map[string]*AdapterNode         // compiled adapter declarations; keyed by "<type>.<name>"
	AdapterOrder       []string                        // declaration order for stable iteration
	Subworkflows       map[string]*SubworkflowNode     // compiled subworkflow declarations; keyed by subworkflow name
	SubworkflowOrder   []string                        // declaration order for stable iteration
	Steps              map[string]*StepNode            // by step name
	States             map[string]*StateNode           // by state name (terminal etc.)
	Waits              map[string]*WaitNode            // by wait node name (W05)
	Approvals          map[string]*ApprovalNode        // by approval node name (W05)
	Switches           map[string]*SwitchNode          // by switch node name (W16)
	ResolvedPolicies   map[string]*ResolvedPolicy      // cached per (adapter, environment); key = "adapterRef:envKey"
	Policy             Policy
	// WorkflowDir is the absolute directory of the workflow that produced this
	// graph. It is used at runtime to attribute adapter resolution errors to a
	// workflow directory.
	WorkflowDir string
	// PinSet is the merged lockfile for this workflow and every transitive
	// subworkflow, resolved once at compile time. The engine uses it as the
	// single source of truth for adapter pins.
	PinSet *lockfile.Lockfile
	// FileCache holds the content of every file() reference resolved while
	// compiling adapter config. Runtime config re-evaluation reads from this
	// cache so runs are immune to changes in prompt files after compile time.
	FileCache map[string]string
	// Order of step declarations (stable for diagnostics).
	stepOrder []string
}

// VariableNode is a compiled variable declaration.
// Variables are read-only in W04; write support is tracked as future work.
type VariableNode struct {
	Name         string
	Type         cty.Type
	TypeDefaults *typeexpr.Defaults // nil when type has no optional defaults
	Default      cty.Value          // cty.NilVal when no default was declared
	Description  string
	Secret       bool
}

// IsRequired returns true when the variable has no declared default.
// Used by the body input validation logic to detect unbound required vars.
func (v *VariableNode) IsRequired() bool { return v.Default == cty.NilVal }

// AdapterNode is a compiled adapter declaration with resolved type and configuration.
// The key in FSMGraph.Adapters is "<type>.<name>" (both labels).
type AdapterNode struct {
	Type        string            // adapter type (first label)
	Name        string            // instance name (second label)
	Source      string            // OCI source location (empty for non-OCI adapters)
	Environment string            // optional "<env_type>.<env_name>" reference; resolved to default at scope start if not set
	OnCrash     string            // "fail" (default) or "continue"
	Config      map[string]string // compile-folded config from adapter.config { }
	// Secrets holds raw HCL expressions from the adapter-level `secrets { }` block.
	// These are treated as taint sources.
	Secrets map[string]hcl.Expression
	// ConfigExprs holds the raw HCL attribute expressions from the adapter-level
	// `config { }` block. Preserved so that TaintPass can detect tainted values
	// flowing into non-secret adapter config channels (D65).
	ConfigExprs map[string]hcl.Expression
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
	// Writes is the list of compiled data writes for this outcome. After output
	// projection, the engine evaluates each ValueExpr against the post-projection
	// scope and applies the writes atomically to the scope's DataStore.
	//
	// HCL form: write { target = data.<kind>.<name>.value, value = output.<key> }
	Writes []CompiledWrite
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
	// non-success outcome. Values: "continue" (default for sequential
	// for_each/count steps), "abort" (default for parallel steps), "ignore".
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
	// SecretInputs holds the per-step adapter secret input from the `secret_input { }` block.
	// These values are treated as taint sources and may only flow through secret channels.
	SecretInputs map[string]string
	// SecretInputExprs holds the raw HCL attribute expressions from the secret_input{} block.
	SecretInputExprs map[string]hcl.Expression
	Timeout          time.Duration               // zero = no timeout
	Outcomes         map[string]*CompiledOutcome // outcome name -> compiled outcome
	// DefaultOutcome, when set, is applied when the adapter returns an
	// outcome name not present in Outcomes. The unknown name is silently mapped
	// to this outcome. When nil, an unknown outcome is a runtime error.
	DefaultOutcome *CompiledOutcome
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
	// While is the raw HCL expression for condition-driven iteration. Evaluated
	// before each iteration; when false the loop exits and the aggregate outcome
	// fires. Must evaluate to cty.Bool. Total = -1 in the cursor signals an
	// unbounded while loop. Mutually exclusive with ForEach, Count, and Parallel.
	While hcl.Expression
	// ParallelMax is the maximum number of concurrent goroutines for a parallel step.
	// Populated from the compile-time parallel_max attribute; default is
	// runtime.GOMAXPROCS(0) when the attribute is absent. Never 0 at runtime.
	ParallelMax int
	// Environment is an optional per-step override for the execution environment,
	// in the form "<env_type>.<env_name>". When set, it overrides the adapter
	// block's environment and the workflow-level default for this step only.
	// Applies env-var injection only; does not create a new adapter session.
	Environment string
	// OutputSchema is the adapter's declared output schema for this step.
	// Populated during compilation when the step targets an adapter.
	OutputSchema map[string]ConfigField
	// Tainted is set by the taint compiler pass when this step receives secret
	// data (secret_input, predecessor taint, or sensitive output references).
	Tainted bool
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

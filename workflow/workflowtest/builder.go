package workflowtest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"
)

// PolicyDef defines workflow execution policies.
type PolicyDef struct {
	MaxTotalSteps          int
	MaxVisitsWarnThreshold *int
	MaxStepRetries         int
}

// AdapterDef defines an adapter block.
type AdapterDef struct {
	Type string
	Name string
}

// OutcomeDef defines a step outcome.
type OutcomeDef struct {
	Name     string
	Next     string
	Fallback bool
	Outputs  map[string]string
}

// StepDef defines a workflow step.
type StepDef struct {
	Name      string
	Target    string
	MaxVisits int
	Count     int
	ForEach   []string
	Parallel  []string
	Outcomes  []OutcomeDef
	RawBody   string
}

// StepOpt configures a StepDef.
type StepOpt func(*StepDef)

// Target sets the adapter target reference for a step.
func Target(target string) StepOpt {
	return func(s *StepDef) {
		s.Target = target
	}
}

// MaxVisits sets the max_visits attribute on a step.
func MaxVisits(n int) StepOpt {
	return func(s *StepDef) {
		s.MaxVisits = n
	}
}

// Count sets the count attribute on a step.
func Count(n int) StepOpt {
	return func(s *StepDef) {
		s.Count = n
	}
}

// ForEach sets the for_each attribute on a step.
func ForEach(items ...string) StepOpt {
	return func(s *StepDef) {
		s.ForEach = items
	}
}

// Parallel sets the parallel attribute on a step.
func Parallel(items ...string) StepOpt {
	return func(s *StepDef) {
		s.Parallel = items
	}
}

// Outcome adds a single outcome block to a step.
func Outcome(name, next string) StepOpt {
	return func(s *StepDef) {
		s.Outcomes = append(s.Outcomes, OutcomeDef{
			Name: name,
			Next: next,
		})
	}
}

// OutcomeWithOutput adds an outcome block with output projections.
func OutcomeWithOutput(name, next string, outputs map[string]string) StepOpt {
	return func(s *StepDef) {
		s.Outcomes = append(s.Outcomes, OutcomeDef{
			Name:    name,
			Next:    next,
			Outputs: outputs,
		})
	}
}

// FallbackOutcome adds a fallback outcome block to a step.
func FallbackOutcome(name, next string) StepOpt {
	return func(s *StepDef) {
		s.Outcomes = append(s.Outcomes, OutcomeDef{
			Name:     name,
			Next:     next,
			Fallback: true,
		})
	}
}

// Next routes both success and failure outcomes to the given target.
// Useful for creating complete steps in tests where outcome branch logic is not the focus.
func Next(target string) StepOpt {
	return func(s *StepDef) {
		s.Outcomes = append(s.Outcomes,
			OutcomeDef{Name: "success", Next: target},
			OutcomeDef{Name: "failure", Next: target},
		)
	}
}

// StepRaw appends raw HCL body statements into the step block.
func StepRaw(body string) StepOpt {
	return func(s *StepDef) {
		s.RawBody = body
	}
}

// MatchArm defines a switch match block.
type MatchArm struct {
	Condition string
	Next      string
	Outputs   map[string]string
}

// SwitchDef defines a switch block.
type SwitchDef struct {
	Name        string
	Matches     []MatchArm
	DefaultNext string
}

// SwitchOpt configures a SwitchDef.
type SwitchOpt func(*SwitchDef)

// Match adds a match condition block to a switch.
func Match(condition, next string) SwitchOpt {
	return func(sw *SwitchDef) {
		sw.Matches = append(sw.Matches, MatchArm{
			Condition: condition,
			Next:      next,
		})
	}
}

// MatchWithOutput adds a match condition block with output projections.
func MatchWithOutput(condition, next string, outputs map[string]string) SwitchOpt {
	return func(sw *SwitchDef) {
		sw.Matches = append(sw.Matches, MatchArm{
			Condition: condition,
			Next:      next,
			Outputs:   outputs,
		})
	}
}

// DefaultNext sets the next target of the switch default block.
func DefaultNext(next string) SwitchOpt {
	return func(sw *SwitchDef) {
		sw.DefaultNext = next
	}
}

// WaitDef defines a wait block.
type WaitDef struct {
	Name     string
	Signal   string
	Outcomes []OutcomeDef
}

// WaitOpt configures a WaitDef.
type WaitOpt func(*WaitDef)

// WaitOutcome adds an outcome to a wait block.
func WaitOutcome(name, next string) WaitOpt {
	return func(w *WaitDef) {
		w.Outcomes = append(w.Outcomes, OutcomeDef{
			Name: name,
			Next: next,
		})
	}
}

// ApprovalDef defines an approval block.
type ApprovalDef struct {
	Name      string
	Approvers []string
	Reason    string
	Outcomes  []OutcomeDef
}

// ApprovalOpt configures an ApprovalDef.
type ApprovalOpt func(*ApprovalDef)

// ApprovalOutcome adds an outcome to an approval block.
func ApprovalOutcome(name, next string) ApprovalOpt {
	return func(a *ApprovalDef) {
		a.Outcomes = append(a.Outcomes, OutcomeDef{
			Name: name,
			Next: next,
		})
	}
}

// StateDef defines a state block.
type StateDef struct {
	Name     string
	Terminal bool
	Success  *bool
}

// VariableDef defines a workflow variable block.
type VariableDef struct {
	Name    string
	Type    string
	Default string
}

// SubworkflowDef defines a subworkflow declaration block.
type SubworkflowDef struct {
	Name   string
	Source string
	Ref    string
	Input  string
}

// SubworkflowOpt configures a SubworkflowDef.
type SubworkflowOpt func(*SubworkflowDef)

// SubworkflowRef sets the expected pin ref attribute.
func SubworkflowRef(ref string) SubworkflowOpt {
	return func(s *SubworkflowDef) {
		s.Ref = ref
	}
}

// SubworkflowInput sets the input block body inside the subworkflow declaration.
func SubworkflowInput(input string) SubworkflowOpt {
	return func(s *SubworkflowDef) {
		s.Input = input
	}
}

// WorkflowBuilder constructs deterministic HCL workflows for tests.
type WorkflowBuilder struct {
	name         string
	version      string
	initialState string
	targetState  string
	policy       *PolicyDef
	variables    []VariableDef
	adapters     []AdapterDef
	steps        []StepDef
	switches     []SwitchDef
	waits        []WaitDef
	approvals    []ApprovalDef
	states       []StateDef
	subworkflows []SubworkflowDef
	rawBlocks    []string
}

// New creates an empty WorkflowBuilder.
func New() *WorkflowBuilder {
	return &WorkflowBuilder{
		name:    "t",
		version: "0.1",
	}
}

// NewStandard creates a WorkflowBuilder pre-configured with:
// - name: "t", version: "0.1"
// - initial_state: "start"
// - target_state: "done"
// - adapter "noop" "default" {}
// - terminal states "done" (success=true) and "failed" (success=false)
func NewStandard() *WorkflowBuilder {
	trueVal := true
	falseVal := false
	return New().
		WithHeader("t", "0.1", "start", "done").
		WithAdapter("noop", "default").
		WithState("done", true, &trueVal).
		WithState("failed", true, &falseVal)
}

// WithHeader sets the workflow header block attributes.
func (b *WorkflowBuilder) WithHeader(name, version, initial, target string) *WorkflowBuilder {
	b.name = name
	b.version = version
	b.initialState = initial
	b.targetState = target
	return b
}

// WithName sets the workflow name.
func (b *WorkflowBuilder) WithName(name string) *WorkflowBuilder {
	b.name = name
	return b
}

// WithVersion sets the workflow version.
func (b *WorkflowBuilder) WithVersion(version string) *WorkflowBuilder {
	b.version = version
	return b
}

// WithInitial sets initial_state.
func (b *WorkflowBuilder) WithInitial(initial string) *WorkflowBuilder {
	b.initialState = initial
	return b
}

// WithTarget sets target_state.
func (b *WorkflowBuilder) WithTarget(target string) *WorkflowBuilder {
	b.targetState = target
	return b
}

// WithPolicy sets the policy block.
func (b *WorkflowBuilder) WithPolicy(policy PolicyDef) *WorkflowBuilder {
	b.policy = &policy
	return b
}

// WithMaxTotalSteps sets max_total_steps in the policy block.
func (b *WorkflowBuilder) WithMaxTotalSteps(n int) *WorkflowBuilder {
	if b.policy == nil {
		b.policy = &PolicyDef{}
	}
	b.policy.MaxTotalSteps = n
	return b
}

// WithMaxVisitsWarnThreshold sets max_visits_warn_threshold in the policy block.
func (b *WorkflowBuilder) WithMaxVisitsWarnThreshold(n int) *WorkflowBuilder {
	if b.policy == nil {
		b.policy = &PolicyDef{}
	}
	b.policy.MaxVisitsWarnThreshold = &n
	return b
}

// WithVariable adds a variable declaration.
func (b *WorkflowBuilder) WithVariable(name, typeName, defaultVal string) *WorkflowBuilder {
	b.variables = append(b.variables, VariableDef{
		Name:    name,
		Type:    typeName,
		Default: defaultVal,
	})
	return b
}

// WithAdapter adds an adapter declaration.
func (b *WorkflowBuilder) WithAdapter(kind, name string) *WorkflowBuilder {
	b.adapters = append(b.adapters, AdapterDef{Type: kind, Name: name})
	return b
}

// WithStep adds a step block.
func (b *WorkflowBuilder) WithStep(name string, opts ...StepOpt) *WorkflowBuilder {
	s := StepDef{
		Name:   name,
		Target: "adapter.noop.default",
	}
	for _, opt := range opts {
		opt(&s)
	}
	b.steps = append(b.steps, s)
	return b
}

// WithSwitch adds a switch block.
func (b *WorkflowBuilder) WithSwitch(name string, opts ...SwitchOpt) *WorkflowBuilder {
	sw := SwitchDef{Name: name}
	for _, opt := range opts {
		opt(&sw)
	}
	b.switches = append(b.switches, sw)
	return b
}

// WithWait adds a wait block.
func (b *WorkflowBuilder) WithWait(name, signal string, opts ...WaitOpt) *WorkflowBuilder {
	w := WaitDef{Name: name, Signal: signal}
	for _, opt := range opts {
		opt(&w)
	}
	b.waits = append(b.waits, w)
	return b
}

// WithApproval adds an approval block.
func (b *WorkflowBuilder) WithApproval(name string, approvers []string, reason string, opts ...ApprovalOpt) *WorkflowBuilder {
	a := ApprovalDef{
		Name:      name,
		Approvers: approvers,
		Reason:    reason,
	}
	for _, opt := range opts {
		opt(&a)
	}
	b.approvals = append(b.approvals, a)
	return b
}

// WithState adds or updates a state block.
func (b *WorkflowBuilder) WithState(name string, terminal bool, success *bool) *WorkflowBuilder {
	for i, st := range b.states {
		if st.Name == name {
			b.states[i] = StateDef{
				Name:     name,
				Terminal: terminal,
				Success:  success,
			}
			return b
		}
	}
	b.states = append(b.states, StateDef{
		Name:     name,
		Terminal: terminal,
		Success:  success,
	})
	return b
}

// WithSubworkflow adds a subworkflow block.
func (b *WorkflowBuilder) WithSubworkflow(name, source string, opts ...SubworkflowOpt) *WorkflowBuilder {
	sw := SubworkflowDef{
		Name:   name,
		Source: source,
	}
	for _, opt := range opts {
		opt(&sw)
	}
	b.subworkflows = append(b.subworkflows, sw)
	return b
}

// WithRaw appends raw HCL blocks or attributes.
func (b *WorkflowBuilder) WithRaw(raw string) *WorkflowBuilder {
	if strings.TrimSpace(raw) != "" {
		b.rawBlocks = append(b.rawBlocks, raw)
	}
	return b
}

// Build generates formatted, canonical HCL source.
//
//nolint:gocognit,funlen,gocyclo // Build serializes all builder blocks into canonical HCL text
func (b *WorkflowBuilder) Build() string {
	var sb strings.Builder

	// 1. Workflow Header
	sb.WriteString("workflow {\n")
	if b.name != "" {
		sb.WriteString(fmt.Sprintf("  name = %q\n", b.name))
	}
	if b.version != "" {
		sb.WriteString(fmt.Sprintf("  version = %q\n", b.version))
	}
	if b.initialState != "" {
		sb.WriteString(fmt.Sprintf("  initial_state = %q\n", b.initialState))
	}
	if b.targetState != "" {
		sb.WriteString(fmt.Sprintf("  target_state = %q\n", b.targetState))
	}
	if b.policy != nil {
		sb.WriteString("  policy {\n")
		if b.policy.MaxTotalSteps > 0 {
			sb.WriteString(fmt.Sprintf("    max_total_steps = %d\n", b.policy.MaxTotalSteps))
		}
		if b.policy.MaxVisitsWarnThreshold != nil {
			sb.WriteString(fmt.Sprintf("    max_visits_warn_threshold = %d\n", *b.policy.MaxVisitsWarnThreshold))
		}
		if b.policy.MaxStepRetries > 0 {
			sb.WriteString(fmt.Sprintf("    max_step_retries = %d\n", b.policy.MaxStepRetries))
		}
		sb.WriteString("  }\n")
	}
	sb.WriteString("}\n\n")

	// 2. Variables
	for _, v := range b.variables {
		sb.WriteString(fmt.Sprintf("variable %q {\n", v.Name))
		if v.Type != "" {
			sb.WriteString(fmt.Sprintf("  type = %s\n", v.Type))
		}
		if v.Default != "" {
			sb.WriteString(fmt.Sprintf("  default = %s\n", v.Default))
		}
		sb.WriteString("}\n\n")
	}

	// 3. Adapters
	for _, a := range b.adapters {
		sb.WriteString(fmt.Sprintf("adapter %q %q {}\n", a.Type, a.Name))
	}
	if len(b.adapters) > 0 {
		sb.WriteString("\n")
	}

	// 4. Subworkflows
	for _, sw := range b.subworkflows {
		sb.WriteString(fmt.Sprintf("subworkflow %q {\n", sw.Name))
		sb.WriteString(fmt.Sprintf("  source = %q\n", sw.Source))
		if sw.Ref != "" {
			sb.WriteString(fmt.Sprintf("  ref    = %q\n", sw.Ref))
		}
		if sw.Input != "" {
			sb.WriteString(fmt.Sprintf("  input = {\n    %s\n  }\n", strings.TrimSpace(sw.Input)))
		}
		sb.WriteString("}\n\n")
	}

	// 5. Steps
	for stepIdx := range b.steps {
		s := &b.steps[stepIdx]
		sb.WriteString(fmt.Sprintf("step %q {\n", s.Name))
		if s.Target != "" {
			sb.WriteString(fmt.Sprintf("  target = %s\n", s.Target))
		}
		if s.MaxVisits != 0 {
			sb.WriteString(fmt.Sprintf("  max_visits = %d\n", s.MaxVisits))
		}
		if s.Count > 0 {
			sb.WriteString(fmt.Sprintf("  count = %d\n", s.Count))
		}
		if len(s.ForEach) > 0 {
			sb.WriteString("  for_each = [")
			for i, item := range s.ForEach {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(fmt.Sprintf("%q", item))
			}
			sb.WriteString("]\n")
		}
		if len(s.Parallel) > 0 {
			sb.WriteString("  parallel = [")
			for i, item := range s.Parallel {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(fmt.Sprintf("%q", item))
			}
			sb.WriteString("]\n")
		}
		if s.RawBody != "" {
			sb.WriteString("  " + strings.TrimSpace(s.RawBody) + "\n")
		}
		for outcomeIdx := range s.Outcomes {
			o := &s.Outcomes[outcomeIdx]
			sb.WriteString(fmt.Sprintf("  outcome %q {\n", o.Name))
			if o.Fallback {
				sb.WriteString("    fallback = true\n")
			}
			if o.Next != "" {
				sb.WriteString(fmt.Sprintf("    next = %s\n", o.Next))
			}
			if len(o.Outputs) > 0 {
				sb.WriteString("    output = {\n")
				keys := make([]string, 0, len(o.Outputs))
				for k := range o.Outputs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					sb.WriteString(fmt.Sprintf("      %s = %s\n", k, o.Outputs[k]))
				}
				sb.WriteString("    }\n")
			}
			sb.WriteString("  }\n")
		}
		sb.WriteString("}\n\n")
	}

	// 5. Switches
	for _, sw := range b.switches {
		sb.WriteString(fmt.Sprintf("switch %q {\n", sw.Name))
		for _, m := range sw.Matches {
			sb.WriteString("  match {\n")
			sb.WriteString(fmt.Sprintf("    condition = %s\n", m.Condition))
			sb.WriteString(fmt.Sprintf("    next = %s\n", m.Next))
			if len(m.Outputs) > 0 {
				sb.WriteString("    output = {\n")
				keys := make([]string, 0, len(m.Outputs))
				for k := range m.Outputs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					sb.WriteString(fmt.Sprintf("      %s = %s\n", k, m.Outputs[k]))
				}
				sb.WriteString("    }\n")
			}
			sb.WriteString("  }\n")
		}
		if sw.DefaultNext != "" {
			sb.WriteString(fmt.Sprintf("  default {\n    next = %s\n  }\n", sw.DefaultNext))
		}
		sb.WriteString("}\n\n")
	}

	// 6. Waits
	for _, w := range b.waits {
		sb.WriteString(fmt.Sprintf("wait %q {\n", w.Name))
		if w.Signal != "" {
			sb.WriteString(fmt.Sprintf("  signal = %q\n", w.Signal))
		}
		for _, o := range w.Outcomes {
			sb.WriteString(fmt.Sprintf("  outcome %q { next = %s }\n", o.Name, o.Next))
		}
		sb.WriteString("}\n\n")
	}

	// 7. Approvals
	for _, a := range b.approvals {
		sb.WriteString(fmt.Sprintf("approval %q {\n", a.Name))
		if len(a.Approvers) > 0 {
			sb.WriteString("  approvers = [")
			for i, app := range a.Approvers {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(fmt.Sprintf("%q", app))
			}
			sb.WriteString("]\n")
		}
		if a.Reason != "" {
			sb.WriteString(fmt.Sprintf("  reason = %q\n", a.Reason))
		}
		for _, o := range a.Outcomes {
			sb.WriteString(fmt.Sprintf("  outcome %q { next = %s }\n", o.Name, o.Next))
		}
		sb.WriteString("}\n\n")
	}

	// 8. States
	for _, st := range b.states {
		sb.WriteString(fmt.Sprintf("state %q {\n", st.Name))
		sb.WriteString(fmt.Sprintf("  terminal = %t\n", st.Terminal))
		if st.Success != nil {
			sb.WriteString(fmt.Sprintf("  success = %t\n", *st.Success))
		}
		sb.WriteString("}\n\n")
	}

	// 9. Raw Blocks
	for _, raw := range b.rawBlocks {
		sb.WriteString(strings.TrimSpace(raw))
		sb.WriteString("\n\n")
	}

	formatted := hclwrite.Format([]byte(sb.String()))
	return string(formatted)
}

// Standalone snippet formatters for quick inline injection.

// Step generates a step block snippet.
func Step(name string, opts ...StepOpt) string {
	s := StepDef{Name: name, Target: "adapter.noop.default"}
	for _, opt := range opts {
		opt(&s)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("step %q {\n", s.Name))
	if s.Target != "" {
		sb.WriteString(fmt.Sprintf("  target = %s\n", s.Target))
	}
	if s.MaxVisits > 0 {
		sb.WriteString(fmt.Sprintf("  max_visits = %d\n", s.MaxVisits))
	}
	if s.Count > 0 {
		sb.WriteString(fmt.Sprintf("  count = %d\n", s.Count))
	}
	if len(s.ForEach) > 0 {
		sb.WriteString("  for_each = [")
		for i, item := range s.ForEach {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(fmt.Sprintf("%q", item))
		}
		sb.WriteString("]\n")
	}
	if len(s.Parallel) > 0 {
		sb.WriteString("  parallel = [")
		for i, item := range s.Parallel {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(fmt.Sprintf("%q", item))
		}
		sb.WriteString("]\n")
	}
	for _, o := range s.Outcomes {
		sb.WriteString(fmt.Sprintf("  outcome %q { next = %s }\n", o.Name, o.Next))
	}
	sb.WriteString("}\n")
	return string(hclwrite.Format([]byte(sb.String())))
}

// Switch generates a switch block snippet.
func Switch(name string, opts ...SwitchOpt) string {
	sw := SwitchDef{Name: name}
	for _, opt := range opts {
		opt(&sw)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("switch %q {\n", sw.Name))
	for _, m := range sw.Matches {
		sb.WriteString("  match {\n")
		sb.WriteString(fmt.Sprintf("    condition = %s\n", m.Condition))
		sb.WriteString(fmt.Sprintf("    next = %s\n", m.Next))
		sb.WriteString("  }\n")
	}
	if sw.DefaultNext != "" {
		sb.WriteString(fmt.Sprintf("  default { next = %s }\n", sw.DefaultNext))
	}
	sb.WriteString("}\n")
	return string(hclwrite.Format([]byte(sb.String())))
}

// Wait generates a wait block snippet.
func Wait(name, signal string, opts ...WaitOpt) string {
	w := WaitDef{Name: name, Signal: signal}
	for _, opt := range opts {
		opt(&w)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("wait %q {\n", w.Name))
	sb.WriteString(fmt.Sprintf("  signal = %q\n", w.Signal))
	for _, o := range w.Outcomes {
		sb.WriteString(fmt.Sprintf("  outcome %q { next = %s }\n", o.Name, o.Next))
	}
	sb.WriteString("}\n")
	return string(hclwrite.Format([]byte(sb.String())))
}

// Approval generates an approval block snippet.
func Approval(name string, approvers []string, reason string, opts ...ApprovalOpt) string {
	a := ApprovalDef{Name: name, Approvers: approvers, Reason: reason}
	for _, opt := range opts {
		opt(&a)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("approval %q {\n", a.Name))
	sb.WriteString("  approvers = [")
	for i, app := range a.Approvers {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("%q", app))
	}
	sb.WriteString("]\n")
	sb.WriteString(fmt.Sprintf("  reason = %q\n", a.Reason))
	for _, o := range a.Outcomes {
		sb.WriteString(fmt.Sprintf("  outcome %q { next = %s }\n", o.Name, o.Next))
	}
	sb.WriteString("}\n")
	return string(hclwrite.Format([]byte(sb.String())))
}

// State generates a state block snippet.
func State(name string, terminal, success bool) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("state %q {\n", name))
	sb.WriteString(fmt.Sprintf("  terminal = %t\n", terminal))
	sb.WriteString(fmt.Sprintf("  success = %t\n", success))
	sb.WriteString("}\n")
	return string(hclwrite.Format([]byte(sb.String())))
}

// WrapStep wraps a step body inside a standard compilable workflow with initial_state "work" and target_state "done".
func WrapStep(stepBody string) string {
	return WrapStepWithAdapter("noop", stepBody)
}

// WrapStepWithAdapter wraps a step body inside a standard compilable workflow using the specified adapter type.
func WrapStepWithAdapter(adapterType, stepBody string) string {
	b := NewStandard().
		WithInitial("work")
	if adapterType != "noop" {
		b.WithAdapter(adapterType, "default")
	}
	b.WithStep("work",
		Target(fmt.Sprintf("adapter.%s.default", adapterType)),
		StepRaw(stepBody),
	)
	return b.Build()
}

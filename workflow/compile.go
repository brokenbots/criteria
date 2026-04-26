package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
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
// schemas maps adapter name to its declared AdapterInfo for compile-time
// validation of agent config and step input blocks. Pass nil to skip schema
// validation (permissive mode: any keys accepted).
func Compile(spec *Spec, schemas map[string]AdapterInfo) (*FSMGraph, hcl.Diagnostics) {
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
		Variables:    map[string]*VariableNode{},
		Agents:       map[string]*AgentNode{},
		Steps:        map[string]*StepNode{},
		States:       map[string]*StateNode{},
		Waits:        map[string]*WaitNode{},
		Approvals:    map[string]*ApprovalNode{},
		Branches:     map[string]*BranchNode{},
		ForEachs:     map[string]*ForEachNode{},
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

	// First pass: compile variables, detect duplicates and type errors.
	for _, vs := range spec.Variables {
		name := vs.Name
		if _, dup := g.Variables[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate variable %q", name)})
			continue
		}
		typ, err := parseVariableType(vs.TypeStr)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("variable %q: %v", name, err)})
			continue
		}
		defaultVal := cty.NilVal
		if vs.Remain != nil {
			attrs, d := vs.Remain.JustAttributes()
			diags = append(diags, d...)
			if defAttr, ok := attrs["default"]; ok {
				var defDiags hcl.Diagnostics
				defaultVal, defDiags = defAttr.Expr.Value(nil)
				if defDiags.HasErrors() {
					diags = append(diags, defDiags...)
					defaultVal = cty.NilVal
				} else {
					// Coerce to declared type.
					defaultVal, err = convertCtyValue(defaultVal, typ)
					if err != nil {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  fmt.Sprintf("variable %q: default value does not match declared type %q: %v", name, vs.TypeStr, err),
						})
						defaultVal = cty.NilVal
					}
				}
			}
		}
		g.Variables[name] = &VariableNode{
			Name:        name,
			Type:        typ,
			Default:     defaultVal,
			Description: vs.Description,
		}
	}

	// Second pass: declare agents and states, detect duplicates.
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
		var agentConfig map[string]string
		if ag.Config != nil {
			attrs, d := ag.Config.Remain.JustAttributes()
			diags = append(diags, d...)
			ctxLabel := fmt.Sprintf("agent %q config", name)
			missingRange := ag.Config.Remain.MissingItemRange()
			if info, ok := adapterInfo(schemas, ag.Adapter); ok {
				// Schema-aware decode: validates types, unknown keys, required fields.
				agentConfig, d = validateSchemaAttrs(ctxLabel, attrs, info.ConfigSchema, missingRange)
			} else {
				// Permissive decode: no schema available.
				agentConfig, d = decodeAttrsToStringMap(attrs)
			}
			diags = append(diags, d...)
		}
		g.Agents[name] = &AgentNode{
			Name:    name,
			Adapter: ag.Adapter,
			OnCrash: effectiveOnCrash,
			Config:  agentConfig,
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
		if len(sp.AllowTools) > 0 && !hasAgent {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: allow_tools requires agent", sp.Name)})
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
			if sp.Input != nil {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: lifecycle %q must not include input", sp.Name, sp.Lifecycle)})
			}
			if len(sp.AllowTools) > 0 {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: allow_tools is only valid on execute-shape steps (not lifecycle open/close)", sp.Name)})
			}
		}
		// Legacy config = { ... } attribute: emit a helpful migration diagnostic.
		if len(sp.Config) > 0 {
			var subject *hcl.Range
			if sp.LegacyConfigRange != nil {
				r := *sp.LegacyConfigRange
				subject = &r
			}
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: \"config\" attribute removed; use \"input { }\" block instead (Phase 1.5)", sp.Name),
				Detail:   "Replace `config = { key = \"value\" }` with `input { key = \"value\" }` in your workflow.",
				Subject:  subject,
			})
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
		// Decode input { } block into a string map and collect raw expressions.
		// Attributes with variable references (e.g. "${var.env}") cannot be
		// evaluated at compile time; validateSchemaAttrs skips value evaluation
		// for them (see permissive expression handling). The engine re-evaluates
		// all InputExprs at step entry via BuildEvalContext(rs.Vars).
		var inputMap map[string]string
		var inputExprs map[string]hcl.Expression
		if sp.Input != nil {
			attrs, d := sp.Input.Remain.JustAttributes()
			diags = append(diags, d...)
			ctxLabel := fmt.Sprintf("step %q input", sp.Name)
			missingRange := sp.Input.Remain.MissingItemRange()
			adapterName := sp.Adapter
			if hasAgent {
				if agent, ok := g.Agents[sp.Agent]; ok {
					adapterName = agent.Adapter
				}
			}
			if adapterName != "" {
				if info, ok := adapterInfo(schemas, adapterName); ok {
					// Schema-aware decode: validates types, unknown keys, required fields.
					inputMap, d = validateSchemaAttrs(ctxLabel, attrs, info.InputSchema, missingRange)
				} else {
					// Permissive decode.
					inputMap, d = decodeAttrsToStringMap(attrs)
				}
			} else {
				inputMap, d = decodeAttrsToStringMap(attrs)
			}
			diags = append(diags, d...)
			// Collect all attribute expressions for runtime evaluation (W04).
			inputExprs = make(map[string]hcl.Expression, len(attrs))
			for k, attr := range attrs {
				inputExprs[k] = attr.Expr
			}
		}
		node := &StepNode{
			Name:       sp.Name,
			Adapter:    sp.Adapter,
			Agent:      sp.Agent,
			Lifecycle:  sp.Lifecycle,
			OnCrash:    effectiveOnCrash,
			Input:      inputMap,
			InputExprs: inputExprs,
			Timeout:    timeout,
			Outcomes:   map[string]string{},
			AllowTools: allowToolsForStep(sp, spec),
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

	// Compile wait blocks (W05).
	for _, ws := range spec.Waits {
		name := ws.Name
		if _, dup := g.Steps[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q clashes with step of the same name", name)})
			continue
		}
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q clashes with state of the same name", name)})
			continue
		}
		if _, dup := g.Waits[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate wait %q", name)})
			continue
		}
		hasDuration := ws.Duration != ""
		hasSignal := ws.Signal != ""
		if hasDuration == hasSignal { // both or neither
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q: exactly one of duration or signal must be set", name)})
			continue
		}
		node := &WaitNode{
			Name:     name,
			Signal:   ws.Signal,
			Outcomes: map[string]string{},
		}
		if hasDuration {
			d, err := time.ParseDuration(ws.Duration)
			if err != nil {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q: invalid duration %q: %v", name, ws.Duration, err)})
				continue
			}
			node.Duration = d
		}
		if len(ws.Outcomes) == 0 {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q: at least one outcome is required", name)})
		}
		for _, o := range ws.Outcomes {
			if o.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("wait %q outcome %q: transition_to required", name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.TransitionTo
		}
		g.Waits[name] = node
	}

	// Compile approval blocks (W05).
	for _, as := range spec.Approvals {
		name := as.Name
		if _, dup := g.Steps[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q clashes with step of the same name", name)})
			continue
		}
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q clashes with state of the same name", name)})
			continue
		}
		if _, dup := g.Waits[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q clashes with wait of the same name", name)})
			continue
		}
		if _, dup := g.Approvals[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate approval %q", name)})
			continue
		}
		node := &ApprovalNode{
			Name:      name,
			Approvers: as.Approvers,
			Reason:    as.Reason,
			Outcomes:  map[string]string{},
		}
		for _, o := range as.Outcomes {
			if o.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q outcome %q: transition_to required", name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.TransitionTo
		}
		// Enforce required outcomes: approved and rejected must both be present.
		if _, ok := node.Outcomes["approved"]; !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q: outcome \"approved\" is required", name)})
		}
		if _, ok := node.Outcomes["rejected"]; !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("approval %q: outcome \"rejected\" is required", name)})
		}
		g.Approvals[name] = node
	}

	// Compile branch blocks (W06).
	for _, bs := range spec.Branches {
		name := bs.Name
		// Clash checks: must not duplicate any existing node kind.
		if _, dup := g.Steps[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with step of the same name", name)})
			continue
		}
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with state of the same name", name)})
			continue
		}
		if _, dup := g.Waits[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with wait of the same name", name)})
			continue
		}
		if _, dup := g.Approvals[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q clashes with approval of the same name", name)})
			continue
		}
		if _, dup := g.Branches[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate branch %q", name)})
			continue
		}
		// Default block is required.
		if bs.Default == nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q: default block is required", name)})
			continue
		}
		if bs.Default.TransitionTo == "" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q: default transition_to is required", name)})
			continue
		}
		// Compile arms.
		node := &BranchNode{
			Name:          name,
			DefaultTarget: bs.Default.TransitionTo,
		}
		for i, arm := range bs.Arms {
			if arm.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q arm[%d]: transition_to is required", name, i)})
				continue
			}
			// Extract the `when` expression from the remain body.
			var condExpr hcl.Expression
			if arm.Remain != nil {
				attrs, d := arm.Remain.JustAttributes()
				diags = append(diags, d...)
				if whenAttr, ok := attrs["when"]; ok {
					condExpr = whenAttr.Expr
				}
			}
			if condExpr == nil {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("branch %q arm[%d]: when expression is required", name, i)})
				continue
			}
			// Walk the expression traversals: validate that referenced var/<name>
			// and steps/<name> names exist in the graph. This is a best-effort
			// static check; full type evaluation is a runtime concern.
			for _, traversal := range condExpr.Variables() {
				if len(traversal) < 2 {
					continue
				}
				root, rootOK := traversal[0].(hcl.TraverseRoot)
				if !rootOK {
					continue
				}
				attr, attrOK := traversal[1].(hcl.TraverseAttr)
				if !attrOK {
					continue
				}
				switch root.Name {
				case "var":
					if _, known := g.Variables[attr.Name]; !known {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  fmt.Sprintf("branch %q arm[%d]: undefined variable %q", name, i, attr.Name),
						})
					}
				case "steps":
					if _, known := g.Steps[attr.Name]; !known {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  fmt.Sprintf("branch %q arm[%d]: unknown step %q referenced in condition", name, i, attr.Name),
						})
					}
				}
			}
			// Capture the source text of the condition expression so it can be
			// surfaced in BranchEvaluated events (W06).
			condSrc := ""
			if spec.SourceBytes != nil {
				type noder interface{ Range() hcl.Range }
				if nr, ok := condExpr.(noder); ok {
					r := nr.Range()
					if r.Start.Byte < r.End.Byte && r.End.Byte <= len(spec.SourceBytes) {
						condSrc = string(spec.SourceBytes[r.Start.Byte:r.End.Byte])
					}
				}
			}
			node.Arms = append(node.Arms, BranchArm{
				Condition:    condExpr,
				ConditionSrc: condSrc,
				Target:       arm.TransitionTo,
			})
		}
		g.Branches[name] = node
	}

	// Compile for_each blocks (W07).
	for _, fs := range spec.ForEachs {
		name := fs.Name
		// Clash checks against every existing node kind.
		if _, dup := g.Steps[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q clashes with step of the same name", name)})
			continue
		}
		if _, dup := g.States[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q clashes with state of the same name", name)})
			continue
		}
		if _, dup := g.Waits[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q clashes with wait of the same name", name)})
			continue
		}
		if _, dup := g.Approvals[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q clashes with approval of the same name", name)})
			continue
		}
		if _, dup := g.Branches[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q clashes with branch of the same name", name)})
			continue
		}
		if _, dup := g.ForEachs[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate for_each %q", name)})
			continue
		}

		// `do` must reference an existing step.
		if fs.Do == "" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q: do is required", name)})
			continue
		}
		doStep, doKnown := g.Steps[fs.Do]
		if !doKnown {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q: do = %q does not reference a known step", name, fs.Do)})
		}

		// Extract the `items` expression from the remain body.
		var itemsExpr hcl.Expression
		if fs.Remain != nil {
			// Use PartialContent to fetch only the "items" attribute, so that
			// the "outcome" blocks that gohcl placed in Remain do not cause
			// "Blocks are not allowed here" diagnostics from JustAttributes.
			content, _, d := fs.Remain.PartialContent(&hcl.BodySchema{
				Attributes: []hcl.AttributeSchema{
					{Name: "items", Required: false},
				},
			})
			diags = append(diags, d...)
			if content != nil {
				if itemsAttr, ok := content.Attributes["items"]; ok {
					itemsExpr = itemsAttr.Expr
				}
			}
		}
		if itemsExpr == nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q: items is required", name)})
		}

		// Compile outcomes.
		node := &ForEachNode{
			Name:     name,
			Items:    itemsExpr,
			Do:       fs.Do,
			Outcomes: map[string]string{},
		}
		for _, o := range fs.Outcomes {
			if o.TransitionTo == "" {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q outcome %q: transition_to required", name, o.Name)})
				continue
			}
			node.Outcomes[o.Name] = o.TransitionTo
		}

		// all_succeeded is required.
		if _, ok := node.Outcomes["all_succeeded"]; !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("for_each %q: outcome \"all_succeeded\" is required", name)})
		}
		// any_failed is recommended (warning if absent).
		if _, ok := node.Outcomes["any_failed"]; !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("for_each %q: outcome \"any_failed\" is not declared; failed iterations will fall through to \"all_succeeded\"", name),
			})
		}

		// Warn if the do step has no _continue transition (implies single-iteration only).
		if doKnown {
			hasContinue := false
			for _, target := range doStep.Outcomes {
				if target == "_continue" {
					hasContinue = true
					break
				}
			}
			if !hasContinue {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagWarning,
					Summary:  fmt.Sprintf("for_each %q: step %q has no outcome transitioning to \"_continue\"; the loop will execute at most one iteration", name, fs.Do),
				})
			}
		}

		g.ForEachs[name] = node
	}

	// Validate that _continue is not declared as a state name (it is engine-internal).
	for _, st := range spec.States {
		if st.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a state`})
		}
	}
	for _, st := range spec.Steps {
		if st.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a step`})
		}
	}
	for _, w := range spec.Waits {
		if w.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a wait`})
		}
	}
	for _, a := range spec.Approvals {
		if a.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as an approval`})
		}
	}
	for _, fe := range spec.ForEachs {
		if fe.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a for_each`})
		}
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
	for _, fe := range g.ForEachs {
		for outcome, target := range fe.Outcomes {
			if _, ok := g.Lookup(target); !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("for_each %q outcome %q -> unknown target %q", fe.Name, outcome, target),
				})
			}
		}
	}

	// Reachability from initial state.
	if g.InitialState != "" && !diags.HasErrors() {
		reachable := map[string]bool{g.InitialState: true}
		var walk func(name string)
		walk = func(name string) {
			if step, isStep := g.Steps[name]; isStep {
				for _, target := range step.Outcomes {
					if target == "_continue" {
						// _continue is synthetic; mark the owning for_each node reachable
						// by letting the for_each's outgoing edges drive reachability.
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
			if fe, isForEach := g.ForEachs[name]; isForEach {
				// Mark the do-step reachable via the for_each.
				if !reachable[fe.Do] {
					reachable[fe.Do] = true
					walk(fe.Do)
				}
				// Mark all aggregate outcome targets reachable.
				for _, target := range fe.Outcomes {
					if !reachable[target] {
						reachable[target] = true
						walk(target)
					}
				}
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
		for name := range g.ForEachs {
			if !reachable[name] {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: fmt.Sprintf("for_each %q is unreachable from initial_state", name)})
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

// allowToolsForStep returns the effective AllowTools for a step. Lifecycle
// steps (open/close) never receive allow_tools — permission gating is only
// meaningful on execute-shape steps.
func allowToolsForStep(sp StepSpec, spec *Spec) []string {
	if sp.Lifecycle != "" {
		return nil
	}
	return unionAllowTools(sp.AllowTools, workflowAllowTools(spec))
}

// workflowAllowTools extracts the workflow-level AllowTools list from a Spec.
func workflowAllowTools(spec *Spec) []string {
	if spec.Permissions == nil {
		return nil
	}
	return spec.Permissions.AllowTools
}

// unionAllowTools returns the union of step-level and workflow-level patterns.
// Duplicates are not removed — first-match-wins semantics make them harmless.
func unionAllowTools(stepTools, workflowTools []string) []string {
	if len(stepTools) == 0 && len(workflowTools) == 0 {
		return nil
	}
	out := make([]string, 0, len(stepTools)+len(workflowTools))
	out = append(out, stepTools...)
	out = append(out, workflowTools...)
	return out
}

// decodeAttrsToStringMap converts pre-fetched hcl.Attributes into a map[string]string.
// Numbers and bools are converted to their string representations.
// Attributes that cannot be evaluated without an EvalContext (e.g. variable
// references like "${var.env}") are stored as empty strings and deferred to
// runtime evaluation via InputExprs / BuildEvalContext (W04).
func decodeAttrsToStringMap(attrs hcl.Attributes) (map[string]string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	result := make(map[string]string, len(attrs))
	for k, attr := range attrs {
		val, d := attr.Expr.Value(nil)
		if d.HasErrors() {
			// Expression needs an EvalContext (e.g. variable references).
			// Store an empty placeholder; the engine evaluates at step entry.
			result[k] = ""
			continue
		}
		diags = append(diags, d...)
		switch val.Type() {
		case cty.String:
			result[k] = val.AsString()
		case cty.Number:
			bf := val.AsBigFloat()
			result[k] = bf.Text('f', -1)
		case cty.Bool:
			if val.True() {
				result[k] = "true"
			} else {
				result[k] = "false"
			}
		default:
			result[k] = val.GoString()
		}
	}
	return result, diags
}

// decodeBodyToStringMap converts an hcl.Body of key = "value" attributes into
// a map[string]string. Numbers and bools are converted to their string
// representations. Expression references (variables, functions) that cannot be
// evaluated without a context are deferred to W04.
func decodeBodyToStringMap(body hcl.Body) (map[string]string, hcl.Diagnostics) {
	if body == nil {
		return nil, nil
	}
	attrs, diags := body.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}
	return decodeAttrsToStringMap(attrs)
}

// adapterInfo looks up the AdapterInfo for a given adapter name in the schemas
// map. Returns (info, true) when found and the schema is non-empty (i.e. the
// adapter declared schemas). Returns (zero, false) when permissive mode applies.
func adapterInfo(schemas map[string]AdapterInfo, adapterName string) (AdapterInfo, bool) {
	if schemas == nil {
		return AdapterInfo{}, false
	}
	info, ok := schemas[adapterName]
	return info, ok
}

// validateSchemaAttrs validates raw HCL attributes against a ConfigField schema,
// attaching source ranges to diagnostics. It handles required/unknown key checks
// and type mismatch checks. Returns (decoded string map, diagnostics).
func validateSchemaAttrs(context string, attrs hcl.Attributes, schema map[string]ConfigField, missingRange hcl.Range) (map[string]string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	result := make(map[string]string, len(attrs))

	for k, attr := range attrs {
		field, known := schema[k]
		if len(schema) > 0 && !known {
			r := attr.NameRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("%s: unknown field %q", context, k),
				Subject:  &r,
			})
			continue
		}
		val, d := attr.Expr.Value(nil)
		if d.HasErrors() {
			// Expression needs an EvalContext (e.g. variable references).
			// Store an empty placeholder; the engine evaluates at step entry.
			// Unknown-key check already ran above; type check is deferred to runtime.
			result[k] = ""
			continue
		}
		diags = append(diags, d...)
		// Type check against declared schema type.
		if len(schema) > 0 {
			r := attr.Expr.StartRange()
			switch field.Type {
			case ConfigFieldNumber:
				if val.Type() != cty.Number {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a number", context, k),
						Subject:  &r,
					})
					continue
				}
			case ConfigFieldBool:
				if val.Type() != cty.Bool {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a bool", context, k),
						Subject:  &r,
					})
					continue
				}
			case ConfigFieldListString:
				if !isListStringValue(val) {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a list of strings", context, k),
						Subject:  &r,
					})
					continue
				}
			case ConfigFieldString:
				if val.Type() != cty.String {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("%s: field %q must be a string", context, k),
						Subject:  &r,
					})
					continue
				}
			}
		}
		// Coerce to string for the output map.
		switch val.Type() {
		case cty.String:
			result[k] = val.AsString()
		case cty.Number:
			bf := val.AsBigFloat()
			result[k] = bf.Text('f', -1)
		case cty.Bool:
			if val.True() {
				result[k] = "true"
			} else {
				result[k] = "false"
			}
		default:
			result[k] = val.GoString()
		}
	}

	// Check required fields. Use attrs for presence check so that expression-
	// valued attributes (deferred to runtime) are not reported as missing.
	for k, field := range schema {
		if field.Required {
			if _, present := attrs[k]; !present {
				var subject *hcl.Range
				if missingRange.Filename != "" {
					r := missingRange
					subject = &r
				}
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("%s: required field %q is missing", context, k),
					Subject:  subject,
				})
			}
		}
	}

	return result, diags
}

func isListStringValue(val cty.Value) bool {
	t := val.Type()
	if t.IsListType() {
		return t.ElementType() == cty.String
	}
	if !t.IsTupleType() {
		return false
	}
	for _, et := range t.TupleElementTypes() {
		if et != cty.String {
			return false
		}
	}
	return true
}

// parseVariableType converts a type string from a variable declaration into
// a cty.Type. Only the subset documented in W04 is supported.
func parseVariableType(typeStr string) (cty.Type, error) {
	switch strings.TrimSpace(typeStr) {
	case "", "string":
		return cty.String, nil
	case "number":
		return cty.Number, nil
	case "bool":
		return cty.Bool, nil
	case "list(string)":
		return cty.List(cty.String), nil
	case "list(number)":
		return cty.List(cty.Number), nil
	case "list(bool)":
		return cty.List(cty.Bool), nil
	case "map(string)":
		return cty.Map(cty.String), nil
	default:
		return cty.NilType, fmt.Errorf("unsupported type %q; supported: string, number, bool, list(string), list(number), list(bool), map(string)", typeStr)
	}
}

// convertCtyValue verifies that v matches typ exactly. No implicit coercions
// are performed: a number default declared on a string variable is an error,
// matching the W04 rule that "default must match declared type".
func convertCtyValue(v cty.Value, typ cty.Type) (cty.Value, error) {
	if v.Type().Equals(typ) {
		return v, nil
	}
	return cty.NilVal, fmt.Errorf("default value is %s but variable is declared as %s", v.Type().FriendlyName(), typ.FriendlyName())
}

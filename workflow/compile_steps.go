package workflow

// compile_steps.go — step block compile, input handling, outcome wiring,
// and step-level allow-tools resolution.

import (
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// compileSteps compiles all step blocks from spec into g.Steps and g.stepOrder.
// Must be called after compileAgents so that agent references can be resolved.
// opts carries compile options including WorkflowDir (for file() validation)
// and LoadDepth (for nested workflow body compilation).
func compileSteps(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
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
		hasWorkflowType := sp.Type == "workflow"

		// Exactly one of adapter, agent, or type="workflow" must be set.
		numKinds := 0
		if hasAdapter {
			numKinds++
		}
		if hasAgent {
			numKinds++
		}
		if hasWorkflowType {
			numKinds++
		}
		if sp.Type != "" && sp.Type != "workflow" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid type %q; only \"workflow\" is recognised", sp.Name, sp.Type)})
		} else if numKinds != 1 {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: exactly one of adapter, agent, or type=\"workflow\" must be set", sp.Name)})
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

		// Validate on_failure.
		if sp.OnFailure != "" {
			switch sp.OnFailure {
			case "continue", "abort", "ignore":
				// valid
			default:
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid on_failure %q; must be \"continue\", \"abort\", or \"ignore\"", sp.Name, sp.OnFailure)})
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

		// Decode for_each and count expressions from the Remain body.
		var forEachExpr hcl.Expression
		var countExpr hcl.Expression
		if sp.Remain != nil {
			content, _, d := sp.Remain.PartialContent(&hcl.BodySchema{
				Attributes: []hcl.AttributeSchema{
					{Name: "for_each", Required: false},
					{Name: "count", Required: false},
				},
			})
			diags = append(diags, d...)
			if content != nil {
				if attr, ok := content.Attributes["for_each"]; ok {
					forEachExpr = attr.Expr
				}
				if attr, ok := content.Attributes["count"]; ok {
					countExpr = attr.Expr
				}
			}
		}
		if forEachExpr != nil && countExpr != nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: for_each and count are mutually exclusive", sp.Name)})
		}
		isIterating := forEachExpr != nil || countExpr != nil

		// on_failure is only meaningful on iterating steps.
		if sp.OnFailure != "" && !isIterating {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: on_failure requires for_each or count", sp.Name)})
		}

		// Decode input { } block.
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
					inputMap, d = validateSchemaAttrs(ctxLabel, attrs, info.InputSchema, missingRange, adapterName)
				} else {
					inputMap, d = decodeAttrsToStringMap(attrs)
				}
			} else {
				inputMap, d = decodeAttrsToStringMap(attrs)
			}
			diags = append(diags, d...)
			inputExprs = make(map[string]hcl.Expression, len(attrs))
			for k, attr := range attrs {
				inputExprs[k] = attr.Expr
			}
			if opts.WorkflowDir != "" {
				diags = append(diags, validateFileFunctionCalls(attrs, opts.WorkflowDir)...)
			}
		}

		// each.* references are only valid inside iterating steps (for_each or count)
		// or inside a workflow body (LoadDepth > 0, where the parent step provides
		// each.* bindings).
		if !isIterating && opts.LoadDepth == 0 {
			for k, expr := range inputExprs {
				if refsEach(expr) {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("step %q input.%s: each._idx, each.key, each.value, each._prev, each._total, each._first, and each._last are only available inside iterating steps (for_each or count)", sp.Name, k),
					})
				}
			}
		}

		node := &StepNode{
			Name:       sp.Name,
			Adapter:    sp.Adapter,
			Agent:      sp.Agent,
			Lifecycle:  sp.Lifecycle,
			OnCrash:    effectiveOnCrash,
			Type:       sp.Type,
			OnFailure:  sp.OnFailure,
			Input:      inputMap,
			InputExprs: inputExprs,
			Timeout:    timeout,
			Outcomes:   map[string]string{},
			AllowTools: allowToolsForStep(sp, spec),
			ForEach:    forEachExpr,
			Count:      countExpr,
		}

		// Compile outcomes.
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

		// Iterating steps must declare all_succeeded; any_failed is recommended.
		if isIterating {
			if _, ok := node.Outcomes["all_succeeded"]; !ok {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: iterating steps must declare outcome \"all_succeeded\"", sp.Name)})
			}
			if _, ok := node.Outcomes["any_failed"]; !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagWarning,
					Summary:  fmt.Sprintf("step %q: outcome \"any_failed\" not declared; failed iterations will fall through to \"all_succeeded\"", sp.Name),
				})
			}
		} else if len(node.Outcomes) == 0 {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
		}

		// Compile workflow body for type="workflow" steps.
		if hasWorkflowType {
			bodyDiags, body, bodyEntry := compileWorkflowBody(&sp, schemas, opts)
			diags = append(diags, bodyDiags...)
			node.Body = body
			node.BodyEntry = bodyEntry

			// Extract output{} block value expressions and populate node.Outputs.
			if sp.Workflow != nil && len(sp.Workflow.Outputs) > 0 {
				seen := map[string]bool{}
				node.Outputs = make(map[string]hcl.Expression, len(sp.Workflow.Outputs))
				for _, out := range sp.Workflow.Outputs {
					if out == nil {
						continue
					}
					if seen[out.Name] {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  fmt.Sprintf("step %q: duplicate output name %q", sp.Name, out.Name),
						})
						continue
					}
					seen[out.Name] = true
					// Extract the value attribute from Remain.
					content, _, d := out.Remain.PartialContent(&hcl.BodySchema{
						Attributes: []hcl.AttributeSchema{{Name: "value", Required: true}},
					})
					diags = append(diags, d...)
					if content != nil {
						if attr, ok := content.Attributes["value"]; ok {
							node.Outputs[out.Name] = attr.Expr
						}
					}
				}
			}
		}

		g.Steps[sp.Name] = node
		g.stepOrder = append(g.stepOrder, sp.Name)
	}
	return diags
}

// compileWorkflowBody compiles the inline workflow body for a type="workflow" step.
// Returns the compiled FSMGraph, the body entry state name, and any diagnostics.
func compileWorkflowBody(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	const maxLoadDepth = 4
	var diags hcl.Diagnostics

	if opts.LoadDepth >= maxLoadDepth {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: maximum workflow nesting depth (%d) exceeded", sp.Name, maxLoadDepth),
		}), nil, ""
	}

	if sp.Workflow != nil && sp.WorkflowFile != "" {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow { } block and workflow_file are mutually exclusive; remove one", sp.Name),
		}), nil, ""
	}

	if sp.Workflow == nil {
		return compileWorkflowBodyFromFile(sp, schemas, opts)
	}
	return compileWorkflowBodyInline(sp, schemas, opts)
}

// compileWorkflowBodyFromFile handles the workflow_file = "..." loading path.
func compileWorkflowBodyFromFile(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	var diags hcl.Diagnostics
	if sp.WorkflowFile == "" {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: type=\"workflow\" requires a workflow { ... } block", sp.Name),
		}), nil, ""
	}
	if opts.SubWorkflowResolver == nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow_file requires SubWorkflowResolver in CompileOpts", sp.Name),
		}), nil, ""
	}
	for _, f := range opts.LoadedFiles {
		if f == sp.WorkflowFile {
			return append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: workflow_file %q creates a load cycle", sp.Name, sp.WorkflowFile),
			}), nil, ""
		}
	}
	loaded, err := opts.SubWorkflowResolver(sp.WorkflowFile, opts.WorkflowDir)
	if err != nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: failed to load workflow_file %q: %s", sp.Name, sp.WorkflowFile, err),
		}), nil, ""
	}
	childOpts := CompileOpts{
		WorkflowDir:         opts.WorkflowDir,
		LoadDepth:           opts.LoadDepth + 1,
		LoadedFiles:         append(append([]string{}, opts.LoadedFiles...), sp.WorkflowFile),
		SubWorkflowResolver: opts.SubWorkflowResolver,
	}
	body, d := CompileWithOpts(loaded, schemas, childOpts)
	diags = append(diags, d...)
	if diags.HasErrors() {
		return diags, nil, ""
	}
	return diags, body, loaded.InitialState
}

// compileWorkflowBodyInline handles the inline workflow { ... } block path.
func compileWorkflowBodyInline(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	var diags hcl.Diagnostics
	wb := sp.Workflow
	if len(wb.Steps) == 0 {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow body must contain at least one step", sp.Name),
		}), nil, ""
	}

	entry := wb.Entry
	if entry == "" {
		entry = wb.Steps[0].Name
	}

	bodySpec := buildBodySpec(sp.Name, entry, wb)
	childOpts := CompileOpts{
		WorkflowDir:         opts.WorkflowDir,
		LoadDepth:           opts.LoadDepth + 1,
		LoadedFiles:         append([]string{}, opts.LoadedFiles...),
		SubWorkflowResolver: opts.SubWorkflowResolver,
	}
	body, d := CompileWithOpts(bodySpec, schemas, childOpts)
	diags = append(diags, d...)

	if err := validateBodyHasContinuePath(sp.Name, body); err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  err.Error(),
		})
	}

	return diags, body, entry
}

// validateBodyHasContinuePath returns an error when none of the body steps
// has a transition_to = "_continue" outcome. This ensures that the iteration
// loop can terminate normally; a body with no path to _continue is a compile
// error (B-09).
func validateBodyHasContinuePath(stepName string, body *FSMGraph) error {
	if body == nil {
		return nil
	}
	for _, stepNode := range body.Steps {
		for _, target := range stepNode.Outcomes {
			if target == "_continue" {
				return nil
			}
		}
	}
	return fmt.Errorf("step %q: workflow body has no path to \"_continue\"; at least one outcome must transition_to = \"_continue\"", stepName)
}

// buildBodySpec constructs a synthetic Spec from a WorkflowBodySpec for
// recursive compilation. It adds a "_continue" terminal state that body steps
// can transition to in order to signal iteration advance.
func buildBodySpec(stepName, entry string, wb *WorkflowBodySpec) *Spec {
	// Convert pointer slices to value slices for the synthetic Spec.
	steps := make([]StepSpec, 0, len(wb.Steps))
	for _, s := range wb.Steps {
		if s != nil {
			steps = append(steps, *s)
		}
	}
	states := make([]StateSpec, 0, len(wb.States)+1)
	for _, s := range wb.States {
		if s != nil {
			states = append(states, *s)
		}
	}
	// Add the synthetic _continue terminal state (body-complete signal).
	states = append(states, StateSpec{
		Name:     "_continue",
		Terminal: true,
	})
	waits := make([]WaitSpec, 0, len(wb.Waits))
	for _, w := range wb.Waits {
		if w != nil {
			waits = append(waits, *w)
		}
	}
	approvals := make([]ApprovalSpec, 0, len(wb.Approvals))
	for _, a := range wb.Approvals {
		if a != nil {
			approvals = append(approvals, *a)
		}
	}
	branches := make([]BranchSpec, 0, len(wb.Branches))
	for _, b := range wb.Branches {
		if b != nil {
			branches = append(branches, *b)
		}
	}
	return &Spec{
		Name:         stepName + ":body",
		Version:      "1",
		InitialState: entry,
		TargetState:  "_continue",
		Steps:        steps,
		States:       states,
		Waits:        waits,
		Approvals:    approvals,
		Branches:     branches,
	}
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

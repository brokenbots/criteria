package workflow

// compile_steps_helpers.go — shared private helpers used by all step-kind
// compilers (adapter, iteration, workflow).

import (
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// validateAdapterAndAgent validates adapter name syntax, agent reference
// existence, allow_tools/agent constraint, and lifecycle/agent constraint.
func validateAdapterAndAgent(g *FSMGraph, sp *StepSpec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if sp.Adapter != "" && !isValidAdapterName(sp.Adapter) {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid adapter %q", sp.Name, sp.Adapter)})
	}
	if sp.Agent != "" {
		if _, ok := g.Agents[sp.Agent]; !ok {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: unknown agent %q", sp.Name, sp.Agent)})
		}
	}
	if len(sp.AllowTools) > 0 && sp.Agent == "" {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: allow_tools requires agent", sp.Name)})
	}
	if sp.Lifecycle != "" {
		if !isValidLifecycle(sp.Lifecycle) {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid lifecycle %q", sp.Name, sp.Lifecycle)})
		}
		if sp.Agent == "" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: lifecycle requires agent", sp.Name)})
		}
		if sp.Input != nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: lifecycle %q must not include input", sp.Name, sp.Lifecycle)})
		}
		if len(sp.AllowTools) > 0 {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: allow_tools is only valid on execute-shape steps (not lifecycle open/close)", sp.Name)})
		}
	}
	return diags
}

// validateLegacyConfig emits a migration diagnostic when a step uses the
// deprecated config = { ... } attribute instead of input { }.
func validateLegacyConfig(sp *StepSpec) hcl.Diagnostics {
	if len(sp.Config) == 0 {
		return nil
	}
	var subject *hcl.Range
	if sp.LegacyConfigRange != nil {
		r := *sp.LegacyConfigRange
		subject = &r
	}
	return hcl.Diagnostics{&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q: \"config\" attribute removed; use \"input { }\" block instead (Phase 1.5)", sp.Name),
		Detail:   "Replace `config = { key = \"value\" }` with `input { key = \"value\" }` in your workflow.",
		Subject:  subject,
	}}
}

// resolveAdapterName returns the effective adapter name for a step: either
// sp.Adapter (direct) or the adapter backing sp.Agent (indirect).
func resolveAdapterName(g *FSMGraph, sp *StepSpec) string {
	if sp.Adapter != "" {
		return sp.Adapter
	}
	if sp.Agent != "" {
		if agent, ok := g.Agents[sp.Agent]; ok {
			return agent.Adapter
		}
	}
	return ""
}

// resolveStepOnCrash returns the effective on_crash for a step, falling back
// to the backing agent's on_crash if the step doesn't specify one.
func resolveStepOnCrash(g *FSMGraph, sp *StepSpec) (string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if sp.OnCrash != "" && !isValidOnCrash(sp.OnCrash) {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: invalid on_crash %q", sp.Name, sp.OnCrash)})
	}
	effective := sp.OnCrash
	if effective == "" {
		if sp.Agent != "" {
			if agent, ok := g.Agents[sp.Agent]; ok {
				effective = agent.OnCrash
			} else {
				effective = onCrashFail
			}
		} else {
			effective = onCrashFail
		}
	}
	return effective, diags
}

// decodeStepTimeout parses sp.Timeout and returns the duration and any
// diagnostic.
func decodeStepTimeout(sp *StepSpec) (time.Duration, hcl.Diagnostics) {
	if sp.Timeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(sp.Timeout)
	if err != nil {
		return 0, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: invalid timeout %q: %v", sp.Name, sp.Timeout, err),
		}}
	}
	return d, nil
}

// decodeRemainIter reads the for_each and count expressions from sp.Remain
// without side-effects on any prior or future PartialContent calls.
func decodeRemainIter(sp *StepSpec) (forEachExpr, countExpr hcl.Expression, diags hcl.Diagnostics) {
	if sp.Remain == nil {
		return nil, nil, nil
	}
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
	return forEachExpr, countExpr, diags
}

// decodeStepInput decodes the input { } block for sp, validates against the
// adapter schema when one is known, and returns the static map and expression
// map.
func decodeStepInput(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts, adapterName string) (inputMap map[string]string, inputExprs map[string]hcl.Expression, diags hcl.Diagnostics) {
	if sp.Input == nil {
		return nil, nil, nil
	}
	attrs, d := sp.Input.Remain.JustAttributes()
	diags = append(diags, d...)
	ctxLabel := fmt.Sprintf("step %q input", sp.Name)
	missingRange := sp.Input.Remain.MissingItemRange()
	if adapterName != "" {
		if info, ok := adapterInfo(schemas, adapterName); ok {
			inputMap, d = validateSchemaAttrs(ctxLabel, attrs, info.InputSchema, missingRange, adapterName, nil)
		} else {
			inputMap, d = decodeAttrsToStringMap(attrs, nil)
		}
	} else {
		inputMap, d = decodeAttrsToStringMap(attrs, nil)
	}
	diags = append(diags, d...)
	inputExprs = make(map[string]hcl.Expression, len(attrs))
	for k, attr := range attrs {
		inputExprs[k] = attr.Expr
	}
	if opts.WorkflowDir != "" {
		diags = append(diags, validateFileFunctionCalls(attrs, opts.WorkflowDir)...)
	}
	return inputMap, inputExprs, diags
}

// validateOnFailureValue checks that sp.OnFailure is a recognised value.
// It does not check whether on_failure is allowed on this step kind.
func validateOnFailureValue(sp *StepSpec) hcl.Diagnostics {
	if sp.OnFailure == "" {
		return nil
	}
	switch sp.OnFailure {
	case "continue", "abort", "ignore":
		return nil
	default:
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: invalid on_failure %q; must be \"continue\", \"abort\", or \"ignore\"", sp.Name, sp.OnFailure),
		}}
	}
}

// validateEachRefs emits a diagnostic for each input expression that
// references each.* when the step is not iterating.
func validateEachRefs(stepName string, inputExprs map[string]hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for k, expr := range inputExprs {
		if refsEach(expr) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q input.%s: each._idx, each.key, each.value, each._prev, each._total, each._first, and each._last are only available inside iterating steps (for_each or count)", stepName, k),
			})
		}
	}
	return diags
}

// compileOutcomeBlock populates node.Outcomes from sp.Outcomes, checking for
// duplicates and missing transition_to values.
func compileOutcomeBlock(sp *StepSpec, node *StepNode) hcl.Diagnostics {
	var diags hcl.Diagnostics
	seen := map[string]bool{}
	for _, o := range sp.Outcomes {
		if seen[o.Name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: duplicate outcome %q", sp.Name, o.Name)})
			continue
		}
		seen[o.Name] = true
		if o.TransitionTo == "" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q outcome %q: transition_to required", sp.Name, o.Name)})
			continue
		}
		node.Outcomes[o.Name] = o.TransitionTo
	}
	return diags
}

// validateIteratingOutcomes checks that iterating steps declare the required
// all_succeeded outcome and warns when any_failed is absent.
func validateIteratingOutcomes(sp *StepSpec, node *StepNode) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if _, ok := node.Outcomes["all_succeeded"]; !ok {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: iterating steps must declare outcome \"all_succeeded\"", sp.Name)})
	}
	if _, ok := node.Outcomes["any_failed"]; !ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  fmt.Sprintf("step %q: outcome \"any_failed\" not declared; failed iterations will fall through to \"all_succeeded\"", sp.Name),
		})
	}
	return diags
}

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
// workflowDir enables compile-time validation of constant file() arguments; pass
// "" to skip.
func compileSteps(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, workflowDir string) hcl.Diagnostics {
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
			// Compile-time file() validation: constant-literal file() calls
			// with a missing or path-escaping argument surface as diagnostics
			// here rather than at runtime (W07).
			if workflowDir != "" {
				diags = append(diags, validateFileFunctionCalls(attrs, workflowDir)...)
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
	return diags
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

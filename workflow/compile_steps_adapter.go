package workflow

// compile_steps_adapter.go — compile path for adapter- and agent-backed steps
// (non-iterating).

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// copilotAllowToolsAliases maps legacy user-facing allow_tools names to the
// canonical Copilot SDK permission kind. When a step using the copilot adapter
// lists one of these aliases, a compile-time warning is emitted pointing toward
// the canonical form.
//
// This map is a workflow-package copy of the alias table in
// internal/plugin/policy.go (adapterPermissionAliases["copilot"]). The two must
// stay in sync; the duplication is intentional since the workflow package cannot
// import internal/plugin due to import-boundary rules.
var copilotAllowToolsAliases = map[string]string{
	"read_file":  "read",
	"write_file": "write",
}

// compileAdapterStep compiles a non-iterating adapter- or agent-backed step
// and registers it in g.
//
//nolint:funlen // W11: function length unavoidable due to comprehensive adapter validation and resolution
func compileAdapterStep(g *FSMGraph, sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateStepKindSelectionDiags(sp)...)

	// Resolve the adapter reference from the step's Remain body as an HCL traversal.
	// This replaces the old sp.Adapter string field with proper traversal parsing.
	adapterRef, adapterPresent, d := resolveStepAdapterRef(sp.Remain)
	diags = append(diags, d...)

	// Validate that the resolved adapter reference (if present) exists in the graph.
	// This check comes after traversal resolution to provide precise diagnostics.
	if adapterPresent {
		if _, ok := g.Adapters[adapterRef]; !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: referenced adapter %q is not declared", sp.Name, adapterRef),
			})
		}
	}

	diags = append(diags, validateAdapterRefRequired(sp, adapterPresent)...)
	diags = append(diags, validateLegacyConfig(sp)...)
	diags = append(diags, validateOnFailureForNonIterating(sp)...)

	effectiveOnCrash, d := resolveStepOnCrashWithAdapter(g, sp, adapterRef)
	diags = append(diags, d...)

	timeout, d := decodeStepTimeout(sp)
	diags = append(diags, d...)

	if sp.MaxVisits < 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: max_visits must be >= 0", sp.Name)})
	}

	// Extract the adapter type from the dotted reference for validation and warnings.
	adapterType := ""
	if adapterRef != "" {
		parts := strings.Split(adapterRef, ".")
		if len(parts) == 2 {
			adapterType = parts[0]
		}
	}

	inputMap, inputExprs, d := decodeStepInput(g, sp, schemas, opts, adapterType)
	diags = append(diags, d...)

	// each.* references are only valid inside iterating steps or workflow bodies
	// (LoadDepth > 0). Non-iterating top-level steps must not reference them.
	if opts.LoadDepth == 0 {
		diags = append(diags, validateEachRefs(sp.Name, inputExprs)...)
	}

	node := newBaseStepNodeWithAdapterRef(sp, spec, adapterRef, effectiveOnCrash, timeout, inputMap, inputExprs)
	diags = append(diags, maybeCopilotAliasWarnings(sp.Name, adapterType, node.AllowTools)...)
	diags = append(diags, compileOutcomeBlock(sp, node)...)

	if len(node.Outcomes) == 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
	}

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}

// allowToolsForStep returns the effective AllowTools for a step.
func allowToolsForStep(sp *StepSpec, spec *Spec) []string {
	return unionAllowTools(sp.AllowTools, workflowAllowTools(spec))
}

// validateOnFailureForNonIterating validates on_failure for steps that do not
// carry for_each or count. It checks the value is recognised and always errors
// because on_failure requires iterating.
func validateOnFailureForNonIterating(sp *StepSpec) hcl.Diagnostics {
	diags := validateOnFailureValue(sp)
	if sp.OnFailure != "" {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: on_failure requires for_each or count", sp.Name),
		})
	}
	return diags
}

// maybeCopilotAliasWarnings emits per-tool alias warnings when adapterName is
// "copilot" and a tool in tools is a known alias of a canonical SDK kind.
func maybeCopilotAliasWarnings(stepName, adapterName string, tools []string) hcl.Diagnostics {
	if adapterName != "copilot" {
		return nil
	}
	var diags hcl.Diagnostics
	for _, tool := range tools {
		if canonical, ok := copilotAllowToolsAliases[tool]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("step %q allow_tools: %q is a recognized alias for the Copilot SDK kind %q; consider using the canonical form for clarity", stepName, tool, canonical),
			})
		}
	}
	return diags
}

// newBaseStepNodeWithAdapterRef constructs a StepNode with all fields common to both
// non-iterating and iterating adapter steps, using the resolved adapterRef instead
// of reading from sp.Adapter (which is no longer a gohcl-decoded field).
func newBaseStepNodeWithAdapterRef(sp *StepSpec, spec *Spec, adapterRef string, effectiveOnCrash string, timeout time.Duration,
	inputMap map[string]string, inputExprs map[string]hcl.Expression) *StepNode {
	return &StepNode{
		Name:       sp.Name,
		Adapter:    adapterRef, // resolved "<type>.<name>" from traversal expression
		OnCrash:    effectiveOnCrash,
		Type:       sp.Type,
		OnFailure:  sp.OnFailure,
		MaxVisits:  sp.MaxVisits,
		Input:      inputMap,
		InputExprs: inputExprs,
		Timeout:    timeout,
		Outcomes:   map[string]string{},
		AllowTools: allowToolsForStep(sp, spec),
	}
}

// validateAdapterRefRequired checks constraints that require a valid adapter reference.
// This is called after the adapter reference has been resolved from the step's
// Remain body and validated. adapterPresent indicates whether an adapter attribute
// was found in the step.
func validateAdapterRefRequired(sp *StepSpec, adapterPresent bool) hcl.Diagnostics {
	var diags hcl.Diagnostics

	if len(sp.AllowTools) > 0 && !adapterPresent {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: allow_tools requires an adapter reference", sp.Name),
		})
	}

	return diags
}

// resolveStepOnCrashWithAdapter returns the effective on_crash for a step,
// falling back to the backing adapter's on_crash if the step doesn't specify one.
// adapterRef is the resolved adapter "<type>.<name>" reference.
func resolveStepOnCrashWithAdapter(g *FSMGraph, sp *StepSpec, adapterRef string) (string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if sp.OnCrash != "" && !isValidOnCrash(sp.OnCrash) {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: invalid on_crash %q", sp.Name, sp.OnCrash),
		})
		return "", diags
	}
	if sp.OnCrash != "" {
		return sp.OnCrash, nil // step explicitly specifies on_crash
	}
	// Fall back to the adapter's on_crash (if set).
	if adapterRef != "" {
		if adapterNode, ok := g.Adapters[adapterRef]; ok {
			return adapterNode.OnCrash, nil
		}
	}
	return "", nil
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

// decodeStepInput decodes the input { } block for sp, validates against the
// adapter schema when one is known, and returns the static map and expression
// map.
func decodeStepInput(g *FSMGraph, sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts, adapterName string) (inputMap map[string]string, inputExprs map[string]hcl.Expression, diags hcl.Diagnostics) {
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
	diags = append(diags, validateFoldableAttrs(attrs, graphVars(g), graphLocals(g), opts.WorkflowDir)...)
	return inputMap, inputExprs, diags
}

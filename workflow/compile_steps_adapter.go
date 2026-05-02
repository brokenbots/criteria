package workflow

// compile_steps_adapter.go — compile path for adapter- and agent-backed steps
// (non-iterating).

import (
	"fmt"
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
func compileAdapterStep(g *FSMGraph, sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateStepKindSelectionDiags(sp)...)
	diags = append(diags, validateAdapterAndAgent(g, sp)...)
	diags = append(diags, validateLegacyConfig(sp)...)
	diags = append(diags, validateOnFailureForNonIterating(sp)...)

	effectiveOnCrash, d := resolveStepOnCrash(g, sp)
	diags = append(diags, d...)

	timeout, d := decodeStepTimeout(sp)
	diags = append(diags, d...)

	if sp.MaxVisits < 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: max_visits must be >= 0", sp.Name)})
	}

	adapterName := resolveAdapterName(g, sp)
	inputMap, inputExprs, d := decodeStepInput(sp, schemas, opts, adapterName)
	diags = append(diags, d...)

	// each.* references are only valid inside iterating steps or workflow bodies
	// (LoadDepth > 0). Non-iterating top-level steps must not reference them.
	if opts.LoadDepth == 0 {
		diags = append(diags, validateEachRefs(sp.Name, inputExprs)...)
	}

	node := newBaseStepNode(sp, spec, effectiveOnCrash, timeout, inputMap, inputExprs)
	diags = append(diags, maybeCopilotAliasWarnings(sp.Name, adapterName, node.AllowTools)...)
	diags = append(diags, compileOutcomeBlock(sp, node)...)

	if len(node.Outcomes) == 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
	}

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}

// allowToolsForStep returns the effective AllowTools for a step. Lifecycle
// steps (open/close) never receive allow_tools — permission gating is only
// meaningful on execute-shape steps.
func allowToolsForStep(sp *StepSpec, spec *Spec) []string {
	if sp.Lifecycle != "" {
		return nil
	}
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

// newBaseStepNode constructs a StepNode with all fields common to both
// non-iterating and iterating adapter/agent steps.
func newBaseStepNode(sp *StepSpec, spec *Spec, effectiveOnCrash string, timeout time.Duration,
	inputMap map[string]string, inputExprs map[string]hcl.Expression) *StepNode {
	return &StepNode{
		Name:       sp.Name,
		Adapter:    sp.Adapter,
		Agent:      sp.Agent,
		Lifecycle:  sp.Lifecycle,
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

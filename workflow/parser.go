package workflow

import (
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// ParseFile reads and decodes a single HCL file into a Spec.
func ParseFile(path string) (*Spec, hcl.Diagnostics) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "cannot read workflow file",
			Detail:   err.Error(),
		}}
	}
	return Parse(path, src)
}

// Parse decodes HCL source into a Spec. The workflow { ... } block is
// header-only in the new format; all content blocks (step, state, adapter, etc.)
// live at the top level of the file. A nil Header is valid here (for content-only
// files in a multi-file directory); callers that require a header (ParseDir,
// CompileWithOpts) perform the check themselves.
func Parse(filename string, src []byte) (*Spec, hcl.Diagnostics) {
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCL(src, filename)
	if f == nil {
		if len(diags) == 0 {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "cannot parse workflow file",
				Detail:   "parser returned nil file without diagnostics",
			})
		}
		return nil, diags
	}
	if diags.HasErrors() {
		return nil, diags
	}

	// Check for legacy attributes and blocks before attempting decode.
	if legacyDiags := checkLegacyAttributes(f.Body); legacyDiags.HasErrors() {
		return nil, legacyDiags
	}

	var spec Spec
	if decodeDiags := gohcl.DecodeBody(f.Body, nil, &spec); decodeDiags.HasErrors() {
		return nil, decodeDiags
	}
	spec.SourceBytes = src
	captureCriteriaVersionRange(&spec, f.Body)
	if annotateDiags := annotateLegacyConfigRanges(&spec, f.Body); annotateDiags.HasErrors() {
		diags = append(diags, annotateDiags...)
		return nil, diags
	}
	if toolDiags := captureStepToolRefs(&spec); toolDiags.HasErrors() {
		return nil, append(diags, toolDiags...)
	}
	if depthDiags := checkMaxToolDepthRange(&spec, f.Body); depthDiags.HasErrors() {
		return nil, append(diags, depthDiags...)
	}
	return &spec, diags
}

// captureCriteriaVersionRange records the source range of the
// criteria_version attribute inside the workflow block. gohcl decodes the value
// but does not expose positional metadata for optional attributes.
func captureCriteriaVersionRange(spec *Spec, body hcl.Body) {
	if spec == nil || spec.Header == nil || body == nil {
		return
	}
	blockSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "workflow"}},
	}
	content, _, _ := body.PartialContent(blockSchema)
	if content == nil {
		return
	}
	attrSchema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "criteria_version"}},
	}
	for _, block := range content.Blocks {
		attrs, _, _ := block.Body.PartialContent(attrSchema)
		if attrs == nil {
			continue
		}
		if attr, ok := attrs.Attributes["criteria_version"]; ok {
			rng := attr.Expr.Range()
			spec.Header.CriteriaVersionRange = &rng
			break
		}
	}
}

// captureStepToolRefs decodes each step's `tools` attribute into raw traversal
// grants on StepSpec.Tools. The attribute is captured by StepSpec.Remain (it
// has no gohcl tag because gohcl cannot decode hcl.Traversal targets
// directly), so the parse pass extracts it per step. Only bare traversal
// entries are kept; anything else is silently dropped here — shape diagnostics
// land in CRI-156, which re-walks the Remain bodies with positions (CRI-155:
// no reference resolution, no graph changes).
func captureStepToolRefs(spec *Spec) hcl.Diagnostics {
	if spec == nil {
		return nil
	}
	var diags hcl.Diagnostics
	toolsSchema := &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "tools"}},
	}
	for i := range spec.Steps {
		if spec.Steps[i].Remain == nil {
			continue
		}
		attrs, _, attrDiags := spec.Steps[i].Remain.PartialContent(toolsSchema)
		if attrDiags.HasErrors() {
			diags = append(diags, attrDiags...)
			continue
		}
		attr, ok := attrs.Attributes["tools"]
		if !ok {
			continue
		}
		spec.Steps[i].Tools = decodeToolRefTraversals(attr.Expr)
	}
	return diags
}

// decodeToolRefTraversals converts a `tools` list expression into raw
// traversals, skipping entries that are not bare traversals (their diagnostics
// are deferred to CRI-156). A non-list value yields nil.
func decodeToolRefTraversals(expr hcl.Expression) []hcl.Traversal {
	exprs, listDiags := hcl.ExprList(expr)
	if listDiags.HasErrors() {
		return nil
	}
	traversals := make([]hcl.Traversal, 0, len(exprs))
	for _, item := range exprs {
		traversal, _ := hcl.AbsTraversalForExpr(item)
		if len(traversal) == 0 {
			continue
		}
		traversals = append(traversals, traversal)
	}
	if len(traversals) == 0 {
		return nil
	}
	return traversals
}

// checkMaxToolDepthRange reports a decode diagnostic when a declared
// policy.max_tool_depth is < 1. An absent attribute is valid (0 = engine
// default of 8). CRI-155 placement decision: the >= 1 range check lands at
// parse time as a plain decode diagnostic; CRI-157 owns graph-level wiring
// only.
func checkMaxToolDepthRange(spec *Spec, body hcl.Body) hcl.Diagnostics {
	if spec == nil || spec.Header == nil || spec.Header.Policy == nil || body == nil {
		return nil
	}
	if spec.Header.Policy.MaxToolDepth >= 1 {
		return nil
	}
	rng := maxToolDepthAttrRange(body)
	if rng == nil {
		return nil
	}
	return hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  "invalid policy.max_tool_depth",
		Detail: fmt.Sprintf("max_tool_depth must be an integer >= 1 (got %d); unset uses the engine default of 8",
			spec.Header.Policy.MaxToolDepth),
		Subject: rng,
	}}
}

// maxToolDepthAttrRange locates the max_tool_depth attribute inside the
// workflow header's policy block to source a decode diagnostic. Returns nil
// when the attribute is not declared.
func maxToolDepthAttrRange(body hcl.Body) *hcl.Range {
	wfSchema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "workflow"}}}
	content, _, _ := body.PartialContent(wfSchema)
	for _, wf := range content.Blocks {
		policySchema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "policy"}}}
		policyContent, _, _ := wf.Body.PartialContent(policySchema)
		for _, pol := range policyContent.Blocks {
			attrSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "max_tool_depth"}}}
			attrs, _, _ := pol.Body.PartialContent(attrSchema)
			if attr, ok := attrs.Attributes["max_tool_depth"]; ok {
				rng := attr.Expr.Range()
				return &rng
			}
		}
	}
	return nil
}

// checkLegacyAttributes runs all legacy attribute and block rejection checks.
func checkLegacyAttributes(body hcl.Body) hcl.Diagnostics {
	checks := []func(hcl.Body) hcl.Diagnostics{
		rejectLegacyWorkflowLabel,
		rejectLegacyPolicyBlock,
		rejectLegacyBlocks,
		rejectLegacySwitchConditionBlock,
		rejectLegacySharedVariableBlock,
		rejectLegacyStepAgentAttr,
		rejectLegacyStepAdapterAttr,
		rejectLegacyStepLifecycleAttr,
		rejectLegacyStepWorkflowBlock,
		rejectLegacyStepWorkflowFile,
		rejectLegacyStepTypeAttr,
		rejectLegacySharedWrites,
		rejectLegacyOutcomeTransitionTo,
		rejectLegacyDefaultOutcome,
		rejectLegacyTypeString,
		rejectLegacyEnvironmentString,
	}

	var diags hcl.Diagnostics
	for _, check := range checks {
		diags = append(diags, check(body)...)
		if diags.HasErrors() {
			return diags
		}
	}
	return diags
}

// annotateLegacyConfigRanges records source ranges for legacy step
// `config = { ... }` attributes so compile-time diagnostics can include
// file/line context.
func annotateLegacyConfigRanges(spec *Spec, body hcl.Body) hcl.Diagnostics {
	if spec == nil || body == nil {
		return nil
	}

	// Steps are now at the top level of the file (not inside a workflow block).
	stepSchema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "step", LabelNames: []string{"name"}}}}
	content, _, diags := body.PartialContent(stepSchema)
	if diags.HasErrors() {
		return diags
	}

	// Preserve ordering by assigning ranges to matching step names in sequence.
	nameToIdx := map[string][]int{}
	for i := range spec.Steps {
		nameToIdx[spec.Steps[i].Name] = append(nameToIdx[spec.Steps[i].Name], i)
	}

	consumed := map[string]int{}
	for _, blk := range content.Blocks {
		if len(blk.Labels) != 1 {
			continue
		}
		name := blk.Labels[0]
		indices := nameToIdx[name]
		if len(indices) == 0 {
			continue
		}
		seq := consumed[name]
		if seq >= len(indices) {
			continue
		}
		idx := indices[seq]
		consumed[name] = seq + 1

		cfgOnly := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "config"}}}
		attrs, _, attrDiags := blk.Body.PartialContent(cfgOnly)
		diags = append(diags, attrDiags...)
		if attrDiags.HasErrors() {
			continue
		}
		if attr, ok := attrs.Attributes["config"]; ok {
			r := attr.NameRange
			spec.Steps[idx].LegacyConfigRange = &r
		}
	}

	return diags
}

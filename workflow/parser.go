package workflow

import (
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

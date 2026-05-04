package workflow

import (
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// File is the top-level HCL file structure: one workflow block.
type File struct {
	Workflows []Spec `hcl:"workflow,block"`
}

// ParseFile reads and decodes an HCL file into a Spec. The file must contain
// exactly one `workflow` block.
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

// Parse decodes HCL source into a Spec.
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

	var file File
	if decodeDiags := gohcl.DecodeBody(f.Body, nil, &file); decodeDiags.HasErrors() {
		return nil, decodeDiags
	}
	if len(file.Workflows) != 1 {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "expected exactly one workflow block",
			Detail:   fmt.Sprintf("got %d", len(file.Workflows)),
		}}
	}
	spec := &file.Workflows[0]
	spec.SourceBytes = src
	if annotateDiags := annotateLegacyConfigRanges(spec, f.Body); annotateDiags.HasErrors() {
		diags = append(diags, annotateDiags...)
		return nil, diags
	}
	return spec, diags
}

// checkLegacyAttributes runs all legacy attribute and block rejection checks.
func checkLegacyAttributes(body hcl.Body) hcl.Diagnostics {
	checks := []func(hcl.Body) hcl.Diagnostics{
		rejectLegacyBlocks,
		rejectLegacyStepAgentAttr,
		rejectLegacyStepLifecycleAttr,
		rejectLegacyStepWorkflowBlock,
		rejectLegacyStepWorkflowFile,
		rejectLegacyStepTypeAttr,
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
func annotateLegacyConfigRanges(spec *Spec, body hcl.Body) hcl.Diagnostics { //nolint:funlen // W03: iterates over all steps/blocks to collect legacy config attribute source ranges
	if spec == nil || body == nil {
		return nil
	}

	rootSchema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "workflow", LabelNames: []string{"name"}}}}
	root, _, diags := body.PartialContent(rootSchema)
	if diags.HasErrors() || len(root.Blocks) == 0 {
		return diags
	}

	workflowBody := root.Blocks[0].Body
	workflowSchema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "step", LabelNames: []string{"name"}}}}
	content, _, partialDiags := workflowBody.PartialContent(workflowSchema)
	diags = append(diags, partialDiags...)
	if partialDiags.HasErrors() {
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

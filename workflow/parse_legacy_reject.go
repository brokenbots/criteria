package workflow

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// rejectLegacyBlocks checks for and rejects blocks that were renamed in v0.3.0.
// Returns an error diagnostic with the source range for any legacy block found.
func rejectLegacyBlocks(body hcl.Body) hcl.Diagnostics {
	legacyBlockNames := map[string]string{
		"agent": `the "agent" block was renamed to "adapter" in v0.3.0; declare adapter "<type>" "<name>" { ... } and remove the legacy agent block. See CHANGELOG.md migration note.`,
		// [15] adds: "branch": ...
		// [16] adds: "transition_to": (attribute, not block — handled separately)
	}

	var diags hcl.Diagnostics
	schema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{}}
	for name := range legacyBlockNames {
		schema.Blocks = append(schema.Blocks, hcl.BlockHeaderSchema{Type: name, LabelNames: nil})
	}

	content, _, _ := body.PartialContent(schema)
	for _, block := range content.Blocks {
		if msg, ok := legacyBlockNames[block.Type]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("removed block %q", block.Type),
				Detail:   msg,
				Subject:  &block.DefRange,
			})
		}
	}
	return diags
}

// rejectLegacyStepAgentAttr checks for and rejects the legacy `agent = "..."` attribute on step blocks.
// This must be checked recursively in the workflow body for all step blocks.
func rejectLegacyStepAgentAttr(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Look for step blocks.
	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	content, _, _ := body.PartialContent(schema)

	for _, block := range content.Blocks {
		// Check for "agent" attribute in the step block body.
		stepSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "agent"}}}
		stepContent, _, _ := block.Body.PartialContent(stepSchema)

		if attr, ok := stepContent.Attributes["agent"]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed attribute "agent" on steps`,
				Detail:   `the "agent" attribute on steps was removed in v0.3.0. Use adapter = "<type>.<name>" to reference a declared adapter.`,
				Subject:  &attr.NameRange,
			})
		}
	}

	return diags
}

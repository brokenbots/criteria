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
// This recursively checks all step blocks, including those inside nested workflow step bodies.
func rejectLegacyStepAgentAttr(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// First find the workflow block(s)
	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)

	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyStepAgentAttrInBody(wfBlock.Body)...)
	}

	return diags
}

// rejectLegacyStepAgentAttrInBody recursively checks for agent attributes in all steps within a body.
func rejectLegacyStepAgentAttrInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Look for step blocks within this body.
	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		// Check for "agent" attribute in the step block body.
		agentSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "agent"}}}
		agentContent, _, _ := block.Body.PartialContent(agentSchema)

		if attr, ok := agentContent.Attributes["agent"]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed attribute "agent" on steps`,
				Detail:   `the "agent" attribute on steps was removed in v0.3.0. Use adapter = "<type>.<name>" to reference a declared adapter.`,
				Subject:  &attr.NameRange,
			})
		}

		// Recursively check nested workflow blocks inside this step
		nestedWfSchema := &hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "workflow", LabelNames: []string{}},
			},
		}
		nestedWfContent, _, _ := block.Body.PartialContent(nestedWfSchema)
		for _, nestedWfBlock := range nestedWfContent.Blocks {
			diags = append(diags, rejectLegacyStepAgentAttrInBody(nestedWfBlock.Body)...)
		}
	}

	return diags
}

// rejectLegacyStepLifecycleAttr checks for and rejects the legacy `lifecycle = "open"|"close"` attribute on step blocks.
// This recursively checks all step blocks, including those inside nested workflow step bodies.
func rejectLegacyStepLifecycleAttr(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// First find the workflow block(s)
	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)

	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyStepLifecycleAttrInBody(wfBlock.Body)...)
	}

	return diags
}

// rejectLegacyStepLifecycleAttrInBody recursively checks for lifecycle attributes in all steps within a body.
func rejectLegacyStepLifecycleAttrInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Look for step blocks within this body.
	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		// Check for "lifecycle" attribute in the step block body.
		lifecycleSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "lifecycle"}}}
		lifecycleContent, _, _ := block.Body.PartialContent(lifecycleSchema)

		if attr, ok := lifecycleContent.Attributes["lifecycle"]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed attribute "lifecycle" on steps`,
				Detail:   `attribute "lifecycle" was removed in v0.3.0 — adapter lifecycle is automatic. Delete this step. The engine provisions and tears down adapter sessions at workflow scope boundaries. See CHANGELOG.md migration note.`,
				Subject:  &attr.NameRange,
			})
		}

		// Recursively check nested workflow blocks inside this step
		nestedWfSchema := &hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "workflow", LabelNames: []string{}},
			},
		}
		nestedWfContent, _, _ := block.Body.PartialContent(nestedWfSchema)
		for _, nestedWfBlock := range nestedWfContent.Blocks {
			diags = append(diags, rejectLegacyStepLifecycleAttrInBody(nestedWfBlock.Body)...)
		}
	}

	return diags
}

// rejectLegacyStepWorkflowBlock checks for and rejects the removed `step { workflow { ... } }` inline body block.
func rejectLegacyStepWorkflowBlock(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// First find the workflow block(s)
	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)

	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyStepWorkflowBlockInBody(wfBlock.Body)...)
	}

	return diags
}

// rejectLegacyStepWorkflowBlockInBody recursively checks for inline workflow blocks in all steps.
func rejectLegacyStepWorkflowBlockInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		workflowSchema := &hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "workflow", LabelNames: []string{}},
			},
		}
		workflowContent, _, _ := block.Body.PartialContent(workflowSchema)

		for _, wfBlock := range workflowContent.Blocks {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed block "workflow" on steps`,
				Detail:   `inline "workflow { ... }" blocks on steps were removed in v0.3.0. Declare a top-level "subworkflow" block and reference it via target in W14. See CHANGELOG.md migration note.`,
				Subject:  &wfBlock.DefRange,
			})
		}

		// Recursively check nested workflow steps (for iteration bodies with inline workflows)
		diags = append(diags, rejectLegacyStepWorkflowBlockInBody(block.Body)...)
	}

	return diags
}

// rejectLegacyStepWorkflowFile checks for and rejects the removed `step { workflow_file = "..." }` attribute.
func rejectLegacyStepWorkflowFile(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// First find the workflow block(s)
	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)

	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyStepWorkflowFileInBody(wfBlock.Body)...)
	}

	return diags
}

// rejectLegacyStepWorkflowFileInBody recursively checks for workflow_file attributes in all steps.
func rejectLegacyStepWorkflowFileInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		workflowFileSchema := &hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "workflow_file"}},
		}
		workflowFileContent, _, _ := block.Body.PartialContent(workflowFileSchema)

		if attr, ok := workflowFileContent.Attributes["workflow_file"]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed attribute "workflow_file" on steps`,
				Detail:   `attribute "workflow_file" was removed in v0.3.0. Declare a top-level "subworkflow" block and reference it via target in W14. See CHANGELOG.md migration note.`,
				Subject:  &attr.NameRange,
			})
		}

		// Recursively check nested workflow steps
		diags = append(diags, rejectLegacyStepWorkflowFileInBody(block.Body)...)
	}

	return diags
}

// rejectLegacyStepInputBlock checks for and rejects the removed `step { input { ... } }` block (W08 stopgap).
func rejectLegacyStepInputBlock(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// First find the workflow block(s)
	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)

	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyStepInputBlockInBody(wfBlock.Body)...)
	}

	return diags
}

// rejectLegacyStepInputBlockInBody recursively checks for input blocks on all steps.
func rejectLegacyStepInputBlockInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		inputSchema := &hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "input", LabelNames: []string{}},
			},
		}
		inputContent, _, _ := block.Body.PartialContent(inputSchema)

		for _, inputBlock := range inputContent.Blocks {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed block "input" on steps`,
				Detail:   `step "input { ... }" blocks were removed in v0.3.0. Declare a top-level "subworkflow" block with input bindings and reference it via target in W14. See CHANGELOG.md migration note.`,
				Subject:  &inputBlock.DefRange,
			})
		}

		// Recursively check nested workflow steps
		diags = append(diags, rejectLegacyStepInputBlockInBody(block.Body)...)
	}

	return diags
}

// rejectLegacyStepTypeAttr checks for and rejects the removed `step { type = "..." }` attribute.
func rejectLegacyStepTypeAttr(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// First find the workflow block(s)
	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)

	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyStepTypeAttrInBody(wfBlock.Body)...)
	}

	return diags
}

// rejectLegacyStepTypeAttrInBody recursively checks for type attributes in all steps within a body.
func rejectLegacyStepTypeAttrInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Look for step blocks within this body.
	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		// Check for "type" attribute in the step block body.
		typeSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "type"}}}
		typeContent, _, _ := block.Body.PartialContent(typeSchema)

		if attr, ok := typeContent.Attributes["type"]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed attribute "type" on steps`,
				Detail:   `attribute "type" was removed in v0.3.0. All steps are now adapter steps. Use adapter = "<type>.<name>" to declare which adapter to run. Inline workflow bodies are replaced by top-level "subworkflow" blocks referenced via target in W14. See CHANGELOG.md migration note.`,
				Subject:  &attr.NameRange,
			})
		}
	}

	return diags
}

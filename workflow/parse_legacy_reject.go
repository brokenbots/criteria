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
		"branch": `block "branch" was renamed to "switch" in v0.3.0. The arm shape changed ` +
			`from arm { when, transition_to } to condition { match, next, output }. ` +
			`The default block uses next instead of transition_to. See CHANGELOG.md migration note.`,
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
				Detail:   `the "agent" attribute on steps was removed in v0.3.0. Use target = adapter.<type>.<name> to reference a declared adapter. See CHANGELOG.md migration note.`,
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

// rejectLegacyStepAdapterAttr checks for and rejects the old `adapter = adapter.<type>.<name>` attribute
// on step blocks, which was replaced by `target = adapter.<type>.<name>` in W14.
func rejectLegacyStepAdapterAttr(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)

	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyStepAdapterAttrInBody(wfBlock.Body)...)
	}

	return diags
}

// rejectLegacyStepAdapterAttrInBody checks all step blocks in body for the old adapter attribute.
func rejectLegacyStepAdapterAttrInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		adapterSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "adapter"}}}
		adapterContent, _, _ := block.Body.PartialContent(adapterSchema)

		if attr, ok := adapterContent.Attributes["adapter"]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  `removed attribute "adapter" on steps`,
				Detail:   `the "adapter" attribute on steps was replaced by "target" in v0.3.0. Use target = adapter.<type>.<name> instead. See CHANGELOG.md migration note.`,
				Subject:  &attr.NameRange,
			})
		}
	}

	return diags
}

// rejectLegacyOutcomeTransitionTo checks for and rejects the old transition_to
// attribute inside outcome blocks. In v0.3.0, transition_to was renamed to next.
func rejectLegacyOutcomeTransitionTo(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	wfSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
			{Type: "subworkflow", LabelNames: []string{"name"}},
		},
	}
	wfContent, _, _ := body.PartialContent(wfSchema)
	for _, wfBlock := range wfContent.Blocks {
		diags = append(diags, rejectLegacyOutcomeTransitionToInBody(wfBlock.Body)...)
	}
	return diags
}

// rejectLegacyOutcomeTransitionToInBody walks step and wait/approval blocks to
// find any outcome blocks with a transition_to attribute.
func rejectLegacyOutcomeTransitionToInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	stepSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
			{Type: "wait", LabelNames: []string{"name"}},
			{Type: "approval", LabelNames: []string{"name"}},
		},
	}
	stepContent, _, _ := body.PartialContent(stepSchema)

	for _, block := range stepContent.Blocks {
		outcomeSchema := &hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "outcome", LabelNames: []string{"name"}},
			},
		}
		outcomeContent, _, _ := block.Body.PartialContent(outcomeSchema)

		for _, outcomeBlock := range outcomeContent.Blocks {
			attrSchema := &hcl.BodySchema{
				Attributes: []hcl.AttributeSchema{{Name: "transition_to"}},
			}
			attrContent, _, _ := outcomeBlock.Body.PartialContent(attrSchema)

			if attr, ok := attrContent.Attributes["transition_to"]; ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  `removed attribute "transition_to" on outcome blocks`,
					Detail:   `attribute "transition_to" was renamed to "next" in v0.3.0. For outcomes that bubble the result to the caller, use next = "return". See CHANGELOG.md migration note.`,
					Subject:  &attr.NameRange,
				})
			}
		}
	}

	return diags
}
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
				Detail:   `attribute "type" was removed in v0.3.0. All steps are now adapter steps. Use target = adapter.<type>.<name> to declare which adapter to run. Inline workflow bodies are replaced by top-level "subworkflow" blocks referenced via target. See CHANGELOG.md migration note.`,
				Subject:  &attr.NameRange,
			})
		}
	}

	return diags
}

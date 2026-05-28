package workflow

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// rejectLegacyBlocks checks for and rejects blocks that were renamed in v0.3.0.
// Returns an error diagnostic with the source range for any legacy block found.
func rejectLegacyBlocks(body hcl.Body) hcl.Diagnostics {
	legacyBlockNames := map[string]string{
		"agent": `the "agent" block was renamed to "adapter" in v0.3.0; declare adapter "<type>" "<name>" { ... } and remove the legacy agent block. See CHANGELOG.md migration note.`,
		"branch": `block "branch" was renamed to "switch" in v0.3.0. The arm shape changed ` +
			`from arm { when, transition_to } to condition { match, next, output }. ` +
			`The default block uses next instead of transition_to. See CHANGELOG.md migration note.`,
		"shared_variable": `the "shared_variable" block was replaced by "data" blocks in WS02. Use data "internal" "<name>" { type = ... value = ... } and reference values as data.internal.<name>.value.`,
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
// Steps are now at the top level of the file (not inside a workflow block).
func rejectLegacyStepAgentAttr(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyStepAgentAttrInBody(body)
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
// Steps are now at the top level of the file (not inside a workflow block).
func rejectLegacyStepLifecycleAttr(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyStepLifecycleAttrInBody(body)
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
// Steps are now at the top level of the file (not inside a workflow block).
func rejectLegacyStepWorkflowBlock(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyStepWorkflowBlockInBody(body)
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
// Steps are now at the top level of the file (not inside a workflow block).
func rejectLegacyStepWorkflowFile(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyStepWorkflowFileInBody(body)
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
// Steps are now at the top level of the file (not inside a workflow block).
func rejectLegacyStepAdapterAttr(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyStepAdapterAttrInBody(body)
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
// Steps/waits/approvals are now at the top level of the file (not inside a workflow block).
func rejectLegacyOutcomeTransitionTo(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyOutcomeTransitionToInBody(body)
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
					Detail:   `attribute "transition_to" was renamed to "next" in v0.3.0. For outcomes that bubble the result to the caller, use next = step.return. See CHANGELOG.md migration note.`,
					Subject:  &attr.NameRange,
				})
			}
		}
	}

	return diags
}

// rejectLegacyStepTypeAttr checks for and rejects the old `type` attribute on step blocks.
// Steps are now at the top level of the file (not inside a workflow block).
func rejectLegacyStepTypeAttr(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyStepTypeAttrInBody(body)
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

// rejectLegacySharedWrites checks for and rejects the old `shared_writes`
// attribute inside outcome blocks. It was replaced by `write` blocks in WS02.
func rejectLegacySharedWrites(body hcl.Body) hcl.Diagnostics {
	return rejectLegacySharedWritesInBody(body)
}

// rejectLegacySharedWritesInBody recursively checks for shared_writes attributes
// in all outcome blocks within step/wait/approval blocks.
func rejectLegacySharedWritesInBody(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics

	containerSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "step", LabelNames: []string{"name"}},
			{Type: "wait", LabelNames: []string{"name"}},
			{Type: "approval", LabelNames: []string{"name"}},
		},
	}
	containerContent, _, _ := body.PartialContent(containerSchema)

	for _, block := range containerContent.Blocks {
		outcomeSchema := &hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: "outcome", LabelNames: []string{"name"}},
			},
		}
		outcomeContent, _, _ := block.Body.PartialContent(outcomeSchema)

		for _, outcomeBlock := range outcomeContent.Blocks {
			attrSchema := &hcl.BodySchema{
				Attributes: []hcl.AttributeSchema{{Name: "shared_writes"}},
			}
			attrContent, _, _ := outcomeBlock.Body.PartialContent(attrSchema)

			if attr, ok := attrContent.Attributes["shared_writes"]; ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  `removed attribute "shared_writes" on outcome blocks`,
					Detail:   `shared_writes has been replaced by per-target write blocks: write { target = data.internal.<name>.value, value = output.<key> }. See CHANGELOG.md migration note.`,
					Subject:  &attr.NameRange,
				})
			}
		}
	}

	return diags
}

// rejectLegacySharedVariableBlock checks for and rejects the legacy
// `shared_variable "name" { ... }` block, replaced by `data "internal" "name"` in WS02.
func rejectLegacySharedVariableBlock(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics
	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "shared_variable", LabelNames: []string{"name"}},
		},
	}
	content, _, _ := body.PartialContent(schema)
	for _, block := range content.Blocks {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `removed block "shared_variable"`,
			Detail:   `the "shared_variable" block was replaced by "data" blocks in WS02. Use data "internal" "<name>" { type = ... value = ... } and reference values as data.internal.<name>.value.`,
			Subject:  &block.DefRange,
		})
	}
	return diags
}

// rejectLegacyAttrInBlocks is a helper that searches for a single legacy attribute
func rejectLegacyAttrInBlocks(body hcl.Body, blockType string, blockLabels []string, attrName, summary, detail string) hcl.Diagnostics {
	var diags hcl.Diagnostics
	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: blockType, LabelNames: blockLabels},
		},
	}
	content, _, _ := body.PartialContent(schema)
	for _, block := range content.Blocks {
		attrSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: attrName}}}
		attrContent, _, _ := block.Body.PartialContent(attrSchema)
		if attr, ok := attrContent.Attributes[attrName]; ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  summary,
				Detail:   detail,
				Subject:  &attr.NameRange,
			})
		}
	}
	return diags
}

// rejectLegacyWorkflowLabel checks for and rejects the legacy labelled
// workflow "name" { ... } header block. The new syntax is workflow { name = "..." ... }.
func rejectLegacyWorkflowLabel(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics
	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "workflow", LabelNames: []string{"name"}},
		},
	}
	content, _, _ := body.PartialContent(schema)
	for _, block := range content.Blocks {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `removed labelled workflow block`,
			Detail:   `the workflow block no longer takes a label. Move the name into the block body: workflow { name = "..." version = "..." initial_state = "..." target_state = "..." }. See CHANGELOG.md migration note.`,
			Subject:  &block.DefRange,
		})
	}
	return diags
}

// rejectLegacyPolicyBlock checks for and rejects a top-level policy { ... } block.
// Policy is now nested inside the workflow header block: workflow { policy { ... } }.
func rejectLegacyPolicyBlock(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics
	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "policy", LabelNames: nil},
		},
	}
	content, _, _ := body.PartialContent(schema)
	for _, block := range content.Blocks {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `removed top-level policy block`,
			Detail:   `the policy block must now be nested inside the workflow header block: workflow { policy { ... } }. See CHANGELOG.md migration note.`,
			Subject:  &block.DefRange,
		})
	}
	return diags
}

// rejectLegacyDefaultOutcome checks for and rejects the legacy default_outcome
// attribute on step blocks. Use outcome "default" { ... } instead.
func rejectLegacyDefaultOutcome(body hcl.Body) hcl.Diagnostics {
	return rejectLegacyAttrInBlocks(body, "step", []string{"name"}, "default_outcome",
		`removed attribute "default_outcome" on steps`,
		`the "default_outcome" attribute was replaced by an outcome "default" { ... } block in v0.3.0. Declare outcome "default" { next = step.... } inside the step block. See CHANGELOG.md migration note.`)
}

// rejectLegacyTypeString checks for and rejects string-literal type attributes on
// variable, shared_variable, and output blocks. The new syntax uses type
// expressions: type = string, type = list(string), etc.
func rejectLegacyTypeString(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics
	blockTypes := []struct {
		typ        string
		labelNames []string
	}{
		{"variable", []string{"name"}},
		{"shared_variable", []string{"name"}},
		{"data", []string{"kind", "name"}},
		{"output", []string{"name"}},
	}
	for _, bt := range blockTypes {
		schema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: bt.typ, LabelNames: bt.labelNames}}}
		content, _, _ := body.PartialContent(schema)
		for _, block := range content.Blocks {
			attrSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "type"}}}
			attrContent, _, _ := block.Body.PartialContent(attrSchema)
			if attr, ok := attrContent.Attributes["type"]; ok {
				if isStringLiteralExpr(attr.Expr) {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("removed quoted-string type on %s block", bt.typ),
						Detail:   fmt.Sprintf("the \"type\" attribute on %s blocks now uses a type expression, not a quoted string. Remove the quotes: type = string, type = number, type = bool, type = list(string), type = map(string). See CHANGELOG.md migration note.", bt.typ),
						Subject:  &attr.NameRange,
					})
				}
			}
		}
	}
	return diags
}

// rejectLegacyEnvironmentString checks for and rejects quoted-string
// environment attributes on workflow, step, adapter, and subworkflow blocks.
// The new syntax uses a bare traversal: environment = shell.default.
func rejectLegacyEnvironmentString(body hcl.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics
	blockTypes := []struct {
		typ        string
		labelNames []string
	}{
		{"workflow", nil},
		{"step", []string{"name"}},
		{"adapter", []string{"type", "name"}},
		{"subworkflow", []string{"name"}},
	}
	for _, bt := range blockTypes {
		schema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: bt.typ, LabelNames: bt.labelNames}}}
		content, _, _ := body.PartialContent(schema)
		for _, block := range content.Blocks {
			attrSchema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "environment"}}}
			attrContent, _, _ := block.Body.PartialContent(attrSchema)
			if attr, ok := attrContent.Attributes["environment"]; ok {
				if isStringLiteralExpr(attr.Expr) {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  fmt.Sprintf("removed quoted-string environment on %s block", bt.typ),
						Detail:   fmt.Sprintf("the \"environment\" attribute on %s blocks now uses a bare traversal reference, not a quoted string. Remove the quotes: environment = shell.default. See CHANGELOG.md migration note.", bt.typ),
						Subject:  &attr.NameRange,
					})
				}
			}
		}
	}
	return diags
}

// isStringLiteralExpr reports whether expr is a literal string expression.
// HCL v2 parses "string" as *hclsyntax.TemplateExpr containing a single
// *hclsyntax.LiteralValueExpr part, so both shapes must be checked.
func isStringLiteralExpr(expr hcl.Expression) bool {
	if lit, ok := expr.(*hclsyntax.LiteralValueExpr); ok {
		return lit.Val.Type() == cty.String
	}
	if tmpl, ok := expr.(*hclsyntax.TemplateExpr); ok {
		if len(tmpl.Parts) == 1 {
			if lit, ok := tmpl.Parts[0].(*hclsyntax.LiteralValueExpr); ok {
				return lit.Val.Type() == cty.String
			}
		}
	}
	return false
}

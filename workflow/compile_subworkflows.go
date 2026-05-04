package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// compileSubworkflows resolves each subworkflow.source via opts.SubWorkflowResolver,
// parses and compiles the callee Spec into a child FSMGraph, validates input bindings,
// and stores the result in g.Subworkflows. Cycle detection is enforced via opts.SubworkflowChain.
func compileSubworkflows(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	if len(spec.Subworkflows) == 0 {
		return diags
	}

	if opts.SubWorkflowResolver == nil {
		for _, sw := range spec.Subworkflows {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q requires SubWorkflowResolver in CompileOpts", sw.Name),
			})
		}
		return diags
	}

	// Track which subworkflow names we've seen (for duplicate detection)
	seenNames := make(map[string]bool)

	for _, swSpec := range spec.Subworkflows {
		// Check for duplicate names
		if seenNames[swSpec.Name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate subworkflow name %q", swSpec.Name),
			})
			continue
		}
		seenNames[swSpec.Name] = true

		// Resolve the source using context.Background() (context propagation not available
		// through CompileOpts to avoid increasing parameter size)
		ctx := context.Background()
		resolvedDir, err := opts.SubWorkflowResolver.ResolveSource(ctx, opts.WorkflowDir, swSpec.Source)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("failed to resolve subworkflow %q source: %v", swSpec.Name, err),
			})
			continue
		}

		// Check for cycles before recursing.
		cycleDetected := false
		for _, chainPath := range opts.SubworkflowChain {
			if chainPath == resolvedDir {
				cycle := make([]string, len(opts.SubworkflowChain)+1)
				copy(cycle, opts.SubworkflowChain)
				cycle[len(cycle)-1] = resolvedDir
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("subworkflow cycle detected: %s", strings.Join(cycle, " -> ")),
				})
				cycleDetected = true
				break
			}
		}
		if cycleDetected {
			continue
		}

		// Read and parse all .hcl files in the directory
		calleeSpec, readDiags := readAndParseSubworkflowDir(resolvedDir)
		diags = append(diags, readDiags...)
		if calleeSpec == nil {
			continue
		}

		// Create new compile options for the recursive call with updated chain
		childOpts := opts
		newChain := make([]string, len(opts.SubworkflowChain), len(opts.SubworkflowChain)+1)
		copy(newChain, opts.SubworkflowChain)
		newChain = append(newChain, resolvedDir)
		childOpts.SubworkflowChain = newChain
		childOpts.LoadDepth = opts.LoadDepth + 1

		// Recursively compile the callee
		calleeGraph, compileDiags := CompileWithOpts(calleeSpec, nil, childOpts) // nil schemas: use permissive mode
		diags = append(diags, compileDiags...)
		if compileDiags.HasErrors() {
			continue
		}

		// Extract declared variable types from the compiled callee
		declaredVars := make(map[string]*VariableNode)
		for varName, varNode := range calleeGraph.Variables {
			declaredVars[varName] = varNode
		}

		// Extract input expressions from the Remain body and validate against callee's declared variables.
		inputs, inputDiags := extractSubworkflowInputs(swSpec, declaredVars)
		diags = append(diags, inputDiags...)

		// Store the SubworkflowNode
		g.Subworkflows[swSpec.Name] = &SubworkflowNode{
			Name:         swSpec.Name,
			SourcePath:   resolvedDir,
			Body:         calleeGraph,
			BodyEntry:    calleeGraph.InitialState,
			Environment:  swSpec.Environment,
			Inputs:       inputs,
			DeclaredVars: declaredVars,
		}
		g.SubworkflowOrder = append(g.SubworkflowOrder, swSpec.Name)
	}

	return diags
}

// readAndParseSubworkflowDir reads all .hcl files in a directory, merges them into a single Spec.
func readAndParseSubworkflowDir(dir string) (*Spec, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	// List all .hcl files in the directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("failed to list directory %q: %v", dir, err),
		})
		return nil, diags
	}

	var specs []*Spec
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".hcl") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		src, err := os.ReadFile(filePath)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("failed to read %q: %v", filePath, err),
			})
			continue
		}

		spec, parseDiags := Parse(filePath, src)
		diags = append(diags, parseDiags...)
		if spec != nil {
			specs = append(specs, spec)
		}
	}

	if len(specs) == 0 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("no .hcl files found in subworkflow directory %q", dir),
		})
		return nil, diags
	}

	// Merge all specs
	mergedSpec := mergeSubworkflowSpecs(specs...)
	return mergedSpec, diags
}

// mergeSubworkflowSpecs merges multiple Specs into one.
func mergeSubworkflowSpecs(specs ...*Spec) *Spec {
	if len(specs) == 0 {
		return nil
	}
	if len(specs) == 1 {
		return specs[0]
	}

	// Use the first spec as the base
	merged := specs[0]

	// Merge remaining specs
	for _, spec := range specs[1:] {
		merged.Variables = append(merged.Variables, spec.Variables...)
		merged.Locals = append(merged.Locals, spec.Locals...)
		merged.Outputs = append(merged.Outputs, spec.Outputs...)
		merged.Adapters = append(merged.Adapters, spec.Adapters...)
		merged.States = append(merged.States, spec.States...)
		merged.Steps = append(merged.Steps, spec.Steps...)
		merged.Waits = append(merged.Waits, spec.Waits...)
		merged.Approvals = append(merged.Approvals, spec.Approvals...)
		merged.Branches = append(merged.Branches, spec.Branches...)
		merged.Environments = append(merged.Environments, spec.Environments...)
	}

	return merged
}

// extractSubworkflowInputs decodes the "input = { ... }" attribute from the
// subworkflow block's Remain body, and validates the input keys against the
// callee's declared variables: every required variable must be bound, and
// no extra keys are allowed.
//
// The returned map contains the parent-scope hcl.Expression for each input
// key, for later runtime evaluation against the parent's eval context.
func extractSubworkflowInputs(swSpec SubworkflowSpec, declaredVars map[string]*VariableNode) (map[string]hcl.Expression, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	inputs := make(map[string]hcl.Expression)

	if swSpec.Remain == nil {
		// No Remain body — no input provided; check for required callee vars.
		diags = append(diags, checkMissingInputKeys(swSpec.Name, inputs, declaredVars)...)
		return inputs, diags
	}

	attrs, d := swSpec.Remain.JustAttributes()
	diags = append(diags, d...)

	inputAttr, hasInput := attrs["input"]
	if !hasInput {
		// No input attribute — validate that callee has no required vars.
		diags = append(diags, checkMissingInputKeys(swSpec.Name, inputs, declaredVars)...)
		return inputs, diags
	}

	// Reject unknown attributes in the subworkflow block remain body.
	for k, attr := range attrs {
		if k != "input" {
			r := attr.NameRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q: unknown attribute %q; only \"source\", \"environment\", and \"input\" are allowed", swSpec.Name, k),
				Subject:  &r,
			})
		}
	}

	// The input attribute must be an object constructor expression:
	// input = { key1 = expr1, key2 = expr2 }
	oc, ok := inputAttr.Expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		r := inputAttr.Expr.StartRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("subworkflow %q: \"input\" must be an object literal ({ key = value, ... })", swSpec.Name),
			Subject:  &r,
		})
		return inputs, diags
	}

	// Extract key-value pairs from the object expression.
	for _, item := range oc.Items {
		keyVal, keyDiags := item.KeyExpr.Value(nil)
		if keyDiags.HasErrors() || keyVal.Type() != cty.String {
			r := item.KeyExpr.StartRange()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q: input key must be a literal string identifier", swSpec.Name),
				Subject:  &r,
			})
			continue
		}
		key := keyVal.AsString()

		// Check for duplicate input keys.
		if _, exists := inputs[key]; exists {
			r := item.KeyExpr.StartRange()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q: duplicate input key %q", swSpec.Name, key),
				Subject:  &r,
			})
			continue
		}

		// Check that the key is a declared variable in the callee.
		if _, declared := declaredVars[key]; !declared {
			r := item.KeyExpr.StartRange()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q: input key %q is not declared as a variable in the callee workflow", swSpec.Name, key),
				Subject:  &r,
			})
			continue
		}

		inputs[key] = item.ValueExpr
	}

	// Check that all required callee variables are bound.
	diags = append(diags, checkMissingInputKeys(swSpec.Name, inputs, declaredVars)...)

	return inputs, diags
}

// checkMissingInputKeys reports an error for each required callee variable (no default)
// that is absent from the provided input map.
func checkMissingInputKeys(swName string, inputs map[string]hcl.Expression, declaredVars map[string]*VariableNode) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for varName, varNode := range declaredVars {
		if !varNode.IsRequired() {
			continue // has default — not required
		}
		if _, bound := inputs[varName]; !bound {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q: required variable %q has no input binding; add it to the input = { ... } block", swName, varName),
			})
		}
	}
	return diags
}

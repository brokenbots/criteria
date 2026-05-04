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
	"github.com/zclconf/go-cty/cty/convert"
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
			if chainPath != resolvedDir {
				continue
			}
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
		childOpts.WorkflowDir = resolvedDir

		// Recursively compile the callee, propagating schemas for adapter validation.
		calleeGraph, compileDiags := CompileWithOpts(calleeSpec, opts.Schemas, childOpts)
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
		merged.Subworkflows = append(merged.Subworkflows, spec.Subworkflows...)
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
		diags = append(diags, checkMissingInputKeys(swSpec.Name, inputs, declaredVars)...)
		return inputs, diags
	}

	attrs, d := swSpec.Remain.JustAttributes()
	diags = append(diags, d...)

	inputAttr, hasInput := attrs["input"]
	if !hasInput {
		diags = append(diags, checkMissingInputKeys(swSpec.Name, inputs, declaredVars)...)
		return inputs, diags
	}

	diags = append(diags, checkUnknownSubworkflowAttrs(swSpec.Name, attrs)...)

	oc, ocDiags := parseInputObjectExpr(swSpec.Name, inputAttr)
	diags = append(diags, ocDiags...)
	if oc == nil {
		return inputs, diags
	}

	for _, item := range oc.Items {
		key, keyDiag := extractInputItemKey(swSpec.Name, item)
		if keyDiag != nil {
			diags = append(diags, keyDiag)
			continue
		}
		if _, exists := inputs[key]; exists {
			r := item.KeyExpr.StartRange()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q: duplicate input key %q", swSpec.Name, key),
				Subject:  &r,
			})
			continue
		}
		if d := validateInputItem(swSpec.Name, key, item.ValueExpr, declaredVars); len(d) > 0 {
			diags = append(diags, d...)
			continue
		}
		inputs[key] = item.ValueExpr
	}

	diags = append(diags, checkMissingInputKeys(swSpec.Name, inputs, declaredVars)...)
	return inputs, diags
}

// checkUnknownSubworkflowAttrs reports errors for attributes other than "input".
func checkUnknownSubworkflowAttrs(swName string, attrs hcl.Attributes) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for k, attr := range attrs {
		if k == "input" {
			continue
		}
		r := attr.NameRange
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("subworkflow %q: unknown attribute %q; only \"source\", \"environment\", and \"input\" are allowed", swName, k),
			Subject:  &r,
		})
	}
	return diags
}

// parseInputObjectExpr asserts that the "input" attribute is an object literal
// and returns its Items for further processing. Returns nil and a diagnostic on failure.
func parseInputObjectExpr(swName string, inputAttr *hcl.Attribute) (*hclsyntax.ObjectConsExpr, hcl.Diagnostics) {
	oc, ok := inputAttr.Expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		r := inputAttr.Expr.StartRange()
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("subworkflow %q: \"input\" must be an object literal ({ key = value, ... })", swName),
			Subject:  &r,
		}}
	}
	return oc, nil
}

// extractInputItemKey evaluates the key expression of an input object item and
// returns the string key, or a diagnostic if the key is not a literal string.
func extractInputItemKey(swName string, item hclsyntax.ObjectConsItem) (string, *hcl.Diagnostic) {
	keyVal, keyDiags := item.KeyExpr.Value(nil)
	if keyDiags.HasErrors() || keyVal.Type() != cty.String {
		r := item.KeyExpr.StartRange()
		return "", &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("subworkflow %q: input key must be a literal string identifier", swName),
			Subject:  &r,
		}
	}
	return keyVal.AsString(), nil
}

// validateInputItem checks that the input key is declared in the callee and that
// the literal value (if evaluable at compile time) is type-compatible.
func validateInputItem(swName, key string, expr hcl.Expression, declaredVars map[string]*VariableNode) hcl.Diagnostics {
	var diags hcl.Diagnostics
	varNode, declared := declaredVars[key]
	if !declared {
		r := expr.StartRange()
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("subworkflow %q: input key %q is not declared as a variable in the callee workflow", swName, key),
			Subject:  &r,
		})
		return diags
	}
	// Type-check: try to evaluate the expression with an empty context.
	// This catches literal type mismatches (e.g. "not-a-number" for a number
	// variable) at compile time. Expressions that reference runtime variables
	// (var.*, steps.*, etc.) cannot be evaluated here and are skipped.
	if varNode.Type != cty.NilType {
		if val, evalDiags := expr.Value(nil); !evalDiags.HasErrors() && val.IsKnown() {
			if err := checkInputTypeCompat(val, varNode.Type); err != nil {
				r := expr.StartRange()
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("subworkflow %q: input %q type mismatch: %v", swName, key, err),
					Subject:  &r,
				})
			}
		}
	}
	return diags
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

// checkInputTypeCompat returns an error when a compile-time-known input value
// is incompatible with the declared callee variable type. It accepts reasonable
// conversions (number string "3" → number) and rejects clearly incompatible ones
// (string "abc" → number). Expressions that cannot be evaluated at compile time
// are not checked here.
func checkInputTypeCompat(val cty.Value, wantType cty.Type) error {
	if wantType == cty.NilType || val == cty.NilVal || !val.IsKnown() || val.IsNull() {
		return nil
	}
	gotType := val.Type()
	if gotType == wantType {
		return nil // exact match
	}
	// Allow safe conversions between string/number/bool via cty convert.
	if _, err := convert.Convert(val, wantType); err != nil {
		return fmt.Errorf("expected %s, got %s", wantType.FriendlyName(), gotType.FriendlyName())
	}
	return nil
}

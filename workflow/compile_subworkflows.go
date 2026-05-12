package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// compileSubworkflows resolves each subworkflow.source via opts.SubWorkflowResolver,
// parses and compiles the callee Spec into a child FSMGraph, validates input bindings,
// and stores the result in g.Subworkflows. Cycle detection is enforced via opts.SubworkflowChain.
func compileSubworkflows(ctx context.Context, g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	if len(spec.Subworkflows) == 0 {
		return nil
	}
	if opts.SubWorkflowResolver == nil {
		return missingResolverDiags(spec.Subworkflows)
	}
	seenNames := make(map[string]bool)
	var diags hcl.Diagnostics
	for _, swSpec := range spec.Subworkflows {
		diags = append(diags, compileSingleSubworkflow(ctx, g, swSpec, opts, seenNames)...)
	}
	return diags
}

// missingResolverDiags returns an error diagnostic for each subworkflow when no
// SubWorkflowResolver is configured in CompileOpts.
func missingResolverDiags(subworkflows []SubworkflowSpec) hcl.Diagnostics {
	diags := make(hcl.Diagnostics, 0, len(subworkflows))
	for _, sw := range subworkflows {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("subworkflow %q requires SubWorkflowResolver in CompileOpts", sw.Name),
		})
	}
	return diags
}

// compileSingleSubworkflow resolves, parses, and compiles one subworkflow entry.
// seenNames tracks duplicate names within the same parent call.
func compileSingleSubworkflow(ctx context.Context, g *FSMGraph, swSpec SubworkflowSpec, opts CompileOpts, seenNames map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if seenNames[swSpec.Name] {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("duplicate subworkflow name %q", swSpec.Name),
		}}
	}
	seenNames[swSpec.Name] = true

	resolvedDir, err := opts.SubWorkflowResolver.ResolveSource(ctx, opts.WorkflowDir, swSpec.Source)
	if err != nil {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("failed to resolve subworkflow %q source: %v", swSpec.Name, err),
		}}
	}

	if cycleDiag := detectSubworkflowCycle(resolvedDir, opts.SubworkflowChain); cycleDiag != nil {
		return hcl.Diagnostics{cycleDiag}
	}

	calleeSpec, readDiags := ParseDir(resolvedDir)
	diags = append(diags, readDiags...)
	if calleeSpec == nil {
		return diags
	}

	calleeGraph, compileDiags := CompileWithContext(ctx, calleeSpec, opts.Schemas, buildChildOpts(opts, resolvedDir))
	diags = append(diags, compileDiags...)
	if compileDiags.HasErrors() {
		return diags
	}

	inputs, inputDiags := extractSubworkflowInputs(swSpec, calleeGraph.Variables)
	diags = append(diags, inputDiags...)
	g.Subworkflows[swSpec.Name] = &SubworkflowNode{
		Name:         swSpec.Name,
		SourcePath:   resolvedDir,
		Body:         calleeGraph,
		BodyEntry:    calleeGraph.InitialState,
		Environment:  swSpec.Environment,
		Inputs:       inputs,
		DeclaredVars: calleeGraph.Variables,
	}
	g.SubworkflowOrder = append(g.SubworkflowOrder, swSpec.Name)
	return diags
}

// buildChildOpts builds the CompileOpts for a recursive subworkflow compilation,
// appending resolvedDir to the load chain and incrementing the depth.
func buildChildOpts(opts CompileOpts, resolvedDir string) CompileOpts {
	child := opts
	newChain := make([]string, len(opts.SubworkflowChain), len(opts.SubworkflowChain)+1)
	copy(newChain, opts.SubworkflowChain)
	newChain = append(newChain, resolvedDir)
	child.SubworkflowChain = newChain
	child.LoadDepth = opts.LoadDepth + 1
	child.WorkflowDir = resolvedDir
	return child
}

// detectSubworkflowCycle returns a DiagError if resolvedDir already appears in
// the current subworkflow load chain, indicating a recursive import cycle.
func detectSubworkflowCycle(resolvedDir string, chain []string) *hcl.Diagnostic {
	for _, chainPath := range chain {
		if chainPath != resolvedDir {
			continue
		}
		cycle := make([]string, len(chain)+1)
		copy(cycle, chain)
		cycle[len(chain)] = resolvedDir
		return &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("subworkflow cycle detected: %s", strings.Join(cycle, " -> ")),
		}
	}
	return nil
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

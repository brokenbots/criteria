package workflow

// compile_steps_graph.go — FSM graph traversal and node construction helpers
// used by every step-kind compiler: back-edge detection, reachable-target
// enumeration, outcome compilation, and node constructors.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// compileOutcomeBlock populates node.Outcomes from sp.Outcomes. It validates:
//   - no duplicate outcome names
//   - "next" is present and resolves to a valid node target
//   - "return" is not used as a step name (reserved sentinel)
//   - outcome "default" block, if present, has a valid next target
//   - the optional "output" expression, when present, references only known
//     vars/locals (runtime-only refs like steps.* are deferred, not errors)
//     and, when foldable at compile time, evaluates to an object type.
//   - the optional "write" blocks, when present, reference declared data blocks
//     and map to output keys that exist in the output projection (when
//     declared) or in the adapter's output schema (when adapterOutputSchema
//     is non-nil).
//
// The optional "output" expression is extracted from the outcome's Remain body
// and stored in CompiledOutcome.OutputExpr. The optional "write" blocks are
// stored in CompiledOutcome.Writes.
func compileOutcomeBlock(sp *StepSpec, node *StepNode, g *FSMGraph, opts CompileOpts, adapterOutputSchema map[string]ConfigField) hcl.Diagnostics {
	var diags hcl.Diagnostics
	seen := map[string]bool{}
	isIter := node.ForEach != nil || node.Count != nil || node.Parallel != nil || node.While != nil
	for _, o := range sp.Outcomes {
		if seen[o.Name] {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: duplicate outcome %q", sp.Name, o.Name)})
			continue
		}
		seen[o.Name] = true
		if o.Next == nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q outcome %q: next is required", sp.Name, o.Name)})
			continue
		}
		nextStr, d := resolveNextAttr(o.Next, fmt.Sprintf("step %q", sp.Name), fmt.Sprintf("outcome %q", o.Name))
		diags = append(diags, d...)
		if nextStr == "" {
			continue
		}
		compiled := &CompiledOutcome{Name: o.Name, Next: nextStr}
		// Aggregate iterating outcomes (next != "_continue") fire after all
		// iterations complete; the engine has no raw adapter outputs at that
		// point. write blocks on these outcomes must use an explicit
		// output = { ... } projection block — never the adapter output schema.
		isAggregateIter := isIter && nextStr != "_continue"
		d = compileOutcomeRemain(sp.Name, o.Name, o.Remain, o.Writes, g, opts, adapterOutputSchema, compiled, isAggregateIter)
		diags = append(diags, d...)
		if o.Name == "default" {
			node.DefaultOutcome = compiled
		} else {
			node.Outcomes[o.Name] = compiled
		}
	}

	diags = append(diags, warnWritesReadingWrittenData(sp.Name, node)...)

	return diags
}

// warnWritesReadingWrittenData emits an informational diagnostic for every
// write whose value expression reads a data.<kind>.<name> value that the same
// step also writes. This is legal and well-defined — data.* in a write value
// resolves to the step-entry snapshot, not to any value written earlier in the
// step, so the write set is atomic against that snapshot — but the read-vs-write
// distinction is subtle, so we surface it to aid troubleshooting.
func warnWritesReadingWrittenData(stepName string, node *StepNode) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Collect every (kind, name) the step writes, across all outcomes.
	written := map[[2]string]bool{}
	forEachOutcome(node, func(co *CompiledOutcome) {
		for _, w := range co.Writes {
			written[[2]string{w.DataKind, w.DataName}] = true
		}
	})
	if len(written) == 0 {
		return nil
	}

	forEachOutcome(node, func(co *CompiledOutcome) {
		for _, w := range co.Writes {
			seen := map[[2]string]bool{}
			for _, tr := range w.ValueExpr.Variables() {
				kind, name, ok := dataRefKindName(tr)
				if !ok || !written[[2]string{kind, name}] || seen[[2]string{kind, name}] {
					continue
				}
				seen[[2]string{kind, name}] = true
				r := tr.SourceRange()
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagWarning,
					Summary:  fmt.Sprintf("step %q outcome %q: write to data %q %q reads data.%s.%s, which this step also writes", stepName, co.Name, w.DataKind, w.DataName, kind, name),
					Detail:   fmt.Sprintf("data.%s.%s resolves to the step-entry snapshot, not to any value written earlier in this step. All writes in a step are applied atomically against that snapshot, so this read is well-defined and deterministic. This message is informational; no change is required if that is the intended behavior.", kind, name),
					Subject:  r.Ptr(),
				})
			}
		}
	})

	return diags
}

// forEachOutcome invokes fn for every compiled outcome on the node, including
// the default outcome when present.
func forEachOutcome(node *StepNode, fn func(*CompiledOutcome)) {
	for _, co := range node.Outcomes {
		fn(co)
	}
	if node.DefaultOutcome != nil {
		fn(node.DefaultOutcome)
	}
}

// dataRefKindName extracts (kind, name) from a data.<kind>.<name>[.value...]
// traversal. ok is false for any traversal that is not a data reference of at
// least three segments.
func dataRefKindName(tr hcl.Traversal) (kind, name string, ok bool) {
	if len(tr) < 3 {
		return "", "", false
	}
	root, ok1 := tr[0].(hcl.TraverseRoot)
	kindSeg, ok2 := tr[1].(hcl.TraverseAttr)
	nameSeg, ok3 := tr[2].(hcl.TraverseAttr)
	if !ok1 || !ok2 || !ok3 || root.Name != "data" {
		return "", "", false
	}
	return kindSeg.Name, nameSeg.Name, true
}

// validateOutcomeOutputExpr validates the output = { ... } expression on an
// outcome block. It:
//  1. Checks for unknown var/local references using validateFoldableAttrs
//     (runtime-only namespaces like "steps" and "each" are silently deferred).
//  2. When the expression is foldable at compile time (no runtime refs), verifies
//     that the result is an object type so non-object literals (e.g. strings)
//     are caught at compile time.
func validateOutcomeOutputExpr(stepName, outcomeName string, attr *hcl.Attribute, g *FSMGraph, opts CompileOpts) hcl.Diagnostics {
	// Step 1: check for unresolvable free-variable references.
	refDiags := validateFoldableAttrs(hcl.Attributes{attr.Name: attr}, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if refDiags.HasErrors() {
		return refDiags
	}

	// Step 2: if foldable at compile time, validate the type is an object.
	val, foldable, foldDiags := FoldExpr(attr.Expr, graphVars(g), graphLocals(g), opts.WorkflowDir)
	if foldDiags.HasErrors() {
		return foldDiags
	}
	if !foldable {
		// Expression contains runtime-only refs — defer to runtime evaluation.
		return nil
	}
	if val == cty.NilVal || !val.IsKnown() {
		return nil
	}
	if !val.Type().IsObjectType() && val.Type() != cty.DynamicPseudoType {
		r := attr.Expr.StartRange()
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q outcome %q: output must be an object literal; got %s", stepName, outcomeName, val.Type().FriendlyName()),
			Subject:  &r,
		}}
	}
	return nil
}

// validateOutputExprStepOutputRefs checks that every step.output.<field>
// traversal in expr references a field that exists in adapterOutputSchema.
// When schema is empty (nil or zero-length), no check is performed — the
// adapter has no declared output contract and all field references are valid.
// Traversals that do not match the step.output.<field> shape are ignored.
func validateOutputExprStepOutputRefs(stepName, outcomeName string, expr hcl.Expression, schema map[string]ConfigField) hcl.Diagnostics {
	if len(schema) == 0 {
		return nil
	}
	var diags hcl.Diagnostics
	for _, traversal := range expr.Variables() {
		// Require at least step.output.<field> — three segments minimum.
		if len(traversal) < 3 {
			continue
		}
		root, rootOK := traversal[0].(hcl.TraverseRoot)
		mid, midOK := traversal[1].(hcl.TraverseAttr)
		field, fieldOK := traversal[2].(hcl.TraverseAttr)
		if !rootOK || !midOK || !fieldOK {
			continue
		}
		if root.Name != "step" || mid.Name != "output" {
			continue
		}
		if _, known := schema[field.Name]; !known {
			r := field.SrcRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q outcome %q: output field %q is not declared in the adapter's output schema", stepName, outcomeName, field.Name),
				Subject:  &r,
			})
		}
	}
	return diags
}

// staticObjectExprKeys extracts the string keys of a literal object expression
// at compile time. It returns a non-nil map only when the expression is an
// hclsyntax.ObjectConsExpr with at least one literal string key; computed keys
// are silently skipped. Returns nil if the expression is not an object literal.
func staticObjectExprKeys(expr hcl.Expression) map[string]bool {
	oc, ok := expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil
	}
	keys := make(map[string]bool, len(oc.Items))
	for _, item := range oc.Items {
		tv, diags := item.KeyExpr.Value(nil)
		if diags.HasErrors() || !tv.IsKnown() || tv.IsNull() || tv.Type() != cty.String {
			continue
		}
		keys[tv.AsString()] = true
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// compileOutcomeRemain processes the Remain body of an outcome block, extracting
// the optional "output" attribute and compiling "write" blocks into compiled.
// isAggregateIter must be true when this outcome is an aggregate outcome on an
// iterating step (next != "_continue"): in that case the engine has no raw
// adapter outputs at the time the outcome fires, so write blocks can only
// reference keys from an explicit output = { ... } projection block.
func compileOutcomeRemain(stepName, outcomeName string, remain hcl.Body, writes []WriteSpec, g *FSMGraph, opts CompileOpts, adapterOutputSchema map[string]ConfigField, compiled *CompiledOutcome, isAggregateIter bool) hcl.Diagnostics {
	var diags hcl.Diagnostics

	var knownOutputKeys map[string]bool
	if remain != nil {
		content, _, d := remain.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{
				{Name: "output", Required: false},
			},
		})
		diags = append(diags, d...)

		if attr, ok := content.Attributes["output"]; ok {
			compiled.OutputExpr = attr.Expr
			diags = append(diags, validateOutcomeOutputExpr(stepName, outcomeName, attr, g, opts)...)
			if !isAggregateIter {
				diags = append(diags, validateOutputExprStepOutputRefs(stepName, outcomeName, attr.Expr, adapterOutputSchema)...)
			}
			knownOutputKeys = staticObjectExprKeys(attr.Expr)
		}
	}

	if len(writes) > 0 {
		if isAggregateIter && knownOutputKeys == nil {
			// Aggregate outcomes have no raw adapter outputs at runtime.
			// Require an explicit output = { ... } projection so the compiler
			// can validate the keys and the engine has values to write from.
			r := writes[0].Target.StartRange()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q outcome %q: write blocks on aggregate outcomes require an output = { ... } projection block", stepName, outcomeName),
				Detail:   `Aggregate outcomes (e.g. "all_succeeded", "any_failed") fire after all iterations complete and have no single adapter output available. Add an output = { ... } block inside this outcome to project the values you want to write, then reference those projection keys in write blocks.`,
				Subject:  &r,
			})
		} else {
			effectiveKeys := resolveWriteKeys(knownOutputKeys, adapterOutputSchema)
			compiledWrites, d := compileWrites(stepName, outcomeName, writes, g, effectiveKeys)
			diags = append(diags, d...)
			compiled.Writes = compiledWrites
		}
	}

	return diags
}

// resolveWriteKeys returns the set of known output keys for write block
// validation. Prefers projection keys; falls back to the adapter output schema.
func resolveWriteKeys(projectionKeys map[string]bool, schema map[string]ConfigField) map[string]bool {
	if projectionKeys != nil {
		return projectionKeys
	}
	if len(schema) == 0 {
		return nil
	}
	keys := make(map[string]bool, len(schema))
	for k := range schema {
		keys[k] = true
	}
	return keys
}

// validateStepNameNotReturn errors when a step is named "return" since that
// string is the reserved outcome routing sentinel.
func validateStepNameNotReturn(sp *StepSpec) hcl.Diagnostics {
	if sp.Name == ReturnSentinel {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `step "return": "return" is a reserved name; steps cannot be named "return"`,
			Detail:   `The name "return" is reserved as a sentinel for outcome routing (next = step.return). Choose a different step name.`,
		}}
	}
	return nil
}

//   - has a back-edge (a path in the outcome graph that leads back to itself), AND
//   - has no max_visits set (MaxVisits == 0), AND
//   - the workflow's max_total_steps exceeds the MaxVisitsWarnThreshold (and
//     the threshold is non-zero).
//
// The check runs after compileSteps so all outcome targets are populated.
// A DFS from each step's outcomes is used to detect back-edges; visited nodes
// are tracked to keep the walk O(N) per step.
func warnBackEdges(g *FSMGraph) hcl.Diagnostics {
	threshold := g.Policy.MaxVisitsWarnThreshold
	if threshold == 0 {
		// Warning disabled via max_visits_warn_threshold = 0.
		return nil
	}
	if g.Policy.MaxTotalSteps <= threshold {
		// max_total_steps is within the threshold; no warning needed.
		return nil
	}

	var diags hcl.Diagnostics
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		if step.MaxVisits != 0 {
			continue // already bounded; no warning
		}
		if stepHasBackEdge(name, g) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary: fmt.Sprintf(
					"step %q: appears in a loop with max_total_steps=%d and no max_visits; consider setting max_visits to bound back-edge iteration",
					name, g.Policy.MaxTotalSteps,
				),
			})
		}
	}
	return diags
}

// nodeTargets returns the names of all FSM nodes that the named node can
// transition to. Recognises steps, branches, waits, and approvals; returns
// nil for unknown nodes (state nodes have no outgoing edges and are dead-ends).
// The "_continue" and "return" pseudo-targets are excluded because they are
// never real nodes.
func nodeTargets(name string, g *FSMGraph) []string {
	if step, ok := g.Steps[name]; ok {
		var targets []string
		for _, co := range step.Outcomes {
			if co.Next != "_continue" && co.Next != ReturnSentinel {
				targets = append(targets, co.Next)
			}
		}
		return targets
	}
	if sw, ok := g.Switches[name]; ok {
		targets := make([]string, 0, len(sw.Conditions)+1)
		for _, cond := range sw.Conditions {
			if cond.Next != ReturnSentinel {
				targets = append(targets, cond.Next)
			}
		}
		if sw.DefaultNext != "" && sw.DefaultNext != ReturnSentinel {
			targets = append(targets, sw.DefaultNext)
		}
		return targets
	}
	if wait, ok := g.Waits[name]; ok {
		targets := make([]string, 0, len(wait.Outcomes))
		for _, t := range wait.Outcomes {
			targets = append(targets, t)
		}
		return targets
	}
	if approval, ok := g.Approvals[name]; ok {
		targets := make([]string, 0, len(approval.Outcomes))
		for _, t := range approval.Outcomes {
			targets = append(targets, t)
		}
		return targets
	}
	return nil
}

// checkCrossStepFieldRefs walks every compiled expression that may contain
// steps.<name>.<field> traversals and emits DiagError when <field> is absent
// from the referenced step's declared OutputSchema or when the step name is
// unknown. Only fires when a schema is available; steps with no OutputSchema
// are skipped (permissive).
//
// Expression sites checked:
//   - StepNode.InputExprs (step input block attribute expressions)
//   - CompiledOutcome.OutputExpr (outcome output projections, cross-step form)
//   - SwitchNode.DefaultOutput (switch default output expressions)
//   - SwitchCondition.OutputExpr (per-arm output projections in switch conditions)
//
// Switch condition match expressions are intentionally excluded: they are
// already checked inline by validateSwitchExprRefs during compileSwitches,
// which runs after all steps are registered. Including them here would produce
// duplicate diagnostics for the same traversal.
//
// This is a post-compilation pass: all steps must be registered in g.Steps
// before it runs so forward-references resolve correctly.
func checkCrossStepFieldRefs(g *FSMGraph, schemas map[string]AdapterInfo) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, ne := range collectCrossStepExprs(g) {
		diags = append(diags, checkStepsFieldTraversals(ne.context, ne.expr, g, schemas)...)
	}
	return diags
}

type namedExpr struct {
	context string
	expr    hcl.Expression
}

// collectCrossStepExprs gathers every expression site that may contain
// steps.<name>.<field> traversals, in deterministic declaration order.
// Switch condition match expressions are excluded — they are validated
// inline by validateSwitchExprRefs and would produce duplicate warnings.
func collectCrossStepExprs(g *FSMGraph) []namedExpr {
	var exprs []namedExpr
	for _, name := range g.stepOrder {
		step := g.Steps[name]
		for k, expr := range step.InputExprs {
			exprs = append(exprs, namedExpr{fmt.Sprintf("step %q input %q", step.Name, k), expr})
		}
		for outName, co := range step.Outcomes {
			exprs = appendOutcomeRefExprs(exprs, step.Name, outName, co)
		}
		if step.DefaultOutcome != nil {
			exprs = appendOutcomeRefExprs(exprs, step.Name, "default", step.DefaultOutcome)
		}
	}
	swNames := make([]string, 0, len(g.Switches))
	for swName := range g.Switches {
		swNames = append(swNames, swName)
	}
	sort.Strings(swNames)
	for _, swName := range swNames {
		sw := g.Switches[swName]
		if sw.DefaultOutput != nil {
			exprs = append(exprs, namedExpr{fmt.Sprintf("switch %q default output", swName), sw.DefaultOutput})
		}
		for i, cond := range sw.Conditions {
			if cond.OutputExpr != nil {
				exprs = append(exprs, namedExpr{fmt.Sprintf("switch %q condition %d output", swName, i), cond.OutputExpr})
			}
		}
	}
	return exprs
}

// appendOutcomeRefExprs adds the output projection and every write value
// expression of one outcome to exprs, so cross-step field references in both
// are validated.
func appendOutcomeRefExprs(exprs []namedExpr, stepName, outName string, co *CompiledOutcome) []namedExpr {
	if co.OutputExpr != nil {
		exprs = append(exprs, namedExpr{fmt.Sprintf("step %q outcome %q output", stepName, outName), co.OutputExpr})
	}
	for _, w := range co.Writes {
		exprs = append(exprs, namedExpr{fmt.Sprintf("step %q outcome %q write %q %q", stepName, outName, w.DataKind, w.DataName), w.ValueExpr})
	}
	return exprs
}

// isStepsOutputsTraversal reports whether traversal is steps.X.outputs.Y, which
// is validated by compileOutputRefs and should be skipped by other passes.
func isStepsOutputsTraversal(traversal hcl.Traversal) bool {
	if len(traversal) < 4 {
		return false
	}
	outAttr, ok := traversal[2].(hcl.TraverseAttr)
	return ok && outAttr.Name == "outputs"
}

// checkStepsFieldTraversals inspects expr for steps.<name>.<field> traversals
// and emits errors for fields absent from the step's OutputSchema.
func checkStepsFieldTraversals(context string, expr hcl.Expression, g *FSMGraph, schemas map[string]AdapterInfo) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, traversal := range expr.Variables() {
		// Require at least: steps . <name> . <field>
		if len(traversal) < 3 {
			continue
		}
		root, rootOK := traversal[0].(hcl.TraverseRoot)
		nameAttr, nameOK := traversal[1].(hcl.TraverseAttr)
		fieldAttr, fieldOK := traversal[2].(hcl.TraverseAttr)
		if !rootOK || !nameOK || !fieldOK {
			continue
		}
		if root.Name != "steps" {
			continue
		}

		// steps.X.outputs.Y is validated by compileOutputRefs; skip here.
		if isStepsOutputsTraversal(traversal) {
			continue
		}

		step, isStep := g.Steps[nameAttr.Name]
		if !isStep {
			// Unknown step name at this site — no other pass validates step
			// input, outcome output, or switch output expressions at compile
			// time, so emit an error here for early feedback.
			r := nameAttr.SrcRange
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary: fmt.Sprintf(
					"%s: references unknown step %q",
					context, nameAttr.Name,
				),
				Subject: &r,
			})
			continue
		}

		// Iterating steps expose an indexed namespace (steps.<name>[idx].<field>),
		// not a flat field set; a bare field traversal can't be validated here.
		if step.ForEach != nil || step.Count != nil || step.Parallel != nil || step.While != nil {
			continue
		}

		declared, hasContract := declaredStepOutputNames(step, g, schemas)
		if !hasContract {
			continue // no declared contract; permissive
		}

		if _, known := declared[fieldAttr.Name]; !known {
			r := fieldAttr.SrcRange
			detail := fmt.Sprintf("step %q does not declare an output named %q", nameAttr.Name, fieldAttr.Name)
			if s := suggestFromNameSet(fieldAttr.Name, declared); len(s) > 0 {
				detail += fmt.Sprintf("; did you mean %s?", strings.Join(s, ", "))
			}
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary: fmt.Sprintf(
					"%s: field %q is not declared in the outputs of step %q",
					context, fieldAttr.Name, nameAttr.Name,
				),
				Detail:  detail,
				Subject: &r,
			})
		}
	}
	return diags
}

// declaredStepOutputNames returns the set of output field names a step declares
// and whether it has a declared contract at all. Adapter steps use their
// adapter's OutputSchema; subworkflow steps use the callee's declared output
// names. A step with no declared outputs returns (nil, false) so references to
// it stay permissive (no false positives against an unknown contract).
func declaredStepOutputNames(step *StepNode, g *FSMGraph, schemas map[string]AdapterInfo) (map[string]bool, bool) {
	if step.TargetKind == StepTargetSubworkflow {
		sw := g.Subworkflows[step.SubworkflowRef]
		if sw == nil || sw.Body == nil || len(sw.Body.Outputs) == 0 {
			return nil, false
		}
		names := make(map[string]bool, len(sw.Body.Outputs))
		for n := range sw.Body.Outputs {
			names[n] = true
		}
		return names, true
	}
	info, ok := adapterInfo(schemas, adapterTypeFromRef(step.AdapterRef))
	if !ok || len(info.OutputSchema) == 0 {
		return nil, false
	}
	names := make(map[string]bool, len(info.OutputSchema))
	for n := range info.OutputSchema {
		names[n] = true
	}
	return names, true
}

// suggestFromNameSet returns up to 3 candidate names from the set, sorted by
// Levenshtein distance to the misspelled name then lexically.
func suggestFromNameSet(misspelled string, names map[string]bool) []string {
	type candidate struct {
		name     string
		distance int
	}
	candidates := make([]candidate, 0, len(names))
	for name := range names {
		candidates = append(candidates, candidate{name: name, distance: levenshteinDistance(misspelled, name)})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].distance != candidates[j].distance {
			return candidates[i].distance < candidates[j].distance
		}
		return candidates[i].name < candidates[j].name
	})
	var out []string
	for i := 0; i < len(candidates) && i < 3; i++ {
		out = append(out, fmt.Sprintf("%q", candidates[i].name))
	}
	return out
}

// stepHasBackEdge reports whether the named step can reach itself via outcome
// transitions (i.e. it is part of a cycle in the FSM graph). The walk follows
// edges through all node kinds — steps, branches, waits, and approvals — so
// that loops mediated by non-step nodes are also detected. StateNodes have no
// outgoing edges and are treated as dead-ends.
func stepHasBackEdge(startName string, g *FSMGraph) bool {
	step, ok := g.Steps[startName]
	if !ok {
		return false
	}
	// while steps are inherently looping: the condition is re-evaluated before
	// each iteration, so they always have an implicit back-edge.
	if step.While != nil {
		return true
	}
	visited := map[string]bool{}
	var walk func(name string) bool
	walk = func(name string) bool {
		if name == startName {
			return true
		}
		if visited[name] {
			return false
		}
		visited[name] = true
		for _, target := range nodeTargets(name, g) {
			if walk(target) {
				return true
			}
		}
		return false
	}
	for _, target := range nodeTargets(startName, g) {
		if walk(target) {
			return true
		}
	}
	return false
}

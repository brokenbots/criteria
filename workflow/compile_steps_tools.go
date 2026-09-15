package workflow

// compile_steps_tools.go — compile-time validation of step-level tool grants
// (CRI-156). Consumes the raw traversals captured by captureStepToolRefs
// (CRI-155) and re-walks each step's `tools` attribute with positions, so
// entries that are not bare traversals are diagnosed here too (the parse pass
// silently drops them).
//
// The diagnostic set is one per failure mode (CRI-156):
//
//  1. resolution: every entry must be a bare traversal of the form
//     adapter.<type>.<name>.tools[.<tool>] referencing an adapter declared in
//     the same workflow (error).
//  2. static tool-name check: on an adapter that declares tool blocks, a
//     named ref must resolve to a declared tool (error); bare .tools refs
//     grant the adapter's full static surface and skip the name check.
//  3. dynamic_tools = true adapters are extensible: the name check is skipped
//     and nothing is emitted (the runtime resolves names; M7 hook).
//  4. strict default: an adapter that declares neither tool blocks nor
//     dynamic_tools rejects named refs — callee presents no tool surface (error).
//  5. pointless caller: a caller adapter that does not declare the
//     adapter_tools capability in its Info().Capabilities cannot issue tool
//     calls, so passing tools is pointless (warning; compilation proceeds).
//  6. duplicate entry in one list (warning).
//  7. bare .tools leniency: a bare ref on an adapter with no declared surface
//     is accepted with an advisory diagnostic (runtime-validated; it resolves
//     to nothing unless the adapter presents tools at runtime). HCL v2
//     exposes only error and warning severities, so the ticket's "info"
//     severity is realized as the least-severe representable class
//     (hcl.DiagWarning) with advisory wording; the language server maps
//     non-error/warning severities to LSP Information.
//
// allow_tools interplay (mode 8): policy is runtime; a matching allow_tools
// entry is NOT required for compilation and validateAllowToolsEntry is
// untouched by this file.

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// toolRefSyntaxDoc is the canonical documentation reference used in tool-ref
// diagnostics.
const toolRefSyntaxDoc = "see docs/LANGUAGE-SPEC.md (Adapter tools) for the tools entry grammar"

// adapterToolsCapability is the well-known AdapterInfo capability that marks
// an adapter as able to issue tool calls (ADR-0004 §9).
const adapterToolsCapability = "adapter_tools"

// toolSurface describes the tool surface an adapter declaration presents to
// step-level tools entries.
type toolSurface struct {
	staticToolNames []string // tool "<name>" block labels, in declaration order
	dynamic         bool     // dynamic_tools = true
}

func (s toolSurface) hasStaticTools() bool { return len(s.staticToolNames) > 0 }

func (s toolSurface) declaresTool(name string) bool {
	for _, n := range s.staticToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// stepToolsAttr returns the step's `tools` attribute from its Remain body, or
// nil when the step declares none. PartialContent on an hclsyntax body hides
// matched attributes only on the returned remain copy, so this never affects
// later readers of sp.Remain.
func stepToolsAttr(sp *StepSpec) *hcl.Attribute {
	if sp == nil || sp.Remain == nil {
		return nil
	}
	schema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "tools"}}}
	attrs, _, _ := sp.Remain.PartialContent(schema)
	if attrs == nil {
		return nil
	}
	return attrs.Attributes["tools"]
}

// buildToolSurfaces indexes the workflow's adapter declarations by
// "<type>.<name>". AdapterNode does not carry tool surfaces, so this is built
// fresh per validation call; adapters are few and the cost is trivial.
func buildToolSurfaces(spec *Spec) map[string]toolSurface {
	if spec == nil {
		return nil
	}
	out := make(map[string]toolSurface, len(spec.Adapters))
	for i := range spec.Adapters {
		ad := &spec.Adapters[i]
		names := make([]string, 0, len(ad.Tools))
		for _, t := range ad.Tools {
			names = append(names, t.Name)
		}
		out[ad.Type+"."+ad.Name] = toolSurface{staticToolNames: names, dynamic: ad.DynamicTools}
	}
	return out
}

// validateStepToolRefs validates a step's `tools` attribute (CRI-156).
// callerType is the step's own adapter type for adapter-targeted steps and ""
// for subworkflow-targeted steps, which have no caller adapter and therefore
// never trip the pointless-caller warning.
func validateStepToolRefs(g *FSMGraph, sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, callerType string) hcl.Diagnostics {
	attr := stepToolsAttr(sp)
	if attr == nil {
		return nil
	}
	items, listDiags := hcl.ExprList(attr.Expr)
	if listDiags.HasErrors() {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: tools must be a list of tool references", sp.Name),
			Detail: fmt.Sprintf(
				"a tools attribute must be a list of bare traversals of the form adapter.<type>.<name>.tools[.<tool>]; %s",
				toolRefSyntaxDoc),
			Subject: attr.Expr.Range().Ptr(),
		}}
	}
	var diags hcl.Diagnostics
	if callerType != "" && len(items) > 0 {
		if info, ok := adapterInfo(schemas, callerType); ok && !adapterHasCapability(&info, adapterToolsCapability) {
			diags = append(diags, pointlessCallerToolDiag(sp, callerType, attr))
		}
	}
	return append(diags, validateToolRefEntries(g, sp, spec, items)...)
}

// pointlessCallerToolDiag is the mode 5 warning: the caller cannot issue tool
// calls, so its tools list has no effect.
func pointlessCallerToolDiag(sp *StepSpec, callerType string, attr *hcl.Attribute) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("step %q: passing tools is pointless", sp.Name),
		Detail: fmt.Sprintf(
			"the step's adapter type %q does not declare the %q capability in its Info().Capabilities, "+
				"so it cannot issue tool calls and this tools list has no effect; remove the list or declare the capability. %s",
			callerType, adapterToolsCapability, toolRefSyntaxDoc),
		Subject: attr.Expr.Range().Ptr(),
	}
}

// validateToolRefEntries walks the tools list in declaration order: mode 1
// (shape + resolution), mode 6 (duplicates), and modes 2/3/4/7 (callee tool
// surface). A duplicated target is warned once at its second occurrence and
// skipped there, since its first occurrence was already validated.
func validateToolRefEntries(g *FSMGraph, sp *StepSpec, spec *Spec, items []hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	surfaces := buildToolSurfaces(spec)
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		tr, _ := hcl.AbsTraversalForExpr(item)
		if len(tr) == 0 {
			diags = append(diags, nonTraversalToolRefDiag(sp, item))
			continue
		}
		if d := malformedToolRefDiag(sp, tr); d != nil {
			diags = append(diags, d)
			continue
		}
		target := toolRefTargetString(tr)
		if _, dup := seen[target]; dup {
			diags = append(diags, duplicateToolRefDiag(sp, target, tr))
			continue
		}
		seen[target] = struct{}{}
		diags = append(diags, validateToolRefSurface(g, sp, surfaces, target, tr)...)
	}
	return diags
}

// nonTraversalToolRefDiag is the mode 1 error for entries that are not bare
// traversals (string literals, function calls, splats, template expressions).
func nonTraversalToolRefDiag(sp *StepSpec, item hcl.Expression) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q: tools entry is not a bare traversal", sp.Name),
		Detail: fmt.Sprintf(
			"a tools entry must be a bare traversal of the form adapter.<type>.<name>.tools[.<tool>]; "+
				"quoted strings, function calls, and index expressions are not accepted. %s", toolRefSyntaxDoc),
		Subject: item.Range().Ptr(),
	}
}

// malformedToolRefDiag is the mode 1 error for a traversal that does not have
// the shape adapter.<type>.<name>.tools[.<tool>]: wrong label count, a first
// label other than the literal "adapter", non-bareword segments, or a
// non-"tools" fourth label. Returns nil when the shape is well-formed.
func malformedToolRefDiag(sp *StepSpec, tr hcl.Traversal) *hcl.Diagnostic {
	if toolRefShapeProblem(tr) == "" {
		return nil
	}
	summary := fmt.Sprintf("step %q: malformed tools entry", sp.Name)
	if target := toolRefTargetString(tr); target != "" {
		summary = fmt.Sprintf("step %q: malformed tools entry %q", sp.Name, target)
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  summary,
		Detail: fmt.Sprintf(
			"a tools entry must have the form adapter.<type>.<name>.tools[.<tool>]: the first label must be the "+
				"literal \"adapter\", the <type>, <name>, and \"tools\" labels are required, and the optional <tool> "+
				"segment must be a bareword identifier. %s", toolRefSyntaxDoc),
		Subject: tr.SourceRange().Ptr(),
	}
}

// toolRefShapeProblem describes the first shape violation of a traversal, or
// "" when the traversal has the shape adapter.<type>.<name>.tools[.<tool>].
func toolRefShapeProblem(tr hcl.Traversal) string {
	if len(tr) != 4 && len(tr) != 5 {
		return "wrong label count"
	}
	root, ok := tr[0].(hcl.TraverseRoot)
	if !ok || root.Name != "adapter" {
		return "first label is not adapter"
	}
	for i := 1; i < len(tr); i++ {
		attr, ok := tr[i].(hcl.TraverseAttr)
		if !ok || attr.Name == "" {
			return "segment is not a bareword identifier"
		}
	}
	if tr[3].(hcl.TraverseAttr).Name != "tools" {
		return "fourth label is not tools"
	}
	return ""
}

// toolRefTargetString renders a traversal as its dotted target string, or ""
// when it contains non-name segments (e.g. indexes), which cannot name a tool.
func toolRefTargetString(tr hcl.Traversal) string {
	var sb strings.Builder
	for i, part := range tr {
		switch p := part.(type) {
		case hcl.TraverseRoot:
			if i > 0 {
				return ""
			}
			sb.WriteString(p.Name)
		case hcl.TraverseAttr:
			sb.WriteString(".")
			sb.WriteString(p.Name)
		default:
			return ""
		}
	}
	return sb.String()
}

// duplicateToolRefDiag is the mode 6 warning for a target repeated in one list.
func duplicateToolRefDiag(sp *StepSpec, target string, tr hcl.Traversal) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("step %q: duplicate tools entry %q", sp.Name, target),
		Detail:   fmt.Sprintf("the same target appears more than once in this tools list; remove the duplicate. %s", toolRefSyntaxDoc),
		Subject:  tr.SourceRange().Ptr(),
	}
}

// validateToolRefSurface checks a well-formed target against the callee
// adapter: mode 1 resolution, then modes 2/3/4/7 against the callee's tool
// surface.
func validateToolRefSurface(g *FSMGraph, sp *StepSpec, surfaces map[string]toolSurface, target string, tr hcl.Traversal) hcl.Diagnostics {
	key := tr[1].(hcl.TraverseAttr).Name + "." + tr[2].(hcl.TraverseAttr).Name
	if _, ok := g.Adapters[key]; !ok {
		return hcl.Diagnostics{unknownAdapterToolRefDiag(sp, key, tr)}
	}
	named := ""
	if len(tr) == 5 {
		named = tr[4].(hcl.TraverseAttr).Name
	}
	return validateToolRefAgainstSurface(sp, surfaces[key], key, target, named, tr)
}

// validateToolRefAgainstSurface applies modes 2/3/4/7 against the callee's
// declared surface. Dynamic adapters (mode 3) accept anything; on an adapter
// with no declared surface a bare ref is advisory (mode 7) while a named ref
// is rejected (mode 4); on a static surface a named ref must resolve (mode 2).
func validateToolRefAgainstSurface(sp *StepSpec, surface toolSurface, key, target, named string, tr hcl.Traversal) hcl.Diagnostics {
	switch {
	case surface.dynamic:
		// Mode 3: the surface is extensible; the runtime resolves names.
		return nil
	case !surface.hasStaticTools() && named == "":
		return hcl.Diagnostics{bareToolRefInfoDiag(sp, target, tr)}
	case !surface.hasStaticTools():
		return hcl.Diagnostics{noToolSurfaceDiag(sp, key, tr)}
	case named == "":
		// Mode 2 is skipped for bare .tools refs: all-tools surface.
		return nil
	case !surface.declaresTool(named):
		return hcl.Diagnostics{unknownToolRefDiag(sp, surface, key, target, tr)}
	default:
		return nil
	}
}

// unknownAdapterToolRefDiag is the mode 1 error for a target that names an
// adapter not declared in this workflow.
func unknownAdapterToolRefDiag(sp *StepSpec, key string, tr hcl.Traversal) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q: tools entry references adapter %q which is not declared", sp.Name, key),
		Detail: fmt.Sprintf("%q is not an adapter declared in this workflow; tools entries must resolve against a declared adapter. %s",
			"adapter."+key, toolRefSyntaxDoc),
		Subject: tr.SourceRange().Ptr(),
	}
}

// bareToolRefInfoDiag is the mode 7 advisory ("info") diagnostic for a bare
// .tools ref on an adapter with no declared surface. It is emitted as a
// warning because hcl has no info severity; see the file comment.
func bareToolRefInfoDiag(sp *StepSpec, target string, tr hcl.Traversal) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("step %q: bare tools reference %q on an adapter with no declared tool surface", sp.Name, target),
		Detail: fmt.Sprintf(
			"the adapter declares neither tool blocks nor dynamic_tools = true; this reference is validated at "+
				"runtime and resolves to nothing unless the adapter presents tools at runtime. %s", toolRefSyntaxDoc),
		Subject: tr.SourceRange().Ptr(),
	}
}

// noToolSurfaceDiag is the mode 4 error for a named ref on an adapter that
// declares neither tool blocks nor dynamic_tools.
func noToolSurfaceDiag(sp *StepSpec, key string, tr hcl.Traversal) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q: callee %q presents no tool surface", sp.Name, key),
		Detail: fmt.Sprintf(
			"adapter %q declares neither tool blocks nor dynamic_tools = true, so tools entries cannot name tools on "+
				"it; declare tool blocks or set dynamic_tools = true on the adapter. %s", key, toolRefSyntaxDoc),
		Subject: tr.SourceRange().Ptr(),
	}
}

// unknownToolRefDiag is the mode 2 error for a named ref that does not match
// the callee's static tool blocks.
func unknownToolRefDiag(sp *StepSpec, surface toolSurface, key, target string, tr hcl.Traversal) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q: tools entry references unknown tool %q", sp.Name, target),
		Detail: fmt.Sprintf(
			"adapter %q declares tools %s; a bare %q reference grants its full tool surface. %s",
			key, strings.Join(surface.staticToolNames, ", "), "adapter."+key+".tools", toolRefSyntaxDoc),
		Subject: tr.SourceRange().Ptr(),
	}
}

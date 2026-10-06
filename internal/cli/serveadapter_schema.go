package cli

// Schema derivation for the serve-adapter mode's AdapterService.Info (ADR-0008
// D1): the config schema is the workflow's variable declarations, the output
// schema is the terminal output projection, and the outcomes vocabulary is the
// graph's declared step outcomes (plus the conventional terminal mappings).

import (
	"encoding/json"
	"sort"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/brokenbots/criteria/workflow"
)

// serveAdapterConfigSchema derives the InfoResponse.config_schema from the
// graph's variable declarations. Field order is declaration-agnostic: sorted
// by name for a stable wire surface. Complex types (objects, maps, non-string
// lists) cannot be expressed in the v2 type vocabulary; they degrade to
// "string" with the exact HCL type carried in the description so UIs still
// show the truth.
func serveAdapterConfigSchema(graph *workflow.FSMGraph) *criteriav2.AdapterSchemaProto {
	fields := make(map[string]*criteriav2.ConfigFieldProto, len(graph.Variables))
	for _, name := range sortedStringKeys(graph.Variables) {
		v := graph.Variables[name]
		typeName, exact := ctyConfigFieldTypeName(v.Type)
		description := v.Description
		if !exact && v.Type != cty.NilType {
			if description != "" {
				description += " "
			}
			description += "(workflow declares this as type " + typeexpr.TypeString(v.Type) + "; the engine coerces the bound value)"
		}
		if v.Secret {
			if description != "" {
				description += " "
			}
			description += "(secret: values are taint sources and are never echoed)"
		}
		fields[name] = &criteriav2.ConfigFieldProto{
			Type:        typeName,
			Description: description,
			DefaultStr:  renderCtyDefault(v.Default),
			Sensitive:   v.Secret,
			Required:    v.IsRequired(),
		}
	}
	return &criteriav2.AdapterSchemaProto{Fields: fields}
}

// ctyConfigFieldTypeName maps a variable's cty type into the adapter v2
// ConfigFieldProto type vocabulary ("string"|"number"|"bool"|"list_string").
// The bool result reports whether the mapping is exact.
func ctyConfigFieldTypeName(t cty.Type) (string, bool) {
	switch {
	case t == cty.String:
		return "string", true
	case t == cty.Number:
		return "number", true
	case t == cty.Bool:
		return "bool", true
	case t.IsListType() && t.ElementType() == cty.String:
		return "list_string", true
	default:
		return "string", false
	}
}

// renderCtyDefault renders a variable default into the
// ConfigFieldProto.default_str wire shape ("" for no declared default).
func renderCtyDefault(v cty.Value) string {
	if v == cty.NilVal || !v.IsKnown() {
		return ""
	}
	switch {
	case v.Type() == cty.String:
		return v.AsString()
	case v.Type() == cty.Number:
		return v.AsBigFloat().Text('f', -1)
	case v.Type() == cty.Bool:
		if v.True() {
			return "true"
		}
		return "false"
	default:
		raw, err := json.Marshal(ctyjson.SimpleJSONValue{Value: v})
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// serveAdapterOutputSchema derives the InfoResponse.output_schema from the
// terminal output projection declarations (W09). The v2 schema shape is a
// name-keyed ConfigFieldProto map; the optional declared type renders as its
// HCL type expression, and empty types stay empty (the engine coerces the
// projected value to a string). Outputs are not required inputs, so Required
// is always false and defaults are unset.
func serveAdapterOutputSchema(graph *workflow.FSMGraph) *criteriav2.AdapterSchemaProto {
	fields := make(map[string]*criteriav2.ConfigFieldProto, len(graph.OutputOrder))
	for _, name := range graph.OutputOrder {
		on := graph.Outputs[name]
		typeStr := ""
		if on.DeclaredType != cty.NilType {
			typeStr = typeexpr.TypeString(on.DeclaredType)
		}
		fields[name] = &criteriav2.ConfigFieldProto{
			Type:        typeStr,
			Description: on.Description,
		}
	}
	return &criteriav2.AdapterSchemaProto{Fields: fields}
}

// serveAdapterOutcomes returns the graph's declared outcomes vocabulary: every
// outcome declared by an adapter step (subworkflow bodies included), plus the
// two terminal mappings Execute guarantees ("success" and "failure"). Deduped
// and sorted for a stable Info response.
func serveAdapterOutcomes(graph *workflow.FSMGraph) []string {
	set := map[string]struct{}{
		serveAdapterOutcomeSuccess: {},
		serveAdapterOutcomeFailure: {},
	}
	var walk func(g *workflow.FSMGraph)
	walk = func(g *workflow.FSMGraph) {
		for _, step := range sortedStringKeys(g.Steps) {
			for outcome := range g.Steps[step].Outcomes {
				set[outcome] = struct{}{}
			}
		}
		for _, name := range sortedStringKeys(g.Subworkflows) {
			if body := g.Subworkflows[name].Body; body != nil {
				walk(body)
			}
		}
	}
	walk(graph)

	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

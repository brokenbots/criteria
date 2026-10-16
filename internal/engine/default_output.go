package engine

import (
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
)

// default_output.go — doctrine for adapter default output types (KB-264).
//
// Every adapter declares its default output type in its handshake
// (InfoResponse.output_schema; the step's compiled outputSchema carries the
// resolved type). A TYPED outcome's output is its declared payload schema,
// so typed steps are untouched here. An UNTYPED outcome takes the adapter's
// default output type: the adapter's emitted outputs are its shape as-is.
//
// Legacy-lane adapters carry a comment (the legacy `reason` param) instead
// of typed outputs. When the adapter's default output type declares a
// `reason` field, that field is materialized from the comment on untyped
// outcomes — an empty comment materializes `reason = ""`, because an
// untyped outcome's output IS the declared default type in full (a host-
// synthesized fallback lane finalizes with an empty comment, so the field
// must exist) — so output.reason reads uniformly (e.g. copilot's untyped
// arms) without touching shell-style adapters whose default type has no
// reason field.
//
// The step-level override keeps the KB-45 formalization: an outcome `schema`
// replaces the default for that outcome, and a schema the adapter cannot
// satisfy is a compile-time rejection validated against the declared default
// output type (workflow.validateOutcomeSchemaPayloadContract).

// defaultReasonOutputField names the only field the legacy lane carries
// outside typed outputs. Deliberately specialized: comment→reason is the one
// asymmetry the doctrine identifies; a general "declared fields carried
// elsewhere" mapping would be dead generalization until a second asymmetry
// exists.
const defaultReasonOutputField = "reason"

// withDefaultOutputs resolves an untyped outcome's raw outputs from the
// adapter's default output type. Typed (schema-declared) outcomes pass
// through untouched: the model-submitted payload is the finalized output.
func (n *stepNode) withDefaultOutputs(result adapter.Result) adapter.Result {
	if _, has := result.Outputs[defaultReasonOutputField]; has {
		return result
	}
	compiled, ok := n.step.Outcomes[result.Outcome]
	if !ok {
		compiled = n.step.DefaultOutcome
	}
	if compiled == nil || compiled.Schema != nil {
		// Unmapped outcome (unresolved default) or typed outcome: the engine
		// never enriches such payloads.
		return result
	}
	if _, declared := n.step.OutputSchema[defaultReasonOutputField]; !declared {
		// The adapter's default output type is what an untyped outcome takes;
		// without a declared reason field (e.g. shell) the comment stays
		// event-stream-only and captures are unchanged.
		return result
	}
	enriched := make(map[string]cty.Value, len(result.Outputs)+1)
	for k, v := range result.Outputs {
		enriched[k] = v
	}
	enriched[defaultReasonOutputField] = cty.StringVal(result.Comment)
	result.Outputs = enriched
	return result
}

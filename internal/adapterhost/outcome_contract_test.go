package adapterhost

// KB-45 unit tests for the host-side outcome-contract integration: the wire
// contract construction, the pinned evaluator lanes through
// evaluateLocalOutcomeContracts, the fallback synthesis, and the rejection
// chain derivation. Engine-runtime behavior is pinned in
// internal/engine/outcome_contract_repair_test.go.

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

func auditSchema() []byte {
	t := cty.Object(map[string]cty.Type{"attempts": cty.Number})
	b, err := workflow.CTypeToJSONSchema(t, nil)
	if err != nil {
		panic(err)
	}
	return b
}

func contractStep() *workflow.StepNode {
	return &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success", Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
		},
	}
}

func auditType() *cty.Type {
	t := cty.Object(map[string]cty.Type{"attempts": cty.Number})
	return &t
}

func TestOutcomeContractsForStep_LegacyStepsCarryNoContracts(t *testing.T) {
	step := &workflow.StepNode{
		Name: "plain",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: "done"},
			"failure": {Next: "failed"},
		},
	}
	require.Nil(t, outcomeContractsForStep(step), "legacy steps must not activate contract mode")
	require.Equal(t, []string{"failure", "success"}, allowedOutcomesForContracts(step),
		"legacy steps keep the legacy allowed_outcomes lane (the loader gates on contracts being nil)")
}

func TestOutcomeContractsForStep_DeterministicOrderAndOnlyContracted(t *testing.T) {
	step := &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"zebra":   {Next: "done"},
			"success": {Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
			"alpha":   {Next: "failed", RequireComment: true},
			"beta":    {Next: "failed", Fallback: true},
		},
	}
	contracts := outcomeContractsForStep(step)
	require.NotNil(t, contracts)
	names := make([]string, 0, len(contracts))
	for _, c := range contracts {
		names = append(names, c.GetName())
	}
	require.Equal(t, []string{"alpha", "beta", "success"}, names,
		"map iteration order must not leak into the wire contract list; bare outcomes are excluded")
	requireContainsContractSchemaJSON(t, contracts, "success")
}

func requireContainsContractSchemaJSON(t *testing.T, contracts []*v2.OutcomeContract, name string) {
	t.Helper()
	for _, c := range contracts {
		if c.GetName() == name {
			require.Equal(t, auditSchema(), c.GetSchemaJson())
			return
		}
	}
	t.Fatalf("no contract named %q", name)
}

func TestAllowedOutcomesForContracts_IncludesUndeclaredDefault(t *testing.T) {
	step := &workflow.StepNode{
		Name:           "ship",
		DefaultOutcome: &workflow.CompiledOutcome{Name: "default", Next: "else"},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
		},
	}
	require.Equal(t, []string{"default", "success"}, allowedOutcomesForContracts(step))
}

func TestEvaluateLocal_LegacyStepPassesThroughVerbatim(t *testing.T) {
	step := &workflow.StepNode{Name: "plain", Outcomes: map[string]*workflow.CompiledOutcome{
		"success": {Next: "done"},
	}}
	in := adapter.Result{Outcome: "success", Outputs: map[string]cty.Value{"x": cty.StringVal("y")}, Comment: "hi"}
	out, issues := evaluateLocalOutcomeContracts(step, in)
	require.Nil(t, issues)
	require.Equal(t, in, out, "legacy results must byte-forward: no wire round-trip")
}

func TestEvaluateLocal_ValidPayloadForwardsVerbatim(t *testing.T) {
	step := contractStep()
	in := adapter.Result{
		Outcome: "success",
		Outputs: map[string]cty.Value{"attempts": cty.NumberIntVal(3)},
		Comment: "done well",
	}
	out, issues := evaluateLocalOutcomeContracts(step, in)
	require.Nil(t, issues)
	require.Equal(t, "success", out.Outcome)
	require.Equal(t, "done well", out.Comment, "ExecuteResult.comment surfaces verbatim")
	require.Len(t, out.Outputs, 1)
	require.True(t, out.Outputs["attempts"].RawEquals(cty.NumberIntVal(3)))
}

func TestEvaluateLocal_InvalidPayloadYieldsPinnedIssue(t *testing.T) {
	step := contractStep()
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{Outcome: "success"})
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], `payload_schema: property "attempts": required property is missing`)
	require.Equal(t, adapter.Result{}, out, "rejected attempts return a zero result")
}

func TestEvaluateLocal_TypeMismatchIssue(t *testing.T) {
	step := contractStep()
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{
		Outcome: "success",
		Outputs: map[string]cty.Value{"attempts": cty.StringVal("three")},
	})
	require.Empty(t, out, "rejected attempts return a zero result")
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], "payload_schema")
	require.Contains(t, issues[0], `payload_schema: property "attempts": expected "number", got "string"`)
}

func TestEvaluateLocal_UnmappedOutcomeNotAllowedIssue(t *testing.T) {
	step := contractStep()
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{Outcome: "ghost-name"})
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], `outcome_not_allowed: outcome "ghost-name" is not in allowed_outcomes`)
	require.Equal(t, adapter.Result{}, out)
}

func TestEvaluateLocal_ButDeclaredBareOutcomeIsUncontracted(t *testing.T) {
	// A declared outcome WITHOUT contract attrs inside a contract-bearing
	// step is not a contract lane: the pinned evaluator treats it as
	// uncontracted. This is the documented mixed-step posture; the compile
	// gate does not forbid it.
	step := &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
			"plain":   {Next: "plainstate"},
		},
	}
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{Outcome: "plain"})
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], `outcome_uncontracted: outcome "plain" has no outcome_contracts entry`)
	require.Equal(t, adapter.Result{}, out)
}

func TestEvaluateLocal_EmptyOutcomeIssue(t *testing.T) {
	// An EMPTY-OUTCOME verdict only exists on the wire lane (a delivered
	// result with an empty outcome name). Locally, a fully empty Result never
	// enters the gate — it takes the fallback / no_result lane.
	step := contractStep()
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{Outputs: map[string]cty.Value{"x": cty.StringVal("y")}})
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], "empty_outcome")
	require.Equal(t, adapter.Result{}, out)
}

func TestEvaluateLocal_RequireCommentMissingCommentIssue(t *testing.T) {
	step := &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success", Next: "done", Schema: auditType(), SchemaJSON: auditSchema(), RequireComment: true},
		},
	}
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{
		Outcome: "success",
		Outputs: map[string]cty.Value{"attempts": cty.NumberIntVal(1)},
	})
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], `missing_comment: outcome "success" requires a comment (require_comment)`)
	require.Equal(t, adapter.Result{}, out)
}

func TestEvaluateLocal_FallbackSynthesisFromEmptyResult(t *testing.T) {
	step := &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
			"failure": {Next: "failed", Fallback: true},
		},
	}
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{})
	require.Nil(t, issues)
	require.Equal(t, "failure", out.Outcome, "the evaluator synthesizes the fallback outcome")
	require.Empty(t, out.Comment)
	require.Empty(t, out.Outputs)
}

func TestEvaluateLocal_NoResultWithoutFallbackIsRejected(t *testing.T) {
	step := contractStep()
	out, issues := evaluateLocalOutcomeContracts(step, adapter.Result{})
	require.Empty(t, out)
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], "no_result: step ended without a finalized result")
}

func TestEvaluateOutcomeContracts_WireLaneFallsBackFromEmptyResults(t *testing.T) {
	step := &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
			"failure": {Next: "failed", Fallback: true},
		},
	}
	out, issues := evaluateOutcomeContracts(step, nil)
	require.Nil(t, issues)
	require.NotNil(t, out)
	require.Equal(t, "failure", out.Outcome)
}

func TestEvaluateOutcomeContracts_LegacyWireLaneUntouched(t *testing.T) {
	step := &workflow.StepNode{Name: "plain", Outcomes: map[string]*workflow.CompiledOutcome{
		"success": {Next: "done"},
	}}
	out, issues := evaluateOutcomeContracts(step, []*v2.ExecuteResult{{Outcome: "success"}})
	require.Nil(t, issues)
	require.Nil(t, out, "legacy wire results validate nowhere host-side: no synthesis, no issues")
}

func TestExecutionRejectionChain_BuildsAndChains(t *testing.T) {
	first := ExecutionRejectionChain("success", []string{"payload_schema: property \"attempts\": required property is missing"}, nil)
	require.NotNil(t, first)
	require.Equal(t, "success", first.GetOutcome())
	require.Equal(t, uint32(1), first.GetAttempt())

	second := ExecutionRejectionChain("success", []string{"missing_comment: outcome \"success\" requires a comment (require_comment)"}, first)
	require.Equal(t, uint32(2), second.GetAttempt())
	require.Contains(t, second.GetIssues(), "missing_comment")

	var noIssuesPrior *v2.ExecutionRejection = first
	require.Equal(t, noIssuesPrior, executionRejectionChain("success", nil, noIssuesPrior),
		"an empty issue list carries the prior rejection unchanged")
}

func TestOutcomeInvalidError_MessageIncludesAllIssues(t *testing.T) {
	err := &OutcomeInvalidError{Outcome: "success", Issues: []string{
		`payload_schema: property "attempts": required property is missing`,
		`missing_comment: outcome "success" requires a comment (require_comment)`,
	}}
	require.Equal(t, "outcome \"success\" rejected by contract validation:\n"+
		"payload_schema: property \"attempts\": required property is missing\n"+
		"missing_comment: outcome \"success\" requires a comment (require_comment)", err.Error())
}

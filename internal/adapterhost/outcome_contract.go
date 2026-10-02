// Outcome-contract integration (KB-45): builds the wire ExecuteRequest
// outcome_contracts from compiled steps and validates adapter results
// host-side through the shared criteria-adapter-proto evaluator, so local
// (typed) and remote (wire) adapter results are treated identically.
package adapterhost

import (
	"fmt"
	"sort"
	"strings"

	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// OutcomeInvalidError reports a host-side contract validation rejection
// (KB-45). The engine routes it to the standard step attempt loop: each
// rejected attempt emits a StepOutcomeInvalid event and the next Execute
// carries the corresponding ExecutionRejection until the retry budget is
// exhausted.
type OutcomeInvalidError struct {
	// Outcome is the rejected result's outcome name (empty when the step
	// ended without a finalized result).
	Outcome string
	// Issues carries the per-contract validation errors in evaluation order
	// (pinned criteria-adapter-proto conformance issue vocabulary).
	Issues []string
}

func (e *OutcomeInvalidError) Error() string {
	return fmt.Sprintf("outcome %q rejected by contract validation:\n%s", e.Outcome, joinIssues(e.Issues))
}

func joinIssues(issues []string) string {
	return strings.Join(issues, "\n")
}

// outcomeContractsForStep builds the ExecuteRequest contracts from the
// compiled step outcomes (KB-45). Named and inline schemas are
// indistinguishable on the wire: CompiledOutcome.SchemaJSON already carries
// the deterministic conversion bytes. The contract list activates the
// evaluator's contract mode, so it is built ONLY for contract-bearing steps
// (any outcome or default carries a schema, require_comment, or fallback):
// legacy steps compile and run exactly as before v0.7.0.
func outcomeContractsForStep(step *workflow.StepNode) []*v2.OutcomeContract {
	contracts := make([]*v2.OutcomeContract, 0, len(step.Outcomes)+1)
	add := func(name string, compiled *workflow.CompiledOutcome) {
		c := &v2.OutcomeContract{Name: name}
		if compiled.Schema != nil {
			c.SchemaJson = compiled.SchemaJSON
		}
		c.RequireComment = compiled.RequireComment
		c.Fallback = compiled.Fallback
		contracts = append(contracts, c)
	}
	for name, compiled := range step.Outcomes {
		if compiled == nil {
			continue
		}
		if compiled.Schema != nil || compiled.RequireComment || compiled.Fallback {
			add(name, compiled)
		}
	}
	if d := step.DefaultOutcome; d != nil && (d.Schema != nil || d.RequireComment || d.Fallback) {
		if _, declared := step.Outcomes[d.Name]; !declared {
			// The default outcome participates under its own name so an
			// adapter returning the default name (or a later
			// post-exhaustion mapping to it) validates its contract too.
			add(d.Name, d)
		}
	}
	if len(contracts) == 0 {
		return nil
	}
	// Deterministic wire bytes: map iteration order must not leak into the
	// serialized contract list.
	sort.Slice(contracts, func(i, j int) bool { return contracts[i].GetName() < contracts[j].GetName() })
	return contracts
}

// allowedOutcomesForContracts returns the allowed outcome names for a
// contract-bearing step: declared outcome names plus the default outcome's
// name when it is not itself declared. The default name must be allowed so an
// adapter that explicitly returns it passes allowed_outcomes and still hits
// the engine's default mapping with its payload validated against the
// default's own contract.
func allowedOutcomesForContracts(step *workflow.StepNode) []string {
	allowed := make([]string, 0, len(step.Outcomes)+1)
	for name := range step.Outcomes {
		allowed = append(allowed, name)
	}
	if d := step.DefaultOutcome; d != nil {
		if _, declared := step.Outcomes[d.Name]; !declared {
			allowed = append(allowed, d.Name)
		}
	}
	sort.Strings(allowed)
	return allowed
}

// evaluateOutcomeContracts applies the pinned shared evaluator to one
// attempt's captured wire results, gated on contract-bearing steps. A nil
// results slice (adapter produced no finalized result) engages the fallback
// lane: the evaluator synthesizes the fallback outcome, returned as a fresh
// Result with no outputs. Legacy steps pass through untouched
// (synthesized=nil, issues=nil). Issues follow the pinned conformance
// vocabulary exactly.
func evaluateOutcomeContracts(step *workflow.StepNode, results []*v2.ExecuteResult) (*adapter.Result, []string) {
	contracts := outcomeContractsForStep(step)
	if len(contracts) == 0 {
		return nil, nil
	}
	req := &v2.ExecuteRequest{
		AllowedOutcomes:  allowedOutcomesForContracts(step),
		OutcomeContracts: contracts,
	}
	evaluated, issues := v2.EvaluateOutcomeContracts(req, results)
	if len(issues) > 0 {
		return nil, issues
	}
	if evaluated == nil {
		// Unreachable with the pinned evaluator; would otherwise read as an
		// accepted empty-outcome delivery.
		return nil, []string{"no_result: step ended without a finalized result"}
	}
	if len(results) == 0 {
		// The evaluator synthesized the fallback outcome for the
		// never-finalized attempt.
		return &adapter.Result{Outcome: evaluated.GetOutcome(), Comment: evaluated.GetComment()}, nil
	}
	return nil, nil
}

// evaluateLocalOutcomeContracts validates one locally produced adapter Result
// against the step's compiled outcome contracts through the shared evaluator.
// On success the original Result is forwarded verbatim (outputs, outcome, and
// comment) unless the fallback lane synthesized it. On rejection a zero-value
// Result is returned with the pinned issue list for the engine's attempt loop.
func evaluateLocalOutcomeContracts(step *workflow.StepNode, result adapter.Result) (adapter.Result, []string) {
	if outcomeContractsForStep(step) == nil {
		return result, nil
	}
	wire, err := wireOutcomeResult(result)
	if err != nil {
		return adapter.Result{}, []string{fmt.Sprintf("payload error: %v", err)}
	}
	var results []*v2.ExecuteResult
	if result.Outcome != "" || len(result.Outputs) > 0 {
		results = []*v2.ExecuteResult{wire}
	}
	synth, issues := evaluateOutcomeContracts(step, results)
	if len(issues) > 0 {
		return adapter.Result{}, issues
	}
	if synth != nil {
		return *synth, nil
	}
	return result, nil
}

// wireOutcomeResult serializes a locally typed adapter Result into the wire
// shape the shared evaluator validates. outputs_json is a deterministic
// ctyjson object of the raw typed outputs.
func wireOutcomeResult(result adapter.Result) (*v2.ExecuteResult, error) {
	res := &v2.ExecuteResult{Outcome: result.Outcome, Comment: result.Comment}
	if len(result.Outputs) > 0 {
		obj := cty.ObjectVal(result.Outputs)
		b, err := ctyjson.Marshal(obj, obj.Type())
		if err != nil {
			return nil, fmt.Errorf("serialize outputs: %w", err)
		}
		res.OutputsJson = b
	}
	return res, nil
}

// executionRejectionChain derives the ExecutionRejection carried on the next
// Execute call after a rejected attempt: the rejected outcome, the pinned
// issue list, and the incrementing repair attempt counter. Rejections apply
// to local and wire adapters alike (KB-45 repair loop).
func executionRejectionChain(outcome string, issues []string, prior *v2.ExecutionRejection) *v2.ExecutionRejection {
	if len(issues) == 0 {
		return prior
	}
	return v2.NewExecutionRejection(outcome, issues, prior)
}

// ExecutionRejectionChain is the exported helper the engine uses to derive
// the next repair-loop rejection (see the unexported doc).
func ExecutionRejectionChain(outcome string, issues []string, prior *v2.ExecutionRejection) *v2.ExecutionRejection {
	return executionRejectionChain(outcome, issues, prior)
}
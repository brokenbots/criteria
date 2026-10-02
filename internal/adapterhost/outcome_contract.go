// Outcome-contract integration (KB-45): builds the wire ExecuteRequest
// outcome_contracts from compiled steps and evaluates host-side result
// validation through the shared criteria-adapter-proto evaluator so local
// (typed) and remote (wire) adapter results are treated identically.
package adapterhost

import (
	"fmt"
	"sort"
	"strings"

	ctyjson "github.com/zclconf/go-cty/cty/json"

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
	// Outcome is the rejected result's outcome name (empty for empty/missing
	// outcomes).
	Outcome string
	// Issues carries the per-contract validation errors in evaluation order.
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
// the deterministic conversion bytes. Legacy steps with no schemas produce
// name-only contracts; collectAllowedOutcomes remains populated through the
// deprecation cycle.
func outcomeContractsForStep(step *workflow.StepNode) []*v2.OutcomeContract {
	if len(step.Outcomes) == 0 && step.DefaultOutcome == nil {
		return nil
	}
	contracts := make([]*v2.OutcomeContract, 0, len(step.Outcomes)+1)
	for _, name := range collectAllowedOutcomes(step) {
		compiled := step.Outcomes[name]
		if compiled == nil {
			continue
		}
		c := &v2.OutcomeContract{Name: name}
		if compiled.Schema != nil {
			c.SchemaJson = compiled.SchemaJSON
		}
		c.RequireComment = compiled.RequireComment
		c.Fallback = compiled.Fallback
		contracts = append(contracts, c)
	}
	// The default outcome participates under its own name so a run that
	// exhausts retries and maps to it still validates its contract (if any).
	if d := step.DefaultOutcome; d != nil {
		if _, declared := step.Outcomes[d.Name]; !declared {
			c := &v2.OutcomeContract{Name: d.Name}
			if d.Schema != nil {
				c.SchemaJson = d.SchemaJSON
			}
			c.RequireComment = d.RequireComment
			c.Fallback = d.Fallback
			contracts = append(contracts, c)
		}
	}
	return contracts
}

// evaluateLocalOutcomeContracts validates one locally produced adapter Result
// against the step's compiled outcome contracts through the shared evaluator.
// A zero-value result (no outcome, no outputs) is the local equivalent of the
// wire's no-finalize case: EvaluateOutcomeContracts synthesizes the fallback
// contract result there, which we return as a Result. A valid result is
// forwarded verbatim. Issues are returned for the engine's attempt loop.
func evaluateLocalOutcomeContracts(step *workflow.StepNode, result adapter.Result) (adapter.Result, []string) {
	req := &v2.ExecuteRequest{
		AllowedOutcomes:  collectAllowedOutcomes(step),
		OutcomeContracts: outcomeContractsForStep(step),
	}
	results := []*v2.ExecuteResult{}
	if result.Outcome != "" || len(result.Outputs) > 0 {
		res, err := wireOutcomeResult(result)
		if err != nil {
			return adapter.Result{}, []string{fmt.Sprintf("payload_error: %v", err)}
		}
		results = []*v2.ExecuteResult{res}
	}
	evaluated, issues := v2.EvaluateOutcomeContracts(req, results)
	if len(issues) > 0 {
		return adapter.Result{}, issues
	}
	return adapter.Result{Outcome: evaluated.GetOutcome(), Comment: evaluated.GetComment()}, nil
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

// contractsForValidate is used by SessionManager.execute; it keeps the
// contracts/allowed lists deterministic across attempts.
func sortedOutcomeNames(step *workflow.StepNode) []string {
	names := collectAllowedOutcomes(step)
	sort.Strings(names)
	return names
}
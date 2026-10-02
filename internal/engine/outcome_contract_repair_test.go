package engine

// KB-45 outcome-contract runtime tests. Compile-time behavior lives in
// workflow/compile_outcome_schema_test.go; these pin the ENGINE behavior: a
// host-rejected verdict feeds the STANDARD attempt loop (one StepOutcomeInvalid
// event per rejected attempt, each retry Execute carrying the chained
// ExecutionRejection), exhaustion maps to the step's default outcome, no
// retries with no default fails the run with the pinned issues, fallback fires
// only when the adapter never finalized, ExecuteResult.comment surfaces
// verbatim on the event stream, and legacy steps keep pre-contract behavior.

import (
	"context"
	"errors"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/zclconf/go-cty/cty"
)

// contractSink shadows the event callbacks the repair-loop tests assert on,
// delegating the rest of the sink surface to the shared fakeSink.
type contractSink struct {
	fakeSink
	invalid   []invalidOutcomeEvent
	outcome   []recordedOutcome
	defaulted [][3]string
}

type invalidOutcomeEvent struct {
	step    string
	outcome string
	issues  []string
	attempt int
}

type recordedOutcome struct {
	step    string
	outcome string
	err     error
	comment string
}

func (s *contractSink) OnStepOutcome(step, outcome string, _ time.Duration, err error, comment string) {
	s.mu.Lock()
	s.outcome = append(s.outcome, recordedOutcome{step: step, outcome: outcome, err: err, comment: comment})
	s.mu.Unlock()
}

func (s *contractSink) OnStepOutcomeInvalid(step, outcome string, issues []string, attempt int) {
	s.mu.Lock()
	cp := make([]string, len(issues))
	copy(cp, issues)
	s.invalid = append(s.invalid, invalidOutcomeEvent{step: step, outcome: outcome, issues: cp, attempt: attempt})
	s.mu.Unlock()
}

func (s *contractSink) OnStepOutcomeDefaulted(step, original, mapped string) {
	s.defaulted = append(s.defaulted, [3]string{step, original, mapped})
}

// contractWorkflow declares a named type block plus per-outcome contracts:
// a typed+require_comment success, an inline-schema aborted, a fallback
// failure, and a plain default mapping.
func contractWorkflow(policy string) string {
	return `
workflow {
  name          = "contracts"
  version       = "0.1"
  initial_state = "ship"
  target_state  = "done"
` + policy + `
}

type "audit" {
  schema = object({
    summary  = optional(string, "(no summary)")
    attempts = number
  })
}

adapter "fake" "default" {}

step "ship" {
  target = adapter.fake.default
  outcome "success" {
    schema          = type.audit
    require_comment = true
    next            = state.done
  }
  outcome "aborted" {
    schema = object({ reason = string })
    next   = state.aborted
  }
  outcome "failure" {
    fallback = true
    next     = state.failed
  }
  outcome "default" {
    next = state.defaulted
  }
}

state "done" { terminal = true }
state "aborted" {
  terminal = true
  success  = false
}
state "failed" {
  terminal = true
  success  = false
}
state "defaulted" {
  terminal = true
  success  = false
}
`
}

func repairRun(t *testing.T, seeds []adapter.Result, capture *[]*criteriav2.ExecutionRejection, policy string) (*contractSink, error) {
	t.Helper()
	i := 0
	plug := &adapterFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
		*capture = append(*capture, rejection)
		if i < len(seeds)-1 {
			r := seeds[i]
			i++
			return r, nil
		}
		return seeds[i], nil
	}}
	g := compile(t, contractWorkflow(policy))
	sink := &contractSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	return sink, err
}

func validAudit(attempts int) adapter.Result {
	return adapter.Result{
		Outcome: "success",
		Outputs: map[string]cty.Value{
			"summary":  cty.StringVal("(no summary)"),
			"attempts": cty.NumberIntVal(int64(attempts)),
		},
		Comment: "all good",
	}
}

// TestRun_OutcomeRepairedAfterInvalidPayload pins the KB-45 repair loop: the
// first attempt ships a payload the schema rejects, the host emits one
// StepOutcomeInvalid carrying the pinned issues, the retry Execute carries
// the chained ExecutionRejection, and the repaired second attempt ends the
// run with the adapter's comment surfaced verbatim on OnStepOutcome.
func TestRun_OutcomeRepairedAfterInvalidPayload(t *testing.T) {
	var rejections []*criteriav2.ExecutionRejection
	sink, err := repairRun(t, []adapter.Result{
		{Outcome: "success", Comment: "forgot the payload"},
		validAudit(2),
	}, &rejections, "policy {\n  max_step_retries = 2\n}")
	require.NoError(t, err)
	require.Equal(t, "done", sink.terminal)
	require.True(t, sink.terminalOK)

	require.Len(t, sink.invalid, 1)
	inv := sink.invalid[0]
	require.Equal(t, "ship", inv.step)
	require.Equal(t, "success", inv.outcome)
	require.Contains(t, inv.issues, `payload_schema: property "attempts": required property is missing`)
	require.Equal(t, 1, inv.attempt)

	require.Len(t, rejections, 2)
	require.Nil(t, rejections[0], "the first Execute has no prior rejection")
	require.NotNil(t, rejections[1])
	require.Equal(t, "success", rejections[1].GetOutcome())
	require.Equal(t, uint32(1), rejections[1].GetAttempt())
	require.Contains(t, rejections[1].GetIssues(), `payload_schema: property "attempts": required property is missing`)

	var comments []string
	for _, o := range sink.outcome {
		if o.step == "ship" && o.err == nil {
			comments = append(comments, o.outcome+":"+o.comment)
		}
	}
	require.Equal(t, []string{"success:all good"}, comments)
}

// TestRun_RepairLoopExhaustedFallsBackToDefaultOutcome pins that exhausted
// repairs map to the step's default outcome through the standard mapping
// path (Defaulted event), with one StepOutcomeInvalid event per attempt.
func TestRun_RepairLoopExhaustedFallsBackToDefaultOutcome(t *testing.T) {
	var rejections []*criteriav2.ExecutionRejection
	sink, err := repairRun(t, []adapter.Result{
		{Outcome: "ghost-name"}, // not in allowed_outcomes
	}, &rejections, "policy {\n  max_step_retries = 1\n}")
	require.NoError(t, err)
	require.Equal(t, "defaulted", sink.terminal)

	require.Len(t, sink.invalid, 2)
	for i, inv := range sink.invalid {
		require.Equal(t, "ghost-name", inv.outcome)
		require.Equal(t, i+1, inv.attempt)
		require.Contains(t, inv.issues, `outcome_not_allowed: outcome "ghost-name" is not in allowed_outcomes`)
	}

	// The exhaustion path returns the default outcome name directly, so the
	// mapping sees "default" as the original name.
	require.Len(t, sink.defaulted, 1)
	require.Equal(t, [3]string{"ship", "default", "default"}, sink.defaulted[0])
}

// TestRun_RepairExhaustionWithoutDefaultFailsTheRun pins the failure
// signature when the step declares no default outcome: the run fails after
// the retry budget with the wrapped pinned issues.
func TestRun_RepairExhaustionWithoutDefaultFailsTheRun(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "noinc"
  version       = "0.1"
  initial_state = "ship"
  target_state  = "done"
  policy {
    max_step_retries = 1
  }
}
adapter "fake" "default" {}
step "ship" {
  target = adapter.fake.default
  outcome "success" {
    schema = object({ reason = string })
    next   = state.done
  }
}
state "done" { terminal = true }`)
	plug := &adapterFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
		_ = rejection
		return adapter.Result{Outcome: "success"}, nil
	}}
	sink := &contractSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	require.Error(t, err, "the run must fail without a default outcome to map to")
	require.Len(t, sink.invalid, 2, "one event per rejected attempt (budget 1 + initial)")
	require.Contains(t, sink.invalid[0].issues, `payload_schema: property "reason": required property is missing`)
	require.GreaterOrEqual(t, len(sink.invalid), 2)
}

// TestRun_FallbackOutcomeFiresWhenAdapterNeverFinalizes pins the fallback
// lane: an adapter that produces NO result at all finalizes with the step's
// fallback outcome (engine synthesizes it: no payload, no comment) and never
// enters the repair loop.
func TestRun_FallbackOutcomeFiresWhenAdapterNeverFinalizes(t *testing.T) {
	var rejections []*criteriav2.ExecutionRejection
	sink, err := repairRun(t, []adapter.Result{{}}, &rejections, "policy {\n  max_step_retries = 2\n}")
	require.NoError(t, err)
	require.Equal(t, "failed", sink.terminal)

	require.Len(t, rejections, 1, "no repair retries: the fallback lane is not a rejection")
	require.Nil(t, rejections[0])
	require.Empty(t, sink.invalid)
	for _, o := range sink.outcome {
		if o.step == "ship" {
			require.Equal(t, "failure", o.outcome)
			require.Empty(t, o.comment)
			require.NoError(t, o.err)
		}
	}
}

// TestRun_RequireCommentRejectsEmptyCommentThenRepairs pins require_comment:
// an otherwise-valid success with an empty comment is host-rejected with the
// pinned missing_comment issue and feeds the same repair loop.
func TestRun_RequireCommentRejectsEmptyCommentThenRepairs(t *testing.T) {
	good := validAudit(1)
	good.Comment = ""
	var rejections []*criteriav2.ExecutionRejection
	sink, err := repairRun(t, []adapter.Result{
		good,
		validAudit(2),
	}, &rejections, "policy {\n  max_step_retries = 2\n}")
	require.NoError(t, err)
	require.Equal(t, "done", sink.terminal)

	require.Len(t, sink.invalid, 1)
	require.Contains(t, sink.invalid[0].issues, `missing_comment: outcome "success" requires a comment (require_comment)`)
}

// TestRun_LegacyStepsNeverTouchTheContractLoop pins the back-compat boundary
// at the RUN level: a workflow without any type blocks, schemas,
// require_comment, or fallback produces NO StepOutcomeInvalid events, NO
// ExecutionRejection on the wire, and keeps the classic unmapped-name →
// default-mapping behavior.
func TestRun_LegacyStepsNeverTouchTheContractLoop(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "legacy"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
adapter "fake" "default" {}
step "a" {
  target = adapter.fake.default
  outcome "again" { next = step.b }
}
step "b" {
  target = adapter.fake.default
  outcome "success" { next = state.failedlegacy }
  outcome "default" { next = state.done }
}
state "done" { terminal = true }
state "failedlegacy" {
  terminal = true
  success  = false
}`)
	var rejections []*criteriav2.ExecutionRejection
	plug := &adapterFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
		rejections = append(rejections, rejection)
		return adapter.Result{Outcome: "again"}, nil
	}}
	sink := &contractSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	require.NoError(t, NewTestEngine(g, loader, sink).Run(context.Background()))

	require.Empty(t, sink.invalid)
	for _, r := range rejections {
		require.Nil(t, r)
	}
	require.Len(t, sink.defaulted, 1)
	require.Equal(t, [3]string{"b", "again", "default"}, sink.defaulted[0])
}

// TestRun_RepairContextResetsOnNonRejectionFailure pins that a plain adapter
// error between two contract rejections resets the repair chain: the attempt
// after the transport error carries NO rejection.
func TestRun_RepairContextResetsOnNonRejectionFailure(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "reset"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
  policy {
    max_step_retries = 3
  }
}
adapter "fake" "default" {}
step "a" {
  target = adapter.fake.default
  outcome "success" {
    schema = object({ name = string })
    next   = state.done
  }
}
state "done" { terminal = true }`)
	var rejections []*criteriav2.ExecutionRejection
	calls := 0
	plug := &adapterFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
		calls++
		rejections = append(rejections, rejection)
		switch calls {
		case 1:
			return adapter.Result{Outcome: "success"}, nil // invalid payload → rejection
		case 2:
			return adapter.Result{}, errors.New("transport boom") // non-rejection failure
		default:
			return adapter.Result{Outcome: "success", Outputs: map[string]cty.Value{"name": cty.StringVal("ok")}}, nil
		}
	}}
	sink := &contractSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}
	require.NoError(t, NewTestEngine(g, loader, sink).Run(context.Background()))

	require.Len(t, rejections, 3)
	require.Nil(t, rejections[0], "attempt 1 has no rejection context")
	require.NotNil(t, rejections[1], "attempt 2 carries the payload rejection")
	require.Nil(t, rejections[2], "the attempt after the transport error resets the repair chain")
	require.Equal(t, "done", sink.terminal)
}

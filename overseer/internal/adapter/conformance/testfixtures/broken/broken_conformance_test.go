//go:build conformancefail

package broken

import (
	"context"
	"testing"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/overseer/internal/adapter/conformance"
	"github.com/brokenbots/overlord/workflow"
)

type badOutcomeAdapter struct{}

func (badOutcomeAdapter) Name() string { return "broken" }

func (badOutcomeAdapter) Execute(context.Context, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	// Deliberately invalid outcome so conformance outcome_domain sub-test fails.
	return adapter.Result{Outcome: "not-allowed"}, nil
}

func TestBrokenAdapterConformance(t *testing.T) {
	conformance.Run(
		t,
		"broken",
		func() adapter.Adapter { return badOutcomeAdapter{} },
		conformance.Options{
			StepConfig:      map[string]string{},
			AllowedOutcomes: []string{"success", "failure"},
		},
	)
}

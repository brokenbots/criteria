//go:build !copilot

package copilot_test

import (
	"errors"
	"testing"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/overseer/internal/adapter/conformance"
	"github.com/brokenbots/overlord/overseer/internal/adapters/copilot"
)

func TestCopilotStubConformance(t *testing.T) {
	conformance.Run(
		t,
		copilot.Name,
		func() adapter.Adapter { return copilot.New() },
		conformance.Options{
			StepConfig:      map[string]string{"prompt": "hello"},
			AllowedOutcomes: []string{"failure"},
			ExpectError: func(err error) bool {
				return errors.Is(err, copilot.ErrNotEnabled)
			},
		},
	)
}

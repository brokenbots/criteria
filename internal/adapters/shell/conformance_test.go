package shell_test

import (
	"testing"

	"github.com/brokenbots/overseer/internal/adapter"
	"github.com/brokenbots/overseer/internal/adapter/conformance"
	"github.com/brokenbots/overseer/internal/adapters/shell"
)

func TestShellConformance(t *testing.T) {
	conformance.Run(
		t,
		shell.Name,
		func() adapter.Adapter { return shell.New() },
		conformance.Options{
			StepConfig:      map[string]string{"command": "echo conformance"},
			AllowedOutcomes: []string{"success", "failure"},
			Streaming:       true,
		},
	)
}

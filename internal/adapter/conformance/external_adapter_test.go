package conformance_test

// external_adapter_test.go — runs the conformance suite against an adapter
// binary provided via the CRITERIA_CONFORMANCE_ADAPTER environment variable.
// Opt-in tool for ad-hoc validation of an arbitrary adapter binary; skips when
// the env var is unset. Not wired into CI — per ADR-0003 this repo's conformance
// is scoped to the host plus the imported Go SDK (see noop_adapter_test.go), and
// each other-language SDK owns its own conformance against the proto package.

import (
	"os"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestExternalAdapterConformance(t *testing.T) {
	bin := os.Getenv("CRITERIA_CONFORMANCE_ADAPTER")
	if bin == "" {
		t.Skip("CRITERIA_CONFORMANCE_ADAPTER not set")
	}
	name := os.Getenv("CRITERIA_CONFORMANCE_ADAPTER_NAME")
	if name == "" {
		name = "conformance-target"
	}

	// Default options exercise the broadest suite; individual
	// capabilities are gated by the adapter's advertised features.
	conformance.RunAdapter(t, name, bin, conformance.Options{
		StepConfig:          map[string]string{"delay_ms": "0"},
		AllowedOutcomes:     []string{"success"},
		Streaming:           true,
		ErrorInjection:      true,
		PermissionDenyPaths: true,
		ConcurrentStressN:   8,
		LifecycleOrder:      []string{"session_opened", "execute_started", "execute_finished", "session_closed"},
	})
}

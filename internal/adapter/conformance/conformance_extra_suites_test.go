package conformance_test

// conformance_extra_suites_test.go — enables the capability-gated v2 suites
// the baseline noop conformance run leaves skipped: the concurrent-session
// stress contract. The noop adapter is parallel_safe and never echoes step
// input into log output, so it satisfies the suite's isolation assertions.
// The error-injection and lifecycle-ordering suites are NOT enabled: no
// in-tree adapter implements partial_failure_after_events, and the ordering
// suite requires an adapter-declared event sequence.

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestNoopAdapterConcurrentStress(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	conformance.RunAdapter(
		t,
		"noop",
		adapterBin,
		conformance.Options{
			StepConfig:        map[string]string{"delay_ms": "0"},
			PermissionConfig:  map[string]string{"emit_permission_request": "true"},
			AllowedOutcomes:   []string{"success"},
			ConcurrentStressN: 4,
		},
	)
}

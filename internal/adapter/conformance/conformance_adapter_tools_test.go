package conformance_test

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

// TestAdapterToolsFailureMatrix drives the CRI-167 adapter-tools failure
// matrix (deny, unknown callee, callee crash, callee timeout,
// capability-missing caller) through the real engine. Host-side and
// unconditional: it is not gated on any adapter's declared capabilities, so
// it always runs in CI.
func TestAdapterToolsFailureMatrix(t *testing.T) {
	conformance.RunAdapterToolsFailureMatrix(t)
}

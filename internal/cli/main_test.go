package cli

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// IgnoreCurrent captures goroutines started before tests run (e.g. race
	// detector, test infrastructure) so they do not trigger false positives.
	// Engine+fake-harness tests use per-test goleak.VerifyNone(t) via
	// requireNoGoroutineLeak to assert that HTTP/2 transport goroutines are
	// cleaned up individually for each test.
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

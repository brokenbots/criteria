package engine

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// IgnoreCurrent captures any goroutines started by the Go runtime or
	// test infrastructure before our tests run (e.g. the race detector's own
	// goroutines). This avoids false-positive leak reports for goroutines that
	// exist before any test code executes.
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

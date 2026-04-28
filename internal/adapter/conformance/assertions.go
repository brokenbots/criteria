package conformance

// assertions.go — shared assertion and error-classification helpers used
// across multiple conformance test files.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// executeNoPanic runs target.Execute and fails the test if a panic occurs.
func executeNoPanic(t *testing.T, target executeTarget, ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (res adapter.Result, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("adapter %q panicked: %v", target.Name(), r)
		}
	}()
	return target.Execute(ctx, step, sink)
}

// assertValidOutcome fails t if outcome is empty or not in opts.AllowedOutcomes
// (when a set is configured).
func assertValidOutcome(t *testing.T, outcome string, opts Options) {
	t.Helper()
	if strings.TrimSpace(outcome) == "" {
		t.Fatal("empty outcome")
	}
	if len(opts.AllowedOutcomes) == 0 {
		return
	}
	for _, allowed := range opts.AllowedOutcomes {
		if allowed == outcome {
			return
		}
	}
	t.Fatalf("outcome %q not in allowed set %v", outcome, opts.AllowedOutcomes)
}

// isCancellationLikeError returns true for errors that look like context.Canceled.
func isCancellationLikeError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context canceled") || strings.Contains(msg, "deadline exceeded")
}

// isDeadlineLikeError returns true for errors that look like context.DeadlineExceeded.
func isDeadlineLikeError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadline exceeded") ||
		// gRPC surfaces deadline as DeadlineExceeded status code even when the
		// transport layer delivers it as RST_STREAM CANCEL.
		strings.Contains(s, "code = deadlineexceeded")
}

// hasCapability reports whether capability is in capabilities.
func hasCapability(capabilities []string, capability string) bool {
	for _, c := range capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// isPluginTarget reports whether target is a pluginSessionTarget.
func isPluginTarget(target executeTarget) bool {
	_, ok := target.(pluginSessionTarget)
	return ok
}

// newSessionID returns a unique session identifier with the given prefix.
func newSessionID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

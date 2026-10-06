// Package heartbeatutil provides a shared, transitional helper for keeping
// an adapter's Log RPC alive with periodic heartbeat events. It is used by
// in-tree adapter fixtures and by the MCP bridge until the Go SDK owns
// session-lifetime heartbeats itself (see PR #283 Follow-ups).
package heartbeatutil

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/tunables"
)

// LogEventSender is the minimal surface this helper needs from the adapter
// SDK's LogEventSender. Keeping the interface here lets the package stay free
// of the SDK import, which is required by the repo's import-lint rules for
// non-testfixture internal/ code.
type LogEventSender interface {
	Send(*v2.LogEvent) error
}

// RunLogHeartbeat blocks until ctx is canceled, emitting log-stream heartbeat
// events at the registered heartbeat cadence (tunables registry:
// CRITERIA_HEARTBEAT_INTERVAL, default 30s). The lenient override replaces
// the removed CRITERIA_TEST_HEARTBEAT_INTERVAL_MS test hatch (KB-132): an
// unset, malformed, or non-positive value keeps the built-in default, and a
// short override lets conformance tests prove liveness with a short stall
// threshold without waiting the full production interval. Adapter fixture
// subprocesses inherit the variable, so the conformance harness only sets it
// in the test process.
//
// Returning nil for host-initiated cancellation is not a contract violation.
func RunLogHeartbeat(ctx context.Context, sender LogEventSender) error {
	interval := tunables.FromEnv().HeartbeatInterval
	if interval <= 0 {
		interval = tunables.DefaultHeartbeatInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-ticker.C:
			if err := sender.Send(&v2.LogEvent{Heartbeat: &v2.Heartbeat{StreamName: "log", SentAt: timestamppb.New(t)}}); err != nil {
				return err
			}
		}
	}
}

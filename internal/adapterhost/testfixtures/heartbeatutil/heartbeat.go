// Package heartbeatutil provides a shared, transitional helper for keeping
// an adapter's Log RPC alive with periodic heartbeat events. It is used by
// in-tree adapter fixtures and by the MCP bridge until the Go SDK owns
// session-lifetime heartbeats itself (see PR #283 Follow-ups).
package heartbeatutil

import (
	"context"
	"os"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

// RunLogHeartbeat blocks until ctx is canceled, emitting log-stream heartbeat
// events at the protocol default cadence of 30 s. If the environment variable
// CRITERIA_TEST_HEARTBEAT_INTERVAL_MS is set to a positive integer, that
// millisecond interval is used instead so conformance tests can prove liveness
// with a short stall threshold without waiting the full production interval.
//
// Returning nil for host-initiated cancellation is not a contract violation.
func RunLogHeartbeat(ctx context.Context, sender adapterhost.LogEventSender) error {
	interval := 30 * time.Second
	if raw := os.Getenv("CRITERIA_TEST_HEARTBEAT_INTERVAL_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			interval = time.Duration(ms) * time.Millisecond
		}
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

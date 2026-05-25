package criteriav2

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// HeartbeatInterval is the cadence at which idle server-streams must emit
	// a Heartbeat message.  Two missed heartbeats (~60 s) triggers the host's
	// crash-recovery policy.
	HeartbeatInterval = 30 * time.Second
)

// HeartbeatSender is a function type that sends a Heartbeat on a stream.
// Callers implement this to bridge the generic ticker to their concrete stream
// send method.
type HeartbeatSender func(*Heartbeat) error

// RunHeartbeat runs a background heartbeat ticker for streamName, calling
// send every HeartbeatInterval until ctx is cancelled.
//
// It returns the first send error, or ctx.Err() if the context is cancelled
// before any send error occurs.  Callers should run this in a separate
// goroutine:
//
//	go func() {
//	    if err := criteriav2.RunHeartbeat(ctx, "execute", send); err != nil {
//	        // handle or log
//	    }
//	}()
func RunHeartbeat(ctx context.Context, streamName string, send HeartbeatSender) error {
	return RunHeartbeatWithInterval(ctx, streamName, send, HeartbeatInterval)
}

// RunHeartbeatWithInterval is the testable form of RunHeartbeat.  Production
// code should use RunHeartbeat (which fixes the interval to HeartbeatInterval).
func RunHeartbeatWithInterval(ctx context.Context, streamName string, send HeartbeatSender, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-ticker.C:
			hb := &Heartbeat{
				StreamName: streamName,
				SentAt:     timestamppb.New(t),
			}
			if err := send(hb); err != nil {
				return err
			}
		}
	}
}

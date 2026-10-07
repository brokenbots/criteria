package servertrans

// client_heartbeat.go — periodic heartbeat to the server.

import (
	"context"
	"time"

	connect "connectrpc.com/connect/v2"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// StartHeartbeat fires Heartbeat RPCs periodically until ctx is done.
func (c *Client) StartHeartbeat(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.heartbeat(ctx)
			}
		}
	}()
}

func (c *Client) heartbeat(ctx context.Context) {
	if c.criteriaID == "" {
		return
	}
	ctx, info := connect.NewClientContext(ctx)
	c.authorize(info.RequestHeader())
	if _, err := c.grpc.Heartbeat(ctx, &pb.HeartbeatRequest{CriteriaId: c.criteriaID}); err != nil {
		c.log.Warn("heartbeat failed", "error", err)
	}
}

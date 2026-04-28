package servertrans

// client_pending.go — pending-envelope tracking for reconnect replay.

import (
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func (c *Client) appendPending(env *pb.Envelope) {
	c.pendingMu.Lock()
	c.pending = append(c.pending, env)
	c.pendingMu.Unlock()
}

func (c *Client) snapshotPending() []*pb.Envelope {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	out := make([]*pb.Envelope, len(c.pending))
	copy(out, c.pending)
	return out
}

func (c *Client) clearPending(correlationID string) {
	if correlationID == "" {
		return
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for i, env := range c.pending {
		if env.CorrelationId == correlationID {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return
		}
	}
}

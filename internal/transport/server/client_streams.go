package servertrans

// client_streams.go — Control server-stream management and per-run publisher
// lifecycle. The actual SubmitEvents reconnect loop lives in run_publisher.go.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// NewRunPublisher creates and starts a SubmitEvents publisher for runID.
// The returned publisher is independent from the Client's default publisher
// and must be closed by the caller when the run is terminal. Long-lived agent
// mode uses this to execute sequential assignments over one control stream.
func (c *Client) NewRunPublisher(ctx context.Context, runID string) (*RunPublisher, error) {
	if c.criteriaID == "" {
		return nil, errors.New("not registered")
	}
	p := newRunPublisher(c, runID, c.opts.SendBuffer)
	p.Start(ctx)
	return p, nil
}

// StartPublishStream starts the SubmitEvents bidi stream for runID without
// starting the Control stream. Used by crash-recovery resumptions where the
// main client owns the Control subscription. This creates the Client's default
// publisher; only one default publisher may be active at a time.
func (c *Client) StartPublishStream(ctx context.Context, runID string) error {
	if c.criteriaID == "" {
		return errors.New("credentials not set")
	}
	return c.startDefaultPublish(ctx, runID)
}

// StartStreams attaches the Control server-stream (if not already) and starts
// the long-running SubmitEvents bidi for runID.
func (c *Client) StartStreams(ctx context.Context, runID string) error {
	if c.criteriaID == "" {
		return errors.New("not registered")
	}
	if err := c.StartControl(ctx); err != nil {
		return fmt.Errorf("control stream: %w", err)
	}
	return c.startDefaultPublish(ctx, runID)
}

func (c *Client) startDefaultPublish(ctx context.Context, runID string) error {
	c.defaultMu.Lock()
	defer c.defaultMu.Unlock()
	if c.defaultPublisher != nil {
		if c.defaultPublisher.IsStarted() {
			return errors.New("publish stream already started")
		}
		c.defaultPublisher.Start(ctx)
		return nil
	}
	p := newRunPublisher(c, runID, c.opts.SendBuffer)
	p.Start(ctx)
	c.defaultPublisher = p
	return nil
}

// Publish enqueues env on the default SubmitEvents stream. It is a shorthand
// for callers that use StartStreams/StartPublishStream and do not need agent
// mode's per-run publishers. For backward compatibility with callers that
// publish before starting the stream, the first publish creates a default
// publisher for the envelope's run id.
func (c *Client) Publish(ctx context.Context, env *pb.Envelope) {
	if env == nil {
		return
	}
	c.defaultMu.Lock()
	p := c.defaultPublisher
	if p == nil && env.RunId != "" {
		p = newRunPublisher(c, env.RunId, c.opts.SendBuffer)
		c.defaultPublisher = p
	}
	c.defaultMu.Unlock()
	if p == nil {
		c.log.Warn("publish dropped (no default publisher)")
		return
	}
	if env.RunId != "" && p.RunID() != env.RunId {
		c.log.Warn("publish dropped (run id mismatch)", "publisher_run_id", p.RunID(), "envelope_run_id", env.RunId)
		return
	}
	p.Publish(ctx, env)
}

// Drain blocks until the default publisher's pending envelopes are acknowledged.
func (c *Client) Drain(ctx context.Context) {
	c.defaultMu.Lock()
	p := c.defaultPublisher
	c.defaultMu.Unlock()
	if p == nil {
		return
	}
	p.Drain(ctx)
}

// controlMessageType returns a stable discriminator for the ControlMessage
// command oneof, used to log unrecognized or unset arms. Reflection-based so
// a future schema arm (unknown to this build) is still identified as unset.
func controlMessageType(msg *pb.ControlMessage) string {
	if msg == nil {
		return "nil"
	}
	m := msg.ProtoReflect()
	oneofs := m.Descriptor().Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		oo := oneofs.Get(i)
		if oo.Name() != "command" {
			continue
		}
		if fd := m.WhichOneof(oo); fd != nil {
			return string(fd.Name())
		}
		return "unset"
	}
	return "unknown"
}

func (c *Client) StartControl(ctx context.Context) error {
	if !c.controlStarted.CompareAndSwap(false, true) {
		return nil
	}
	ready := make(chan error, 1)
	go c.controlLoop(ctx, ready)
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("control stream: timed out waiting for ready")
	}
}

func (c *Client) controlLoop(ctx context.Context, ready chan<- error) { //nolint:funlen,gocognit,gocyclo // reconnect loop with backoff, ready signalling, and event dispatch across stream lifecycle
	backoff := 500 * time.Millisecond
	firstAttempt := true
	for {
		if ctx.Err() != nil {
			return
		}
		req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: c.criteriaID})
		c.authorize(req.Header())
		stream, err := c.grpc.Control(ctx, req)
		if err != nil {
			if firstAttempt {
				ready <- err
				return
			}
			c.log.Warn("control stream dial failed", "error", err)
			if !c.backoffSleep(ctx, &backoff) {
				return
			}
			continue
		}

		readySent := false
		for stream.Receive() {
			msg := stream.Msg()
			if msg.GetControlReady() != nil {
				if firstAttempt && !readySent {
					ready <- nil
					readySent = true
					firstAttempt = false
				}
				c.log.Debug("control stream attached")
				continue
			}
			// Dispatch: every ControlMessage oneof arm is either forwarded
			// onto its channel or logged — nothing falls through silently
			// (R1, ADR-0006 D1).
			if rc := msg.GetRunCancel(); rc != nil {
				if rc.RunId != "" {
					select {
					case c.runCancelCh <- rc.RunId:
					default:
						c.log.Warn("dropping run.cancel control message", "run_id", rc.RunId)
					}
				} else {
					c.log.Warn("ignoring run.cancel control message without run_id")
				}
			}
			if rr := msg.GetResumeRun(); rr != nil {
				if rr.RunId != "" {
					select {
					case c.resumeCh <- rr:
					default:
						c.log.Warn("dropping resume_run control message", "run_id", rr.RunId)
					}
				} else {
					c.log.Warn("ignoring resume_run control message without run_id")
				}
			}
			if wa := msg.GetWorkflowAssignment(); wa != nil {
				if wa.RunId != "" {
					select {
					case c.assignmentCh <- wa:
					default:
						c.log.Warn("dropping workflow assignment", "run_id", wa.RunId)
					}
				} else {
					c.log.Warn("ignoring workflow assignment without run_id")
				}
			}
			if ap := msg.GetAgentPrompt(); ap != nil {
				// Dispatch even when run_id is empty: the routing layer
				// records a prompt for an unknown run as a typed failure
				// (R2/R6) rather than dropping it here unobserved.
				select {
				case c.promptCh <- ap:
				default:
					c.log.Warn("dropping agent_prompt control message", "run_id", ap.GetRunId(), "step", ap.GetStep())
				}
			}
			if msg.GetRunCancel() == nil && msg.GetResumeRun() == nil && msg.GetWorkflowAssignment() == nil && msg.GetAgentPrompt() == nil {
				// Unset or unrecognized command (e.g. a newer server speaking
				// a future schema). Log instead of dropping silently.
				c.log.Warn("unhandled control message", "type", controlMessageType(msg))
			}
		}
		if firstAttempt && !readySent {
			ready <- fmt.Errorf("control stream closed before ready: %w", stream.Err())
			return
		}
		firstAttempt = false
		if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
			c.log.Warn("control stream closed", "error", err)
		}
		backoff = 500 * time.Millisecond
		if !c.backoffSleep(ctx, &backoff) {
			return
		}
	}
}

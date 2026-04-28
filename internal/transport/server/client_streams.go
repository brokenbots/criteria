package servertrans

// client_streams.go — SubmitEvents bidi stream and Control server-stream
// management: StartStreams, StartPublishStream, and their background loops.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// StartPublishStream starts the SubmitEvents bidi stream for runID without
// starting the Control stream. Used by crash-recovery resumptions where the
// main client owns the Control subscription.
func (c *Client) StartPublishStream(ctx context.Context, runID string) error {
	if c.criteriaID == "" {
		return errors.New("credentials not set")
	}
	return c.startPublish(ctx, runID)
}

// StartStreams attaches the Control server-stream (if not already) and starts
// the long-running SubmitEvents bidi for runID.
func (c *Client) StartStreams(ctx context.Context, runID string) error {
	if c.criteriaID == "" {
		return errors.New("not registered")
	}
	if err := c.startControl(ctx); err != nil {
		return fmt.Errorf("control stream: %w", err)
	}
	return c.startPublish(ctx, runID)
}

func (c *Client) startControl(ctx context.Context) error {
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

func (c *Client) controlLoop(ctx context.Context, ready chan<- error) {
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
			if rc := msg.GetRunCancel(); rc != nil && rc.RunId != "" {
				select {
				case c.runCancelCh <- rc.RunId:
				default:
					c.log.Warn("dropping run.cancel control message", "run_id", rc.RunId)
				}
			}
			if rr := msg.GetResumeRun(); rr != nil && rr.RunId != "" {
				select {
				case c.resumeCh <- rr:
				default:
					c.log.Warn("dropping resume_run control message", "run_id", rr.RunId)
				}
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

func (c *Client) startPublish(ctx context.Context, runID string) error {
	if !c.streamStarted.CompareAndSwap(false, true) {
		return errors.New("publish stream already started")
	}
	c.runID = runID
	go c.publishLoop(ctx)
	return nil
}

func (c *Client) publishLoop(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil || c.isClosed() {
			return
		}
		if err := c.runSubmitEvents(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			c.log.Warn("submit events stream ended", "run_id", c.runID, "error", err)
		}
		if ctx.Err() != nil || c.isClosed() {
			return
		}
		if !c.backoffSleep(ctx, &backoff) {
			return
		}
	}
}

// runSubmitEvents opens one SubmitEvents stream and runs until it errors.
func (c *Client) runSubmitEvents(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream := c.grpc.SubmitEvents(streamCtx)
	c.authorize(stream.RequestHeader())
	pendingSnap := c.snapshotPending()
	if lastAck := c.lastAckedSeq.Load(); lastAck > 0 {
		stream.RequestHeader().Set("since_seq", strconv.FormatUint(lastAck, 10))
	} else if len(pendingSnap) > 0 {
		// Even with lastAckedSeq == 0 we request replay so the server can tell
		// us about acks it persisted on a previous connection before our
		// ack reader saw them.
		stream.RequestHeader().Set("since_seq", "0")
	}

	recvErr := make(chan error, 1)
	go func() {
		err := c.recvAcks(stream)
		// When the receive side ends (EOF or error), unblock sendLoop so
		// runSubmitEvents can return promptly and publishLoop can reconnect.
		cancel()
		recvErr <- err
	}()

	// Resend any pending envelopes (sent on the prior stream but not yet
	// acknowledged), preserving submission order.
	for _, env := range pendingSnap {
		if err := stream.Send(env); err != nil {
			cancel()
			<-recvErr
			_ = stream.CloseRequest()
			return err
		}
	}

	sendErr := c.sendLoop(streamCtx, stream)
	_ = stream.CloseRequest()
	cancel()
	rerr := <-recvErr
	_ = stream.CloseResponse()
	if sendErr != nil && !errors.Is(sendErr, context.Canceled) {
		return sendErr
	}
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, context.Canceled) {
		return rerr
	}
	return nil
}

func (c *Client) sendLoop(ctx context.Context, stream *connect.BidiStreamForClient[pb.Envelope, pb.Ack]) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return nil
		case env, ok := <-c.sendCh:
			if !ok {
				return nil
			}
			c.appendPending(env)
			if err := stream.Send(env); err != nil {
				return err
			}
		}
	}
}

func (c *Client) recvAcks(stream *connect.BidiStreamForClient[pb.Envelope, pb.Ack]) error {
	for {
		ack, err := stream.Receive()
		if err != nil {
			return err
		}
		if ack.Seq > c.lastAckedSeq.Load() {
			c.lastAckedSeq.Store(ack.Seq)
		}
		c.clearPending(ack.CorrelationId)
	}
}

// Publish enqueues env on the SubmitEvents stream. It blocks (bounded by ctx
// and client shutdown) rather than dropping events silently. Publish always
// overwrites the envelope's correlation id with a per-envelope UUID so
// the server can deduplicate on (run_id, correlation_id) during reconnect replay.
// The timestamp is filled in if unset.
func (c *Client) Publish(ctx context.Context, env *pb.Envelope) {
	if env == nil {
		return
	}
	if env.Ts == nil || env.Ts.AsTime().IsZero() {
		env.Ts = timestamppb.New(time.Now().UTC())
	}
	if env.SchemaVersion == 0 {
		env.SchemaVersion = 1
	}
	// Per-envelope id is required for reconnect dedup; any caller-supplied
	// value (e.g. the sink's run-scoped UUID) is intentionally replaced.
	env.CorrelationId = uuid.NewString()
	select {
	case c.sendCh <- env:
	case <-ctx.Done():
		c.log.Warn("publish dropped (ctx done)")
	case <-c.closed:
		c.log.Warn("publish dropped (client closed)")
	}
}

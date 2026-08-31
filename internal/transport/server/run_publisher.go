package servertrans

// run_publisher.go — per-run SubmitEvents publisher. A Client may host one or
// more RunPublishers over its lifetime, each feeding a single run_id into the
// server via the Connect bidi SubmitEvents stream.

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// RunPublisher manages the SubmitEvents bidi stream for a single run.
type RunPublisher struct {
	client *Client
	runID  string

	sendCh       chan *pb.Envelope
	lastAckedSeq atomic.Uint64
	pendingMu    sync.Mutex
	pending      []*pb.Envelope

	// published and acked track how many envelopes have been enqueued and
	// how many have been acknowledged by the server. Drain waits until the two
	// counters match so that an event which has left sendCh but not yet been
	// added to pending (or whose ack is in flight) is not mistaken for an
	// empty publisher and dropped by an immediate Close.
	published atomic.Uint64
	acked     atomic.Uint64

	streamStarted atomic.Bool
	closeOnce     sync.Once
	closed        chan struct{}
}

// newRunPublisher allocates a publisher for runID. The caller must call start()
// to begin the background reconnect loop.
func newRunPublisher(c *Client, runID string, sendBuffer int) *RunPublisher {
	return &RunPublisher{
		client: c,
		runID:  runID,
		sendCh: make(chan *pb.Envelope, sendBuffer),
		closed: make(chan struct{}),
	}
}

// RunID returns the run id this publisher feeds.
func (p *RunPublisher) RunID() string { return p.runID }

// IsStarted reports whether the publisher's background reconnect loop has
// been started.
func (p *RunPublisher) IsStarted() bool { return p.streamStarted.Load() }

// Publish enqueues env on this publisher's SubmitEvents stream. It blocks
// (bounded by ctx and publisher shutdown) rather than dropping events silently.
// Publish always overwrites the envelope's correlation id with a per-envelope
// UUID so the server can deduplicate on (run_id, correlation_id) during
// reconnect replay. The timestamp and schema version are filled in if unset.
func (p *RunPublisher) Publish(ctx context.Context, env *pb.Envelope) {
	if env == nil {
		return
	}
	if env.Ts == nil || env.Ts.AsTime().IsZero() {
		env.Ts = timestamppb.New(time.Now().UTC())
	}
	if env.SchemaVersion == 0 {
		env.SchemaVersion = 1
	}
	env.CorrelationId = uuid.NewString()
	select {
	case p.sendCh <- env:
		p.published.Add(1)
	case <-ctx.Done():
	case <-p.closed:
	}
}

// Start begins the background SubmitEvents reconnect loop for this run.
func (p *RunPublisher) Start(ctx context.Context) {
	if !p.streamStarted.CompareAndSwap(false, true) {
		return
	}
	go p.publishLoop(ctx)
}

// Close signals the publisher to shut down. It is safe to call concurrently
// with Publish.
func (p *RunPublisher) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
	})
	return nil
}

// Drain blocks until every published envelope has been acknowledged, ctx is
// done, or the publisher is closed.
func (p *RunPublisher) Drain(ctx context.Context) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		// Wait until every published envelope has been acknowledged by the
		// server, not just until buffers appear empty. A fast Publish/Drain
		// sequence can otherwise observe an empty sendCh and pending while
		// an event is in transit between the two, causing Close to tear down
		// the stream before the server persists and acks it.
		if p.published.Load() == p.acked.Load() && len(p.snapshotPending()) == 0 && len(p.sendCh) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-p.closed:
			return
		case <-t.C:
		}
	}
}

func (p *RunPublisher) publishLoop(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil || p.isClosed() {
			return
		}
		if err := p.runSubmitEvents(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			p.client.log.Warn("submit events stream ended", "run_id", p.runID, "error", err)
		}
		if ctx.Err() != nil || p.isClosed() {
			return
		}
		if !p.client.backoffSleep(ctx, &backoff) {
			return
		}
	}
}

func (p *RunPublisher) runSubmitEvents(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream := p.client.grpc.SubmitEvents(streamCtx)
	p.client.authorize(stream.RequestHeader())
	pendingSnap := p.snapshotPending()
	if lastAck := p.lastAckedSeq.Load(); lastAck > 0 {
		stream.RequestHeader().Set("since_seq", strconv.FormatUint(lastAck, 10))
	} else if len(pendingSnap) > 0 {
		stream.RequestHeader().Set("since_seq", "0")
	}

	recvErr := make(chan error, 1)
	go func() {
		err := p.recvAcks(stream)
		cancel()
		recvErr <- err
	}()

	for _, env := range pendingSnap {
		if err := stream.Send(env); err != nil {
			cancel()
			<-recvErr
			_ = stream.CloseRequest()
			return err
		}
	}

	sendErr := p.sendLoop(streamCtx, stream)
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

func (p *RunPublisher) sendLoop(ctx context.Context, stream *connect.BidiStreamForClient[pb.Envelope, pb.Ack]) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.closed:
			return nil
		case env, ok := <-p.sendCh:
			if !ok {
				return nil
			}
			p.appendPending(env)
			if err := stream.Send(env); err != nil {
				return err
			}
		}
	}
}

func (p *RunPublisher) recvAcks(stream *connect.BidiStreamForClient[pb.Envelope, pb.Ack]) error {
	for {
		ack, err := stream.Receive()
		if err != nil {
			return err
		}
		if ack.Seq > p.lastAckedSeq.Load() {
			p.lastAckedSeq.Store(ack.Seq)
			p.acked.Add(1)
		}
		p.clearPending(ack.CorrelationId)
	}
}

func (p *RunPublisher) appendPending(env *pb.Envelope) {
	p.pendingMu.Lock()
	p.pending = append(p.pending, env)
	p.pendingMu.Unlock()
}

func (p *RunPublisher) snapshotPending() []*pb.Envelope {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if len(p.pending) == 0 {
		return nil
	}
	out := make([]*pb.Envelope, len(p.pending))
	copy(out, p.pending)
	return out
}

func (p *RunPublisher) clearPending(correlationID string) {
	if correlationID == "" {
		return
	}
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	for i, env := range p.pending {
		if env.CorrelationId == correlationID {
			p.pending = append(p.pending[:i], p.pending[i+1:]...)
			return
		}
	}
}

func (p *RunPublisher) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

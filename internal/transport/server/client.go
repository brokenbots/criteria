// Package servertrans implements the Criteria agent side of the server wire
// protocol. Since Phase 1.1 §6 the transport is Connect (bidi SubmitEvents
// stream + server-stream Control) replacing the Phase 0 REST + WebSocket
// implementation.
package servertrans

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// Codec selects the Connect codec.
type Codec string

const (
	CodecProto Codec = "proto"
	CodecJSON  Codec = "json"
)

// TLSMode selects transport security.
type TLSMode string

const (
	TLSDisable TLSMode = "disable"
	TLSEnable  TLSMode = "tls"
	TLSMutual  TLSMode = "mtls"
)

// Options configures a Client.
type Options struct {
	// Codec selects the Connect codec. Defaults to CodecProto.
	Codec Codec
	// TLSMode overrides the default TLS mode. When empty the mode is
	// inferred from the server URL scheme (http -> disable, https -> tls).
	TLSMode TLSMode
	// CAFile, CertFile, KeyFile configure TLS/mTLS. Paths are PEM.
	CAFile   string
	CertFile string
	KeyFile  string
	// SendBuffer is the size of the bounded channel between Publish() and
	// the SubmitEvents sender goroutine. Defaults to 64.
	SendBuffer int
}

// Client talks to a server via Connect.
type Client struct {
	baseURL *url.URL
	http    *http.Client
	grpc    criteriav1connect.CriteriaServiceClient
	log     *slog.Logger
	opts    Options

	criteriaID string
	token      string

	// publish stream state
	// sendCh is allocated in NewClient and is immutable for the client's
	// lifetime so concurrent Publish/sendLoop don't race on the field.
	runID         string
	sendCh        chan *pb.Envelope
	lastAckedSeq  atomic.Uint64
	pendingMu     sync.Mutex
	pending       []*pb.Envelope // ordered by send; matched on ack by correlation_id
	streamStarted atomic.Bool

	// control stream
	controlStarted atomic.Bool
	runCancelCh    chan string
	resumeCh       chan *pb.ResumeRun

	closeOnce sync.Once
	closed    chan struct{}
}

// NewClient builds a server Connect client.
func NewClient(serverURL string, log *slog.Logger, opts ...Options) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("server url must be http(s): %q", serverURL)
	}

	o := Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Codec == "" {
		o.Codec = CodecProto
	}
	if o.SendBuffer <= 0 {
		o.SendBuffer = 64
	}
	if o.TLSMode == "" {
		if u.Scheme == "https" {
			o.TLSMode = TLSEnable
		} else {
			o.TLSMode = TLSDisable
		}
	}

	httpClient, err := buildHTTPClient(u, o)
	if err != nil {
		return nil, err
	}

	var copts []connect.ClientOption
	if o.Codec == CodecJSON {
		copts = append(copts, connect.WithProtoJSON())
	}

	grpc := criteriav1connect.NewCriteriaServiceClient(httpClient, u.String(), copts...)

	return &Client{
		baseURL:     u,
		http:        httpClient,
		grpc:        grpc,
		log:         log,
		opts:        o,
		sendCh:      make(chan *pb.Envelope, o.SendBuffer),
		runCancelCh: make(chan string, 32),
		resumeCh:    make(chan *pb.ResumeRun, 32),
		closed:      make(chan struct{}),
	}, nil
}

func buildHTTPClient(u *url.URL, o Options) (*http.Client, error) {
	switch o.TLSMode {
	case TLSDisable:
		if u.Scheme == "https" {
			return nil, errors.New("tls=disable incompatible with https URL")
		}
		return &http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}, nil
	case TLSEnable, TLSMutual:
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if o.CAFile != "" {
			pemBytes, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read ca: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, errors.New("invalid ca bundle")
			}
			cfg.RootCAs = pool
		}
		if o.TLSMode == TLSMutual {
			if o.CertFile == "" || o.KeyFile == "" {
				return nil, errors.New("mtls requires --tls-cert and --tls-key")
			}
			crt, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("load client cert: %w", err)
			}
			cfg.Certificates = []tls.Certificate{crt}
		}
		tr := &http2.Transport{TLSClientConfig: cfg}
		return &http.Client{Transport: tr}, nil
	default:
		return nil, fmt.Errorf("unknown tls mode %q", o.TLSMode)
	}
}

// Register performs the unary Register RPC.
func (c *Client) Register(ctx context.Context, name, hostname, version string) error {
	req := connect.NewRequest(&pb.RegisterRequest{
		Name:   name,
		Labels: map[string]string{"hostname": hostname, "version": version},
	})
	resp, err := c.grpc.Register(ctx, req)
	if err != nil {
		return err
	}
	c.criteriaID = resp.Msg.CriteriaId
	c.token = resp.Msg.Token
	c.log.Info("registered with server", "criteria_id", c.criteriaID)
	return nil
}

// CriteriaID returns the server-assigned criteria id after Register.
func (c *Client) CriteriaID() string { return c.criteriaID }

// Token returns the auth token assigned during Register.
func (c *Client) Token() string { return c.token }

// SetCredentials configures a pre-existing criteria agent identity on the client
// so that crash-recovery resumptions can authenticate without re-registering.
// Must be called before StartStreams.
func (c *Client) SetCredentials(criteriaID, token string) {
	c.criteriaID = criteriaID
	c.token = token
}

// StartPublishStream starts the SubmitEvents bidi stream for runID without
// starting the Control stream. Used by crash-recovery resumptions where the
// main client owns the Control subscription.
func (c *Client) StartPublishStream(ctx context.Context, runID string) error {
	if c.criteriaID == "" {
		return errors.New("credentials not set")
	}
	return c.startPublish(ctx, runID)
}

// CreateRun registers a new run and returns its server-assigned id.
func (c *Client) CreateRun(ctx context.Context, workflowName, workflowHCL string) (string, error) {
	if c.criteriaID == "" {
		return "", errors.New("not registered")
	}
	req := connect.NewRequest(&pb.CreateRunRequest{
		CriteriaId:   c.criteriaID,
		WorkflowName: workflowName,
		WorkflowHash: workflowHCL,
	})
	c.authorize(req.Header())
	resp, err := c.grpc.CreateRun(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Msg.RunId, nil
}

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
	req := connect.NewRequest(&pb.HeartbeatRequest{CriteriaId: c.criteriaID})
	c.authorize(req.Header())
	if _, err := c.grpc.Heartbeat(ctx, req); err != nil {
		c.log.Warn("heartbeat failed", "error", err)
	}
}

// RunCancelCh returns the channel carrying run ids that the server has asked the
// agent to cancel via the Control server-stream.
func (c *Client) RunCancelCh() <-chan string { return c.runCancelCh }

// ResumeCh returns the channel carrying ResumeRun messages from the server (W05).
// The caller should drain this channel while a run is paused.
func (c *Client) ResumeCh() <-chan *pb.ResumeRun { return c.resumeCh }

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

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
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
		// runSubmitEvents can return promptly and publishLoop can
		// reconnect.
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
// The server can deduplicate on (run_id, correlation_id) during reconnect replay.
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

// ReattachRun queries the server about the state of a run that may have been
// in-flight before a crash. Returns the response or an error.
func (c *Client) ReattachRun(ctx context.Context, runID, criteriaID string) (*pb.ReattachRunResponse, error) {
	req := connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      runID,
		CriteriaId: criteriaID,
	})
	c.authorize(req.Header())
	resp, err := c.grpc.ReattachRun(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Resume calls the server Resume RPC to deliver a signal to a paused run (W05).
func (c *Client) Resume(ctx context.Context, runID, signal string, payload map[string]string) (*pb.ResumeResponse, error) {
	req := connect.NewRequest(&pb.ResumeRequest{
		RunId:   runID,
		Signal:  signal,
		Payload: payload,
	})
	c.authorize(req.Header())
	resp, err := c.grpc.Resume(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Drain blocks until every published envelope has been acknowledged, ctx is
// done, or the client is closed. It is intended for deterministic shutdown
// at the end of a run so trailing events aren't dropped.
func (c *Client) Drain(ctx context.Context) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if len(c.snapshotPending()) == 0 && len(c.sendCh) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
		}
	}
}

// Close stops the streams and releases resources. It is safe to call
// concurrently with Publish; Close signals shutdown via c.closed and never
// closes sendCh, so an in-flight Publish select unblocks cleanly.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return nil
}

// --- helpers -----------------------------------------------------------------

func (c *Client) authorize(h http.Header) {
	if c.token == "" {
		return
	}
	h.Set("Authorization", "Bearer "+c.token)
}

func (c *Client) backoffSleep(ctx context.Context, d *time.Duration) bool {
	cur := *d
	t := time.NewTimer(cur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-c.closed:
		return false
	case <-t.C:
	}
	*d = min(cur*2, 5*time.Second)
	return true
}

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

// --- event <-> proto mapping -------------------------------------------------

// Production code builds *pb.Envelope values directly via the shared events
// helpers; no legacy conversion layer remains here.

package peer

// Server is the peer's phone-home gRPC server (ADR-0007 Stage A, KB-15
// T-05). It dials the criteria host, writes the peer identity frame
// (role "peer"), and serves the FULL v2 AdapterService contract plus the
// PeerService supervision surface on the held connection. When the host
// drops the connection the child stays alive (CRITERIA_PEER_CHILD_KEEPALIVE,
// default true) and the server reconnects with a full-jitter backoff before
// re-dialing, re-handshaking, and re-serving; the Supervise stream replays
// from the host's since_event_seq so the host journal never loses an event.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"path/filepath"
	"time"

	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/version"
)

const (
	// DefaultHeartbeatInterval is the idle interval at which an open
	// Supervise stream emits a SupervisionHeartbeat.
	DefaultHeartbeatInterval = 30 * time.Second

	// peerDialTimeout bounds a single phone-home dial attempt.
	peerDialTimeout = 10 * time.Second
	// peerDialKeepAlive is the TCP keep-alive period for phone-home dials.
	peerDialKeepAlive = 15 * time.Second

	// peerShutdownBudget bounds the whole Serve shutdown sequence (session
	// closes, child grace, loader teardown) after the phone-home loop has
	// ended: bounded so a stalled child cannot hang the peer indefinitely.
	peerShutdownBudget = 30 * time.Second

	// peerServerStopGrace bounds grpc-go's server.Stop() in serveOnce. Stop
	// waits for raw conns still in the HTTP/2 preface handshake, which never
	// finishes when the host stalls after reading the identity frame — force
	// closing the conn unblocks that read so shutdown stays bounded.
	peerServerStopGrace = 2 * time.Second

	// peerHandshakeRole is the identity-frame role value that routes the
	// dial to the host shim's PeerAcceptor seam (ADR-0007 D4).
	peerHandshakeRole = "peer"
	// peerSDKProtocolVersion is the adapter wire protocol the peer serves.
	peerSDKProtocolVersion = 2
	// peerHandshakeFrameCap bounds the identity frame write: the frame is
	// the unauthenticated first write, so it is kept small (16 KiB).
	peerHandshakeFrameCap = 16384

	// peerServiceName is the fully-qualified PeerService name (the wire
	// contract lives in proto/criteria/v1/peer.proto; PeerService has no
	// generated grpc-go stubs, so the ServiceDesc is hand-rolled).
	peerServiceName     = "criteria.v1.PeerService"
	peerControlMethod   = "/" + peerServiceName + "/Control"
	peerSuperviseMethod = "/" + peerServiceName + "/Supervise"

	// peerSupervisionV1Capability advertises the PeerService supervision
	// surface; peerAdapterV2FullCapability advertises the full v2 adapter
	// contract served through the shared adapterhost bridge.
	peerAdapterV2FullCapability = "adapter.v2.full"
	peerSupervisionV1Capability = "supervision.v1"

	// peerLogChannel names the supervision channel StreamFlushed events
	// report (the host-side peer_session consumes the same constant).
	peerLogChannel = "log"
)

// peerNetworkTCP and peerNetworkUnix are the dial networks for a host:port
// address and a unix socket path respectively.
const (
	peerNetworkTCP  = "tcp"
	peerNetworkUnix = "unix"
)

// peerIdentityFrame is the single newline-terminated JSON line the peer
// writes immediately after the transport connection is established, before
// any gRPC traffic. The field order matches the documented frame shape; the
// host shim tolerates unknown fields, so newer peers stay compatible with
// older shims.
type peerIdentityFrame struct {
	Name               string                    `json:"name"`
	Version            string                    `json:"version"`
	Digest             string                    `json:"digest"`
	Token              string                    `json:"token"`
	Scope              string                    `json:"scope,omitempty"`
	SDKProtocolVersion int                       `json:"sdk_protocol_version"`
	Role               string                    `json:"role,omitempty"`
	Peer               *peerIdentityCapabilities `json:"peer,omitempty"`
}

// peerIdentityCapabilities is the `peer` block of the identity frame: peer
// process metadata for the host acceptor (identity verification itself is
// unchanged and happens on the shim side before the role branch).
type peerIdentityCapabilities struct {
	CriteriaVersion string   `json:"criteria_version,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// Server implements the phone-home loop. The child stays alive across host
// disconnects; every reconnect re-dials, re-handshakes (same token, scope,
// and digest), and re-serves both services.
type Server struct {
	cfg *Config
	rt  *peerRuntime
	log *slog.Logger

	// heartbeat is the idle interval for SupervisionHeartbeat emission.
	heartbeat time.Duration
	// keepaliveOpts are the gRPC server options serveOnce builds the
	// phone-home server with; nil falls back to the production remote
	// keepalive policy. Test seam: lets peer-path tests compress the
	// keepalive clock without weakening the production cadence.
	keepaliveOpts []grpc.ServerOption
	// dialFunc, childClient, rand, and sleep are test seams; NewServer
	// installs production defaults.
	dialFunc    func(ctx context.Context, network, addr string) (net.Conn, error)
	childClient func() (adapterhost.Client, bool)
	rand        func() float64
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewServer builds a phone-home server for the given resolved configuration
// and booted runtime. The server takes ownership of neither: Run drives the
// reconnect loop, shutdown is driven by ctx cancellation.
func NewServer(cfg *Config, rt *peerRuntime, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:       cfg,
		rt:        rt,
		log:       log,
		heartbeat: DefaultHeartbeatInterval,
	}
	s.dialFunc = s.dial
	s.childClient = s.defaultChildClient
	s.rand = rand.Float64
	s.sleep = sleepCtx
	return s
}

// defaultChildClient exposes the local adapter child as a raw v2 client for
// the phone-home bridge: in-memory (builtin) handles carry no client, so the
// peer serves only supervision for those.
func (s *Server) defaultChildClient() (adapterhost.Client, bool) {
	return adapterhost.ClientOf(s.rt.Child())
}

// Run drives the phone-home loop until ctx is done: serve, reconnect with a
// full-jitter backoff on connection loss, re-serve. The child stays alive
// across reconnects when child keepalive is enabled (the default); with it
// disabled the child is killed between attempts (legacy-runner parity: no
// child survives a host disconnect).
func (s *Server) Run(ctx context.Context) error {
	prev := s.cfg.BackoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.serveOnce(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !s.cfg.ChildKeepAlive {
			s.rt.killChild()
		}
		delay := s.nextBackoff(prev)
		s.log.Warn("peer phone-home connection lost; reconnecting",
			"host", s.cfg.Host,
			"scope", s.cfg.Scope,
			"error", err,
			"backoff", delay.String(),
		)
		if sleepErr := s.sleep(ctx, delay); sleepErr != nil {
			return ctx.Err()
		}
		prev = delay
	}
}

// Serve runs the phone-home loop until ctx is done (SIGTERM/SIGINT map to a
// cancelled ctx at the call site), then performs the peer shutdown sequence:
// stop accepting, close the sessions the peer served on the child, kill the
// child after the grace period, and journal the final exit fact. The
// shutdown runs on a bounded, non-inherited context (ctx is already done by
// then) so a stalled child cannot hang the peer past the budget. A
// context-caused end maps to a nil error so the process exits 0.
func (s *Server) Serve(ctx context.Context) error {
	err := s.Run(ctx)
	if err != nil && ctx.Err() != nil {
		err = nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), peerShutdownBudget)
	defer cancel()
	if serr := s.rt.Shutdown(shutdownCtx); serr != nil {
		s.log.Warn("peer shutdown", "error", serr)
	}
	return err
}

// serveOnce dials the host, writes the identity frame, and serves both
// services on the held connection. It returns when the connection drops or
// ctx is cancelled (server stopped).
func (s *Server) serveOnce(ctx context.Context) error {
	conn, err := s.dialFunc(ctx, s.network(), s.cfg.Host)
	if err != nil {
		return fmt.Errorf("dial %s: %w", s.cfg.Host, err)
	}
	if err := s.writeIdentityFrame(conn); err != nil {
		_ = conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	keepaliveOpts := s.keepaliveOpts
	if keepaliveOpts == nil {
		keepaliveOpts = adapterhost.RemoteKeepaliveServerOptions()
	}
	server := grpc.NewServer(keepaliveOpts...)
	if child, ok := s.childClient(); ok {
		wrapper := &serveChildClient{Client: child, rt: s.rt}
		s.rt.setServedChild(wrapper)
		adapterhost.RegisterAdapterService(server, wrapper)
	} else {
		s.log.Warn("peer child has no adapter client; serving supervision only",
			"adapter", s.cfg.AdapterName)
	}
	s.registerPeerService(server)

	wrapped := NewCloseSignalConn(conn)
	lis := NewSingleConnListener(wrapped)
	go func() {
		<-wrapped.Done()
		_ = lis.Close()
	}()
	go func() {
		<-ctx.Done()
		// Bounded stop: grpc-go's Stop blocks on raw conns stuck in the
		// preface handshake (a host that never speaks gRPC after the identity
		// frame), so bound it and force close the conn to unblock the read.
		stopDone := make(chan struct{})
		go func() {
			server.Stop()
			close(stopDone)
		}()
		timer := time.NewTimer(peerServerStopGrace)
		defer timer.Stop()
		select {
		case <-stopDone:
		case <-timer.C:
			_ = wrapped.Close()
			<-stopDone
		}
	}()

	s.log.Info("peer phone-home connected",
		"host", s.cfg.Host,
		"scope", s.cfg.Scope,
		"digest", s.cfg.Digest,
	)
	return server.Serve(lis)
}

// dial opens the transport: a context-cancellable TCP or unix dial (SIGTERM
// aborts an in-flight dial), wrapped in TLS for TCP hosts when configured.
// Unix sockets never carry TLS.
func (s *Server) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{
		Timeout:   peerDialTimeout,
		KeepAlive: peerDialKeepAlive,
	}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if s.cfg.TLS != nil && network == peerNetworkTCP {
		tlsConn := tls.Client(conn, s.cfg.TLS)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		return tlsConn, nil
	}
	return conn, nil
}

// network classifies the configured host as a unix socket path or a TCP
// host:port, mirroring the runner's dial classification.
func (s *Server) network() string {
	host := s.cfg.Host
	if filepath.IsAbs(host) || (host != "" && host[0] == '/') {
		return peerNetworkUnix
	}
	return peerNetworkTCP
}

// writeIdentityFrame marshals the peer identity frame and writes it as a
// single newline-terminated JSON line bounded to peerHandshakeFrameCap.
func (s *Server) writeIdentityFrame(conn net.Conn) error {
	line, err := s.identityFrame()
	if err != nil {
		return err
	}
	if _, err := conn.Write(line); err != nil {
		return fmt.Errorf("write identity frame: %w", err)
	}
	return nil
}

func (s *Server) identityFrame() ([]byte, error) {
	cfgVersion := s.cfg.AdapterVersion
	if cfgVersion == "" {
		cfgVersion = version.Version
	}
	data, err := json.Marshal(peerIdentityFrame{
		Name:               s.cfg.AdapterName,
		Version:            cfgVersion,
		Digest:             s.cfg.Digest,
		Token:              s.cfg.Token,
		Scope:              s.cfg.Scope,
		SDKProtocolVersion: peerSDKProtocolVersion,
		Role:               peerHandshakeRole,
		Peer: &peerIdentityCapabilities{
			CriteriaVersion: version.Version,
			Capabilities:    []string{peerAdapterV2FullCapability, peerSupervisionV1Capability},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal identity frame: %w", err)
	}
	line := make([]byte, 0, len(data)+1)
	line = append(line, data...)
	line = append(line, '\n')
	if len(line) > peerHandshakeFrameCap {
		return nil, fmt.Errorf("identity frame is %d bytes; exceeds the %d byte cap", len(line), peerHandshakeFrameCap)
	}
	return line, nil
}

// nextBackoff returns the full-jitter delay before the next reconnect
// attempt: a random duration in [BackoffMin, ceiling) where the ceiling
// doubles per attempt until BackoffMax. This matches the adapter SDKs'
// reconnect backoff and never produces the legacy runner's fixed lockstep
// delay.
func (s *Server) nextBackoff(prev time.Duration) time.Duration {
	ceiling := prev * 2
	if ceiling > s.cfg.BackoffMax {
		ceiling = s.cfg.BackoffMax
	}
	if ceiling <= s.cfg.BackoffMin {
		return s.cfg.BackoffMin
	}
	spread := float64(ceiling - s.cfg.BackoffMin)
	return s.cfg.BackoffMin + time.Duration(spread*s.rand())
}

// registerPeerService registers the hand-rolled PeerService ServiceDesc:
// a Control unary and a Supervise server stream. PeerService has no
// generated grpc-go bindings (the peer.proto bindings are connect-style
// only), so the descriptor is registered directly.
func (s *Server) registerPeerService(srv *grpc.Server) {
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: peerServiceName,
		HandlerType: (*peerServiceServer)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Control",
			Handler:    s.controlHandler,
		}},
		Streams: []grpc.StreamDesc{{
			StreamName:    "Supervise",
			ServerStreams: true,
			Handler:       s.superviseHandler,
		}},
		Metadata: "criteria/v1/peer.proto",
	}, s)
}

// peerServiceServer is the HandlerType marker for the hand-rolled
// PeerService descriptor, mirroring the host-side client's placeholder.
type peerServiceServer interface{}

// controlHandler adapts the generated-style unary handler signature to
// Server.Control.
func (s *Server) controlHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(criteriav1.ControlRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return s.Control(ctx, in), nil
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: peerControlMethod}
	return interceptor(ctx, in, info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return s.Control(ctx, req.(*criteriav1.ControlRequest)), nil
	})
}

// Control implements PeerService.Control: host-initiated child control,
// delegated to the runtime.
func (s *Server) Control(ctx context.Context, req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	return s.rt.Control(ctx, req)
}

// superviseHandler adapts the generated-style stream handler signature.
func (s *Server) superviseHandler(srv interface{}, stream grpc.ServerStream) error {
	req := new(criteriav1.SupervisionRequest)
	if err := stream.RecvMsg(req); err != nil {
		return err
	}
	return s.supervise(stream, req.GetSinceEventSeq())
}

// supervise streams the journal to the host: a replay of every event after
// since_event_seq, then live events as they are journaled, with a
// SupervisionHeartbeat emitted at the idle interval when nothing else flows.
// The cursor advances with each sent event, so a stream-level reset (host
// re-opens Supervise) replays exactly the unseen suffix, once.
func (s *Server) supervise(stream grpc.ServerStream, since uint64) error {
	journal := s.rt.Journal()
	cursor := since
	replay := func() error {
		for _, ev := range journal.Replay(cursor) {
			if err := stream.SendMsg(ev); err != nil {
				return err
			}
			cursor = ev.GetEventSeq()
		}
		return nil
	}

	// Replay first: everything the host has not yet seen.
	if err := replay(); err != nil {
		return err
	}

	heartbeat := time.NewTicker(s.heartbeatInterval())
	defer heartbeat.Stop()
	for {
		wake := journal.WaitFor(cursor)
		select {
		case <-stream.Context().Done():
			return nil
		case <-wake:
			if err := replay(); err != nil {
				return err
			}
			// Events just flowed; the idle heartbeat timer restarts.
			heartbeat.Reset(s.heartbeatInterval())
		case <-heartbeat.C:
			hb := &criteriav1.SupervisionEvent{
				Kind: &criteriav1.SupervisionEvent_Heartbeat{
					Heartbeat: &criteriav1.SupervisionHeartbeat{LastEventSeq: cursor},
				},
			}
			if err := stream.SendMsg(hb); err != nil {
				return err
			}
		}
	}
}

func (s *Server) heartbeatInterval() time.Duration {
	if s.heartbeat <= 0 {
		return DefaultHeartbeatInterval
	}
	return s.heartbeat
}

// sleepCtx sleeps for d or until ctx is done, so a SIGTERM aborts an
// in-flight backoff immediately.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// serveChildClient wraps the local adapter child's Client for the phone-home
// bridge. It tracks the sessions the host opens through this peer (closed on
// peer shutdown) and journals the StreamFlushed fact when the child's log
// stream ends (ADR-0007 supervision emission points).
type serveChildClient struct {
	adapterhost.Client
	rt *peerRuntime
}

func (c *serveChildClient) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	resp, err := c.Client.OpenSession(ctx, req)
	if err == nil {
		c.rt.sessionOpened(req.GetSessionId())
	}
	return resp, err
}

func (c *serveChildClient) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	resp, err := c.Client.CloseSession(ctx, req)
	if err == nil {
		c.rt.sessionClosed(req.GetSessionId())
	}
	return resp, err
}

// Log journals StreamFlushed{channel: "log", up_to_seq} when the child's log
// stream ends: everything the journal has recorded up to the current seq was
// delivered while the stream ran. A cancellation (the host closing the
// session) is a stream end too — the backlog drained up to that point.
func (c *serveChildClient) Log(ctx context.Context, req *v2.LogRequest, sink adapterhost.LogEventSink) error {
	err := c.Client.Log(ctx, req, sink)
	c.rt.journalFlushed(peerLogChannel, c.rt.Journal().LastSeq())
	return err
}

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
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/tunables"
	"github.com/brokenbots/criteria/workflow/version"
)

const (
	// DefaultHeartbeatInterval is the idle interval at which an open
	// Supervise stream emits a SupervisionHeartbeat. Single-sourced from the
	// tunables registry (KB-172), which also owns the adapter log-stream
	// heartbeat cadence.
	DefaultHeartbeatInterval = tunables.DefaultHeartbeatInterval

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
	// contract served through the shared adapterhost bridge. The
	// ADR-0008 child-run arms (CancelChildRun control +
	// ChildRunStarted/ChildRunTerminal supervision) are gated on the
	// workflow.v1 capability string: a peer whose identity frame does not
	// advertise it must reject Control(CancelChildRun) with a typed
	// unimplemented error (see proto/criteria/v1/peer.proto header).
	peerAdapterV2FullCapability = "adapter.v2.full"
	peerSupervisionV1Capability = "supervision.v1"
	peerWorkflowV1Capability    = "workflow.v1"

	// peerAdapterRouteHeader is the gRPC metadata header a multi-adapter
	// host tags each session's adapter calls with (KB-213): its value is the
	// adapter type the session belongs to, and the peer's connection mux
	// routes the call to that child. Must stay in sync with the
	// host-side constant in internal/adapter/environment/remote
	// (the two packages cannot import each other).
	peerAdapterRouteHeader = "x-criteria-adapter"

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
	// Adapters (KB-213) names the FULL child set this peer hosts — every
	// adapter of the environment, not just this connection's dial child.
	// Additive and absent for the legacy single-adapter shape; the host shim
	// verifies each advertised child digest against its own pin (accept-time
	// fail-closed check for a declared adapter missing from the child set).
	Adapters []peerAdapterIdentity `json:"adapters,omitempty"`
}

// peerAdapterIdentity is one hosted child in the identity frame's adapters
// list: the adapter type plus its version and (when pinned) digest.
type peerAdapterIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
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
	// installs production defaults. childClient resolves the served client
	// for one hosted adapter child by adapter type (KB-213 multi-adapter);
	// for the serve-adapter role the resolver ignores the name.
	dialFunc    func(ctx context.Context, network, addr string) (net.Conn, error)
	childClient func(name string) (adapterhost.Client, bool)
	rand        func() float64
	sleep       func(ctx context.Context, d time.Duration) error

	// capabilities is the negotiated capability set this peer announces in
	// its identity frame (ADR-0007 D4). The workflow.v1-scoped Control arm
	// is gated on it. Tests may narrow the list to simulate an older peer.
	capabilities []string

	// serve-adapter child role (ADR-0008): when the criteria process serves
	// a workflow as the adapter itself, it is built without a child runtime;
	// the in-process adapter registers directly on the phone-home server,
	// supervision streams from serveAdapterJournal, and RequestExit ends
	// the loop after a CloseSession teardown (the process exits; the phone
	// home does not reconnect).
	impl                adapterhost.Client
	serveAdapterJournal *EventJournal
	exitSignal          chan struct{}
	exitOnce            sync.Once
}

// NewServeAdapterServer for the serve-adapter child role lives in
// serveadapter.go, next to its child-role support types.

// NewServer builds a phone-home server for the given resolved configuration
// and booted runtime. The server takes ownership of neither: Run drives the
// reconnect loop, shutdown is driven by ctx cancellation.
func NewServer(cfg *Config, rt *peerRuntime, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg: cfg,
		rt:  rt,
		log: log,
		// One source for the heartbeat cadence (KB-172): the registered
		// heartbeat override applies here too, keeping the peer's Supervise
		// idle heartbeat on the same knob as the adapter log-stream
		// heartbeats.
		heartbeat: tunables.FromEnv().HeartbeatInterval,
		// Negotiated capability set advertised in the peer identity frame
		// (ADR-0007 D4); Control arms gated on workflow.v1 inspect this.
		capabilities: defaultPeerCapabilities(),
	}
	s.dialFunc = s.dial
	s.childClient = s.defaultChildClient
	s.rand = rand.Float64
	s.sleep = sleepCtx
	return s
}

// defaultChildClient exposes a hosted adapter child as a raw v2 client for
// the phone-home bridge, resolved by adapter type. In-memory (builtin)
// handles carry no client, so the peer serves only supervision for those.
func (s *Server) defaultChildClient(name string) (adapterhost.Client, bool) {
	if s.rt == nil {
		// Serve-adapter role: the in-process adapter for any name.
		if s.impl == nil {
			return nil, false
		}
		return s.impl, true
	}
	return s.rt.childClientFor(name)
}

// Run drives the phone-home loop until ctx is done: serve, reconnect with a
// full-jitter backoff on connection loss, re-serve. The child stays alive
// across reconnects when child keepalive is enabled (the default); with it
// disabled the child is killed between attempts (legacy-runner parity: no
// child survives a host disconnect). With CRITERIA_REMOTE_SCOPES_DIR set the
// hosted children are reached over one connection per (scope, adapter) token
// (scope-set dialing, KB-213): the Run loop becomes a supervisor that starts
// and stops one connection loop per scanned token, re-scanning at the
// heartbeat cadence so runner rotation (CRI-137/304) self-heals.
func (s *Server) Run(ctx context.Context) error {
	if s.rt != nil && s.cfg.ScopesDir != "" {
		return s.runScopeSet(ctx)
	}
	return s.runSingleConn(ctx)
}

// runSingleConn drives the single-connection phone-home loop for the legacy
// single-adapter shape, the run-wide multi-adapter shape (one connection
// carrying every adapter behind the dial child; routed per session by the
// host's x-criteria-adapter header), and the serve-adapter role.
func (s *Server) runSingleConn(ctx context.Context) error {
	spec, err := s.connProfile()
	if err != nil {
		return err
	}
	prev := s.cfg.BackoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.serveOnce(ctx, &spec)
		s.serveCountAdjust(&spec)
		select {
		case <-s.exitSignal:
			// Serve-adapter teardown (ADR-0008 child role): the host closed
			// the adapter session and the in-process adapter asked for
			// process exit; do not reconnect — the run record is durable and
			// the process ends here.
			return nil
		default:
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		delay := s.nextBackoff(prev)
		s.log.Warn("peer phone-home connection lost; reconnecting",
			"host", s.cfg.Host,
			"adapter", spec.name,
			"scope", spec.scope,
			"error", err,
			"backoff", delay.String(),
		)
		if sleepErr := s.sleep(ctx, delay); sleepErr != nil {
			return ctx.Err()
		}
		prev = delay
	}
}

// serveCountAdjust moves the loop's phone-home conn out of the runtime's
// per-child serve counts: with child keepalive disabled this is where a
// child whose last serving conn just ended is killed (no child survives a
// host disconnect).
func (s *Server) serveCountAdjust(spec *serveConnSpec) {
	if s.rt != nil {
		s.rt.serveClosed(spec.children)
	}
}

// runScopeSet supervises one phone-home connection loop per (scope, adapter)
// token under CRITERIA_REMOTE_SCOPES_DIR. The token set is the engine's
// per-instance dial manifest: runner rotation (CRI-137/304) adds new
// instance dirs and removes dead ones, so the supervisor re-scans at the
// heartbeat cadence and starts/stops loops to converge. A new token's loop
// dials immediately (the engine is waiting for its adapter); a vanished
// token's loop is stopped; its child is killed by the serve-count handoff
// when child keepalive is disabled.
func (s *Server) runScopeSet(ctx context.Context) error {
	var mu sync.Mutex
	loops := map[string]chan struct{}{}

	rescan := func() {
		specs, err := s.connSpecs()
		if err != nil {
			s.log.Error("peer scope scan failed", "dir", s.cfg.ScopesDir, "error", err)
		}
		want := make(map[string]serveConnSpec, len(specs))
		for _, spec := range specs {
			want[connKey(&spec)] = spec
		}
		mu.Lock()
		defer mu.Unlock()
		for key, spec := range want {
			if _, running := loops[key]; running {
				continue
			}
			stop := make(chan struct{})
			loops[key] = stop
			s.log.Info("peer scope conn starting", "adapter", spec.name, "scope", spec.scope)
			go s.loopScopeConn(ctx, &spec, stop)
		}
		for key := range loops {
			if _, wanted := want[key]; !wanted {
				close(loops[key])
				delete(loops, key)
				s.log.Info("peer scope conn stopped", "key", key)
			}
		}
	}

	rescan()
	ticker := time.NewTicker(s.heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			rescan()
		}
	}
}

// loopScopeConn is one (scope, adapter) connection loop: serve once, close
// the serve count (killing the conn's child when keepalive is disabled),
// then back off with full jitter before the next attempt.
func (s *Server) loopScopeConn(ctx context.Context, spec *serveConnSpec, stop <-chan struct{}) {
	prev := s.cfg.BackoffMin
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := s.serveOnce(ctx, spec)
		s.serveCountAdjust(spec)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		delay := s.nextBackoff(prev)
		s.log.Warn("peer phone-home connection lost; reconnecting",
			"host", s.cfg.Host,
			"adapter", spec.name,
			"scope", spec.scope,
			"error", err,
			"backoff", delay.String(),
		)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
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
	if s.rt == nil {
		// Serve-adapter role: there is no child runtime to shut down. The
		// CLI command owns the local teardown (cancel any in-flight child
		// run, close sessions); sessions served in-process are not visible
		// to the peer runtime. Run returns when ctx is done, so a host
		// disconnect followed by reconnects is the only path here.
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), peerShutdownBudget)
	defer cancel()
	if serr := s.rt.Shutdown(shutdownCtx); serr != nil {
		s.log.Warn("peer shutdown", "error", serr)
	}
	return err
}

// serveConnSpec is one phone-home connection profile: the identity the peer
// dials and handshakes with, plus which hosted children the connection
// routes adapter calls to.
type serveConnSpec struct {
	name    string // identity-frame dial adapter (the conn's primary child)
	version string
	digest  string
	scope   string
	token   string
	// children names the adapter types served through this conn: exactly one
	// entry (the dial child) in the legacy and per-scope shapes, or the full
	// hosted set for the run-wide multi-adapter shape (session calls routed
	// by the host's x-criteria-adapter header).
	children []string
}

// connKey is the dedup key for a per-scope connection (scope + adapter).
func connKey(spec *serveConnSpec) string {
	return spec.scope + "\x00" + spec.name
}

// connProfile resolves the connection set for the single-connection shapes:
// the run-wide multi-adapter manifest (one conn, all children) or the legacy
// single adapter. Exactly one conn for both.
func (s *Server) connProfile() (serveConnSpec, error) {
	specs, err := s.connSpecs()
	if err != nil {
		return serveConnSpec{}, err
	}
	if len(specs) != 1 {
		return serveConnSpec{}, fmt.Errorf("expected exactly one phone-home connection profile, got %d", len(specs))
	}
	return specs[0], nil
}

// connSpecs resolves the connection set for the configured peer shape
// (KB-213):
//   - CRITERIA_REMOTE_SCOPES_DIR set: one conn per (scope, adapter) token
//     file scanned from the dir; the identity frame dials with that token
//     and its adapter/scope. A scanned token for an adapter the peer does
//     not host is fail-closed: the conn is refused with a loud error, the
//     advertised child set on the other conns still fails the host's
//     declared-adapter check, and the engine's wait for it types out.
//   - multi-adapter manifest: one run-wide conn carrying the dial identity
//     of the first manifest child, able to route to every hosted child.
//   - legacy: one conn for the single configured adapter.
func (s *Server) connSpecs() ([]serveConnSpec, error) {
	cfg := s.cfg
	if cfg.ScopesDir != "" {
		return s.connSpecsForScopes()
	}
	return legacyOrManifestConnSpecs(cfg), nil
}

// connSpecsForScopes builds one per-(scope, adapter) conn spec from the
// scope-token scan; adapters the peer does not host are refused loudly and
// left out (their host wait types out instead of a silent miss).
func (s *Server) connSpecsForScopes() ([]serveConnSpec, error) {
	scopes, err := ScanRemoteScopes(s.cfg.ScopesDir)
	if err != nil {
		return nil, err
	}
	hosted, err := s.hostedSpecs()
	if err != nil {
		return nil, err
	}
	specs := make([]serveConnSpec, 0, len(scopes))
	for _, sc := range scopes {
		hs, ok := hosted[sc.Adapter]
		if !ok {
			s.log.Error("peer refuses phone-home conn for an adapter it does not host",
				"adapter", sc.Adapter,
				"scope", sc.Scope,
				"hosted", strings.Join(hostedNames(hosted), ","),
			)
			continue
		}
		specs = append(specs, serveConnSpec{
			name:     hs.Name,
			version:  hs.Version,
			digest:   hs.Digest,
			scope:    sc.Scope,
			token:    sc.Token,
			children: []string{hs.Name},
		})
	}
	return specs, nil
}

// legacyOrManifestConnSpecs collapses the manifest and legacy shapes into
// their conn set: a manifest dial is ONE run-wide conn carrying the first
// manifest child's identity, able to route to every hosted child; legacy
// keeps the single-adapter conn.
func legacyOrManifestConnSpecs(cfg *Config) []serveConnSpec {
	if cfg.ManifestMode() {
		children := make([]string, 0, len(cfg.Adapters))
		for _, spec := range cfg.Adapters {
			children = append(children, spec.Name)
		}
		first := cfg.Adapters[0]
		return []serveConnSpec{{
			name:     first.Name,
			version:  first.Version,
			digest:   first.Digest,
			scope:    cfg.Scope,
			token:    cfg.Token,
			children: children,
		}}
	}
	return []serveConnSpec{{
		name:     cfg.AdapterName,
		version:  cfg.AdapterVersion,
		digest:   cfg.Digest,
		scope:    cfg.Scope,
		token:    cfg.Token,
		children: []string{cfg.AdapterName},
	}}
}

// hostedSpecs maps the hosted adapter set by name from the resolved
// configuration (the manifest in multi-adapter mode, the legacy single
// adapter otherwise).
func (s *Server) hostedSpecs() (map[string]AdapterSpec, error) {
	if s.cfg.ManifestMode() {
		out := make(map[string]AdapterSpec, len(s.cfg.Adapters))
		for _, spec := range s.cfg.Adapters {
			out[spec.Name] = spec
		}
		return out, nil
	}
	if s.cfg.AdapterName == "" || s.cfg.Binary() == "" {
		return nil, fmt.Errorf("peer adapter identity unresolved: set CRITERIA_ADAPTER_NAME and CRITERIA_ADAPTER_BINARY")
	}
	return map[string]AdapterSpec{
		s.cfg.AdapterName: {
			Name:     s.cfg.AdapterName,
			Version:  s.cfg.AdapterVersion,
			Binary:   s.cfg.AdapterBinary,
			Digest:   s.cfg.Digest,
			Manifest: s.cfg.AdapterManifest,
		},
	}, nil
}

func hostedNames(hosted map[string]AdapterSpec) []string {
	names := make([]string, 0, len(hosted))
	for name := range hosted {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// newServedServer builds the phone-home gRPC server with both services
// registered: the adapter bridge (when the conn's children are live) and
// PeerService.
func (s *Server) newServedServer(spec *serveConnSpec) *grpc.Server {
	keepaliveOpts := s.keepaliveOpts
	if keepaliveOpts == nil {
		keepaliveOpts = adapterhost.RemoteKeepaliveServerOptions()
	}
	server := grpc.NewServer(keepaliveOpts...)
	if s.rt == nil {
		// Serve-adapter role: the adapter is implemented in this process
		// (no spawned child) and registers unwrapped.
		if child, ok := s.childClient(spec.name); ok {
			adapterhost.RegisterAdapterService(server, child)
		} else {
			s.log.Warn("peer child has no adapter client; serving supervision only",
				"adapter", spec.name)
		}
	} else {
		s.serveChildSet(server, spec)
	}
	s.registerPeerService(server)
	return server
}

// serveChildSet wraps and registers the adapter bridge for the conn's child
// set: one conn serving many adapters registers a routing mux over the
// per-child wrapped clients (session calls carry the x-criteria-adapter
// header); one conn serving one adapter registers that child's wrapper
// directly.
func (s *Server) serveChildSet(server *grpc.Server, spec *serveConnSpec) {
	if len(spec.children) > 1 {
		byName := make(map[string]adapterhost.Client, len(spec.children))
		for _, name := range spec.children {
			child, ok := s.childClient(name)
			if !ok {
				s.log.Warn("peer child has no adapter client; serving supervision only",
					"adapter", name)
				continue
			}
			wrapper := &serveChildClient{Client: child, rt: s.rt, name: name, scope: spec.scope}
			s.rt.setServedChild(name, wrapper)
			byName[name] = wrapper
		}
		if _, ok := byName[spec.name]; !ok {
			// The dial child itself has no client: supervision only.
			s.log.Warn("peer child has no adapter client; serving supervision only",
				"adapter", spec.name)
		}
		if len(byName) == 0 {
			// No hosted child has an adapter client: register nothing (an
			// empty routing mux would resolve nil targets on the first call).
			// Supervision still serves.
			s.log.Warn("peer hosts no adapter client; serving supervision only",
				"children", strings.Join(spec.children, ","))
			return
		}
		adapterhost.RegisterAdapterService(server, &connMux{byName: byName, fallback: spec.name})
		return
	}
	name := spec.children[0]
	child, ok := s.childClient(name)
	if !ok {
		s.log.Warn("peer child has no adapter client; serving supervision only",
			"adapter", name)
		return
	}
	wrapper := &serveChildClient{Client: child, rt: s.rt, name: name, scope: spec.scope}
	s.rt.setServedChild(name, wrapper)
	adapterhost.RegisterAdapterService(server, wrapper)
}

// supervisionEventMatches reports whether a journaled supervision event
// belongs to the filtered stream's adapter type (KB-213): an empty filter
// passes everything (the legacy single-child stream), a set filter passes
// only events attributed to that child.
func supervisionEventMatches(ev *criteriav1.SupervisionEvent, adapterType string) bool {
	if adapterType == "" {
		return true
	}
	return ev.GetAdapterType() == adapterType
}

// serveOnce dials the host, writes the identity frame, and serves both
// services on the held connection. It returns when the connection drops or
// ctx is cancelled (server stopped).
func (s *Server) serveOnce(ctx context.Context, spec *serveConnSpec) error {
	conn, err := s.dialFunc(ctx, s.network(), s.cfg.Host)
	if err != nil {
		return fmt.Errorf("dial %s: %w", s.cfg.Host, err)
	}
	if err := s.writeIdentityFrame(conn, spec); err != nil {
		_ = conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	if s.rt != nil {
		s.rt.serveOpened(spec.children)
	}

	server := s.newServedServer(spec)

	wrapped := NewCloseSignalConn(conn)
	lis := NewSingleConnListener(wrapped)
	go func() {
		<-wrapped.Done()
		_ = lis.Close()
	}()
	go s.watchConnTeardown(ctx, server, wrapped)

	s.log.Info("peer phone-home connected",
		"host", s.cfg.Host,
		"adapter", spec.name,
		"scope", spec.scope,
		"digest", spec.digest,
	)
	err = server.Serve(lis)
	// Serve-adapter teardown: RequestExit stopped the server — report the
	// intentional end as success so Serve->Run does not log a reconnect.
	if s.ExitRequested() {
		return nil
	}
	return err
}

// watchConnTeardown stops the served gRPC server and closes the conn once the
// run exits or the caller context ends. A RequestExit teardown drains with
// GracefulStop — it is armed by an in-flight RPC (CloseSession), whose
// response must reach the host before the transports close — while a
// context-end teardown stops immediately.
func (s *Server) watchConnTeardown(ctx context.Context, server *grpc.Server, wrapped *CloseSignalConn) {
	var graceful bool
	select {
	case <-ctx.Done():
	case <-s.exitSignal:
		// Serve-adapter teardown: stop serving this connection so the
		// phone-home loop unwinds without reconnecting. GracefulStop
		// drains pending RPCs before closing transports — the
		// CloseSession call that armed this signal is itself an in-flight
		// RPC, so its response reaches the host before the teardown.
		graceful = true
	}
	// Bounded stop: grpc-go's Stop blocks on raw conns stuck in the
	// preface handshake (a host that never speaks gRPC after the identity
	// frame), and GracefulStop blocks on long-lived streams the host may
	// hold open (Log tailing), so bound it and force close the conn to
	// unblock the stop.
	stopDone := make(chan struct{})
	go func() {
		if graceful {
			server.GracefulStop()
		} else {
			server.Stop()
		}
		close(stopDone)
	}()
	timer := time.NewTimer(peerServerStopGrace)
	defer timer.Stop()
	select {
	case <-stopDone:
		// Belt-and-braces: close unconditionally (idempotent) so the
		// close-signal watcher's lifetime is bounded by this cleanup,
		// independent of grpc-go's Stop closing behavior.
		_ = wrapped.Close()
	case <-timer.C:
		_ = wrapped.Close()
		<-stopDone
	}
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
func (s *Server) writeIdentityFrame(conn net.Conn, spec *serveConnSpec) error {
	line, err := s.identityFrame(spec)
	if err != nil {
		return err
	}
	if _, err := conn.Write(line); err != nil {
		return fmt.Errorf("write identity frame: %w", err)
	}
	return nil
}

func (s *Server) identityFrame(spec *serveConnSpec) ([]byte, error) {
	cfgVersion := spec.version
	if cfgVersion == "" {
		cfgVersion = version.Version
	}
	frame := peerIdentityFrame{
		Name:               spec.name,
		Version:            cfgVersion,
		Digest:             spec.digest,
		Token:              spec.token,
		Scope:              spec.scope,
		SDKProtocolVersion: peerSDKProtocolVersion,
		Role:               peerHandshakeRole,
		Peer: &peerIdentityCapabilities{
			CriteriaVersion: version.Version,
			Capabilities:    append([]string(nil), s.capabilities...),
		},
	}
	// KB-213: the multi-adapter manifest advertises the full child set on
	// every connection; the legacy single-adapter frame keeps its exact
	// legacy shape (no adapters key).
	if s.cfg.ManifestMode() {
		ids := make([]peerAdapterIdentity, 0, len(s.cfg.Adapters))
		for _, hosted := range s.cfg.Adapters {
			ids = append(ids, peerAdapterIdentity{Name: hosted.Name, Version: hosted.Version, Digest: hosted.Digest})
		}
		frame.Adapters = ids
	}
	data, err := json.Marshal(frame)
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

// defaultPeerCapabilities is the capability set a production peer
// advertises in its identity frame (ADR-0007 D4): the full v2 adapter
// bridge, the PeerService supervision surface, and the ADR-0008 child-run
// arms (workflow.v1).
func defaultPeerCapabilities() []string {
	return []string{peerAdapterV2FullCapability, peerSupervisionV1Capability, peerWorkflowV1Capability}
}

// negotiated reports whether the capability string was announced in this
// peer's identity frame — the negotiated capability set of the phone-home
// connection (ADR-0007 D4).
func (s *Server) negotiated(capability string) bool {
	for _, c := range s.capabilities {
		if c == capability {
			return true
		}
	}
	return false
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
		return s.Control(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: peerControlMethod}
	return interceptor(ctx, in, info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return s.Control(ctx, req.(*criteriav1.ControlRequest))
	})
}

// Control implements PeerService.Control: host-initiated child control,
// delegated to the runtime. CancelChildRun (ADR-0008) is capability-gated:
// when the negotiated capability set (this peer's identity frame) does not
// carry workflow.v1 the request fails with a typed unimplemented error
// instead of the accepted/detail shape — an old peer would have decoded the
// unknown arm into unknown fields and answered an indistinguishable
// accepted=false, so the gRPC status is the only way a caller can tell a
// protocol-level gap from a runtime rejection.
func (s *Server) Control(ctx context.Context, req *criteriav1.ControlRequest) (*criteriav1.ControlResponse, error) {
	if req.GetCancelChildRun() != nil && !s.negotiated(peerWorkflowV1Capability) {
		return nil, status.Errorf(codes.Unimplemented,
			"peer did not negotiate %q: CancelChildRun unavailable",
			peerWorkflowV1Capability)
	}
	if s.rt == nil {
		// Serve-adapter role: control verbs that target a spawned child
		// process or an out-of-band child run are answered here.
		return s.controlServeAdapter(req), nil
	}
	return s.rt.Control(ctx, req), nil
}

// superviseHandler adapts the generated-style stream handler signature.
func (s *Server) superviseHandler(srv interface{}, stream grpc.ServerStream) error {
	req := new(criteriav1.SupervisionRequest)
	if err := stream.RecvMsg(req); err != nil {
		return err
	}
	return s.supervise(stream, req.GetSinceEventSeq(), req.GetAdapterType())
}

// supervise streams the journal to the host: a replay of every event after
// since_event_seq, then live events as they are journaled, with a
// SupervisionHeartbeat emitted at the idle interval when nothing else flows.
// The cursor advances with each sent event, so a stream-level reset (host
// re-opens Supervise) replays exactly the unseen suffix, once.
//
// adapterType (KB-213) filters the stream to one hosted child's events: a
// per-session Supervise on a multi-adapter peer requests the child it
// serves, and the peer filters every journaled event by adapter type so N
// children's streams stay independent. An empty filter (the legacy single
// child and older hosts) passes every event. Heartbeats are stream-generated
// and always pass.
func (s *Server) supervise(stream grpc.ServerStream, since uint64, adapterType string) error {
	journal := s.journalFor()
	cursor := since
	replay := func() error {
		for _, ev := range journal.Replay(cursor) {
			seq := ev.GetEventSeq()
			if supervisionEventMatches(ev, adapterType) {
				if err := stream.SendMsg(ev); err != nil {
					return err
				}
			}
			// The cursor advances past filtered-out events too: this
			// stream's cursor is its own (per-child Supervise), so events
			// of other children must not be re-scanned on every wake.
			cursor = seq
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
		return tunables.DefaultHeartbeatInterval
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

// serveChildClient wraps one local adapter child's Client for the
// phone-home bridge. It tracks the sessions the host opens through this
// child (closed on peer shutdown) and journals the StreamFlushed fact when
// the child's log stream ends (ADR-0007 supervision emission points). The
// wrapping client carries the child's adapter name so the journal and the
// runtime's per-child session tables attribute the facts (KB-213).
type serveChildClient struct {
	adapterhost.Client
	rt *peerRuntime
	// name is the wrapped child's adapter type.
	name string
	// scope is the journal attribution scope for this bridge's flushed
	// facts: the conn's dial scope.
	scope string
}

func (c *serveChildClient) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	resp, err := c.Client.OpenSession(ctx, req)
	if err == nil {
		c.rt.sessionOpened(c.name, req.GetSessionId())
	}
	return resp, err
}

func (c *serveChildClient) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	resp, err := c.Client.CloseSession(ctx, req)
	if err == nil {
		c.rt.sessionClosed(c.name, req.GetSessionId())
	}
	return resp, err
}

// Log journals StreamFlushed{channel: "log", up_to_seq} when the child's log
// stream ends: everything the journal has recorded up to the current seq was
// delivered while the stream ran. A cancellation (the host closing the
// session) is a stream end too — the backlog drained up to that point.
func (c *serveChildClient) Log(ctx context.Context, req *v2.LogRequest, sink adapterhost.LogEventSink) error {
	err := c.Client.Log(ctx, req, sink)
	c.rt.journalFlushed(c.name, c.scope, peerLogChannel, c.rt.Journal().LastSeq())
	return err
}

// connMux routes one multi-adapter phone-home connection's adapter calls to
// the named child's wrapper (KB-213). The host tags each session's calls
// with the x-criteria-adapter metadata header; a call without that header
// (an older host, or an RPC without a session surface) lands on the
// connection's dial child.
type connMux struct {
	byName map[string]adapterhost.Client
	// fallback is the adapter type of the connection's dial child.
	fallback string
}

func (m *connMux) resolve(ctx context.Context) adapterhost.Client {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(peerAdapterRouteHeader); len(vals) == 1 {
			if client, hosted := m.byName[vals[0]]; hosted {
				return client
			}
		}
	}
	if client := m.byName[m.fallback]; client != nil {
		return client
	}
	// Defensive: an unhosted dial child must never produce a nil target —
	// route to the lexicographically-first hosted child. The host never
	// dispatches to an adapter the peer did not advertise, and the mux is
	// only registered when at least one child is hostable, so this path
	// exists to keep the resolve total.
	if len(m.byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.byName))
	for name := range m.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return m.byName[names[0]]
}

func (m *connMux) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return m.resolve(ctx).Info(ctx, req)
}

func (m *connMux) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return m.resolve(ctx).OpenSession(ctx, req)
}

func (m *connMux) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	return m.resolve(ctx).Execute(ctx, req, sink)
}

func (m *connMux) Log(ctx context.Context, req *v2.LogRequest, sink adapterhost.LogEventSink) error {
	return m.resolve(ctx).Log(ctx, req, sink)
}

func (m *connMux) Permissions(ctx context.Context, requests <-chan *v2.PermissionEvent) error {
	return m.resolve(ctx).Permissions(ctx, requests)
}

func (m *connMux) Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error) {
	return m.resolve(ctx).Pause(ctx, req)
}

func (m *connMux) Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return m.resolve(ctx).Resume(ctx, req)
}

func (m *connMux) Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return m.resolve(ctx).Snapshot(ctx, req)
}

func (m *connMux) Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return m.resolve(ctx).Restore(ctx, req)
}

func (m *connMux) Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error) {
	return m.resolve(ctx).Inspect(ctx, req)
}

func (m *connMux) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return m.resolve(ctx).CloseSession(ctx, req)
}

func (m *connMux) Prompt(ctx context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return m.resolve(ctx).Prompt(ctx, req)
}

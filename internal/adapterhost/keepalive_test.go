package adapterhost

// Keepalive policy tests for CRI-276: an idle remote adapter transport must
// never be closed by idleness alone.
//
// grpc-go clamps client keepalive intervals to a 10s floor
// (internal.KeepaliveMinPingTime) and server ping intervals to a 1s floor
// (internal.KeepaliveMinServerPingTime), so ping cadence cannot be compressed
// arbitrarily. The production liveness mechanism — the pod-side server
// pinging idle phone-home connections — runs at its 1s-floor minimum here,
// while an idle-closing TCP proxy stands in for the NAT/conntrack middlebox
// that silently drops connections with no traffic. The end-to-end chain
// (shim + UDS bridge + reattach client) is exercised in
// internal/engine/cri276_repro_test.go.

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

// idleClosingProxy forwards TCP between a local listener and a target,
// closing both directions whenever either side sends nothing for idleWindow.
// It reproduces the failure mechanism of CRI-276: connections carrying no
// traffic disappear.
type idleClosingProxy struct {
	ln         net.Listener
	target     string
	idleWindow time.Duration

	accepts atomic.Int64

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	done  bool
}

func newIdleClosingProxy(t *testing.T, target string, idleWindow time.Duration) *idleClosingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &idleClosingProxy{ln: ln, target: target, idleWindow: idleWindow, conns: map[net.Conn]struct{}{}}
	go p.acceptLoop()
	t.Cleanup(p.Close)
	return p
}

func (p *idleClosingProxy) acceptLoop() {
	for {
		downstream, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.accepts.Add(1)
		upstream, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = downstream.Close()
			continue
		}
		go func() {
			go p.pump(upstream, downstream)
			p.pump(downstream, upstream)
		}()
	}
}

func (p *idleClosingProxy) pump(dst, src net.Conn) {
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		_ = dst.Close()
		_ = src.Close()
		return
	}
	p.conns[dst] = struct{}{}
	p.conns[src] = struct{}{}
	p.mu.Unlock()

	defer func() { _ = dst.Close(); _ = src.Close() }()
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(p.idleWindow))
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (p *idleClosingProxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	_ = p.ln.Close()
	for c := range p.conns {
		_ = c.Close()
	}
}

// startKeepaliveTestServer starts a health gRPC server on 127.0.0.1 with the
// given server options and returns the dial address and an accept counter.
// A gRPC client transparently re-dials after its transport is closed, so the
// accept count distinguishes a surviving transport from a killed one.
func startKeepaliveTestServer(t *testing.T, opts []grpc.ServerOption) (addr string, accepts *atomic.Int64) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	counting := &countingListener{Listener: lis}
	srv := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(counting) }()
	t.Cleanup(srv.Stop)
	return counting.Addr().String(), &counting.accepts
}

// countingListener records how many connections were accepted.
type countingListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return conn, err
}

// dialKeepaliveTestClient dials addr with idle-timeout disabled (the shape
// KeepaliveDialOptionsFor always produces) plus the given keepalive
// parameters. Client ping intervals below grpc-go's 10s floor are clamped by
// the library, so a 10s interval here means "the client never pings during
// these tests".
func dialKeepaliveTestClient(t *testing.T, addr string, params keepalive.ClientParameters) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+addr, append(
		[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		KeepaliveDialOptionsFor(params)...)...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func checkHealth(t *testing.T, conn *grpc.ClientConn) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	return err
}

// TestKeepaliveServerPingsKeepIdleConnectionAlive proves the production
// liveness mechanism: the pod-side server pinging idle phone-home connections
// (ServerParameters.Time with no MaxConnectionIdle/MaxConnectionAge) keeps a
// connection alive across an idle-closing middlebox even while the client
// sends nothing, and the same transport is reused afterwards.
//
// grpc-go clamps ServerParameters.Time to a 1s floor
// (internal.KeepaliveMinServerPingTime), so the test runs the server at that
// floor with a proxy window just above it; the production policy's 1-minute
// interval sits far above the floor and behaves identically.
func TestKeepaliveServerPingsKeepIdleConnectionAlive(t *testing.T) {
	addr, accepts := startKeepaliveTestServer(t, KeepaliveServerOptionsFor(
		keepalive.ServerParameters{Time: time.Second, Timeout: 500 * time.Millisecond},
		keepalive.EnforcementPolicy{MinTime: 50 * time.Millisecond, PermitWithoutStream: true},
	))
	proxy := newIdleClosingProxy(t, addr, 1500*time.Millisecond)
	conn := dialKeepaliveTestClient(t, proxy.ln.Addr().String(), keepalive.ClientParameters{
		Time:                10 * time.Second, // library clamp floor: client never pings in-window
		Timeout:             time.Second,
		PermitWithoutStream: false,
	})

	if err := checkHealth(t, conn); err != nil {
		t.Fatalf("initial Check failed: %v", err)
	}

	// Fully idle: no RPCs, no client pings — only the server's keepalive
	// pings (~1s cadence) keep traffic flowing across the middlebox.
	time.Sleep(3500 * time.Millisecond)

	if err := checkHealth(t, conn); err != nil {
		t.Fatalf("Check after idle failed: the transport did not survive idleness: %v", err)
	}
	if got := accepts.Load(); got != 1 {
		t.Fatalf("server accepted %d connections, want 1: the original transport was closed and transparently re-dialed", got)
	}
}

// TestIdleClosingMiddleboxKillsConnectionWithoutKeepalives is the negative
// control for the test above: with no keepalives in either direction, the
// idle-closing middlebox drops the connection and the client must re-dial a
// second one. This proves the test above detects the failure mode the policy
// exists to prevent.
func TestIdleClosingMiddleboxKillsConnectionWithoutKeepalives(t *testing.T) {
	addr, accepts := startKeepaliveTestServer(t, nil)
	proxy := newIdleClosingProxy(t, addr, 1500*time.Millisecond)
	conn := dialKeepaliveTestClient(t, proxy.ln.Addr().String(), keepalive.ClientParameters{
		Time:                10 * time.Second,
		Timeout:             time.Second,
		PermitWithoutStream: false,
	})

	if err := checkHealth(t, conn); err != nil {
		t.Fatalf("initial Check failed: %v", err)
	}

	// Fully idle with zero keepalive traffic: the middlebox closes the
	// connection at ~1.5s.
	time.Sleep(3500 * time.Millisecond)

	// The Check may succeed via transparent re-dial (the proxy forwards), but
	// a second accept proves the original transport did not survive.
	checkErr := checkHealth(t, conn)
	if got := accepts.Load(); got < 2 && checkErr == nil {
		t.Fatalf("connection survived idleness without any keepalive traffic (accepts=%d); expected the middlebox to have closed it", got)
	}
}

// TestIdleTimeoutClosesTransportWhenEnabled pins the grpc-go default behavior
// the policy disables: with an idle timeout set, a connection with no RPCs is
// closed by idleness alone and the channel must re-dial for the next RPC.
// RemoteKeepaliveDialOptions always passes WithIdleTimeout(0), which grpc-go
// documents as disabling idleness entirely — the property these policy tests
// rely on.
func TestIdleTimeoutClosesTransportWhenEnabled(t *testing.T) {
	addr, accepts := startKeepaliveTestServer(t, nil)
	conn, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithIdleTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := checkHealth(t, conn); err != nil {
		t.Fatalf("initial Check failed: %v", err)
	}

	// Wait for the channel to go idle with no RPCs in flight.
	deadline := time.Now().Add(2 * time.Second)
	for conn.GetState() != connectivity.Idle && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if conn.GetState() != connectivity.Idle {
		t.Fatalf("channel never went idle with a 100ms idle timeout; state = %v", conn.GetState())
	}

	// The next RPC must re-dial: a second accept proves the transport was
	// closed by idleness alone.
	if err := checkHealth(t, conn); err != nil {
		t.Fatalf("Check after idle failed: %v", err)
	}
	if got := accepts.Load(); got < 2 {
		t.Fatalf("server accepted %d connections, want >= 2: the idle timeout did not close the first transport", got)
	}
}

// TestRemoteKeepalivePolicyContract pins the production cadence against the
// compatibility constraints that motivated it. grpc-go's default server
// enforcement rejects client pings faster than 5 minutes apart and forbids
// idle pings entirely; the host-side client cadence must therefore stay at
// or above that floor so a pod serving the SDK's default grpc.NewServer
// never GOAWAYs the connection for too_many_pings.
func TestRemoteKeepalivePolicyContract(t *testing.T) {
	const grpcDefaultMinPing = 5 * time.Minute

	if RemoteClientPingInterval < grpcDefaultMinPing {
		t.Errorf("RemoteClientPingInterval = %v, must be >= grpc-go default server MinTime %v to avoid too_many_pings against default servers", RemoteClientPingInterval, grpcDefaultMinPing)
	}
	if RemoteClientPingTimeout >= RemoteClientPingInterval {
		t.Errorf("RemoteClientPingTimeout = %v must be shorter than RemoteClientPingInterval = %v", RemoteClientPingTimeout, RemoteClientPingInterval)
	}
	if RemoteServerPingTimeout >= RemoteServerPingInterval {
		t.Errorf("RemoteServerPingTimeout = %v must be shorter than RemoteServerPingInterval = %v", RemoteServerPingTimeout, RemoteServerPingInterval)
	}
	if RemoteServerMinClientPing >= RemoteClientPingInterval {
		t.Errorf("RemoteServerMinClientPing = %v must be well below RemoteClientPingInterval = %v so the server tolerates the host client's cadence", RemoteServerMinClientPing, RemoteClientPingInterval)
	}
}
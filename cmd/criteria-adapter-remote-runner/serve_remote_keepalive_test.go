package main

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/test/bufconn"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// TestPhoneHomeServerKeepalivePolicy asserts the production keepalive policy
// (CRI-274): server-initiated pings stay enabled, and client-initiated
// without-stream pings are permitted at (or below) the engine reattach
// client's ping interval, so idle phone-home connections are kept warm
// instead of being killed for "too many pings".
func TestPhoneHomeServerKeepalivePolicy(t *testing.T) {
	if remoteServerKeepaliveParams.Time <= 0 {
		t.Errorf("server ping Time = %v, want > 0 so the transport stays warm while idle", remoteServerKeepaliveParams.Time)
	}
	if remoteServerKeepaliveParams.Timeout <= 0 {
		t.Errorf("server ping Timeout = %v, want > 0 so a dead peer is detected", remoteServerKeepaliveParams.Timeout)
	}
	if !remoteServerKeepaliveEnforcement.PermitWithoutStream {
		t.Fatal("enforcement PermitWithoutStream = false; the engine client pings without streams and would be disconnected after 30 min idle")
	}
	// The engine's reattach client pings every 30s without streams; the
	// server must tolerate that interval.
	const engineClientPingInterval = 30 * time.Second
	if remoteServerKeepaliveEnforcement.MinTime > engineClientPingInterval {
		t.Errorf("enforcement MinTime = %v exceeds the engine client ping interval %v; engine pings would trip too_many_pings",
			remoteServerKeepaliveEnforcement.MinTime, engineClientPingInterval)
	}
}

// TestPermitWithoutStreamPolicyKeepsIdlePingingClientAlive verifies the
// mechanism the production policy relies on (CRI-274): an idle client that
// pings without streams stays connected — single transport, no re-dials —
// under a PermitWithoutStream enforcement policy like the runner's
// phone-home server. The intervals are scaled down to keep the test fast;
// the production values (and both server strike paths) are asserted in
// TestPhoneHomeServerKeepalivePolicy and the adapterhost coherence test:
// without-stream pings are only struck when PermitWithoutStream is false,
// and with-stream pings when they arrive faster than MinTime. A behavioral
// demo of the default policy's kill is not practical here: grpc-go clamps
// client ping intervals to a 10s minimum and the kill needs 4 strikes, so
// the disconnect would not fire until ~40s.
func TestPermitWithoutStreamPolicyKeepsIdlePingingClientAlive(t *testing.T) {
	probingClient := func(t *testing.T, lis *bufconn.Listener) (*grpc.ClientConn, *atomic.Int32) {
		t.Helper()
		dials := &atomic.Int32{}
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				dials.Add(1)
				return lis.Dial()
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                time.Second,
				Timeout:             time.Second,
				PermitWithoutStream: true,
			}),
		)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn, dials
	}

	waitReady := func(t *testing.T, conn *grpc.ClientConn) {
		t.Helper()
		// Prime the lazy transport with a real RPC before observing state.
		client := v2.NewAdapterServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := client.Info(ctx, &v2.InfoRequest{}); err != nil {
			t.Fatalf("Info RPC to prime connection: %v", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for conn.GetState() != connectivity.Ready {
			if !conn.WaitForStateChange(ctx, conn.GetState()) {
				t.Fatalf("connection never became ready (state %v)", conn.GetState())
			}
		}
	}

	t.Run("permitWithoutStream policy keeps the connection", func(t *testing.T) {
		lis := bufconn.Listen(1024)
		// Same policy shape as the runner's phone-home server, scaled down.
		srv := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             500 * time.Millisecond,
			PermitWithoutStream: true,
		}))
		v2.RegisterAdapterServiceServer(srv, &fakeAdapterServer{})
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)

		conn, dials := probingClient(t, lis)
		waitReady(t, conn)
		// Two pings at 1s each, past the scaled MinTime: the connection must
		// stay up with a single transport after the idle-ping window.
		time.Sleep(2500 * time.Millisecond)
		if state := conn.GetState(); state != connectivity.Ready {
			t.Fatalf("idle pinging client was disconnected (state %v): without-stream pings must keep the connection alive", state)
		}
		if got := dials.Load(); got != 1 {
			t.Fatalf("transport was re-dialed %d times under the permitWithoutStream policy; want 1 (no disconnect)", got)
		}
	})
}

// TestPhoneHomeHealthReflectsAdapter verifies the health service served on
// the phone-home connection (CRI-274): a forwarded health check probes the
// local adapter, reporting SERVING while the adapter answers and NOT_SERVING
// when it does not — so the engine's liveness view reflects the adapter
// process, not the transport.
func TestPhoneHomeHealthReflectsAdapter(t *testing.T) {
	lis := bufconn.Listen(1024)
	server := newPhoneHomeServer(&proxyService{client: newFakeClient(t, &fakeAdapterServer{})})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	health := grpc_health_v1.NewHealthClient(conn)
	resp, err := health.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: "plugin"})
	if err != nil {
		t.Fatalf("health Check: %v", err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING while the adapter answers", resp.GetStatus())
	}
}

// TestAdapterHealthServerNotServingWhenAdapterFails verifies the NOT_SERVING
// arm of the health probe: when the adapter cannot answer Info, a forwarded
// health check must report NOT_SERVING.
func TestAdapterHealthServerNotServingWhenAdapterFails(t *testing.T) {
	// An adapter client pointed at a closed bufconn endpoint.
	dead := bufconn.Listen(1)
	_ = dead.Close()
	deadConn, err := grpc.NewClient("passthrough:///dead",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return dead.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = deadConn.Close() })
	brokenProxy := &proxyService{client: v2.NewAdapterServiceClient(deadConn)}

	lis := bufconn.Listen(1024)
	server := newPhoneHomeServer(brokenProxy)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	health := grpc_health_v1.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := health.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check: %v", err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Errorf("status = %v, want NOT_SERVING when the adapter does not answer", resp.GetStatus())
	}
}
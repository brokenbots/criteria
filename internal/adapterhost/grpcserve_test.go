package adapterhost

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// fakeBridgeClient is an in-memory Client backing the shared bridge; every
// method records its request so the round-trip tests can assert the full
// 12-method AdapterService surface survives a serve-back.
type fakeBridgeClient struct {
	mu             sync.Mutex
	infoCalled     int
	openSessionIDs []string
	closedSessions []string
	paused         []string
	resumed        []string
	snapshotted    []string
	restored       []string
	inspected      []string
	executeReq     *v2.ExecuteRequest
	logSessions    []string
	permEvents     []*v2.PermissionEvent
	prompt         *PromptRequest
}

func (f *fakeBridgeClient) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalled++
	return &v2.InfoResponse{Name: "bridgefake", Version: "2.0.0"}, nil
}

func (f *fakeBridgeClient) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openSessionIDs = append(f.openSessionIDs, req.GetSessionId())
	return &v2.OpenSessionResponse{}, nil
}

func (f *fakeBridgeClient) Execute(ctx context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error {
	f.mu.Lock()
	f.executeReq = req
	f.mu.Unlock()
	for _, ev := range []*v2.ExecuteEvent{
		{Event: &v2.ExecuteEvent_Tool{Tool: &v2.ToolInvocation{ToolName: "working"}}},
		{Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}}},
	} {
		if err := sink.Emit(ev); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBridgeClient) Log(ctx context.Context, req *v2.LogRequest, sink LogEventSink) error {
	f.mu.Lock()
	f.logSessions = append(f.logSessions, req.GetSessionId())
	f.mu.Unlock()
	if err := sink.Emit(&v2.LogEvent{Line: []byte("log line")}); err != nil {
		return err
	}
	// Mirror the production adapter: hold the Log stream open until the host
	// cancels the session-scoped context.
	<-ctx.Done()
	return nil
}

func (f *fakeBridgeClient) Permissions(ctx context.Context, requests <-chan *v2.PermissionEvent) error {
	for ev := range requests {
		f.mu.Lock()
		f.permEvents = append(f.permEvents, ev)
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeBridgeClient) Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, req.GetSessionId())
	return &v2.PauseResponse{}, nil
}

func (f *fakeBridgeClient) Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, req.GetSessionId())
	return &v2.ResumeResponse{}, nil
}

func (f *fakeBridgeClient) Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotted = append(f.snapshotted, req.GetSessionId())
	return &v2.SnapshotResponse{State: []byte("state")}, nil
}

func (f *fakeBridgeClient) Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored = append(f.restored, req.GetSessionId())
	return &v2.RestoreResponse{}, nil
}

func (f *fakeBridgeClient) Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected = append(f.inspected, req.GetSessionId())
	return &v2.InspectResponse{}, nil
}

func (f *fakeBridgeClient) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedSessions = append(f.closedSessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func (f *fakeBridgeClient) Prompt(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompt = req
	return &PromptResponse{Accepted: true, Detail: "delivered"}, nil
}

func newBridgeConn(t *testing.T, impl Client) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024)
	srv := grpc.NewServer()
	RegisterAdapterService(srv, impl)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Logf("server returned: %v", err)
			}
		default:
		}
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestAdapterServiceBridge_UnaryRPCs round-trips every unary RPC on the v2
// AdapterService surface through the shared bridge over a served connection.
func TestAdapterServiceBridge_UnaryRPCs(t *testing.T) {
	impl := &fakeBridgeClient{}
	client := NewClientForConn(newBridgeConn(t, impl))
	ctx := context.Background()

	t.Run("Info", func(t *testing.T) {
		resp, err := client.Info(ctx, &v2.InfoRequest{})
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if resp.GetName() != "bridgefake" || resp.GetVersion() != "2.0.0" {
			t.Errorf("Info = %q/%q, want bridgefake/2.0.0", resp.GetName(), resp.GetVersion())
		}
		if impl.infoCalled != 1 {
			t.Errorf("Info called %d times, want 1", impl.infoCalled)
		}
	})

	t.Run("OpenSession", func(t *testing.T) {
		if _, err := client.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: "s-open"}); err != nil {
			t.Fatalf("OpenSession: %v", err)
		}
		if got := impl.openSessionIDs; len(got) != 1 || got[0] != "s-open" {
			t.Errorf("OpenSession ids = %v, want [s-open]", got)
		}
	})

	t.Run("CloseSession", func(t *testing.T) {
		if _, err := client.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "s-close"}); err != nil {
			t.Fatalf("CloseSession: %v", err)
		}
		if got := impl.closedSessions; len(got) != 1 || got[0] != "s-close" {
			t.Errorf("CloseSession ids = %v, want [s-close]", got)
		}
	})

	t.Run("Pause", func(t *testing.T) {
		if _, err := client.Pause(ctx, &v2.PauseRequest{SessionId: "s-pause"}); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if got := impl.paused; len(got) != 1 || got[0] != "s-pause" {
			t.Errorf("paused = %v, want [s-pause]", got)
		}
	})

	t.Run("Resume", func(t *testing.T) {
		if _, err := client.Resume(ctx, &v2.ResumeRequest{SessionId: "s-resume"}); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if got := impl.resumed; len(got) != 1 || got[0] != "s-resume" {
			t.Errorf("resumed = %v, want [s-resume]", got)
		}
	})

	t.Run("Snapshot", func(t *testing.T) {
		resp, err := client.Snapshot(ctx, &v2.SnapshotRequest{SessionId: "s-snap"})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if string(resp.GetState()) != "state" {
			t.Errorf("state = %q, want state", resp.GetState())
		}
		if got := impl.snapshotted; len(got) != 1 || got[0] != "s-snap" {
			t.Errorf("snapshotted = %v, want [s-snap]", got)
		}
	})

	t.Run("Restore", func(t *testing.T) {
		if _, err := client.Restore(ctx, &v2.RestoreRequest{SessionId: "s-restore", State: []byte("state")}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if got := impl.restored; len(got) != 1 || got[0] != "s-restore" {
			t.Errorf("restored = %v, want [s-restore]", got)
		}
	})

	t.Run("Inspect", func(t *testing.T) {
		if _, err := client.Inspect(ctx, &v2.InspectRequest{SessionId: "s-inspect"}); err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if got := impl.inspected; len(got) != 1 || got[0] != "s-inspect" {
			t.Errorf("inspected = %v, want [s-inspect]", got)
		}
	})

	t.Run("Prompt", func(t *testing.T) {
		resp, err := client.Prompt(ctx, &PromptRequest{SessionID: "s-prompt", Prompt: "stop"})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if !resp.Accepted || resp.Detail != "delivered" {
			t.Errorf("Prompt response = %+v, want accepted/delivered", resp)
		}
		impl.mu.Lock()
		got := impl.prompt
		impl.mu.Unlock()
		if got == nil || got.SessionID != "s-prompt" || got.Prompt != "stop" {
			t.Errorf("impl prompt = %+v, want session s-prompt/stop", got)
		}
	})
}

// TestAdapterServiceBridge_ExecuteStream verifies the server-streaming
// Execute RPC relays every backend event to the host stream.
func TestAdapterServiceBridge_ExecuteStream(t *testing.T) {
	impl := &fakeBridgeClient{}
	client := NewClientForConn(newBridgeConn(t, impl))

	var got []*v2.ExecuteEvent
	err := client.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "s-exec"}, sinkFn(func(ev *v2.ExecuteEvent) error {
		got = append(got, ev)
		return nil
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].GetTool().GetToolName() != "working" || got[1].GetResult().GetOutcome() != "success" {
		t.Errorf("unexpected events: %+v", got)
	}
	impl.mu.Lock()
	req := impl.executeReq
	impl.mu.Unlock()
	if req == nil || req.GetSessionId() != "s-exec" {
		t.Errorf("backend got req %+v, want session s-exec", req)
	}
}

type sinkFn func(*v2.ExecuteEvent) error

func (f sinkFn) Emit(ev *v2.ExecuteEvent) error { return f(ev) }

// TestAdapterServiceBridge_LogStreamHoldsUntilCancel verifies the Log bridge
// emits backend events and keeps the RPC open (session-scoped contract) until
// the host cancels, instead of returning after the first backend drain.
func TestAdapterServiceBridge_LogStreamHoldsUntilCancel(t *testing.T) {
	impl := &fakeBridgeClient{}
	client := NewClientForConn(newBridgeConn(t, impl))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []*v2.LogEvent
	callErr := make(chan error, 1)
	go func() {
		callErr <- client.Log(ctx, &v2.LogRequest{SessionId: "s-log"}, logSinkFn(func(ev *v2.LogEvent) error {
			got = append(got, ev)
			return nil
		}))
	}()

	// The single backend event must arrive while the RPC stays open.
	waitFor(t, func() bool { return len(got) == 1 }, "log event")
	cancel()
	if err := <-callErr; err == nil {
		t.Error("Log returned nil after host cancellation; want stream-close error")
	}
	impl.mu.Lock()
	sessions := impl.logSessions
	impl.mu.Unlock()
	if len(sessions) != 1 || sessions[0] != "s-log" {
		t.Errorf("backend log sessions = %v, want [s-log]", sessions)
	}
}

type logSinkFn func(*v2.LogEvent) error

func (f logSinkFn) Emit(ev *v2.LogEvent) error { return f(ev) }

// TestAdapterServiceBridge_PermissionsStream verifies the bidi Permissions
// bridge pumps host events into the backend requests channel until the host
// half-closes, and that decisions ACKs (which the host discards) need no
// forwarding.
func TestAdapterServiceBridge_PermissionsStream(t *testing.T) {
	impl := &fakeBridgeClient{}
	client := NewClientForConn(newBridgeConn(t, impl))
	ctx := context.Background()

	requests := make(chan *v2.PermissionEvent, 2)
	requests <- &v2.PermissionEvent{Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r1"}}}
	requests <- &v2.PermissionEvent{Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r2"}}}
	close(requests)

	if err := client.Permissions(ctx, requests); err != nil {
		t.Fatalf("Permissions: %v", err)
	}
	impl.mu.Lock()
	got := impl.permEvents
	impl.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("backend got %d permission events, want 2", len(got))
	}
	if got[0].GetRequest().GetRequestId() != "r1" || got[1].GetRequest().GetRequestId() != "r2" {
		t.Errorf("unexpected permission events: %+v", got)
	}
}

// TestAdapterServiceDescWithPrompt verifies the extended ServiceDesc carries
// the Prompt method without mutating the generated descriptor, and that the
// bridge satisfies both generated server and PromptService interfaces.
func TestAdapterServiceDescWithPrompt(t *testing.T) {
	desc := AdapterServiceDescWithPrompt()
	names := make(map[string]bool, len(desc.Methods))
	for _, m := range desc.Methods {
		names[m.MethodName] = true
	}
	if !names["Prompt"] {
		t.Error("extended desc is missing the Prompt method")
	}
	if len(v2.AdapterService_ServiceDesc.Methods)+1 != len(desc.Methods) {
		t.Errorf("desc has %d methods, want generated %d + 1", len(desc.Methods), len(v2.AdapterService_ServiceDesc.Methods))
	}
	for _, m := range v2.AdapterService_ServiceDesc.Methods {
		if !names[m.MethodName] {
			t.Errorf("generated method %s missing from extended desc", m.MethodName)
		}
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

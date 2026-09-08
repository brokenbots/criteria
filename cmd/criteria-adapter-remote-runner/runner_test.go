package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

// compile-time check that proxyService satisfies the SDK Service interface.
var _ adapterhost.Service = (*proxyService)(nil)

type fakeAdapterServer struct {
	v2.UnimplementedAdapterServiceServer
	infoReq       *v2.InfoRequest
	openReq       *v2.OpenSessionRequest
	closeReq      *v2.CloseSessionRequest
	executeInputs map[string]string
	logSessionID  string
	permEvents    []*v2.PermissionEvent
}

func (f *fakeAdapterServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	f.infoReq = req
	return &v2.InfoResponse{Name: "fake", Version: "1.0.0"}, nil
}

func (f *fakeAdapterServer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	f.openReq = req
	return &v2.OpenSessionResponse{}, nil
}

func (f *fakeAdapterServer) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	f.closeReq = req
	return &v2.CloseSessionResponse{}, nil
}

func (f *fakeAdapterServer) Execute(req *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	f.executeInputs = req.GetInput()
	return stream.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success", OutputsJson: []byte(`{"greeting":"hello"}`)},
		},
	})
}

func (f *fakeAdapterServer) Log(req *v2.LogRequest, stream v2.AdapterService_LogServer) error {
	f.logSessionID = req.GetSessionId()
	return stream.Send(&v2.LogEvent{Line: []byte("log line")})
}

func (f *fakeAdapterServer) Permissions(stream v2.AdapterService_PermissionsServer) error {
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		f.permEvents = append(f.permEvents, ev)
		req := ev.GetRequest()
		if req != nil {
			if err := stream.Send(&v2.PermissionDecision{RequestId: req.GetRequestId(), Decision: "allow"}); err != nil {
				return err
			}
		}
	}
}

func newFakeClient(t *testing.T, srv v2.AdapterServiceServer) v2.AdapterServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024)
	s := grpc.NewServer()
	v2.RegisterAdapterServiceServer(s, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(lis) }()
	t.Cleanup(func() {
		s.Stop()
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
	return v2.NewAdapterServiceClient(conn)
}

func TestProxyServiceInfo(t *testing.T) {
	fake := &fakeAdapterServer{}
	proxy := &proxyService{client: newFakeClient(t, fake)}

	resp, err := proxy.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if resp.GetName() != "fake" {
		t.Errorf("name = %q, want fake", resp.GetName())
	}
}

func TestProxyServiceOpenCloseSession(t *testing.T) {
	fake := &fakeAdapterServer{}
	proxy := &proxyService{client: newFakeClient(t, fake)}

	if _, err := proxy.OpenSession(context.Background(), &v2.OpenSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if fake.openReq.GetSessionId() != "s1" {
		t.Errorf("open session id = %q, want s1", fake.openReq.GetSessionId())
	}

	if _, err := proxy.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if fake.closeReq.GetSessionId() != "s1" {
		t.Errorf("close session id = %q, want s1", fake.closeReq.GetSessionId())
	}
}

func TestProxyServiceExecute(t *testing.T) {
	fake := &fakeAdapterServer{}
	proxy := &proxyService{client: newFakeClient(t, fake)}

	sink := &collectExecuteSink{}
	req := &v2.ExecuteRequest{SessionId: "s1", Input: map[string]string{"name": "world"}}
	if err := proxy.Execute(context.Background(), req, sink); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.executeInputs["name"] != "world" {
		t.Errorf("adapter did not receive input: %v", fake.executeInputs)
	}
	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
	if sink.events[0].GetResult().GetOutcome() != "success" {
		t.Errorf("outcome = %q, want success", sink.events[0].GetResult().GetOutcome())
	}
}

type collectExecuteSink struct {
	events []*v2.ExecuteEvent
}

func (c *collectExecuteSink) Send(ev *v2.ExecuteEvent) error {
	c.events = append(c.events, ev)
	return nil
}

func TestProxyServiceLog(t *testing.T) {
	fake := &fakeAdapterServer{}
	proxy := &proxyService{client: newFakeClient(t, fake)}

	sink := &collectLogSink{}
	if err := proxy.Log(context.Background(), &v2.LogRequest{SessionId: "s1"}, sink); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if fake.logSessionID != "s1" {
		t.Errorf("log session id = %q, want s1", fake.logSessionID)
	}
	if len(sink.events) != 1 || string(sink.events[0].GetLine()) != "log line" {
		t.Fatalf("unexpected log events: %v", sink.events)
	}
}

type collectLogSink struct {
	events []*v2.LogEvent
}

func (c *collectLogSink) Send(ev *v2.LogEvent) error {
	c.events = append(c.events, ev)
	return nil
}

func TestProxyServicePermissions(t *testing.T) {
	fake := &fakeAdapterServer{}
	proxy := &proxyService{client: newFakeClient(t, fake)}

	reqs := make(chan *v2.PermissionEvent, 2)
	reqs <- &v2.PermissionEvent{Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r1"}}}
	reqs <- &v2.PermissionEvent{Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r2"}}}
	close(reqs)

	stream := &fakePermissionsStream{recvCh: reqs}
	if err := proxy.Permissions(context.Background(), stream); err != nil {
		t.Fatalf("Permissions: %v", err)
	}
	if len(fake.permEvents) != 2 {
		t.Errorf("got %d permission events, want 2", len(fake.permEvents))
	}
	if len(stream.decisions) != 2 {
		t.Fatalf("got %d decisions, want 2", len(stream.decisions))
	}
	if stream.decisions[0].GetDecision() != "allow" {
		t.Errorf("decision = %q, want allow", stream.decisions[0].GetDecision())
	}
}

type fakePermissionsStream struct {
	recvCh    <-chan *v2.PermissionEvent
	decisions []*v2.PermissionDecision
	ctx       context.Context
}

func (f *fakePermissionsStream) Recv() (*v2.PermissionEvent, error) {
	v, ok := <-f.recvCh
	if !ok {
		return nil, io.EOF
	}
	return v, nil
}

func (f *fakePermissionsStream) Send(d *v2.PermissionDecision) error {
	f.decisions = append(f.decisions, d)
	return nil
}

func (f *fakePermissionsStream) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

func TestResolveFromManifest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "adapter.yaml")
	data := `schema_version: 1
name: demo
version: 0.2.0
source_url: https://example.com
`
	if err := os.WriteFile(manifestPath, []byte(data), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := remoteConfig{Manifest: manifestPath, Host: "host:7778"}
	if err := cfg.resolve(slogDiscard()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Name != "demo" {
		t.Errorf("name = %q, want demo", cfg.Name)
	}
	if cfg.Version != "0.2.0" {
		t.Errorf("version = %q, want 0.2.0", cfg.Version)
	}
	if cfg.Binary != "/usr/local/bin/criteria-adapter-demo" {
		t.Errorf("binary = %q, want /usr/local/bin/criteria-adapter-demo", cfg.Binary)
	}
}

func TestNameFromBinary(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/usr/local/bin/criteria-adapter-shell", "shell"},
		{"criteria-adapter-copilot", "copilot"},
		{"/foo/bar", "bar"},
	}
	for _, c := range cases {
		if got := nameFromBinary(c.in); got != c.want {
			t.Errorf("nameFromBinary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

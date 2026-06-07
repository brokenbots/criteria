package adapterhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// permissionEmittingClient is a fake adapter client that emits
// permission.request adapter events during Execute, then a result.
type permissionEmittingClient struct {
	permissionsErr error
	permissionsCh  chan *v2.PermissionEvent
}

func (c *permissionEmittingClient) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "perm-emit"}, nil
}
func (c *permissionEmittingClient) OpenSession(_ context.Context, _ *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}
func (c *permissionEmittingClient) Execute(_ context.Context, _ *v2.ExecuteRequest, sink ExecuteEventSink) error {
	payload, _ := structpb.NewStruct(map[string]any{
		"request_id": "req-1",
		"tool":       "read_file",
	})
	_ = sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: "permission.request", Payload: payload},
		},
	})
	return sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	})
}
func (c *permissionEmittingClient) Log(_ context.Context, _ *v2.LogRequest, _ LogEventSink) error {
	return nil
}
func (c *permissionEmittingClient) Permissions(_ context.Context, ch <-chan *v2.PermissionEvent) error {
	if c.permissionsErr != nil {
		// A real adapter that returns an error does not drain the channel.
		return c.permissionsErr
	}
	for ev := range ch {
		if c.permissionsCh != nil {
			c.permissionsCh <- ev
		}
	}
	return nil
}
func (c *permissionEmittingClient) Pause(_ context.Context, _ *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}
func (c *permissionEmittingClient) Resume(_ context.Context, _ *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}
func (c *permissionEmittingClient) Snapshot(_ context.Context, _ *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (c *permissionEmittingClient) Restore(_ context.Context, _ *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}
func (c *permissionEmittingClient) Inspect(_ context.Context, _ *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (c *permissionEmittingClient) CloseSession(_ context.Context, _ *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

// TestExecuteWithFallbackStream_UnimplementedPermissionsIsOptOut verifies that
// when the adapter returns Unimplemented from Permissions, Execute still
// succeeds and evaluates permission requests locally.
func TestExecuteWithFallbackStream_UnimplementedPermissionsIsOptOut(t *testing.T) {
	client := &permissionEmittingClient{permissionsErr: status.Error(codes.Unimplemented, "not implemented")}
	handle := &rpcHandle{name: "perm-emit", rpc: client}

	step := &workflow.StepNode{
		Name:       "task",
		AllowTools: []string{"read_file"},
		Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
	}

	sink := &adapterEventCollector{}
	result, err := handle.Execute(context.Background(), "sess-1", step, sink)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("outcome=%q want success", result.Outcome)
	}
	if !sink.saw("permission.granted") {
		t.Fatal("expected permission.granted event")
	}
}

// TestExecuteWithFallbackStream_BrokenPermissionsStreamSurfacesError verifies
// that a non-Unimplemented error from the Permissions stream causes Execute to
// return an error.
func TestExecuteWithFallbackStream_BrokenPermissionsStreamSurfacesError(t *testing.T) {
	client := &permissionEmittingClient{permissionsErr: errors.New("stream broken")}
	handle := &rpcHandle{name: "perm-emit", rpc: client}

	step := &workflow.StepNode{
		Name:       "task",
		AllowTools: []string{"read_file"},
		Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
	}

	sink := &adapterEventCollector{}
	_, err := handle.Execute(context.Background(), "sess-1", step, sink)
	if err == nil {
		t.Fatal("expected error from broken permissions stream")
	}
	if !sink.saw("permission.granted") {
		t.Fatal("expected local permission.granted before stream failure")
	}
}

// TestExecuteWithActiveStream_PermActiveForwardsToInterceptSink verifies that
// when permActive is true, permission.request events are forwarded to the
// upstream sink instead of being evaluated locally.
func TestExecuteWithActiveStream_PermActiveForwardsToInterceptSink(t *testing.T) {
	client := &permissionEmittingClient{}
	handle := &rpcHandle{name: "perm-emit", rpc: client}

	// Mark the session as having an active permission stream.
	handle.permActive = map[string]bool{"sess-active": true}

	step := &workflow.StepNode{
		Name:       "task",
		AllowTools: []string{"read_file"}, // Would match if evaluated locally
		Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
	}

	sink := &adapterEventCollector{}
	result, err := handle.Execute(context.Background(), "sess-active", step, sink)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("outcome=%q want success", result.Outcome)
	}

	// Because requests==nil in executeCaptureSink, the event should be forwarded
	// to the upstream sink as-is, NOT evaluated locally (no permission.granted).
	if sink.saw("permission.granted") {
		t.Fatal("expected permission.request to be forwarded, not evaluated locally")
	}
	data, ok := sink.first("permission.request")
	if !ok {
		t.Fatal("expected permission.request to be forwarded to upstream sink")
	}
	if data["tool"] != "read_file" {
		t.Fatalf("tool=%q want read_file", data["tool"])
	}
}

// TestStartPermissionStream_PermActiveTracking verifies that StartPermissionStream
// registers the session in permActive and cleans it up on stream exit.
func TestStartPermissionStream_PermActiveTracking(t *testing.T) {
	client := &permissionEmittingClient{}
	handle := &rpcHandle{name: "perm-emit", rpc: client}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *v2.PermissionEvent)
	streamCancel, err := handle.StartPermissionStream(ctx, "sess-track", ch)
	if err != nil {
		t.Fatalf("StartPermissionStream: %v", err)
	}

	handle.permMu.Lock()
	active := handle.permActive["sess-track"]
	handle.permMu.Unlock()
	if !active {
		t.Fatal("expected permActive[sess-track] to be true")
	}

	// Cancel the stream and wait for cleanup.
	streamCancel()
	// Closing the channel lets the goroutine exit.
	close(ch)

	// Poll for cleanup.
	for i := 0; i < 50; i++ {
		handle.permMu.Lock()
		active = handle.permActive["sess-track"]
		handle.permMu.Unlock()
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if active {
		t.Fatal("expected permActive[sess-track] to be cleaned up")
	}
}

// TestStartPermissionStream_UnimplementedDrains verifies that when the adapter
// returns Unimplemented, the cleanup goroutine drains the requests channel.
func TestStartPermissionStream_UnimplementedDrains(t *testing.T) {
	client := &permissionEmittingClient{permissionsErr: status.Error(codes.Unimplemented, "not implemented")}
	handle := &rpcHandle{name: "perm-emit", rpc: client}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *v2.PermissionEvent, 4)
	streamCancel, err := handle.StartPermissionStream(ctx, "sess-drain", ch)
	if err != nil {
		t.Fatalf("StartPermissionStream: %v", err)
	}
	defer streamCancel()

	// Send events; they should be drained without blocking.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch <- &v2.PermissionEvent{Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r1"}}}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch) // close so the drain loop can exit
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("sends blocked; channel was not drained")
	}
}

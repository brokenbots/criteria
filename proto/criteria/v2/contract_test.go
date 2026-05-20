package criteriav2_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	criteriav2 "github.com/brokenbots/criteria/proto/criteria/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalAdapterServer is a test-only server that embeds
// UnimplementedAdapterServiceServer by value, per gRPC convention.
type minimalAdapterServer struct {
	criteriav2.UnimplementedAdapterServiceServer
}

// newBufconnServer starts an in-process gRPC server backed by a bufconn
// listener and registers the given server implementation.
// The server is stopped automatically at the end of the test via t.Cleanup.
func newBufconnServer(t *testing.T) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1 << 20) // 1 MiB buffer
	srv := grpc.NewServer()
	criteriav2.RegisterAdapterServiceServer(srv, &minimalAdapterServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis
}

// newBufconnClient dials the bufconn listener and returns a connected client.
// The connection is closed automatically at the end of the test.
func newBufconnClient(t *testing.T, lis *bufconn.Listener) criteriav2.AdapterServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return criteriav2.NewAdapterServiceClient(conn)
}

// ─── Descriptor-based contract tests ────────────────────────────────────────

// TestAdapterServiceDescriptor_RPCShapes asserts the generated
// AdapterService_ServiceDesc has exactly the 11 RPCs defined by WS02, with the
// correct unary / server-stream / bidi-stream shapes.
//
// This test fails if a future codegen run drops an RPC, adds an unexpected one,
// or changes a streaming direction — catching regressions before any host or
// SDK code notices.
func TestAdapterServiceDescriptor_RPCShapes(t *testing.T) {
	desc := criteriav2.AdapterService_ServiceDesc

	assert.Equal(t, "criteria.v2.AdapterService", desc.ServiceName)

	// Unary methods: Info, OpenSession, Pause, Resume, Snapshot, Restore,
	// Inspect, CloseSession.
	wantUnary := map[string]bool{
		"Info":         false,
		"OpenSession":  false,
		"Pause":        false,
		"Resume":       false,
		"Snapshot":     false,
		"Restore":      false,
		"Inspect":      false,
		"CloseSession": false,
	}
	for _, m := range desc.Methods {
		if _, ok := wantUnary[m.MethodName]; ok {
			wantUnary[m.MethodName] = true
		} else {
			t.Errorf("unexpected unary method in ServiceDesc: %q", m.MethodName)
		}
	}
	for name, found := range wantUnary {
		assert.True(t, found, "unary method %q must be present in ServiceDesc", name)
	}
	assert.Len(t, desc.Methods, 8, "expected exactly 8 unary methods")

	// Streaming methods with their exact server/client stream flags.
	type streamShape struct{ server, client bool }
	wantStreams := map[string]streamShape{
		"Execute":     {server: true, client: false},
		"Log":         {server: true, client: false},
		"Permissions": {server: true, client: true},
	}
	got := map[string]streamShape{}
	for _, s := range desc.Streams {
		got[s.StreamName] = streamShape{server: s.ServerStreams, client: s.ClientStreams}
	}
	for name, want := range wantStreams {
		shape, ok := got[name]
		require.True(t, ok, "streaming method %q must be present in ServiceDesc", name)
		assert.Equal(t, want.server, shape.server, "%q ServerStreams mismatch", name)
		assert.Equal(t, want.client, shape.client, "%q ClientStreams mismatch", name)
	}
	assert.Len(t, desc.Streams, 3, "expected exactly 3 streaming methods")
}

// TestAdapterService_ProtoDescriptor_RPCShapes performs the same assertions
// using proto file-descriptor reflection instead of the runtime ServiceDesc.
// A second independent check catches discrepancies between the two access paths.
func TestAdapterService_ProtoDescriptor_RPCShapes(t *testing.T) {
	svcs := criteriav2.File_criteria_v2_adapter_proto.Services()
	require.Equal(t, 1, svcs.Len(), "adapter.proto must declare exactly one service")

	svc := svcs.Get(0)
	assert.Equal(t, "AdapterService", string(svc.Name()))

	methods := svc.Methods()
	assert.Equal(t, 11, methods.Len(), "AdapterService must have exactly 11 RPCs")

	type rpcShape struct {
		clientStream bool
		serverStream bool
	}
	want := map[string]rpcShape{
		"Info":         {},
		"OpenSession":  {},
		"Execute":      {serverStream: true},
		"Log":          {serverStream: true},
		"Permissions":  {clientStream: true, serverStream: true},
		"Pause":        {},
		"Resume":       {},
		"Snapshot":     {},
		"Restore":      {},
		"Inspect":      {},
		"CloseSession": {},
	}

	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		name := string(m.Name())
		exp, ok := want[name]
		require.True(t, ok, "unexpected RPC %q in proto descriptor", name)
		assert.Equal(t, exp.clientStream, m.IsStreamingClient(),
			"RPC %q: IsStreamingClient mismatch", name)
		assert.Equal(t, exp.serverStream, m.IsStreamingServer(),
			"RPC %q: IsStreamingServer mismatch", name)
	}
}

// ─── In-process gRPC round-trip tests ────────────────────────────────────────

// TestAdapterService_InProcess_Info starts an in-process gRPC server, calls
// the unary Info RPC through the generated client stub, and asserts
// codes.Unimplemented — proving the stub dispatches to the server correctly.
func TestAdapterService_InProcess_Info(t *testing.T) {
	lis := newBufconnServer(t)
	client := newBufconnClient(t, lis)

	_, err := client.Info(context.Background(), &criteriav2.InfoRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err),
		"Info must return Unimplemented from the stub server")
}

// TestAdapterService_InProcess_Execute calls the server-streaming Execute RPC
// and asserts codes.Unimplemented on the first Recv, proving the streaming stub
// dispatches correctly.
func TestAdapterService_InProcess_Execute(t *testing.T) {
	lis := newBufconnServer(t)
	client := newBufconnClient(t, lis)

	stream, err := client.Execute(context.Background(), &criteriav2.ExecuteRequest{
		SessionId: "sess-contract",
		StepName:  "step-a",
	})
	require.NoError(t, err, "opening the Execute stream must not error immediately")

	_, recvErr := stream.Recv()
	require.Error(t, recvErr)
	assert.Equal(t, codes.Unimplemented, status.Code(recvErr),
		"Execute Recv must return Unimplemented from the stub server")
}

// TestAdapterService_InProcess_Permissions calls the bidi-streaming Permissions
// RPC and asserts codes.Unimplemented on the first Recv, proving the bidi stub
// dispatches correctly.
func TestAdapterService_InProcess_Permissions(t *testing.T) {
	lis := newBufconnServer(t)
	client := newBufconnClient(t, lis)

	stream, err := client.Permissions(context.Background())
	require.NoError(t, err, "opening the Permissions stream must not error immediately")

	_, recvErr := stream.Recv()
	require.Error(t, recvErr)
	assert.Equal(t, codes.Unimplemented, status.Code(recvErr),
		"Permissions Recv must return Unimplemented from the stub server")
}

package adapterhost

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestPromptWireRequestResponseRoundTrip(t *testing.T) {
	in, err := NewPromptRequestMessage("sess-1", "continue the review")
	if err != nil {
		t.Fatalf("NewPromptRequestMessage: %v", err)
	}
	req, err := DecodePromptRequest(in)
	if err != nil {
		t.Fatalf("DecodePromptRequest: %v", err)
	}
	if req.SessionID != "sess-1" || req.Prompt != "continue the review" {
		t.Fatalf("request roundtrip mismatch: %+v", req)
	}

	out, err := NewPromptResponseMessage(true, "")
	if err != nil {
		t.Fatalf("NewPromptResponseMessage: %v", err)
	}
	resp, err := DecodePromptResponse(out)
	if err != nil {
		t.Fatalf("DecodePromptResponse: %v", err)
	}
	if !resp.Accepted || resp.Detail != "" {
		t.Fatalf("response roundtrip mismatch: %+v", resp)
	}

	out2, err := NewPromptResponseMessage(false, "paused by policy")
	if err != nil {
		t.Fatalf("NewPromptResponseMessage: %v", err)
	}
	resp2, err := DecodePromptResponse(out2)
	if err != nil {
		t.Fatalf("DecodePromptResponse: %v", err)
	}
	if resp2.Accepted || resp2.Detail != "paused by policy" {
		t.Fatalf("rejected response roundtrip mismatch: %+v", resp2)
	}
}

// promptRecordService records prompts and answers accepted=true, mirroring
// the promptable conformance fixture.
type promptRecordService struct {
	calls []PromptRequest
}

func (s *promptRecordService) Prompt(_ context.Context, req *PromptRequest) (*PromptResponse, error) {
	s.calls = append(s.calls, *req)
	return &PromptResponse{Accepted: true}, nil
}

// TestPromptMethodDescOverGRPC proves the seam encoding end-to-end: a host
// grpcClient.Prompt call (dynamic request/response) round-trips a real
// in-process gRPC server whose AdapterService gains the Prompt method via
// PromptMethodDesc — the exact serving shape the promptable fixture uses.
func TestPromptMethodDescOverGRPC(t *testing.T) {
	svc := &promptRecordService{}
	desc := &grpc.ServiceDesc{
		ServiceName: "criteria.v2.AdapterService",
		HandlerType: (*PromptService)(nil),
		Methods:     []grpc.MethodDesc{PromptMethodDesc()},
	}
	server := grpc.NewServer()
	server.RegisterService(desc, svc)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go server.Serve(lis)
	defer server.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := &grpcClient{cc: conn}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Prompt(ctx, &PromptRequest{SessionID: "sess-42", Prompt: "rerun the failing tool"})
	if err != nil {
		t.Fatalf("client.Prompt: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("expected accepted=true, got %+v", resp)
	}
	if len(svc.calls) != 1 {
		t.Fatalf("expected 1 prompt call, got %d", len(svc.calls))
	}
	if svc.calls[0].SessionID != "sess-42" || svc.calls[0].Prompt != "rerun the failing tool" {
		t.Fatalf("prompt call mismatch: %+v", svc.calls[0])
	}
	if PromptMethodDesc().MethodName != "Prompt" {
		t.Fatalf("method name mismatch: %q", PromptMethodDesc().MethodName)
	}
	if PromptMethodFullName != "/criteria.v2.AdapterService/Prompt" {
		t.Fatalf("full method name mismatch: %q", PromptMethodFullName)
	}
}

// TestPromptMethodDescRejectsNonPromptService guards the dispatch contract:
// a server implementation without PromptService fails loudly instead of
// silently accepting the method registration.
func TestPromptMethodDescRejectsNonPromptService(t *testing.T) {
	desc := &grpc.ServiceDesc{
		ServiceName: "criteria.v2.AdapterService",
		HandlerType: (*PromptService)(nil),
		Methods:     []grpc.MethodDesc{PromptMethodDesc()},
	}
	server := grpc.NewServer()
	// ss=nil skips grpc's reflective HandlerType check; the Prompt dispatch
	// itself must fail loudly for a service impl that lacks PromptService.
	server.RegisterService(desc, nil)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go server.Serve(lis)
	defer server.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := &grpcClient{cc: conn}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.Prompt(ctx, &PromptRequest{SessionID: "s", Prompt: "p"})
	if err == nil {
		t.Fatal("expected error dispatching Prompt to a non-PromptService server")
	}
}

func TestPromptTypedErrorsDistinct(t *testing.T) {
	errs := []error{ErrPromptUnsupportedAdapter, ErrPromptNoActiveSession, ErrPromptSessionMismatch, ErrPromptRejected}
	seen := make(map[string]bool, len(errs))
	for _, err := range errs {
		if err == nil || seen[err.Error()] {
			t.Fatalf("duplicate or nil typed prompt error: %v", err)
		}
		seen[err.Error()] = true
	}
	rejected := &PromptRejectedError{Detail: "nope"}
	if !errors.Is(rejected, ErrPromptRejected) {
		t.Fatal("PromptRejectedError must unwrap to ErrPromptRejected")
	}
	if rejected.Error() != ErrPromptRejected.Error()+": nope" {
		t.Fatalf("unexpected message: %q", rejected.Error())
	}
}

// promptWireMarshalRoundTrip keeps the descriptor honest at the wire level:
// bytes from NewPromptRequestMessage decode through proto into a fresh
// dynamic message without information loss.
func TestPromptWireBytesRoundTrip(t *testing.T) {
	in, err := NewPromptRequestMessage("s", "p")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reqDesc, _, err := promptDescriptors()
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	fresh := dynamicpb.NewMessage(reqDesc)
	if err := proto.Unmarshal(b, fresh); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := DecodePromptRequest(fresh)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionID != "s" || got.Prompt != "p" {
		t.Fatalf("bytes roundtrip mismatch: %+v", got)
	}
}

package adapterhost

// promptwire.go — the criteria-side half of the adapter v2 Prompt seam
// (ADR-0006 D3). The Prompt RPC definition itself is a coordinated external
// change to the criteria-adapter-proto module; until that lands upstream, the
// exact wire shape is fixed here once and shared by the host client and by
// in-tree test fixtures, so both ends speak the contract the upstream proto
// will carry: criteria.v2.PromptRequest{session_id=1, prompt=2} and
// criteria.v2.PromptResponse{accepted=1, detail=2}, served on the
// AdapterService as /criteria.v2.AdapterService/Prompt.
//
// The messages are built and decoded through dynamic protobuf messages over a
// self-contained descriptor, so no generated Go types for the upstream
// contract are vendored into this repo (the upstream generated client
// replaces this encoding verbatim when it lands).

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	// PromptMethodFullName is the full gRPC method name of the adapter v2
	// Prompt RPC (ADR-0006 D3).
	PromptMethodFullName = "/criteria.v2.AdapterService/Prompt"

	// PromptCapability is the Info capability an adapter declares to
	// advertise that it can absorb prompts into a live session.
	PromptCapability = "supports_prompt"
)

// PromptRequest is the host-side request for the adapter v2 Prompt RPC.
type PromptRequest struct {
	SessionID string
	Prompt    string
}

// PromptResponse is the host-side response for the adapter v2 Prompt RPC.
type PromptResponse struct {
	Accepted bool
	Detail   string
}

var (
	// ErrPromptUnsupportedAdapter reports that the adapter's Info capabilities
	// do not include supports_prompt. The host must short-circuit before any
	// Prompt RPC is issued (ADR-0006 D9).
	ErrPromptUnsupportedAdapter = errors.New("adapter does not support prompts (supports_prompt=false)")

	// ErrPromptNoActiveSession reports that the addressed step has no live
	// adapter session (not entered, completed, or the adapter closed).
	ErrPromptNoActiveSession = errors.New("no live adapter session for the addressed step")

	// ErrPromptSessionMismatch reports that a caller-supplied session_id does
	// not match the live session the host resolved for the addressed step.
	ErrPromptSessionMismatch = errors.New("session_id does not match the live adapter session for the addressed step")

	// ErrPromptRejected is wrapped by PromptRejectedError when the adapter
	// itself refuses the prompt; the adapter's detail is the failure reason.
	ErrPromptRejected = errors.New("adapter rejected the prompt")
)

// PromptRejectedError carries the adapter's own detail through the host to
// the delivery-side failure record (ADR-0006 D9: accepted=false → the
// adapter's detail is forwarded as the failure reason).
type PromptRejectedError struct {
	Detail string
}

func (e *PromptRejectedError) Error() string {
	return fmt.Sprintf("%s: %s", ErrPromptRejected, e.Detail)
}

func (e *PromptRejectedError) Unwrap() error {
	return ErrPromptRejected
}

// promptDescriptors returns the message descriptors for PromptRequest and
// PromptResponse. Built once per process; the file is deliberately not
// registered in the global protoregistry.
func promptDescriptors() (req, resp protoreflect.MessageDescriptor, err error) {
	promptDescOnce.Do(func() {
		promptDescReq, promptDescResp, promptDescErr = buildPromptDescriptors()
	})
	if promptDescErr != nil {
		return nil, nil, promptDescErr
	}
	return promptDescReq, promptDescResp, nil
}

var (
	promptDescOnce sync.Once
	promptDescReq  protoreflect.MessageDescriptor
	promptDescResp protoreflect.MessageDescriptor
	promptDescErr  error
)

func buildPromptDescriptors() (req, resp protoreflect.MessageDescriptor, err error) {
	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("criteria/adapter/v2/prompt_client.proto"),
		Package: proto.String("criteria.v2"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("PromptRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					promptField("session_id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
					promptField("prompt", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				},
			},
			{
				Name: proto.String("PromptResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					promptField("accepted", 1, descriptorpb.FieldDescriptorProto_TYPE_BOOL),
					promptField("detail", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				},
			},
		},
	}
	file, err := protodesc.NewFile(fd, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build prompt wire descriptor: %w", err)
	}
	msgs := file.Messages()
	reqDesc := msgs.ByName("PromptRequest")
	respDesc := msgs.ByName("PromptResponse")
	if reqDesc == nil || respDesc == nil {
		return nil, nil, errors.New("prompt wire descriptor is missing its messages")
	}
	return reqDesc, respDesc, nil
}

func promptField(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(num),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   typ.Enum(),
	}
}

// NewPromptRequestMessage encodes a PromptRequest over the shared Prompt
// descriptor for the host-side Prompt RPC call.
func NewPromptRequestMessage(sessionID, prompt string) (*dynamicpb.Message, error) {
	reqDesc, _, err := promptDescriptors()
	if err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(reqDesc)
	fields := msg.Descriptor().Fields()
	msg.Set(fields.ByName("session_id"), protoreflect.ValueOfString(sessionID))
	msg.Set(fields.ByName("prompt"), protoreflect.ValueOfString(prompt))
	return msg, nil
}

// DecodePromptResponse reads a host-side PromptResponse from a dynamic
// PromptResponse message returned by the adapter.
func DecodePromptResponse(msg *dynamicpb.Message) (PromptResponse, error) {
	if msg == nil {
		return PromptResponse{}, errors.New("prompt response message is nil")
	}
	fields := msg.Descriptor().Fields()
	resp := PromptResponse{}
	if f := fields.ByName("accepted"); f != nil && msg.Has(f) {
		resp.Accepted = msg.Get(f).Bool()
	}
	if f := fields.ByName("detail"); f != nil && msg.Has(f) {
		resp.Detail = msg.Get(f).String()
	}
	return resp, nil
}

// NewPromptResponseMessage encodes a host-side PromptResponse for the
// adapter-side Prompt handler (fixtures serve it directly).
func NewPromptResponseMessage(accepted bool, detail string) (*dynamicpb.Message, error) {
	_, respDesc, err := promptDescriptors()
	if err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(respDesc)
	fields := msg.Descriptor().Fields()
	if accepted {
		msg.Set(fields.ByName("accepted"), protoreflect.ValueOfBool(true))
	}
	if detail != "" {
		msg.Set(fields.ByName("detail"), protoreflect.ValueOfString(detail))
	}
	return msg, nil
}

// DecodePromptRequest reads a host-side PromptRequest from a dynamic
// PromptRequest message received by the adapter-side handler.
func DecodePromptRequest(msg *dynamicpb.Message) (PromptRequest, error) {
	if msg == nil {
		return PromptRequest{}, errors.New("prompt request message is nil")
	}
	fields := msg.Descriptor().Fields()
	req := PromptRequest{}
	if f := fields.ByName("session_id"); f != nil && msg.Has(f) {
		req.SessionID = msg.Get(f).String()
	}
	if f := fields.ByName("prompt"); f != nil && msg.Has(f) {
		req.Prompt = msg.Get(f).String()
	}
	return req, nil
}

// PromptService is implemented by server-side bridges that accept prompts
// into a live adapter session (ADR-0006 D3). Fixtures register
// PromptMethodDesc on an extended copy of the AdapterService ServiceDesc.
type PromptService interface {
	Prompt(ctx context.Context, req *PromptRequest) (*PromptResponse, error)
}

// PromptMethodDesc returns the gRPC unary MethodDesc for the Prompt RPC. The
// handler decodes the wire bytes through the shared descriptor and dispatches
// to PromptService.Prompt on the registered server implementation.
func PromptMethodDesc() grpc.MethodDesc {
	return grpc.MethodDesc{MethodName: "Prompt", Handler: promptServerHandler}
}

func promptServerHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	reqDesc, _, err := promptDescriptors()
	if err != nil {
		return nil, err
	}
	in := dynamicpb.NewMessage(reqDesc)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return promptDispatch(srv, ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: PromptMethodFullName}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return promptDispatch(srv, ctx, req.(*dynamicpb.Message))
	}
	return interceptor(ctx, in, info, handler)
}

func promptDispatch(srv interface{}, ctx context.Context, in *dynamicpb.Message) (interface{}, error) {
	svc, ok := srv.(PromptService)
	if !ok {
		return nil, fmt.Errorf("server %T does not implement PromptService", srv)
	}
	req, err := DecodePromptRequest(in)
	if err != nil {
		return nil, err
	}
	resp, err := svc.Prompt(ctx, &req)
	if err != nil {
		return nil, err
	}
	return NewPromptResponseMessage(resp.Accepted, resp.Detail)
}

// promptCapableHandle is the narrow transport capability a Handle may expose
// for prompt delivery (ADR-0006 D3). The production *rpcHandle implements it
// via the adapter v2 connection; in-memory test handles may implement it
// directly. Deliberately not part of the broad Handle interface so existing
// fakes stay untouched.
type promptCapableHandle interface {
	Prompt(ctx context.Context, req *PromptRequest) (*PromptResponse, error)
}

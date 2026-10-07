package conformance

import (
	connect "connectrpc.com/connect/v2"

	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// authCreateRun creates a run for the agent identified by token, setting the
// Authorization header on the request context, and returns the run_id.
func authCreateRun(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, token, criteriaID, workflowName string) string {
	t.Helper()
	ctx, info := connect.NewClientContext(context.Background())
	info.RequestHeader().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(ctx, &pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: workflowName})
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", workflowName, err)
	}
	return runResp.RunId
}

// authSubmitStream opens an authenticated SubmitEvents stream, setting the
// Authorization header on ctx. The caller owns draining and closing the stream.
func authSubmitStream(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, ctx context.Context, token string) criteriav1connect.CriteriaServiceSubmitEventsClientStream {
	t.Helper()
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+token)
	stream, err := oClient.SubmitEvents(ctx)
	if err != nil {
		t.Fatalf("open submit stream: %v", err)
	}
	return stream
}

// submitDrain drains the stream to EOF after CloseSend so the server handler
// exits cleanly.
func submitDrain(stream criteriav1connect.CriteriaServiceSubmitEventsClientStream) {
	for {
		if _, recvErr := stream.Receive(); recvErr != nil {
			break
		}
	}
}

// submitEnvelopes submits envelopes through SubmitEvents and drains the stream
// to EOF, asserting each ack correlation_id matches the submitted envelope.
func submitEnvelopes(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, token string, envs []*pb.Envelope) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+token)
	stream, err := oClient.SubmitEvents(ctx)
	if err != nil {
		t.Fatalf("open submit stream: %v", err)
	}
	for _, env := range envs {
		if err := stream.Send(env); err != nil {
			t.Fatalf("Send(%s): %v", env.CorrelationId, err)
		}
		ack, err := stream.Receive()
		if err != nil {
			t.Fatalf("Receive ack(%s): %v", env.CorrelationId, err)
		}
		if ack.CorrelationId != env.CorrelationId {
			t.Errorf("ack.correlation_id=%q want %q", ack.CorrelationId, env.CorrelationId)
		}
	}
	_ = stream.CloseSend()
	for {
		if _, recvErr := stream.Receive(); recvErr != nil {
			break
		}
	}
}

// PayloadOneof returns the "payload" oneof descriptor from the Envelope message.
// Exported so conformance test authors can enumerate payload variants.
func PayloadOneof(t *testing.T) protoreflect.OneofDescriptor {
	t.Helper()
	oneofs := (&pb.Envelope{}).ProtoReflect().Descriptor().Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		if oneofs.Get(i).Name() == "payload" {
			return oneofs.Get(i)
		}
	}
	t.Fatal("payload oneof not found in Envelope descriptor")
	return nil
}

// ConcreteMsg returns a zero-value instance of the concrete Go type registered
// for the given oneof field descriptor.
func ConcreteMsg(t *testing.T, fd protoreflect.FieldDescriptor) proto.Message {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(fd.Message().FullName())
	if err != nil {
		t.Fatalf("arm %q: message type %q not in global registry: %v", fd.Name(), fd.Message().FullName(), err)
	}
	return mt.New().Interface()
}

// PopulateMessage sets every field of m to a deterministic non-zero value.
// depth guards against infinite recursion in self-referential message types.
// Well-known types (google.protobuf.*) are left at zero — their zero form
// round-trips correctly.
//
// Exported so Subject implementations and other test infrastructure can share
// the same deterministic population strategy.
func PopulateMessage(m protoreflect.Message, depth int) {
	if depth > 3 || isWellKnown(m.Descriptor().FullName()) {
		return
	}
	fds := m.Descriptor().Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		switch {
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			k := deterministicMapKey(fd.MapKey().Kind())
			v := deterministicValue(fd.MapValue(), m, depth)
			mp.Set(k, v)
		case fd.IsList():
			ls := m.Mutable(fd).List()
			ls.Append(deterministicValue(fd, m, depth))
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			sub := m.Mutable(fd).Message()
			PopulateMessage(sub, depth+1)
		default:
			m.Set(fd, deterministicScalar(fd))
		}
	}
}

// extractPayloadMsg returns the concrete payload proto.Message from env, or nil
// if the payload is unset. Uses proto reflection so it works for any oneof arm
// without a type switch.
func extractPayloadMsg(env *pb.Envelope) proto.Message {
	r := env.ProtoReflect()
	oo := r.Descriptor().Oneofs().ByName("payload")
	fd := r.WhichOneof(oo)
	if fd == nil {
		return nil
	}
	return r.Get(fd).Message().Interface()
}

func deterministicValue(fd protoreflect.FieldDescriptor, parent protoreflect.Message, depth int) protoreflect.Value {
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		if isWellKnown(fd.Message().FullName()) {
			return parent.NewField(fd)
		}
		var sub protoreflect.Message
		if fd.IsList() {
			// For repeated message fields, create a new message by getting one from a scratch parent.
			scratchParent := parent.Type().New()
			list := scratchParent.Mutable(fd).List()
			// Append an empty message to the list (list.AppendMutable returns a Message).
			sub = list.AppendMutable().Message()
		} else {
			sub = parent.NewField(fd).Message()
		}
		PopulateMessage(sub, depth+1)
		return protoreflect.ValueOfMessage(sub)
	}
	return deterministicScalar(fd)
}

func deterministicScalar(fd protoreflect.FieldDescriptor) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(1.0)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(1.0)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte("x"))
	case protoreflect.EnumKind:
		evs := fd.Enum().Values()
		for j := 0; j < evs.Len(); j++ {
			if evs.Get(j).Number() != 0 {
				return protoreflect.ValueOfEnum(evs.Get(j).Number())
			}
		}
		return protoreflect.ValueOfEnum(evs.Get(0).Number())
	default:
		return protoreflect.Value{}
	}
}

func deterministicMapKey(k protoreflect.Kind) protoreflect.MapKey {
	switch k {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("k").MapKey()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1).MapKey()
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1).MapKey()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1).MapKey()
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1).MapKey()
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true).MapKey()
	default:
		return protoreflect.ValueOfString("k").MapKey()
	}
}

func isWellKnown(name protoreflect.FullName) bool {
	s := string(name)
	return len(s) >= 16 && s[:16] == "google.protobuf."
}

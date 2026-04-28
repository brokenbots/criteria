package conformance

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

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

// payloadArmName returns the oneof field name for env's active payload arm
// (e.g. "run_started"), or "" if no payload is set.
func payloadArmName(env *pb.Envelope) string {
	r := env.ProtoReflect()
	oo := r.Descriptor().Oneofs().ByName("payload")
	fd := r.WhichOneof(oo)
	if fd == nil {
		return ""
	}
	return string(fd.Name())
}

func deterministicValue(fd protoreflect.FieldDescriptor, parent protoreflect.Message, depth int) protoreflect.Value {
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		if isWellKnown(fd.Message().FullName()) {
			return parent.NewField(fd)
		}
		sub := parent.NewField(fd).Message()
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

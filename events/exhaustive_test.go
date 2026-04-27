// Package events_test contains descriptor-driven exhaustiveness tests for the
// Envelope.payload oneof contract. These tests are the SDK boundary drift gate:
// adding a new oneof arm to events.proto without updating setPayload or
// TypeString will fail TestExhaustive_setPayload_TypeString on the next CI run.
//
// Note: sides 3 and 4 of the contract (Castle's payloadMessage /
// unmarshalPayload) cannot be tested here because castle is a separate Go
// module. The equivalent descriptor-driven test for those functions lives in
// castle/internal/store/sqlite/sqlite_test.go and must be kept in sync.
// Unifying all four sides into a single test file would require exporting the
// castle codec functions or restructuring module boundaries; reviewer notes in
// the workstream file track that architecture constraint.
package events_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/brokenbots/overseer/events"
	pb "github.com/brokenbots/overseer/sdk/pb/v1"
)

// payloadOneof returns the "payload" oneof descriptor from the Envelope message.
func payloadOneof(t *testing.T) protoreflect.OneofDescriptor {
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

// concreteMsg returns a zero-value instance of the concrete Go type registered
// for the given oneof arm field descriptor.
func concreteMsg(t *testing.T, fd protoreflect.FieldDescriptor) proto.Message {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(fd.Message().FullName())
	if err != nil {
		t.Fatalf("arm %q: message type %q not in global registry: %v", fd.Name(), fd.Message().FullName(), err)
	}
	return mt.New().Interface()
}

// TestExhaustive_setPayload_TypeString enumerates every Envelope.payload oneof
// arm via protoreflect and asserts:
//
//  1. events.NewEnvelope does not panic when called with the arm's concrete Go type.
//  2. events.TypeString returns a non-empty string for the resulting envelope.
//  3. All TypeString values are unique (no two arms share a discriminator).
//
// This test fails as soon as a new oneof arm is added to events.proto without
// updating setPayload and TypeString in shared/events/types.go.
func TestExhaustive_setPayload_TypeString(t *testing.T) {
	oo := payloadOneof(t)
	fields := oo.Fields()

	seen := make(map[string]string, fields.Len()) // typeString → armName

	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		armName := string(fd.Name())

		t.Run(armName, func(t *testing.T) {
			msg := concreteMsg(t, fd)

			// Side 1: setPayload (via NewEnvelope) must not panic.
			var env *pb.Envelope
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("NewEnvelope panicked for arm %q: %v", armName, r)
					}
				}()
				env = events.NewEnvelope("run-exhaustive", msg)
			}()

			// Side 2: TypeString must return a non-empty, unique discriminator.
			ts := events.TypeString(env)
			if ts == "" {
				t.Fatalf("TypeString returned empty string for arm %q", armName)
			}
			if prior, ok := seen[ts]; ok {
				t.Fatalf("TypeString collision: %q returned for both %q and %q", ts, prior, armName)
			}
			seen[ts] = armName
		})
	}

	// Edge cases: nil and no-payload envelopes must return the empty string.
	if ts := events.TypeString(nil); ts != "" {
		t.Errorf("TypeString(nil): got %q want empty", ts)
	}
	if ts := events.TypeString(&pb.Envelope{}); ts != "" {
		t.Errorf("TypeString(&pb.Envelope{}): got %q want empty", ts)
	}
}

// TestExhaustive_ProtoRoundTrip verifies that every payload arm survives a
// full protojson marshal → unmarshal cycle with proto.Equal. Each arm is
// populated with non-zero field values via populateMessage so that a codec
// regression on any field is observable.
func TestExhaustive_ProtoRoundTrip(t *testing.T) {
	oo := payloadOneof(t)
	fields := oo.Fields()

	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		armName := string(fd.Name())

		t.Run(armName, func(t *testing.T) {
			msg := concreteMsg(t, fd)
			populateMessage(msg.ProtoReflect(), 0)

			env := events.NewEnvelope("run-roundtrip", msg)

			raw, err := protojson.Marshal(env)
			if err != nil {
				t.Fatalf("protojson.Marshal: %v", err)
			}
			var back pb.Envelope
			if err := protojson.Unmarshal(raw, &back); err != nil {
				t.Fatalf("protojson.Unmarshal: %v", err)
			}
			if !proto.Equal(env, &back) {
				t.Fatalf("round-trip mismatch for arm %q:\nwant: %v\ngot:  %v", armName, env, &back)
			}
		})
	}
}

// Negative-case simulation example (kept as documentation so the suite remains
// green by default): temporarily remove one payload case from
// shared/events/types.go and rerun TestExhaustive_setPayload_TypeString. The
// test should fail for the missing oneof arm with either a panic (setPayload)
// or an empty/colliding discriminator (TypeString).

// populateMessage sets every field of m to a deterministic non-zero value.
// depth guards against infinite recursion in self-referential message types.
// Well-known types (google.protobuf.*) are left at their zero value because
// protojson handles their encoding specially and the zero form round-trips
// correctly.
func populateMessage(m protoreflect.Message, depth int) {
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
			populateMessage(sub, depth+1)
		default:
			m.Set(fd, deterministicScalar(fd))
		}
	}
}

// deterministicValue returns a non-zero protoreflect.Value for a single field
// element (used for list elements and map values).
func deterministicValue(fd protoreflect.FieldDescriptor, parent protoreflect.Message, depth int) protoreflect.Value {
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		if isWellKnown(fd.Message().FullName()) {
			// Return the empty message; zero-value well-known types round-trip fine.
			return parent.NewField(fd)
		}
		sub := parent.NewField(fd).Message()
		populateMessage(sub, depth+1)
		return protoreflect.ValueOfMessage(sub)
	}
	return deterministicScalar(fd)
}

// deterministicScalar returns a deterministic non-zero scalar value for fd.
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
		// Prefer the first non-zero enum value so we get a meaningful
		// encoding (e.g. LOG_STREAM_STDOUT rather than LOG_STREAM_UNSPECIFIED).
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

// deterministicMapKey returns a deterministic non-zero map key of kind k.
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
	return strings.HasPrefix(string(name), "google.protobuf.")
}

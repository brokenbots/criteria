package criteriav2_test

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	criteriav2 "github.com/brokenbots/criteria/proto/criteria/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── Round-trip helpers ──────────────────────────────────────────────────────

func roundTrip[M proto.Message](t *testing.T, msg M) M {
	t.Helper()
	b, err := proto.Marshal(msg)
	require.NoError(t, err)
	out := msg.ProtoReflect().New().Interface().(M)
	require.NoError(t, proto.Unmarshal(b, out))
	return out
}

// ─── InfoRequest / InfoResponse ─────────────────────────────────────────────

func TestInfoRequest_RoundTrip(t *testing.T) {
	msg := &criteriav2.InfoRequest{}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestInfoResponse_RoundTrip(t *testing.T) {
	msg := &criteriav2.InfoResponse{
		Name:               "test-adapter",
		Version:            "1.2.3",
		Description:        "A test adapter",
		Capabilities:       []string{"cap1", "cap2"},
		Platforms:          []string{"linux/amd64"},
		SdkProtocolVersion: "v2",
		SourceUrl:          "https://example.com",
		ConfigSchema: &criteriav2.AdapterSchemaProto{
			Fields: map[string]*criteriav2.ConfigFieldProto{
				"token": {Type: "string", Required: true, Sensitive: true},
			},
		},
		InputSchema: &criteriav2.AdapterSchemaProto{
			Fields: map[string]*criteriav2.ConfigFieldProto{
				"prompt": {Type: "string", Required: true},
			},
		},
		OutputSchema: &criteriav2.AdapterSchemaProto{
			Fields: map[string]*criteriav2.ConfigFieldProto{
				"result": {Type: "string"},
			},
		},
		Secrets:                map[string]string{"api_key": "The API key"},
		Permissions:            []string{"file.read"},
		CompatibleEnvironments: []string{"linux"},
		ContainerImage:         "ghcr.io/example/adapter:latest",
		SupportedFeatures:      []string{"pause", "snapshot"},
		MaxChunkBytes:          1024 * 1024,
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestInfoResponse_SupportedFeatures_ForwardCompat(t *testing.T) {
	// Unknown feature values must round-trip unchanged (forward-compat guarantee).
	msg := &criteriav2.InfoResponse{
		SupportedFeatures: []string{"pause", "unknown_future_feature_xyz"},
	}
	got := roundTrip(t, msg)
	require.Equal(t, msg.SupportedFeatures, got.SupportedFeatures)
}

// ─── OpenSession ─────────────────────────────────────────────────────────────

func TestOpenSessionRequest_RoundTrip(t *testing.T) {
	msg := &criteriav2.OpenSessionRequest{
		SessionId:       "sess-1",
		Config:          map[string]string{"k": "v"},
		Secrets:         map[string]string{"token": "secret"},
		AllowedOutcomes: []string{"success", "failure"},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestOpenSessionRequest_SecretsFieldSensitive(t *testing.T) {
	fd := criteriav2.File_criteria_v2_options_proto
	require.NotNil(t, fd, "options proto descriptor must be registered")

	msgDesc := criteriav2.File_criteria_v2_adapter_proto.Messages().ByName("OpenSessionRequest")
	require.NotNil(t, msgDesc)

	secretsField := msgDesc.Fields().ByName("secrets")
	require.NotNil(t, secretsField)

	opts := secretsField.Options()
	require.NotNil(t, opts)

	ext := protodesc.ToFieldDescriptorProto(secretsField).GetOptions()
	require.NotNil(t, ext)

	// Retrieve the sensitive extension value via proto reflection.
	sensitiveExt := proto.GetExtension(opts, criteriav2.E_Sensitive)
	sensitiveVal, ok := sensitiveExt.(bool)
	require.True(t, ok, "sensitive extension must be bool")
	assert.True(t, sensitiveVal, "OpenSessionRequest.secrets must be marked sensitive")
}

// ─── ExecuteRequest ──────────────────────────────────────────────────────────

func TestExecuteRequest_RoundTrip(t *testing.T) {
	msg := &criteriav2.ExecuteRequest{
		SessionId:       "sess-1",
		StepName:        "step-a",
		Input:           map[string]string{"prompt": "hello"},
		SecretInputs:    map[string]string{"key": "val"},
		AllowedOutcomes: []string{"done"},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestExecuteRequest_SecretInputsFieldSensitive(t *testing.T) {
	msgDesc := criteriav2.File_criteria_v2_adapter_proto.Messages().ByName("ExecuteRequest")
	require.NotNil(t, msgDesc)

	f := msgDesc.Fields().ByName("secret_inputs")
	require.NotNil(t, f)

	sensitiveExt := proto.GetExtension(f.Options(), criteriav2.E_Sensitive)
	sensitiveVal, ok := sensitiveExt.(bool)
	require.True(t, ok)
	assert.True(t, sensitiveVal, "ExecuteRequest.secret_inputs must be marked sensitive")
}

// ─── ExecuteEvent ────────────────────────────────────────────────────────────

func TestExecuteEvent_AdapterVariant_RoundTrip(t *testing.T) {
	msg := &criteriav2.ExecuteEvent{
		Event: &criteriav2.ExecuteEvent_Adapter{
			Adapter: &criteriav2.AdapterEvent{
				EventKind: "thought",
				Payload: &structpb.Struct{
					Fields: map[string]*structpb.Value{
						"text": structpb.NewStringValue("hello"),
					},
				},
				EmittedAt: timestamppb.Now(),
			},
		},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestExecuteEvent_ResultVariant_RoundTrip(t *testing.T) {
	msg := &criteriav2.ExecuteEvent{
		Event: &criteriav2.ExecuteEvent_Result{
			Result: &criteriav2.ExecuteResult{
				Outcome: "success",
				Outputs: map[string]string{"out": "value"},
			},
		},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestExecuteEvent_HeartbeatVariant_RoundTrip(t *testing.T) {
	msg := &criteriav2.ExecuteEvent{
		Event: &criteriav2.ExecuteEvent_Heartbeat{
			Heartbeat: &criteriav2.Heartbeat{
				StreamName: "execute",
				SentAt:     timestamppb.Now(),
			},
		},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

// ─── LogEvent ────────────────────────────────────────────────────────────────

func TestLogEvent_RoundTrip(t *testing.T) {
	msg := &criteriav2.LogEvent{
		SessionId:  "sess-1",
		StepName:   "step-a",
		StreamName: "stdout",
		Line:       "hello world\n",
		Timestamp:  timestamppb.Now(),
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestLogEvent_WithHeartbeat_RoundTrip(t *testing.T) {
	msg := &criteriav2.LogEvent{
		Heartbeat: &criteriav2.Heartbeat{StreamName: "log", SentAt: timestamppb.Now()},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
	assert.Equal(t, "log", got.Heartbeat.StreamName)
}

func TestLogEvent_WithChunk_RoundTrip(t *testing.T) {
	msg := &criteriav2.LogEvent{
		SessionId:  "sess-1",
		StepName:   "step-a",
		StreamName: "stdout",
		Line:       "partial line segment",
		Chunk:      &criteriav2.Chunk{Seq: 0, Total: 3, Final: false},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
	assert.Equal(t, uint32(3), got.Chunk.Total)
	assert.False(t, got.Chunk.Final)
}

// ─── PermissionEvent ─────────────────────────────────────────────────────────

func TestPermissionEvent_RequestVariant_RoundTrip(t *testing.T) {
	msg := &criteriav2.PermissionEvent{
		Event: &criteriav2.PermissionEvent_Request{
			Request: &criteriav2.PermissionRequest{
				RequestId:   "req-1",
				Tool:        "bash",
				ArgsDigest:  "abc123",
				ArgsPreview: "echo hello",
			},
		},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestPermissionEvent_CancelVariant_RoundTrip(t *testing.T) {
	msg := &criteriav2.PermissionEvent{
		Event: &criteriav2.PermissionEvent_Cancel{
			Cancel: &criteriav2.PermissionCancel{
				RequestId: "req-1",
				Reason:    "step cancelled",
			},
		},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

func TestPermissionDecision_RoundTrip(t *testing.T) {
	msg := &criteriav2.PermissionDecision{
		RequestId: "req-1",
		Decision:  "allow",
		Reason:    "operator approved",
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
	assert.Equal(t, "allow", got.Decision)
}

func TestPermissionDecision_WithHeartbeat_RoundTrip(t *testing.T) {
	msg := &criteriav2.PermissionDecision{
		Heartbeat: &criteriav2.Heartbeat{StreamName: "permissions"},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
	assert.Equal(t, "permissions", got.Heartbeat.StreamName)
}

// ─── Lifecycle messages ──────────────────────────────────────────────────────

func TestLifecycleMessages_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  proto.Message
	}{
		{"PauseRequest", &criteriav2.PauseRequest{SessionId: "s1"}},
		{"PauseResponse", &criteriav2.PauseResponse{}},
		{"ResumeRequest", &criteriav2.ResumeRequest{SessionId: "s1"}},
		{"ResumeResponse", &criteriav2.ResumeResponse{}},
		{"SnapshotRequest", &criteriav2.SnapshotRequest{SessionId: "s1"}},
		{"SnapshotResponse", &criteriav2.SnapshotResponse{State: []byte("state"), SchemaVersion: 1}},
		{"RestoreRequest", &criteriav2.RestoreRequest{SessionId: "s1", State: []byte("state"), SchemaVersion: 1}},
		{"RestoreResponse", &criteriav2.RestoreResponse{}},
		{"InspectRequest", &criteriav2.InspectRequest{SessionId: "s1"}},
		{"CloseSessionRequest", &criteriav2.CloseSessionRequest{SessionId: "s1"}},
		{"CloseSessionResponse", &criteriav2.CloseSessionResponse{}},
		{"SnapshotVersionMismatch", &criteriav2.SnapshotVersionMismatch{Have: 3, Want: 4}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := proto.Marshal(tc.msg)
			require.NoError(t, err)
			out := tc.msg.ProtoReflect().New().Interface()
			require.NoError(t, proto.Unmarshal(b, out))
			assert.True(t, proto.Equal(tc.msg, out))
		})
	}
}

// ─── SnapshotResponse sensitive annotation ──────────────────────────────────

func TestSnapshotResponse_StateFieldSensitive(t *testing.T) {
	msgDesc := criteriav2.File_criteria_v2_adapter_proto.Messages().ByName("SnapshotResponse")
	require.NotNil(t, msgDesc)
	f := msgDesc.Fields().ByName("state")
	require.NotNil(t, f)
	sensitiveExt := proto.GetExtension(f.Options(), criteriav2.E_Sensitive)
	v, ok := sensitiveExt.(bool)
	require.True(t, ok)
	assert.True(t, v)
}

func TestRestoreRequest_StateFieldSensitive(t *testing.T) {
	msgDesc := criteriav2.File_criteria_v2_adapter_proto.Messages().ByName("RestoreRequest")
	require.NotNil(t, msgDesc)
	f := msgDesc.Fields().ByName("state")
	require.NotNil(t, f)
	sensitiveExt := proto.GetExtension(f.Options(), criteriav2.E_Sensitive)
	v, ok := sensitiveExt.(bool)
	require.True(t, ok)
	assert.True(t, v)
}

// ─── ConfigFieldProto sensitive schema flag ──────────────────────────────────

func TestConfigFieldProto_SensitiveFlag_RoundTrip(t *testing.T) {
	msg := &criteriav2.ConfigFieldProto{
		Type:        "string",
		Required:    true,
		Description: "An API token",
		DefaultStr:  "",
		Sensitive:   true,
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
	assert.True(t, got.Sensitive)
}

// ─── InspectResponse ─────────────────────────────────────────────────────────

func TestInspectResponse_RoundTrip(t *testing.T) {
	msg := &criteriav2.InspectResponse{
		CurrentStep:            "step-a",
		PendingPermissionCount: 2,
		LastActivityAt:         timestamppb.Now(),
		Fields: []*criteriav2.InspectField{
			{Key: "model", Label: "LLM Model", Value: structpb.NewStringValue("gpt-4")},
		},
		Extra: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"debug": structpb.NewBoolValue(true),
			},
		},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

// ─── Chunked payload round-trips ─────────────────────────────────────────────

func TestAdapterEvent_WithChunk_RoundTrip(t *testing.T) {
	msg := &criteriav2.ExecuteEvent{
		Event: &criteriav2.ExecuteEvent_Adapter{
			Adapter: &criteriav2.AdapterEvent{
				EventKind: "thought",
				Chunk:     &criteriav2.Chunk{Seq: 1, Total: 3, Final: false},
			},
		},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
	assert.Equal(t, uint32(1), got.GetAdapter().Chunk.Seq)
	assert.Equal(t, uint32(3), got.GetAdapter().Chunk.Total)
}

func TestExecuteResult_WithChunk_RoundTrip(t *testing.T) {
	msg := &criteriav2.ExecuteResult{
		Outcome: "success",
		Chunk:   &criteriav2.Chunk{Seq: 0, Total: 1, Final: true},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
	assert.True(t, got.Chunk.Final)
}

func TestOpenSessionRequest_WithChunk_RoundTrip(t *testing.T) {
	// OpenSession is a unary RPC; chunking does not apply to its request.
	// This test verifies that the message round-trips correctly without a Chunk field.
	msg := &criteriav2.OpenSessionRequest{
		SessionId:       "sess-1",
		Config:          map[string]string{"k": "v"},
		Secrets:         map[string]string{"token": "secret"},
		AllowedOutcomes: []string{"success"},
	}
	got := roundTrip(t, msg)
	assert.True(t, proto.Equal(msg, got))
}

// TestChunkedProtocol_NegotiationAndSplit verifies that a 1 MiB payload is
// correctly split using the negotiated chunk size and that each Chunk has the
// expected seq/total/final fields.
func TestChunkedProtocol_NegotiationAndSplit(t *testing.T) {
	// Adapter declares 1 MiB max; host has 4 MiB default.
	const adapterMax = 1 * 1024 * 1024
	negotiated := criteriav2.NegotiateChunkSize(adapterMax, 0)
	assert.Equal(t, uint32(adapterMax), negotiated, "negotiated must be adapter max when adapter < host")

	// Build a payload slightly larger than negotiated.
	payload := make([]byte, adapterMax+100)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	chunks, payloads := criteriav2.SplitChunks(payload, negotiated)
	require.Len(t, chunks, 2)
	require.Len(t, payloads, 2)

	assert.Equal(t, uint32(0), chunks[0].Seq)
	assert.Equal(t, uint32(2), chunks[0].Total)
	assert.False(t, chunks[0].Final)

	assert.Equal(t, uint32(1), chunks[1].Seq)
	assert.Equal(t, uint32(2), chunks[1].Total)
	assert.True(t, chunks[1].Final)

	// Reassemble and verify round-trip fidelity.
	var reassembled []byte
	for _, p := range payloads {
		reassembled = append(reassembled, p...)
	}
	assert.Equal(t, payload, reassembled)

	// Wrap in an AdapterEvent and verify proto round-trip preserves chunk metadata.
	event := &criteriav2.AdapterEvent{
		EventKind: "data",
		Chunk:     chunks[0],
	}
	got := roundTrip(t, event)
	assert.True(t, proto.Equal(event, got))
}

// TestReservedFields_OpenSessionRequest verifies the reserved range in
// OpenSessionRequest exists in the descriptor and the environment_context
// name is reserved.
func TestReservedFields_OpenSessionRequest(t *testing.T) {
	msgDesc := criteriav2.File_criteria_v2_adapter_proto.Messages().ByName("OpenSessionRequest")
	require.NotNil(t, msgDesc)

	// Check that field number 7 is reserved.
	reservedRanges := msgDesc.ReservedRanges()
	found7 := false
	for i := 0; i < reservedRanges.Len(); i++ {
		r := reservedRanges.Get(i)
		if r[0] <= 7 && 7 < int(r[1]) {
			found7 = true
		}
	}
	assert.True(t, found7, "field number 7 must be reserved in OpenSessionRequest")

	// Check that "environment_context" is in reserved names.
	reservedNames := msgDesc.ReservedNames()
	foundName := false
	for i := 0; i < reservedNames.Len(); i++ {
		if reservedNames.Get(i) == "environment_context" {
			foundName = true
		}
	}
	assert.True(t, foundName, "environment_context must be a reserved name in OpenSessionRequest")
}

// TestReservedFields_PermissionRequest verifies field 5 and name "args" are
// reserved in PermissionRequest.
func TestReservedFields_PermissionRequest(t *testing.T) {
	msgDesc := criteriav2.File_criteria_v2_adapter_proto.Messages().ByName("PermissionRequest")
	require.NotNil(t, msgDesc)

	reservedRanges := msgDesc.ReservedRanges()
	found5 := false
	for i := 0; i < reservedRanges.Len(); i++ {
		r := reservedRanges.Get(i)
		if r[0] <= 5 && 5 < int(r[1]) {
			found5 = true
		}
	}
	assert.True(t, found5, "field number 5 must be reserved in PermissionRequest")

	reservedNames := msgDesc.ReservedNames()
	foundArgs := false
	for i := 0; i < reservedNames.Len(); i++ {
		if reservedNames.Get(i) == "args" {
			foundArgs = true
		}
	}
	assert.True(t, foundArgs, "args must be a reserved name in PermissionRequest")
}

// TestReservedFields_100To999Block verifies the 100–999 reserved range exists
// in every message defined in adapter.proto.  Adding a field in this range
// would break the protocol versioning guarantee (WS02 exit criterion).
func TestReservedFields_100To999Block(t *testing.T) {
	// All messages in criteria/v2/adapter.proto must reserve 100 to 999.
	msgNames := []protoreflect.Name{
		"Chunk",
		"Heartbeat",
		"ConfigFieldProto",
		"AdapterSchemaProto",
		"InfoRequest",
		"InfoResponse",
		"OpenSessionRequest",
		"OpenSessionResponse",
		"CloseSessionRequest",
		"CloseSessionResponse",
		"ExecuteRequest",
		"AdapterEvent",
		"ToolInvocation",
		"ExecuteResult",
		"ExecuteEvent",
		"LogRequest",
		"LogEvent",
		"PermissionRequest",
		"PermissionCancel",
		"PermissionEvent",
		"PermissionDecision",
		"PauseRequest",
		"PauseResponse",
		"ResumeRequest",
		"ResumeResponse",
		"SnapshotRequest",
		"SnapshotResponse",
		"RestoreRequest",
		"RestoreResponse",
		"SnapshotVersionMismatch",
		"InspectRequest",
		"InspectField",
		"InspectResponse",
	}
	for _, name := range msgNames {
		t.Run(string(name), func(t *testing.T) {
			msgDesc := criteriav2.File_criteria_v2_adapter_proto.Messages().ByName(name)
			require.NotNilf(t, msgDesc, "message %s not found in adapter.proto", name)
			found := false
			ranges := msgDesc.ReservedRanges()
			for i := 0; i < ranges.Len(); i++ {
				r := ranges.Get(i)
				if r[0] <= 100 && int(r[1]) > 999 {
					found = true
				}
			}
			assert.Truef(t, found, "message %s must reserve fields 100 to 999", name)
		})
	}
}

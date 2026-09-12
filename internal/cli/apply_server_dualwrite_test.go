package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/brokenbots/criteria/internal/cli/applytest"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// fileEnvelope mirrors the on-disk ND-JSON event envelope written by
// run.LocalSink (see run.localEnvelope). payload_type names are the CamelCase
// payload message names, except "run.outputs" for RunOutputs.
type fileEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Seq           int64           `json:"seq"`
	RunID         string          `json:"run_id"`
	PayloadType   string          `json:"payload_type"`
	Payload       json.RawMessage `json:"payload"`
}

// filePayloadNameAliases maps the non-CamelCase file payload_type names to
// their proto message names so parity checks key both streams by type.
var filePayloadNameAliases = map[string]string{
	"run.outputs": "RunOutputs",
}

// readNDJSONEnvelopes parses an events.ndjson file, rejecting blank or
// non-JSON lines.
func readNDJSONEnvelopes(t *testing.T, path string) []fileEnvelope {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events file %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		t.Fatalf("events file %s is empty", path)
	}
	envs := make([]fileEnvelope, 0, len(lines))
	for i, line := range lines {
		var env fileEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("events file line %d is not JSON: %v (%s)", i+1, err, line)
		}
		if env.PayloadType == "" {
			t.Errorf("events file line %d has no payload_type", i+1)
		}
		envs = append(envs, env)
	}
	return envs
}

// payloadOneof returns the concrete payload message of a server envelope via
// its non-synthetic oneof, so parity checks compare real payload content
// instead of envelope framing fields (ts, correlation, seq).
func payloadOneof(env *pb.Envelope) protoreflect.Message {
	envMsg := env.ProtoReflect()
	for i := 0; i < envMsg.Descriptor().Oneofs().Len(); i++ {
		od := envMsg.Descriptor().Oneofs().Get(i)
		if od.IsSynthetic() {
			continue
		}
		fd := envMsg.WhichOneof(od)
		if fd == nil {
			return nil
		}
		return envMsg.Get(fd).Message()
	}
	return nil
}

// payloadMessageName returns the proto message name of an envelope's payload
// (RunStarted, StepEntered, RunOutputs, ...), used to key payload parity by
// type.
func payloadMessageName(env *pb.Envelope) string {
	msg := payloadOneof(env)
	if msg == nil {
		return ""
	}
	return string(msg.Interface().ProtoReflect().Descriptor().Name())
}

// assertPayloadParity verifies that for every payload type present in the
// events file, the k-th file payload is proto-equal to the k-th server-stream
// payload of the same type. Grouping by type keeps the check robust against
// server-only stream bookkeeping events while still enforcing per-type
// ordering and full payload content equality.
func assertPayloadParity(t *testing.T, serverEvents []*pb.Envelope, fileEvents []fileEnvelope) {
	t.Helper()

	serverByType := make(map[string][]*pb.Envelope)
	for _, env := range serverEvents {
		name := payloadMessageName(env)
		serverByType[name] = append(serverByType[name], env)
	}

	fileByType := make(map[string][]fileEnvelope)
	for _, env := range fileEvents {
		name := env.PayloadType
		if alias, ok := filePayloadNameAliases[name]; ok {
			name = alias
		}
		fileByType[name] = append(fileByType[name], env)
	}

	for typeName, fileOnes := range fileByType {
		serverOnes := serverByType[typeName]
		if len(serverOnes) != len(fileOnes) {
			t.Errorf("payload type %s: file has %d events, server stream has %d", typeName, len(fileOnes), len(serverOnes))
			continue
		}
		for k := range fileOnes {
			filePayload := payloadOneof(serverOnes[k]).Interface().ProtoReflect().New().Interface()
			if err := protojson.Unmarshal(fileOnes[k].Payload, filePayload); err != nil {
				t.Errorf("payload type %s occurrence %d: decode file payload: %v", typeName, k, err)
				continue
			}
			serverPayload := payloadOneof(serverOnes[k]).Interface()
			if !proto.Equal(filePayload, serverPayload) {
				want, _ := protojson.Marshal(serverPayload)
				t.Errorf("payload type %s occurrence %d: file and server payloads differ\n file: %s\n server: %s", typeName, k, fileOnes[k].Payload, want)
			}
		}
	}
}

func TestRunApplyServerDualWriteMirrorsEventsFile(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	fake := applytest.New(t)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	err := runApplyServer(context.Background(), applyOptions{
		workflowPath: writeWorkflowFile(t, twoStepWorkflow),
		serverURL:    fake.URL(),
		eventsPath:   eventsFile,
		name:         "cri134-dual-write",
	})
	if err != nil {
		t.Fatalf("runApplyServer: %v", err)
	}

	fileEvents := readNDJSONEnvelopes(t, eventsFile)

	// Per-run invariants: strictly increasing seq and exactly one run id,
	// matching the server-assigned run id from CreateRun.
	lastSeq := int64(0)
	runIDs := make(map[string]bool)
	for _, env := range fileEvents {
		if env.Seq <= lastSeq {
			t.Errorf("file seq not strictly increasing: %d after %d", env.Seq, lastSeq)
		}
		lastSeq = env.Seq
		runIDs[env.RunID] = true
	}
	if len(runIDs) != 1 {
		t.Fatalf("expected exactly one run id in the file, got %v", runIDs)
	}
	var serverRunID string
	for _, env := range fake.Events() {
		if env.GetRunStarted() != nil {
			serverRunID = env.RunId
			break
		}
	}
	if serverRunID == "" {
		t.Fatal("server stream has no RunStarted envelope")
	}
	for _, env := range fileEvents {
		if env.RunID != serverRunID {
			t.Errorf("file run_id %s does not match server run id %s", env.RunID, serverRunID)
			break
		}
	}

	assertPayloadParity(t, fake.Events(), fileEvents)
}

// TestRunApplyServerDualWriteLeavesServerStreamUnchanged pins the invariant
// that enabling --events-file in server mode does not perturb the server
// stream: the same workflow run with and without dual-write produces
// identical payload sequences and contents.
func TestRunApplyServerDualWriteLeavesServerStreamUnchanged(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	runs := make(map[string][]string, 2)
	for _, mode := range []string{"server-only", "dual-write"} {
		fake := applytest.New(t)
		runOpts := applyOptions{
			workflowPath: writeWorkflowFile(t, twoStepWorkflow),
			serverURL:    fake.URL(),
			name:         "cri134-dual-write-invariance",
		}
		if mode == "dual-write" {
			runOpts.eventsPath = filepath.Join(t.TempDir(), "events.ndjson")
		}
		if err := runApplyServer(context.Background(), runOpts); err != nil {
			t.Fatalf("runApplyServer (%s): %v", mode, err)
		}
		var types []string
		for _, env := range fake.Events() {
			types = append(types, payloadMessageName(env))
		}
		runs[mode] = types
	}
	if strings.Join(runs["server-only"], ",") != strings.Join(runs["dual-write"], ",") {
		t.Errorf("server stream payload sequence changed when dual-write was enabled:\n server-only: %v\n dual-write: %v", runs["server-only"], runs["dual-write"])
	}
}

// TestRunApplyServerBootstrapTokenHeader covers the X-Server-Bootstrap
// Register header: absent without the flag, inline value passthrough, and
// file: secret resolution. A bad token spec must fail before any server
// interaction.
func TestRunApplyServerBootstrapTokenHeader(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	t.Run("no-token", func(t *testing.T) {
		fake := applytest.New(t)
		if err := runApplyServer(context.Background(), applyOptions{
			workflowPath: writeWorkflowFile(t, twoStepWorkflow),
			serverURL:    fake.URL(),
		}); err != nil {
			t.Fatalf("runApplyServer: %v", err)
		}
		got := fake.BootstrapHeaders()
		if len(got) != 1 || got[0] != "" {
			t.Errorf("BootstrapHeaders = %q, want a single empty header", got)
		}
	})

	t.Run("inline", func(t *testing.T) {
		fake := applytest.New(t)
		if err := runApplyServer(context.Background(), applyOptions{
			workflowPath:         writeWorkflowFile(t, twoStepWorkflow),
			serverURL:            fake.URL(),
			serverBootstrapToken: "castle-agent-secret",
		}); err != nil {
			t.Fatalf("runApplyServer: %v", err)
		}
		got := fake.BootstrapHeaders()
		if len(got) != 1 || got[0] != "castle-agent-secret" {
			t.Errorf("BootstrapHeaders = %q, want [castle-agent-secret]", got)
		}
	})

	t.Run("file", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "agent_token")
		if err := os.WriteFile(tokenFile, []byte("castle-file-secret\n"), 0o600); err != nil {
			t.Fatalf("write token file: %v", err)
		}
		fake := applytest.New(t)
		if err := runApplyServer(context.Background(), applyOptions{
			workflowPath:         writeWorkflowFile(t, twoStepWorkflow),
			serverURL:            fake.URL(),
			serverBootstrapToken: "file:" + tokenFile,
		}); err != nil {
			t.Fatalf("runApplyServer: %v", err)
		}
		got := fake.BootstrapHeaders()
		if len(got) != 1 || got[0] != "castle-file-secret" {
			t.Errorf("BootstrapHeaders = %q, want the file content with surrounding whitespace trimmed", got)
		}
	})

	t.Run("missing-token-file", func(t *testing.T) {
		fake := applytest.New(t)
		err := runApplyServer(context.Background(), applyOptions{
			workflowPath:         writeWorkflowFile(t, twoStepWorkflow),
			serverURL:            fake.URL(),
			serverBootstrapToken: "file:" + filepath.Join(t.TempDir(), "absent"),
		})
		if err == nil {
			t.Fatal("runApplyServer succeeded with a missing token file, want an error")
		}
		if fake.RegistrationCount() != 0 {
			t.Errorf("server saw %d Register calls, want 0 before bootstrap auth resolves", fake.RegistrationCount())
		}
		if !strings.Contains(err.Error(), "no such file") {
			t.Errorf("error %q does not mention the missing token file", err)
		}
	})

	t.Run("empty-token-file", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "agent_token")
		if err := os.WriteFile(tokenFile, []byte("   \n"), 0o600); err != nil {
			t.Fatalf("write token file: %v", err)
		}
		if _, err := resolveServerBootstrapToken("file:" + tokenFile); err == nil {
			t.Fatal("resolveServerBootstrapToken accepted an empty token file, want an error")
		}
	})
}

func TestResolveServerBootstrapToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent_token")
	if err := os.WriteFile(tokenFile, []byte("  secret-value \n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Run("empty disables", func(t *testing.T) {
		got, err := resolveServerBootstrapToken("")
		if err != nil {
			t.Fatalf("resolveServerBootstrapToken(\"\"): %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("inline passthrough", func(t *testing.T) {
		got, err := resolveServerBootstrapToken(" raw-secret ")
		if err != nil {
			t.Fatalf("resolveServerBootstrapToken: %v", err)
		}
		if got != "raw-secret" {
			t.Errorf("got %q, want %q", got, "raw-secret")
		}
	})

	t.Run("file reads and trims", func(t *testing.T) {
		got, err := resolveServerBootstrapToken("file:" + tokenFile)
		if err != nil {
			t.Fatalf("resolveServerBootstrapToken: %v", err)
		}
		if got != "secret-value" {
			t.Errorf("got %q, want %q", got, "secret-value")
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, err := resolveServerBootstrapToken("file:" + filepath.Join(t.TempDir(), "absent"))
		if err == nil {
			t.Fatal("want an error for a missing token file")
		}
	})

	t.Run("empty file errors", func(t *testing.T) {
		emptyFile := filepath.Join(t.TempDir(), "empty_token")
		if err := os.WriteFile(emptyFile, nil, 0o600); err != nil {
			t.Fatalf("write empty token file: %v", err)
		}
		if _, err := resolveServerBootstrapToken("file:" + emptyFile); err == nil {
			t.Fatal("want an error for an empty token file")
		}
	})
}

// TestRunApplyServerDualWriteFileCreatedBeforeServerInteraction checks that a
// bad workflow fails after the events file exists (mirroring local mode,
// which creates the file even when compilation fails), while a bad bootstrap
// token spec fails before the file is created.
func TestRunApplyServerDualWriteFileCreationOrder(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	t.Run("events file survives compile failure", func(t *testing.T) {
		eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
		err := runApplyServer(context.Background(), applyOptions{
			workflowPath: filepath.Join(t.TempDir(), "missing.chcl"),
			serverURL:    fake.URL(),
			eventsPath:   eventsFile,
		})
		if err == nil {
			t.Fatal("runApplyServer succeeded with a missing workflow, want an error")
		}
		if _, statErr := os.Stat(eventsFile); statErr != nil {
			t.Errorf("events file not created on compile failure: %v", statErr)
		}
		if fake.RegistrationCount() != 0 {
			t.Errorf("server saw %d Register calls for a workflow that never compiled, want 0", fake.RegistrationCount())
		}
	})

	t.Run("bootstrap token resolved before events file", func(t *testing.T) {
		eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
		err := runApplyServer(context.Background(), applyOptions{
			workflowPath:         writeWorkflowFile(t, twoStepWorkflow),
			serverURL:            fake.URL(),
			eventsPath:           eventsFile,
			serverBootstrapToken: "file:" + filepath.Join(t.TempDir(), "absent-token"),
		})
		if err == nil {
			t.Fatal("runApplyServer succeeded with a missing token file, want an error")
		}
		if _, statErr := os.Stat(eventsFile); !os.IsNotExist(statErr) {
			t.Errorf("events file should not be created when bootstrap auth fails, stat err = %v", statErr)
		}
	})
}

package peer

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestPeerWorkflowArmsAdjacency pins the arm numbers of the ADR-0008
// additions against the compiled proto descriptor: SupervisionEvent.kind
// arms stay append-only and contiguous (spawned=6 … child_run_terminal=12,
// next free 13), and the ControlRequest oneof reuses the free field slot 5
// (adapter_type/scope/grace_ms already own 2/3/4). Renumbering a published
// arm is a breaking wire change; this test makes it fail loudly.
func TestPeerWorkflowArmsAdjacency(t *testing.T) {
	file := criteriav1.File_criteria_v1_peer_proto
	if file == nil {
		t.Fatal("peer.proto descriptor unavailable")
	}
	supervisionMsg := file.Messages().ByName("SupervisionEvent")
	if supervisionMsg.FullName() == "" {
		t.Fatal("SupervisionEvent message not found in peer.proto descriptor")
	}
	// Walk the kind oneof's fields in descriptor (append) order and demand
	// the exact contiguous run [6, 12]: an inserted or renumbered arm
	// breaks the sequence.
	wantArms := []struct {
		name string
		num  protoreflect.FieldNumber
	}{
		{"spawned", 6},
		{"exited", 7},
		{"crash", 8},
		{"flushed", 9},
		{"heartbeat", 10},
		{"child_run_started", 11},
		{"child_run_terminal", 12},
		{"child_run_teardown_partial", 13},
	}
	oneof := supervisionMsg.Oneofs().ByName("kind")
	if oneof == nil {
		t.Fatal("SupervisionEvent.kind oneof not found")
	}
	if got, want := oneof.Fields().Len(), len(wantArms); got != want {
		t.Errorf("SupervisionEvent.kind arm count = %d, want %d", got, want)
	}
	for i, want := range wantArms {
		if i >= oneof.Fields().Len() {
			t.Fatalf("kind oneof missing arm %q", want.name)
		}
		f := oneof.Fields().Get(i)
		if string(f.Name()) != want.name {
			t.Errorf("kind arm %d name = %q, want %q", i, f.Name(), want.name)
		}
		if f.Number() != want.num {
			t.Errorf("kind arm %q number = %d, want %d", f.Name(), f.Number(), want.num)
		}
	}

	controlMsg := file.Messages().ByName("ControlRequest")
	if controlMsg.FullName() == "" {
		t.Fatal("ControlRequest message not found in peer.proto descriptor")
	}
	// Stable fields the downstream host builds literals against.
	for name, num := range map[protoreflect.Name]protoreflect.FieldNumber{
		"adapter_type": 2,
		"scope":        3,
		"grace_ms":     4,
	} {
		f := controlMsg.Fields().ByName(name)
		if f == nil {
			t.Fatalf("ControlRequest.%s missing", name)
		}
		if f.Number() != num {
			t.Errorf("ControlRequest.%s number = %d, want %d", name, f.Number(), num)
		}
	}
	kill := oneofField(t, controlMsg, "kind", "kill_child")
	if got, want := kill.Number(), protoreflect.FieldNumber(1); got != want {
		t.Errorf("ControlRequest.kill_child number = %d, want %d", got, want)
	}
	cancel := oneofField(t, controlMsg, "kind", "cancel_child_run")
	if got, want := cancel.Number(), protoreflect.FieldNumber(5); got != want {
		t.Errorf("ControlRequest.cancel_child_run number = %d, want %d", got, want)
	}
}

func oneofField(t *testing.T, msg protoreflect.MessageDescriptor, oneof, field protoreflect.Name) protoreflect.FieldDescriptor {
	t.Helper()
	o := msg.Oneofs().ByName(oneof)
	if o == nil {
		t.Fatalf("%s oneof %q not found", msg.FullName(), oneof)
	}
	f := o.Fields().ByName(field)
	if f == nil {
		t.Fatalf("%s oneof %q missing field %q", msg.FullName(), oneof, field)
	}
	return f
}

// TestChildRunArmPayloads pins the payload fields the run-graph truth rides
// on (ADR-0008): run_id/workflow_digest/version on start, run_id/outcome/
// outputs_digest on terminal. These names are the contract the parent cards
// (session/run mapping) build against.
func TestChildRunArmPayloads(t *testing.T) {
	file := criteriav1.File_criteria_v1_peer_proto
	started := file.Messages().ByName("ChildRunStarted")
	if started.FullName() == "" {
		t.Fatal("ChildRunStarted message not found")
	}
	terminal := file.Messages().ByName("ChildRunTerminal")
	if terminal.FullName() == "" {
		t.Fatal("ChildRunTerminal message not found")
	}
	cancel := file.Messages().ByName("CancelChildRun")
	if cancel.FullName() == "" {
		t.Fatal("CancelChildRun message not found")
	}
	for msg, want := range map[protoreflect.MessageDescriptor]map[string]uint32{
		started:  {"run_id": 1, "workflow_digest": 2, "version": 3},
		terminal: {"run_id": 1, "outcome": 2, "outputs_digest": 3},
		cancel:   {"run_id": 1, "grace_ms": 2},
	} {
		for name, num := range want {
			f := msg.Fields().ByName(protoreflect.Name(name))
			if f == nil {
				t.Fatalf("%s.%s missing", msg.FullName(), name)
			}
			if f.Number() != protoreflect.FieldNumber(num) {
				t.Errorf("%s.%s number = %d, want %d", msg.FullName(), name, f.Number(), num)
			}
		}
	}
}

// TestServer_ControlCancelChildRunRequiresWorkflowV1 is the negative
// acceptance check: a peer whose negotiated identity-frame capabilities lack
// workflow.v1 (simulating an older peer build or a build with the arm
// disabled) must answer Control(CancelChildRun) with a typed unimplemented
// error — not silence and not the ordinary accepted=false detail shape, so
// the caller can tell a protocol-level gap from a runtime rejection.
func TestServer_ControlCancelChildRunRequiresWorkflowV1(t *testing.T) {
	f := newPeerServeFixture(t)
	f.server.capabilities = []string{peerAdapterV2FullCapability, peerSupervisionV1Capability}

	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{
			CancelChildRun: &criteriav1.CancelChildRun{RunId: "run-1", GraceMs: 500},
		},
	})
	if err == nil {
		t.Fatalf("CancelChildRun accepted without workflow.v1: resp=%+v", resp)
	}
	if got := status.Code(err); got != codes.Unimplemented {
		t.Errorf("status code = %v, want Unimplemented (typed error, not silence): %v", got, err)
	}
	if resp != nil {
		t.Errorf("gate must not return a response body alongside the error: %+v", resp)
	}
	// kill_child stays ungated: on the same narrowed capability set the
	// runtime still answers it with the ordinary accepted=false shape (no
	// live child in this fixture), so the typed error above is specifically
	// the workflow.v1 gate on CancelChildRun.
	if _, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	}); err != nil {
		t.Fatalf("kill_child must stay ungated, got error: %v", err)
	}
}

// TestServer_ControlCancelChildRunNegotiated covers the negotiated path:
// with workflow.v1 advertised, CancelChildRun passes the gate (no typed
// error) and reaches the runtime. There is no child-run substrate yet, so
// the runtime rejects the run id with Accepted=false and an honest detail —
// the arm is known, the run is simply not there. A rejection must not
// poison the runtime the way a rejected kill_child must not.
func TestServer_ControlCancelChildRunNegotiated(t *testing.T) {
	f := newPeerServeFixture(t)

	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{
			CancelChildRun: &criteriav1.CancelChildRun{RunId: "run-9"},
		},
	})
	if err != nil {
		t.Fatalf("CancelChildRun through the workflow.v1 gate: %v", err)
	}
	if resp.GetAccepted() {
		t.Errorf("cancel accepted without a child-run substrate: %+v", resp)
	}
	if !strings.Contains(resp.GetDetail(), "run-9") {
		t.Errorf("detail %q does not name the rejected run id", resp.GetDetail())
	}
	// The negotiated capability list is what the identity frame carries:
	// default build must include workflow.v1.
	if !f.server.negotiated(peerWorkflowV1Capability) {
		t.Fatal("default server does not advertise workflow.v1")
	}
	spec, err := f.server.connProfile()
	if err != nil {
		t.Fatalf("connProfile: %v", err)
	}
	frame, err := f.server.identityFrame(spec)
	if err != nil {
		t.Fatalf("identity frame: %v", err)
	}
	if !strings.Contains(string(frame), "workflow.v1") {
		t.Errorf("identity frame %q lacks workflow.v1", frame)
	}
}

// TestEventJournal_ChildRunArmsRoundTrip covers the journal side of the
// ADR-0008 arms: they append like every other known supervision payload
// (the journal never gates on capabilities — negotiation checks live at
// emission/Control call sites) and survive Replay in order, and malformed
// payloads (missing run_id / outcome) are rejected instead of journaled.
func TestEventJournal_ChildRunArmsRoundTrip(t *testing.T) {
	j := NewEventJournal(8)

	ev1, err := j.Append(&criteriav1.SupervisionEvent_ChildRunStarted{
		ChildRunStarted: &criteriav1.ChildRunStarted{
			RunId:          "run-1",
			WorkflowDigest: "sha256:wf1",
			Version:        "v3",
		},
	}, "fakex", "sc", "sess-1")
	if err != nil {
		t.Fatalf("append ChildRunStarted: %v", err)
	}
	ev2, err := j.Append(&criteriav1.SupervisionEvent_ChildRunTerminal{
		ChildRunTerminal: &criteriav1.ChildRunTerminal{
			RunId:         "run-1",
			Outcome:       "succeeded",
			OutputsDigest: "sha256:out1",
		},
	}, "fakex", "sc", "sess-1")
	if err != nil {
		t.Fatalf("append ChildRunTerminal: %v", err)
	}

	replay := j.Replay(0)
	if len(replay) != 2 {
		t.Fatalf("replay len = %d, want 2", len(replay))
	}
	gotStart := replay[0].GetChildRunStarted()
	if gotStart == nil || gotStart.GetRunId() != "run-1" || gotStart.GetWorkflowDigest() != "sha256:wf1" || gotStart.GetVersion() != "v3" {
		t.Errorf("replayed start = %+v", replay[0])
	}
	gotTerm := replay[1].GetChildRunTerminal()
	if gotTerm == nil || gotTerm.GetRunId() != "run-1" || gotTerm.GetOutcome() != "succeeded" || gotTerm.GetOutputsDigest() != "sha256:out1" {
		t.Errorf("replayed terminal = %+v", replay[1])
	}
	if ev1.GetEventSeq() >= ev2.GetEventSeq() {
		t.Errorf("event seqs not monotonic: %d then %d", ev1.GetEventSeq(), ev2.GetEventSeq())
	}

	// Malformed child-run payloads are rejected, not journaled.
	if _, err := j.Append(&criteriav1.SupervisionEvent_ChildRunStarted{
		ChildRunStarted: &criteriav1.ChildRunStarted{},
	}, "fakex", "sc", "sess-1"); err == nil {
		t.Error("ChildRunStarted without run_id accepted")
	}
	if _, err := j.Append(&criteriav1.SupervisionEvent_ChildRunTerminal{
		ChildRunTerminal: &criteriav1.ChildRunTerminal{RunId: "run-2"},
	}, "fakex", "sc", "sess-1"); err == nil {
		t.Error("ChildRunTerminal without outcome accepted")
	}
}

package conformance

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testLifecycleAutomatic verifies the wire contract for automatic adapter
// lifecycle events (W12). Adapter session lifecycle events are carried by the
// `AdapterEvent` envelope arm with `kind` ∈ {"opened","closed","init_failed",
// "close_failed"}; adapter pod reconcile events are carried by the
// AdapterLifecycleProvisionWanted / AdapterLifecycleReleased envelope arms.
// These tests validate that subjects round-trip those envelopes correctly:
// submitted via SubmitEvents, persisted, and returned by ListRunEvents with
// their fields preserved and event ordering stable.
//
// In-process engine behavior (provisioning before first step, LIFO teardown,
// scope isolation) is covered by internal/engine/lifecycle_test.go and
// internal/engine/node_workflow_test.go. This test specifically guards the
// SDK wire contract that downstream subjects (orchestrators) must honor.
func testLifecycleAutomatic(t *testing.T, s Subject) {
	t.Run("AdapterSessionEventsRoundTrip", func(t *testing.T) {
		testAdapterSessionEventsRoundTrip(t, s)
	})
	t.Run("AdapterSessionEventsOrdered", func(t *testing.T) {
		testAdapterSessionEventsOrdered(t, s)
	})
	t.Run("AdapterPodReconcileEventsRoundTrip", func(t *testing.T) {
		testAdapterPodReconcileEventsRoundTrip(t, s)
	})
}

// testAdapterSessionEventsRoundTrip submits an opened/closed pair of
// AdapterEvent envelopes and asserts both are persisted with the adapter
// instance ID and kind preserved.
func testAdapterSessionEventsRoundTrip(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-lifecycle-rt"
	criteriaID := s.RegisterAgent(t, "criteria-lifecycle-rt", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-lifecycle-rt"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	const adapterID = "noop.default"
	opened := criteria.NewEnvelope(runID, &pb.AdapterEvent{Adapter: adapterID, Kind: "opened"})
	opened.CorrelationId = "lifecycle-opened"
	closed := criteria.NewEnvelope(runID, &pb.AdapterEvent{Adapter: adapterID, Kind: "closed"})
	closed.CorrelationId = "lifecycle-closed"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	for _, env := range []*pb.Envelope{opened, closed} {
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
	_ = stream.CloseRequest()
	for {
		if _, recvErr := stream.Receive(); recvErr != nil {
			break
		}
	}

	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	byCorr := map[string]*pb.AdapterEvent{}
	for _, ev := range events {
		ae := ev.GetAdapterEvent()
		if ae == nil {
			continue
		}
		byCorr[ev.CorrelationId] = ae
	}

	for _, want := range []struct {
		corrID, kind string
	}{
		{"lifecycle-opened", "opened"},
		{"lifecycle-closed", "closed"},
	} {
		got, ok := byCorr[want.corrID]
		if !ok {
			t.Errorf("expected AdapterEvent with correlation_id=%q in run events; got events: %d adapter events",
				want.corrID, len(byCorr))
			continue
		}
		if got.Adapter != adapterID {
			t.Errorf("%s: adapter=%q, want %q", want.corrID, got.Adapter, adapterID)
		}
		if got.Kind != want.kind {
			t.Errorf("%s: kind=%q, want %q", want.corrID, got.Kind, want.kind)
		}
	}
}

// testAdapterSessionEventsOrdered submits opened then closed and asserts the
// stored sequence numbers preserve that order. Subjects MUST persist envelopes
// in submission order — without this guarantee, downstream consumers cannot
// reconstruct LIFO teardown semantics from the event stream.
func testAdapterSessionEventsOrdered(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-lifecycle-ord"
	criteriaID := s.RegisterAgent(t, "criteria-lifecycle-ord", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-lifecycle-ord"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	opened := criteria.NewEnvelope(runID, &pb.AdapterEvent{Adapter: "noop.default", Kind: "opened"})
	opened.CorrelationId = "ord-opened"
	closed := criteria.NewEnvelope(runID, &pb.AdapterEvent{Adapter: "noop.default", Kind: "closed"})
	closed.CorrelationId = "ord-closed"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	for _, env := range []*pb.Envelope{opened, closed} {
		if err := stream.Send(env); err != nil {
			t.Fatalf("Send(%s): %v", env.CorrelationId, err)
		}
		if _, err := stream.Receive(); err != nil {
			t.Fatalf("Receive ack(%s): %v", env.CorrelationId, err)
		}
	}
	_ = stream.CloseRequest()
	for {
		if _, recvErr := stream.Receive(); recvErr != nil {
			break
		}
	}

	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	var openedSeq, closedSeq uint64
	var sawOpened, sawClosed bool
	for _, ev := range events {
		switch ev.CorrelationId {
		case "ord-opened":
			openedSeq, sawOpened = ev.Seq, true
		case "ord-closed":
			closedSeq, sawClosed = ev.Seq, true
		}
	}
	if !sawOpened || !sawClosed {
		t.Fatalf("missing one of the lifecycle events: opened=%v closed=%v", sawOpened, sawClosed)
	}
	if !(openedSeq < closedSeq) {
		t.Errorf("expected opened.seq < closed.seq, got opened=%d closed=%d", openedSeq, closedSeq)
	}
}

// testAdapterPodReconcileEventsRoundTrip submits a provision_wanted/released
// pair carried by the AdapterLifecycleProvisionWanted / AdapterLifecycleReleased
// envelope arms. Subjects MUST persist and return them with scope_instance_id,
// shim_listen_address, and token_ref preserved — orchestrators reconcile
// adapter pods per scope from these fields. TypeString discriminators are also
// asserted: "adapter.lifecycle.provision_wanted" and
// "adapter.lifecycle.released".
func testAdapterPodReconcileEventsRoundTrip(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-lifecycle-pod"
	criteriaID := s.RegisterAgent(t, "criteria-lifecycle-pod", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-lifecycle-pod"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	const scopeInstanceID = "run-pod/step-1/scope-0"
	const shimAddr = "127.0.0.1:52000"
	const tokenRef = "criteria/adapter/run-pod-step-1"
	wanted := criteria.NewEnvelope(runID, &pb.AdapterLifecycleProvisionWanted{
		ScopeInstanceId:   scopeInstanceID,
		ShimListenAddress: shimAddr,
		TokenRef:          tokenRef,
	})
	wanted.CorrelationId = "pod-provision-wanted"
	released := criteria.NewEnvelope(runID, &pb.AdapterLifecycleReleased{
		ScopeInstanceId:   scopeInstanceID,
		ShimListenAddress: shimAddr,
		TokenRef:          tokenRef,
	})
	released.CorrelationId = "pod-released"

	if got := criteria.TypeString(wanted); got != "adapter.lifecycle.provision_wanted" {
		t.Errorf("TypeString(provision_wanted)=%q, want adapter.lifecycle.provision_wanted", got)
	}
	if got := criteria.TypeString(released); got != "adapter.lifecycle.released" {
		t.Errorf("TypeString(released)=%q, want adapter.lifecycle.released", got)
	}
	if criteria.IsTerminal(wanted) || criteria.IsTerminal(released) {
		t.Error("adapter pod reconcile events must not be terminal run events")
	}

	submitEnvelopes(t, oClient, token, []*pb.Envelope{wanted, released})

	sent := map[string]*pb.Envelope{
		wanted.CorrelationId:   wanted,
		released.CorrelationId: released,
	}

	assertPodReconcilePersisted(t, s, baseURL, client, token, runID, sent)
}

// assertPodReconcilePersisted reads the run's events back through
// ListRunEvents and asserts the provision_wanted/released pair was persisted
// field-for-field (compared via proto.Equal) in submission order.
func assertPodReconcilePersisted(t *testing.T, s Subject, baseURL string, client *http.Client, token, runID string, sent map[string]*pb.Envelope) {
	t.Helper()

	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	persisted := map[string]*pb.Envelope{}
	for _, ev := range events {
		switch ev.Payload.(type) {
		case *pb.Envelope_AdapterLifecycleProvisionWanted, *pb.Envelope_AdapterLifecycleReleased:
			persisted[ev.CorrelationId] = ev
		}
	}

	for corrID, env := range sent {
		got, ok := persisted[corrID]
		if !ok {
			t.Fatalf("expected %s persisted; run events=%d", corrID, len(events))
		}
		if !proto.Equal(extractPayloadMsg(env), extractPayloadMsg(got)) {
			t.Errorf("%s: payload round-trip mismatch:\nwant: %v\ngot:  %v",
				corrID, extractPayloadMsg(env), extractPayloadMsg(got))
		}
	}
	wanted, released := sent["pod-provision-wanted"], sent["pod-released"]
	if persisted[wanted.CorrelationId].Seq >= persisted[released.CorrelationId].Seq {
		t.Errorf("expected provision_wanted.seq < released.seq, got %d >= %d",
			persisted[wanted.CorrelationId].Seq, persisted[released.CorrelationId].Seq)
	}
}

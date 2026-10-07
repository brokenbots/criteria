package conformance

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testRunMetadataRoundTrip submits a sequence of RunMetadata envelopes and
// asserts they are persisted and returned with fields preserved and ordering
// stable. Run metadata is the UNIVERSAL wire surface for run context (CRI-131
// ruling): implementations promote their own first-class columns from
// run.metadata envelopes — never from request fields — so fidelity of this
// arm is the contract.
func testRunMetadataRoundTrip(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-run-metadata"
	criteriaID := s.RegisterAgent(t, "criteria-run-metadata", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-run-metadata"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	sent := map[string]*pb.Envelope{
		"metadata-ticket":   criteria.NewEnvelope(runID, &pb.RunMetadata{Ticket: "CRI-131"}),
		"metadata-repo-url": criteria.NewEnvelope(runID, &pb.RunMetadata{RepoUrl: "https://example.invalid/repo"}),
	}
	for corrID, env := range sent {
		env.CorrelationId = corrID
	}
	ticket, repo := sent["metadata-ticket"], sent["metadata-repo-url"]

	if got := criteria.TypeString(ticket); got != "run.metadata" {
		t.Errorf("TypeString(run_metadata)=%q, want run.metadata", got)
	}
	if criteria.IsTerminal(ticket) || criteria.IsTerminal(repo) {
		t.Error("run.metadata events must not be terminal run events")
	}

	submitEnvelopes(t, oClient, token, []*pb.Envelope{ticket, repo})
	assertRunMetadataPersisted(t, s, baseURL, client, token, runID)
}

// assertRunMetadataPersisted reads the run's events back through ListRunEvents
// and asserts the submitted run.metadata envelopes were persisted field-for-field
// in submission order.
func assertRunMetadataPersisted(t *testing.T, s Subject, baseURL string, client *http.Client, token, runID string) {
	t.Helper()

	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	persisted := map[string]*pb.RunMetadata{}
	for _, ev := range events {
		if rm := ev.GetRunMetadata(); rm != nil {
			persisted[ev.CorrelationId] = rm
		}
	}

	for corrID, want := range map[string]*pb.RunMetadata{
		"metadata-ticket":   {Ticket: "CRI-131"},
		"metadata-repo-url": {RepoUrl: "https://example.invalid/repo"},
	} {
		got, ok := persisted[corrID]
		if !ok {
			t.Fatalf("expected run.metadata with correlation_id=%q persisted; run events=%d", corrID, len(events))
		}
		if !proto.Equal(want, got) {
			t.Errorf("run.metadata %s: field round-trip mismatch:\nwant: %v\ngot:  %v", corrID, want, got)
		}
	}
	if ticketSeq(events, "metadata-ticket") >= ticketSeq(events, "metadata-repo-url") {
		t.Error("expected run.metadata seq to preserve submission order")
	}
}

// ticketSeq returns the stored seq for a correlation_id, or 0 when absent.
func ticketSeq(events []*pb.Envelope, corrID string) uint64 {
	for _, ev := range events {
		if ev.CorrelationId == corrID {
			return ev.Seq
		}
	}
	return 0
}

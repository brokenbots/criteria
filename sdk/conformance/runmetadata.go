package conformance

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testRunMetadataRoundTrip submits a sequence of RunMetadata envelopes and
// asserts they are persisted and returned with key/value fields preserved and
// ordering stable. Run metadata is the UNIVERSAL wire surface for run context
// (CRI-131 ruling): implementations promote their own first-class columns from
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

	envs := []*pb.Envelope{
		criteria.NewEnvelope(runID, &pb.RunMetadata{Ticket: "CRI-131"}),
		criteria.NewEnvelope(runID, &pb.RunMetadata{RepoUrl: "https://example.invalid/repo"}),
	}
	envs[0].CorrelationId = "metadata-ticket"
	envs[1].CorrelationId = "metadata-repo-url"

	if got := criteria.TypeString(envs[0]); got != "run.metadata" {
		t.Errorf("TypeString(run_metadata)=%q, want run.metadata", got)
	}
	if criteria.IsTerminal(envs[0]) || criteria.IsTerminal(envs[1]) {
		t.Error("run.metadata events must not be terminal run events")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
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
	_ = stream.CloseRequest()
	for {
		if _, recvErr := stream.Receive(); recvErr != nil {
			break
		}
	}

	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	byCorr := map[string]*pb.RunMetadata{}
	var seqs = map[string]uint64{}
	for _, ev := range events {
		if rm := ev.GetRunMetadata(); rm != nil {
			byCorr[ev.CorrelationId] = rm
			seqs[ev.CorrelationId] = ev.Seq
		}
	}

	for _, corrID := range []string{"metadata-ticket", "metadata-repo-url"} {
		got, ok := byCorr[corrID]
		if !ok {
			t.Fatalf("expected run.metadata with correlation_id=%q persisted; run events=%d", corrID, len(events))
		}
		if corrID == "metadata-ticket" {
			if got.Ticket != "CRI-131" || got.RepoUrl != "" || got.PrUrl != "" {
				t.Errorf("ticket envelope: got ticket=%q repo_url=%q pr_url=%q", got.Ticket, got.RepoUrl, got.PrUrl)
			}
		}
		if corrID == "metadata-repo-url" {
			if got.RepoUrl != "https://example.invalid/repo" || got.Ticket != "" || got.PrUrl != "" {
				t.Errorf("repo_url envelope: got ticket=%q repo_url=%q pr_url=%q", got.Ticket, got.RepoUrl, got.PrUrl)
			}
		}
	}
	if seqs["metadata-ticket"] >= seqs["metadata-repo-url"] {
		t.Errorf("expected metadata.seq to preserve submission order, got ticket=%d >= repo=%d",
			seqs["metadata-ticket"], seqs["metadata-repo-url"])
	}
}

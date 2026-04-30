package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testAckOrdering verifies that SubmitEvents acks arrive with monotonically
// increasing seq per run_id, and that correlation_ids are echoed faithfully.
//
// Scenarios:
//  1. N sequential envelopes → N acks with strictly increasing seq and
//     matching correlation_ids.
//  2. Re-submit with a duplicate correlation_id → idempotent ack (same seq,
//     same correlation_id, no new event row inserted).
//  3. Two concurrent SubmitEvents streams to the same run_id → combined seq
//     values are monotonically non-decreasing and all acks arrive.
func testAckOrdering(t *testing.T, s Subject) {
	t.Run("Sequential", func(t *testing.T) {
		testAckOrderingSequential(t, s)
	})
	t.Run("IdempotentDuplicate", func(t *testing.T) {
		testAckIdempotentDuplicate(t, s)
	})
	t.Run("ConcurrentStreams", func(t *testing.T) {
		testAckConcurrentStreams(t, s)
	})
}

const ackTestN = 5

func testAckOrderingSequential(t *testing.T, s Subject) { //nolint:funlen // W03: sequential ordering test exercises many event/ack sequence steps
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ack-seq"
	criteriaID := s.RegisterAgent(t, "criteria-ack-seq", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-ack-seq"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)

	corrIDs := make([]string, ackTestN)
	for i := 0; i < ackTestN; i++ {
		corrIDs[i] = fmt.Sprintf("ack-seq-%d", i)
		env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: fmt.Sprintf("line %d", i)})
		env.CorrelationId = corrIDs[i]
		if err := stream.Send(env); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}
	stream.CloseRequest()

	var acks []*pb.Ack
	for {
		ack, err := stream.Receive()
		if err != nil {
			break
		}
		acks = append(acks, ack)
	}

	if len(acks) != ackTestN {
		t.Fatalf("expected %d acks, got %d", ackTestN, len(acks))
	}

	// Verify monotonically increasing seq.
	for i := 1; i < len(acks); i++ {
		if acks[i].Seq <= acks[i-1].Seq {
			t.Errorf("ack sequence not monotonically increasing at index %d: prev[%d]=%d curr[%d]=%d",
				i, i-1, acks[i-1].Seq, i, acks[i].Seq)
		}
	}

	// Verify correlation_ids match (by collecting sent vs received).
	receivedCorrs := make(map[string]struct{}, len(acks))
	for _, a := range acks {
		receivedCorrs[a.CorrelationId] = struct{}{}
	}
	for _, c := range corrIDs {
		if _, ok := receivedCorrs[c]; !ok {
			t.Errorf("correlation_id %q not found in acks", c)
		}
	}
}

func testAckIdempotentDuplicate(t *testing.T, s Subject) { //nolint:funlen // W03: idempotency test requires constructing duplicate ack sequences end-to-end
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ack-idem"
	criteriaID := s.RegisterAgent(t, "criteria-ack-idem", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-ack-idem"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	const corrID = "idem-corr-1"

	sendAndCollect := func(t *testing.T) *pb.Ack {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stream := oClient.SubmitEvents(ctx)
		stream.RequestHeader().Set("Authorization", "Bearer "+token)
		env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "hello"})
		env.CorrelationId = corrID
		if err := stream.Send(env); err != nil {
			t.Fatalf("Send: %v", err)
		}
		ack, err := stream.Receive()
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		stream.CloseRequest()
		for {
			if _, recvErr := stream.Receive(); recvErr != nil {
				break
			}
		}
		return ack
	}

	first := sendAndCollect(t)
	second := sendAndCollect(t)

	if first.Seq != second.Seq {
		t.Errorf("idempotent duplicate: first seq=%d second seq=%d (want equal)", first.Seq, second.Seq)
	}
	if second.CorrelationId != corrID {
		t.Errorf("idempotent duplicate: second ack correlation_id=%q want %q", second.CorrelationId, corrID)
	}

	// Verify only one event is persisted (not two).
	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	count := 0
	for _, ev := range events {
		if ev.CorrelationId == corrID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 persisted event for corr=%q, found %d", corrID, count)
	}
}

func testAckConcurrentStreams(t *testing.T, s Subject) { //nolint:funlen // W03: concurrent stream test serialises two interleaved sequences with many assertions
	// Tests the contract: two simultaneously-open bidi streams targeting the
	// SAME run_id both receive acks, and acks for that run are strictly
	// monotonically increasing.
	//
	// Streams are interleaved (one send/receive at a time) rather than
	// goroutine-parallel. This avoids SQLite write-lock contention between
	// concurrent goroutines, which can surface as immediate SQLITE_BUSY errors
	// in file-backed test stores even with busy_timeout set. Interleaving still
	// tests the key contract — both streams are open simultaneously and the
	// server sequences writes from different clients into a monotonic per-run
	// seq — without the non-determinism of goroutine scheduling.
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ack-conc"
	criteriaID := s.RegisterAgent(t, "criteria-ack-conc", token)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-ack-conc"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := criteria.NewServiceClient(client, baseURL).CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	ctxA, cancelA := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelA()
	ctxB, cancelB := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelB()

	// Both bidi streams open simultaneously, both targeting the same run_id.
	streamA := criteria.NewServiceClient(client, baseURL).SubmitEvents(ctxA)
	streamA.RequestHeader().Set("Authorization", "Bearer "+token)
	streamB := criteria.NewServiceClient(client, baseURL).SubmitEvents(ctxB)
	streamB.RequestHeader().Set("Authorization", "Bearer "+token)

	const nPerStream = 3
	var seqs []uint64

	sendReceive := func(stream interface {
		Send(*pb.Envelope) error
		Receive() (*pb.Ack, error)
	}, prefix string, i int) {
		t.Helper()
		env := criteria.NewEnvelope(runID, &pb.StepLog{
			Step:   "s",
			Stream: pb.LogStream_LOG_STREAM_STDOUT,
			Chunk:  fmt.Sprintf("%s-%d", prefix, i),
		})
		env.CorrelationId = fmt.Sprintf("%s-corr-%d", prefix, i)
		if err := stream.Send(env); err != nil {
			t.Errorf("Send(%s[%d]): %v", prefix, i, err)
			return
		}
		ack, err := stream.Receive()
		if err != nil {
			t.Errorf("Receive(%s[%d]): %v", prefix, i, err)
			return
		}
		seqs = append(seqs, ack.Seq)
	}

	// Interleave sends across both simultaneously-open streams, same run_id.
	for i := 0; i < nPerStream; i++ {
		sendReceive(streamA, "A", i)
		sendReceive(streamB, "B", i)
	}

	_ = streamA.CloseRequest()
	_ = streamB.CloseRequest()

	total := nPerStream * 2
	if len(seqs) != total {
		t.Fatalf("concurrent streams: expected %d acks total, got %d", total, len(seqs))
	}

	// Seqs must be strictly monotonically increasing across all events for
	// this run_id — the server serialises concurrent writers into a single
	// monotonic sequence.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("concurrent streams: seq[%d]=%d not > seq[%d]=%d (monotonic per-run contract violated)",
				i, seqs[i], i-1, seqs[i-1])
		}
	}
}

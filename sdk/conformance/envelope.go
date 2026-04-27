package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	overseer "github.com/brokenbots/overlord/shared/sdk/overseer"
)

// testEnvelopeRoundTrip verifies that every Envelope.payload variant submitted
// via SubmitEvents is durably stored and returned verbatim by ListRunEvents.
//
// For each payload arm the test:
//  1. Constructs a non-zero instance of the payload via PopulateMessage.
//  2. Wraps it with overseer.NewEnvelope.
//  3. Submits through SubmitEvents and waits for the ack.
//  4. Reads back via Subject.ListRunEvents.
//  5. Asserts proto.Equal on the payload message (ignoring server-assigned
//     fields such as seq and ts).
//
// The descriptor walk is authoritative: adding a new oneof arm to events.proto
// without updating the SDK's NewEnvelope or TypeString breaks this test.
//
// WatchReady is skipped: it is a server-synthetic event that Castle rejects
// on SubmitEvents ingestion (it has no persistence path).
func testEnvelopeRoundTrip(t *testing.T, s Subject) {
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const overseerName = "overseer-rt"
	const token = "token-rt"
	overseerID := s.RegisterOverseer(t, overseerName, token)

	oClient := overseer.NewServiceClient(client, baseURL)

	// Create a dedicated run for this test.
	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "conformance-envelope-rt"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	oo := PayloadOneof(t)
	fields := oo.Fields()

	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		armName := string(fd.Name())

		// WatchReady is server-only; Castle rejects it on SubmitEvents.
		if armName == "watch_ready" {
			continue
		}

		t.Run(armName, func(t *testing.T) {
			msg := ConcreteMsg(t, fd)
			PopulateMessage(msg.ProtoReflect(), 0)

			env := overseer.NewEnvelope(runID, msg)
			corrID := fmt.Sprintf("rt-%s", armName)
			env.CorrelationId = corrID

			// Submit the envelope.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream := oClient.SubmitEvents(ctx)
			stream.RequestHeader().Set("Authorization", "Bearer "+token)

			if err := stream.Send(env); err != nil {
				t.Fatalf("Send(%s): %v", armName, err)
			}
			ack, err := stream.Receive()
			if err != nil {
				t.Fatalf("Receive ack(%s): %v", armName, err)
			}
			if ack.CorrelationId != corrID {
				t.Errorf("ack.correlation_id=%q want %q", ack.CorrelationId, corrID)
			}
			stream.CloseRequest()
			// Drain to EOF so the server handler exits cleanly.
			for {
				_, recvErr := stream.Receive()
				if recvErr != nil {
					break
				}
			}

			// Read back and locate the event by correlation_id.
			events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
			var found *pb.Envelope
			for _, ev := range events {
				if ev.CorrelationId == corrID {
					found = ev
					break
				}
			}
			if found == nil {
				t.Fatalf("arm %s: event not found in ListRunEvents after submit (corr=%s)", armName, corrID)
			}

			// Compare the payload message (not the full envelope, because
			// Castle mutates seq, ts, etc. on ingest).
			want := extractPayloadMsg(env)
			got := extractPayloadMsg(found)
			if want == nil {
				t.Fatalf("arm %s: extractPayloadMsg(sent) returned nil", armName)
			}
			if got == nil {
				t.Fatalf("arm %s: extractPayloadMsg(retrieved) returned nil", armName)
			}
			if !proto.Equal(want, got) {
				t.Errorf("arm %s: payload round-trip mismatch:\nwant: %v\ngot:  %v", armName, want, got)
			}
		})
	}
}

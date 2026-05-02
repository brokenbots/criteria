package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testTypeStringStability verifies that criteria.TypeString returns a stable,
// non-empty, deterministic discriminator for every Envelope.payload variant,
// and that the discriminator survives a SubmitEvents → ListRunEvents round-trip.
//
// Assertions per variant:
//  1. criteria.TypeString(env) is non-empty.
//  2. All TypeString values are unique across variants.
//  3. TypeString follows the "domain.word" convention
//     (lower-case, dot-separated — e.g. "run.started", "step.log").
//  4. The event retrieved from ListRunEvents yields the same TypeString as
//     the submitted envelope, confirming the discriminator is stable across
//     the wire boundary.
func testTypeStringStability(t *testing.T, s Subject) { //nolint:funlen,gocognit // W03: stability test enumerates all envelope types with submit/retrieve/compare steps
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-ts"
	criteriaID := s.RegisterAgent(t, "criteria-ts", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-typestring"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	oo := PayloadOneof(t)
	fields := oo.Fields()
	seen := make(map[string]string, fields.Len()) // typeString → armName

	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		armName := string(fd.Name())
		if armName == "watch_ready" {
			continue
		}

		t.Run(armName, func(t *testing.T) {
			msg := ConcreteMsg(t, fd)
			PopulateMessage(msg.ProtoReflect(), 0)
			env := criteria.NewEnvelope(runID, msg)
			corrID := fmt.Sprintf("ts-%s", armName)
			env.CorrelationId = corrID

			// 1. TypeString must be non-empty.
			ts := criteria.TypeString(env)
			if ts == "" {
				t.Fatalf("TypeString returned empty string for arm %q", armName)
			}

			// 2. Must be unique.
			if prior, ok := seen[ts]; ok {
				t.Fatalf("TypeString collision: %q returned for both %q and %q", ts, prior, armName)
			}
			seen[ts] = armName

			// 3. Must follow "domain.word" convention (lower-case, dot-separated).
			if !isValidTypeString(ts) {
				t.Errorf("TypeString %q does not follow lower-case dot-separated convention", ts)
			}

			// Submit the envelope and read it back.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream := oClient.SubmitEvents(ctx)
			stream.RequestHeader().Set("Authorization", "Bearer "+token)
			if err := stream.Send(env); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if _, err := stream.Receive(); err != nil {
				t.Fatalf("Receive ack: %v", err)
			}
			_ = stream.CloseRequest()
			for {
				if _, recvErr := stream.Receive(); recvErr != nil {
					break
				}
			}

			// 4. TypeString of the retrieved envelope must match.
			events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
			var found *pb.Envelope
			for _, ev := range events {
				if ev.CorrelationId == corrID {
					found = ev
					break
				}
			}
			if found == nil {
				t.Fatalf("event not found in ListRunEvents (arm=%s, corr=%s)", armName, corrID)
			}
			gotTS := criteria.TypeString(found)
			if gotTS != ts {
				t.Errorf("TypeString stability failure for arm %q: submitted=%q retrieved=%q", armName, ts, gotTS)
			}
		})
	}
}

// isValidTypeString checks that ts is a non-empty lower-case dot-separated
// string in the form "domain.word" or "domain.word_word".
func isValidTypeString(ts string) bool {
	if ts == "" {
		return false
	}
	if !strings.Contains(ts, ".") {
		return false
	}
	for _, c := range ts {
		if !((c >= 'a' && c <= 'z') || c == '.' || c == '_') {
			return false
		}
	}
	return true
}

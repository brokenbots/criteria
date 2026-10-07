package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

type fakePage struct {
	sinceSeq uint64
	resp     *pb.ListRunEventsResponse
}

// fakeEventClient records calls and returns canned pages / live events.
type fakeEventClient struct {
	pages []fakePage
	live  []*pb.Envelope

	listCalls []uint64
	watchReq  *pb.WatchRunRequest
}

func (f *fakeEventClient) ListRunEvents(_ context.Context, req *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	f.listCalls = append(f.listCalls, req.Msg.SinceSeq)
	for i, page := range f.pages {
		if page.sinceSeq == req.Msg.SinceSeq {
			// Each page is only consumed once so repeated sinceSeq values return empty.
			f.pages = append(f.pages[:i], f.pages[i+1:]...)
			return connect.NewResponse(page.resp), nil
		}
	}
	return connect.NewResponse(&pb.ListRunEventsResponse{}), nil
}

func (f *fakeEventClient) WatchRun(_ context.Context, req *connect.Request[pb.WatchRunRequest]) (eventStream, error) {
	f.watchReq = req.Msg
	return &fakeEventStream{events: f.live}, nil
}

// fakeEventStream yields a fixed list of envelopes.
type fakeEventStream struct {
	events []*pb.Envelope
	idx    int
}

func (s *fakeEventStream) Receive() bool {
	if s.idx < len(s.events) {
		s.idx++
		return true
	}
	return false
}

func (s *fakeEventStream) Msg() *pb.Envelope {
	if s.idx == 0 {
		return nil
	}
	return s.events[s.idx-1]
}

func (s *fakeEventStream) Err() error { return nil }

func (s *fakeEventStream) Close() error { return nil }

func TestWatch_HistoricalThenLive_Success(t *testing.T) {
	client := &fakeEventClient{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "r1", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf", InitialStep: "step1"}}},
						{RunId: "r1", Seq: 2, Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "step1", Adapter: "shell", Attempt: 1}}},
					},
					NextSinceSeq: 0,
				},
			},
		},
		live: []*pb.Envelope{
			{RunId: "r1", Seq: 3, Payload: &pb.Envelope_StepOutcome{StepOutcome: &pb.StepOutcome{Step: "step1", Outcome: "success", DurationMs: 100}}},
			{RunId: "r1", Seq: 4, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}

	var out bytes.Buffer
	err := runWatch(context.Background(), client, "r1", outputModeConcise, &out)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "run started: wf") {
		t.Errorf("missing run started line:\n%s", got)
	}
	if !strings.Contains(got, "step step1") {
		t.Errorf("missing step entered line:\n%s", got)
	}
	if !strings.Contains(got, "run completed: done") {
		t.Errorf("missing final status line:\n%s", got)
	}
	if strings.Count(got, "run completed") != 1 {
		t.Errorf("final status should appear exactly once, got:\n%s", got)
	}

	if len(client.listCalls) != 1 || client.listCalls[0] != 0 {
		t.Fatalf("expected one list call starting at 0, got %v", client.listCalls)
	}
	if client.watchReq == nil {
		t.Fatal("expected WatchRun call")
	}
	if client.watchReq.SinceSeq != 2 {
		t.Fatalf("WatchRun since_seq should be last historical seq 2, got %d", client.watchReq.SinceSeq)
	}
}

func TestWatch_HistoricalThenLive_Failure(t *testing.T) {
	client := &fakeEventClient{
		live: []*pb.Envelope{
			{RunId: "r2", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf"}}},
			{RunId: "r2", Seq: 2, Payload: &pb.Envelope_RunFailed{RunFailed: &pb.RunFailed{Reason: "boom", Step: "step1"}}},
		},
	}

	var out bytes.Buffer
	err := runWatch(context.Background(), client, "r2", outputModeConcise, &out)
	if err == nil {
		t.Fatal("expected failure error")
	}
	if !strings.Contains(err.Error(), "run failed at step1: boom") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "run failed at step1: boom") {
		t.Fatalf("expected final failure in output, got:\n%s", out.String())
	}
}

func TestWatch_JSONOutput(t *testing.T) {
	client := &fakeEventClient{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "r3", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf", InitialStep: "step1"}}},
					},
				},
			},
		},
		live: []*pb.Envelope{
			{RunId: "r3", Seq: 2, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}

	var out bytes.Buffer
	err := runWatch(context.Background(), client, "r3", outputModeJSON, &out)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON lines, got %d:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
	}
	if !strings.Contains(out.String(), `"runCompleted"`) && !strings.Contains(out.String(), `"runId":"r3"`) {
		t.Fatalf("expected RunCompleted envelope in JSON output:\n%s", out.String())
	}
}

func TestWatch_HistoricalTerminalSkipsLive(t *testing.T) {
	client := &fakeEventClient{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "r4", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf"}}},
						{RunId: "r4", Seq: 2, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
					},
				},
			},
		},
	}

	var out bytes.Buffer
	err := runWatch(context.Background(), client, "r4", outputModeConcise, &out)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if client.watchReq != nil {
		t.Fatal("expected no WatchRun call when terminal event already in history")
	}
}

func TestWatch_WatchReadyIgnored(t *testing.T) {
	client := &fakeEventClient{
		live: []*pb.Envelope{
			{RunId: "r5", Seq: 0, Payload: &pb.Envelope_WatchReady{WatchReady: &pb.WatchReady{}}},
			{RunId: "r5", Seq: 1, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}

	var out bytes.Buffer
	err := runWatch(context.Background(), client, "r5", outputModeConcise, &out)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if strings.Contains(out.String(), "watchReady") || strings.Contains(out.String(), "WatchReady") {
		t.Fatalf("WatchReady sentinel should not be printed:\n%s", out.String())
	}
}

func TestWatch_Pagination(t *testing.T) {
	client := &fakeEventClient{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "r6", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf"}}},
					},
					NextSinceSeq: 1,
				},
			},
			{
				sinceSeq: 1,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "r6", Seq: 2, Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "step1", Adapter: "shell"}}},
					},
					NextSinceSeq: 0,
				},
			},
		},
		live: []*pb.Envelope{
			{RunId: "r6", Seq: 3, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}

	var out bytes.Buffer
	err := runWatch(context.Background(), client, "r6", outputModeConcise, &out)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if len(client.listCalls) != 2 {
		t.Fatalf("expected two list calls, got %v", client.listCalls)
	}
	if client.listCalls[0] != 0 || client.listCalls[1] != 1 {
		t.Fatalf("expected pagination from 0 then 1, got %v", client.listCalls)
	}
	if client.watchReq == nil || client.watchReq.SinceSeq != 2 {
		t.Fatalf("expected WatchRun since_seq=2, got %+v", client.watchReq)
	}
}

func TestWatch_StreamEndsWithoutTerminal(t *testing.T) {
	client := &fakeEventClient{
		live: []*pb.Envelope{
			{RunId: "r7", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf"}}},
		},
	}

	err := runWatch(context.Background(), client, "r7", outputModeConcise, io.Discard)
	if err == nil {
		t.Fatal("expected error when stream ends without terminal event")
	}
	if !strings.Contains(err.Error(), "ended without terminal event") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatch_ListEventsError(t *testing.T) {
	client := &errorEventClient{listErr: errors.New("server unavailable")}
	err := runWatch(context.Background(), client, "r8", outputModeConcise, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "server unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatch_WatchRunError(t *testing.T) {
	client := &errorEventClient{watchErr: errors.New("stream refused")}
	err := runWatch(context.Background(), client, "r9", outputModeConcise, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "stream refused") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatch_ConciseStepLog(t *testing.T) {
	client := &fakeEventClient{
		live: []*pb.Envelope{
			{RunId: "r10", Seq: 1, Payload: &pb.Envelope_StepLog{StepLog: &pb.StepLog{Step: "s1", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "hello\n"}}},
			{RunId: "r10", Seq: 2, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}

	var out bytes.Buffer
	err := runWatch(context.Background(), client, "r10", outputModeConcise, &out)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !strings.Contains(out.String(), "[s1 stdout] hello") {
		t.Fatalf("expected step log line, got:\n%s", out.String())
	}
}

// errorEventClient implements runEventClient and always returns configured errors.
type errorEventClient struct {
	listErr  error
	watchErr error
}

func (e *errorEventClient) ListRunEvents(context.Context, *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return connect.NewResponse(&pb.ListRunEventsResponse{}), nil
}

func (e *errorEventClient) WatchRun(context.Context, *connect.Request[pb.WatchRunRequest]) (eventStream, error) {
	return nil, e.watchErr
}

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// fakeWatchServerService implements criteriav1connect.ServerServiceHandler for
// command-level watch tests. It serves canned historical pages and streams live
// envelopes in response to WatchRun.
type fakeWatchServerService struct {
	pages            []fakePage
	live             []*pb.Envelope
	listCalls        []uint64
	watchRunRequests []*pb.WatchRunRequest
}

func (f *fakeWatchServerService) ListAgents(context.Context, *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) GetAgent(context.Context, *connect.Request[pb.GetAgentRequest]) (*connect.Response[pb.Agent], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) ListRuns(context.Context, *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) GetRun(context.Context, *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) StopRun(context.Context, *connect.Request[pb.StopRunRequest]) (*connect.Response[pb.StopRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) PauseRun(context.Context, *connect.Request[pb.PauseRunRequest]) (*connect.Response[pb.PauseRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) ResumeRun(context.Context, *connect.Request[pb.ResumeRunRequest]) (*connect.Response[pb.ResumeRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) InspectRun(context.Context, *connect.Request[pb.InspectRunRequest]) (*connect.Response[pb.InspectRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) SendPrompt(context.Context, *connect.Request[pb.SendPromptRequest]) (*connect.Response[pb.SendPromptResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) SubmitWorkflowAssignment(context.Context, *connect.Request[pb.SubmitWorkflowAssignmentRequest]) (*connect.Response[pb.SubmitWorkflowAssignmentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeWatchServerService) GetAssignmentDisposition(context.Context, *connect.Request[pb.GetAssignmentDispositionRequest]) (*connect.Response[pb.GetAssignmentDispositionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (f *fakeWatchServerService) ListRunEvents(_ context.Context, req *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	f.listCalls = append(f.listCalls, req.Msg.SinceSeq)
	for i, page := range f.pages {
		if page.sinceSeq == req.Msg.SinceSeq {
			f.pages = append(f.pages[:i], f.pages[i+1:]...)
			return connect.NewResponse(page.resp), nil
		}
	}
	return connect.NewResponse(&pb.ListRunEventsResponse{}), nil
}

func (f *fakeWatchServerService) WatchRun(_ context.Context, req *connect.Request[pb.WatchRunRequest], stream *connect.ServerStream[pb.Envelope]) error {
	f.watchRunRequests = append(f.watchRunRequests, req.Msg)
	for _, env := range f.live {
		if err := stream.Send(env); err != nil {
			return err
		}
	}
	return nil
}

func startWatchFakeServer(t *testing.T, handler *fakeWatchServerService) string {
	t.Helper()
	mux := http.NewServeMux()
	path, h := criteriav1connect.NewServerServiceHandler(handler)
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &protocols
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestWatchCmd_HistoricalThenLive_Success(t *testing.T) {
	fake := &fakeWatchServerService{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "run-1", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf", InitialStep: "step1"}}},
						{RunId: "run-1", Seq: 2, Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "step1", Adapter: "shell", Attempt: 1}}},
					},
					NextSinceSeq: 0,
				},
			},
		},
		live: []*pb.Envelope{
			{RunId: "run-1", Seq: 3, Payload: &pb.Envelope_StepOutcome{StepOutcome: &pb.StepOutcome{Step: "step1", Outcome: "success", DurationMs: 100}}},
			{RunId: "run-1", Seq: 4, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-1", "--output", "concise"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("watch cmd: %v\noutput:\n%s", err, out.String())
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
	if len(fake.listCalls) != 1 || fake.listCalls[0] != 0 {
		t.Fatalf("expected one list call starting at 0, got %v", fake.listCalls)
	}
	if len(fake.watchRunRequests) != 1 {
		t.Fatalf("expected one WatchRun request, got %d", len(fake.watchRunRequests))
	}
	if fake.watchRunRequests[0].SinceSeq != 2 {
		t.Fatalf("WatchRun since_seq should be 2, got %d", fake.watchRunRequests[0].SinceSeq)
	}
}

func TestWatchCmd_JSONOutput(t *testing.T) {
	fake := &fakeWatchServerService{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "run-json", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf"}}},
					},
				},
			},
		},
		live: []*pb.Envelope{
			{RunId: "run-json", Seq: 2, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-json", "--output", "json"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("watch cmd: %v\noutput:\n%s", err, out.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON lines, got %d:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "{") {
			t.Fatalf("line %d is not JSON: %s", i, line)
		}
	}
}

func TestWatchCmd_PositionalArg(t *testing.T) {
	fake := &fakeWatchServerService{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "run-pos", Seq: 1, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
					},
				},
			},
		},
	}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url, "run-pos"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("watch cmd: %v", err)
	}
	if len(fake.listCalls) != 1 {
		t.Fatalf("expected one list call, got %v", fake.listCalls)
	}
}

func TestWatchCmd_MissingRunID(t *testing.T) {
	fake := &fakeWatchServerService{}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing --run-id")
	}
	if !strings.Contains(err.Error(), "--run-id is required") {
		t.Fatalf("expected missing --run-id error, got: %v", err)
	}
}

func TestWatchCmd_TooManyPositionalArgs(t *testing.T) {
	fake := &fakeWatchServerService{}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url, "run-a", "run-b"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for too many positional args")
	}
	if !strings.Contains(err.Error(), "accepts at most one run-id argument") {
		t.Fatalf("expected too-many-args error, got: %v", err)
	}
}

func TestWatchCmd_PositionalArgMismatch(t *testing.T) {
	fake := &fakeWatchServerService{}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-a", "run-b"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for positional arg / --run-id mismatch")
	}
	if !strings.Contains(err.Error(), "run-id argument and --run-id must match") {
		t.Fatalf("expected mismatch error, got: %v", err)
	}
}

func TestWatchCmd_HistoricalTerminalSkipsLive(t *testing.T) {
	fake := &fakeWatchServerService{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "run-term", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf"}}},
						{RunId: "run-term", Seq: 2, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
					},
				},
			},
		},
	}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-term", "--output", "concise"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("watch cmd: %v", err)
	}
	if len(fake.watchRunRequests) != 0 {
		t.Fatalf("expected no WatchRun call when terminal event already in history, got %d", len(fake.watchRunRequests))
	}
	if !strings.Contains(out.String(), "run completed: done") {
		t.Fatalf("expected final status in output:\n%s", out.String())
	}
}

func TestWatchCmd_StreamEndsWithoutTerminal(t *testing.T) {
	fake := &fakeWatchServerService{
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "run-incomplete", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf"}}},
					},
				},
			},
		},
		live: []*pb.Envelope{
			{RunId: "run-incomplete", Seq: 2, Payload: &pb.Envelope_StepOutcome{StepOutcome: &pb.StepOutcome{Step: "s1", Outcome: "success"}}},
		},
	}
	url := startWatchFakeServer(t, fake)

	var out bytes.Buffer
	cmd := NewWatchCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-incomplete"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when stream ends without terminal event")
	}
	if !strings.Contains(err.Error(), "ended without terminal event") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// unusedCompileCheck prevents the compiler from complaining if any of the
// required handler methods change signature; the fake must remain exhaustive.
func unusedCompileCheck() {
	var _ criteriav1connect.ServerServiceHandler = (*fakeWatchServerService)(nil)
	_ = timestamppb.Now
}

var _ = unusedCompileCheck

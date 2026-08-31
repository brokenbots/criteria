package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// fakeServerService implements criteriav1connect.ServerServiceHandler for testing
// pause/resume/inspect CLI commands.
type fakeServerService struct {
	pausedRuns    map[string]bool
	resumedRuns   map[string]bool
	inspectedRuns map[string]*pb.InspectRunResponse
}

func newFakeServerService() *fakeServerService {
	return &fakeServerService{
		pausedRuns:    make(map[string]bool),
		resumedRuns:   make(map[string]bool),
		inspectedRuns: make(map[string]*pb.InspectRunResponse),
	}
}

func (f *fakeServerService) ListAgents(context.Context, *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) GetAgent(context.Context, *connect.Request[pb.GetAgentRequest]) (*connect.Response[pb.Agent], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) ListRuns(context.Context, *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) GetRun(context.Context, *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) ListRunEvents(context.Context, *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) WatchRun(context.Context, *connect.Request[pb.WatchRunRequest], *connect.ServerStream[pb.Envelope]) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) StopRun(context.Context, *connect.Request[pb.StopRunRequest]) (*connect.Response[pb.StopRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) PauseRun(_ context.Context, req *connect.Request[pb.PauseRunRequest]) (*connect.Response[pb.PauseRunResponse], error) {
	f.pausedRuns[req.Msg.RunId] = true
	return connect.NewResponse(&pb.PauseRunResponse{IssuedAt: timestamppb.New(time.Now())}), nil
}
func (f *fakeServerService) ResumeRun(_ context.Context, req *connect.Request[pb.ResumeRunRequest]) (*connect.Response[pb.ResumeRunResponse], error) {
	f.resumedRuns[req.Msg.RunId] = true
	return connect.NewResponse(&pb.ResumeRunResponse{IssuedAt: timestamppb.New(time.Now())}), nil
}
func (f *fakeServerService) InspectRun(_ context.Context, req *connect.Request[pb.InspectRunRequest]) (*connect.Response[pb.InspectRunResponse], error) {
	if resp, ok := f.inspectedRuns[req.Msg.RunId]; ok {
		return connect.NewResponse(resp), nil
	}
	// Default response
	return connect.NewResponse(&pb.InspectRunResponse{
		RunId:       req.Msg.RunId,
		SessionId:   req.Msg.SessionId,
		CurrentStep: "generate_outline",
		StateJson:   `{"turns_taken":4,"tools_invoked":["read_file","edit_file"]}`,
	}), nil
}
func (f *fakeServerService) SendPrompt(context.Context, *connect.Request[pb.SendPromptRequest]) (*connect.Response[pb.SendPromptResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) SubmitWorkflowAssignment(context.Context, *connect.Request[pb.SubmitWorkflowAssignmentRequest]) (*connect.Response[pb.SubmitWorkflowAssignmentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeServerService) GetAssignmentDisposition(context.Context, *connect.Request[pb.GetAssignmentDispositionRequest]) (*connect.Response[pb.GetAssignmentDispositionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func startFakeServer(t *testing.T, handler *fakeServerService) string {
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

func TestPauseCmd_OK(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)

	cmd := NewPauseCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("pause cmd: %v", err)
	}
	if !fake.pausedRuns["run-123"] {
		t.Fatal("expected run-123 to be paused")
	}
}

func TestPauseCmd_MissingRunID(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)

	cmd := NewPauseCmd()
	cmd.SetArgs([]string{"--server", url})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing --run-id")
	}
	if !strings.Contains(err.Error(), "--run-id is required") {
		t.Fatalf("expected missing --run-id error, got: %v", err)
	}
}

func TestResumeCmd_OK(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)

	cmd := NewResumeCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-456"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("resume cmd: %v", err)
	}
	if !fake.resumedRuns["run-456"] {
		t.Fatal("expected run-456 to be resumed")
	}
}

func TestResumeCmd_MissingRunID(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)

	cmd := NewResumeCmd()
	cmd.SetArgs([]string{"--server", url})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing --run-id")
	}
	if !strings.Contains(err.Error(), "--run-id is required") {
		t.Fatalf("expected missing --run-id error, got: %v", err)
	}
}

func TestInspectCmd_Default(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)

	cmd := NewInspectCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-789"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("inspect cmd: %v", err)
	}
}

func TestInspectCmd_WithSession(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)

	cmd := NewInspectCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-789", "--session", "sess-abc"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("inspect cmd: %v", err)
	}
}

func TestInspectCmd_MissingRunID(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)

	cmd := NewInspectCmd()
	cmd.SetArgs([]string{"--server", url})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing --run-id")
	}
	if !strings.Contains(err.Error(), "--run-id is required") {
		t.Fatalf("expected missing --run-id error, got: %v", err)
	}
}

func TestInspectCmd_RenderCustomState(t *testing.T) {
	fake := newFakeServerService()
	fake.inspectedRuns["run-999"] = &pb.InspectRunResponse{
		RunId:              "run-999",
		SessionId:          "sess-xyz",
		CurrentStep:        "review_output",
		PendingPermissions: 2,
		LastActivityAt:     timestamppb.New(time.Now()),
		StateJson:          `{"turns_taken":7,"last_user_message":"make it shorter"}`,
		Adapter:            "claude.assistant",
	}
	url := startFakeServer(t, fake)

	cmd := NewInspectCmd()
	cmd.SetArgs([]string{"--server", url, "--run-id", "run-999"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("inspect cmd: %v", err)
	}
}

func TestInspectCmd_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	// No handler registered — requests return 404
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cmd := NewInspectCmd()
	cmd.SetArgs([]string{"--server", srv.URL, "--run-id", "run-err"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from missing handler")
	}
}

func TestPauseResume_RoundTrip(t *testing.T) {
	fake := newFakeServerService()
	url := startFakeServer(t, fake)
	runID := uuid.New().String()

	pauseCmd := NewPauseCmd()
	pauseCmd.SetArgs([]string{"--server", url, "--run-id", runID})
	if err := pauseCmd.Execute(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !fake.pausedRuns[runID] {
		t.Fatal("expected run to be paused")
	}

	resumeCmd := NewResumeCmd()
	resumeCmd.SetArgs([]string{"--server", url, "--run-id", runID})
	if err := resumeCmd.Execute(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !fake.resumedRuns[runID] {
		t.Fatal("expected run to be resumed")
	}
}

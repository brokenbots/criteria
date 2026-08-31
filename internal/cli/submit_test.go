package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// submitMinimalWorkflow is a valid workflow that compiles without adapter
// schemas, so tests do not need to build or discover adapter binaries.
const submitMinimalWorkflow = `
workflow {
  name          = "submit_minimal"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}
`

// fakeSubmitServerService implements the ServerService handlers needed for
// submit tests, recording SubmitWorkflowAssignment requests and serving
// canned ListRunEvents/WatchRun streams when --watch is exercised.
type fakeSubmitServerService struct {
	submitReqs []*pb.SubmitWorkflowAssignmentRequest
	submitResp *pb.SubmitWorkflowAssignmentResponse
	submitErr  error

	pages []fakePage
	live  []*pb.Envelope
}

func (f *fakeSubmitServerService) SubmitWorkflowAssignment(_ context.Context, req *connect.Request[pb.SubmitWorkflowAssignmentRequest]) (*connect.Response[pb.SubmitWorkflowAssignmentResponse], error) {
	f.submitReqs = append(f.submitReqs, req.Msg)
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	return connect.NewResponse(f.submitResp), nil
}

func (f *fakeSubmitServerService) ListRunEvents(_ context.Context, req *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	for i, page := range f.pages {
		if page.sinceSeq == req.Msg.SinceSeq {
			f.pages = append(f.pages[:i], f.pages[i+1:]...)
			return connect.NewResponse(page.resp), nil
		}
	}
	return connect.NewResponse(&pb.ListRunEventsResponse{}), nil
}

func (f *fakeSubmitServerService) WatchRun(_ context.Context, _ *connect.Request[pb.WatchRunRequest], stream *connect.ServerStream[pb.Envelope]) error {
	for _, env := range f.live {
		if err := stream.Send(env); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSubmitServerService) ListAgents(context.Context, *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) GetAgent(context.Context, *connect.Request[pb.GetAgentRequest]) (*connect.Response[pb.Agent], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) ListRuns(context.Context, *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) GetRun(context.Context, *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) StopRun(context.Context, *connect.Request[pb.StopRunRequest]) (*connect.Response[pb.StopRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) PauseRun(context.Context, *connect.Request[pb.PauseRunRequest]) (*connect.Response[pb.PauseRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) ResumeRun(context.Context, *connect.Request[pb.ResumeRunRequest]) (*connect.Response[pb.ResumeRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) InspectRun(context.Context, *connect.Request[pb.InspectRunRequest]) (*connect.Response[pb.InspectRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) SendPrompt(context.Context, *connect.Request[pb.SendPromptRequest]) (*connect.Response[pb.SendPromptResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}
func (f *fakeSubmitServerService) GetAssignmentDisposition(context.Context, *connect.Request[pb.GetAssignmentDispositionRequest]) (*connect.Response[pb.GetAssignmentDispositionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func startSubmitFakeServer(t *testing.T, handler *fakeSubmitServerService) string {
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

func TestSubmitCmd_HappyPath_PrintsRunID(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitResp: &pb.SubmitWorkflowAssignmentResponse{
			RunId: "run-submit-1",
			State: pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		},
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	var out bytes.Buffer
	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("submit cmd: %v\noutput:\n%s", err, out.String())
	}

	if got := strings.TrimSpace(out.String()); got != "run-submit-1" {
		t.Fatalf("expected run ID printed, got %q", got)
	}
	if len(handler.submitReqs) != 1 {
		t.Fatalf("expected one submit request, got %d", len(handler.submitReqs))
	}
	req := handler.submitReqs[0]
	if req.WorkflowName != "submit_minimal" {
		t.Errorf("workflow name = %q, want submit_minimal", req.WorkflowName)
	}
	if !strings.Contains(req.WorkflowSource, "workflow {") {
		t.Errorf("workflow source missing workflow block")
	}
	if req.LockfileSource != "" {
		t.Errorf("expected empty lockfile source for workflow without lockfile, got %q", req.LockfileSource)
	}
}

func TestSubmitCmd_LabelsAndIdempotencyKey(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitResp: &pb.SubmitWorkflowAssignmentResponse{
			RunId: "run-submit-2",
			State: pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		},
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{
		"--server", url,
		"--label", "env=prod",
		"--label", "app=demo",
		"--idempotency-key", "key-123",
		workflowPath,
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("submit cmd: %v", err)
	}

	req := handler.submitReqs[0]
	if len(req.Labels) != 2 || req.Labels["env"] != "prod" || req.Labels["app"] != "demo" {
		t.Errorf("labels not passed through correctly: %v", req.Labels)
	}
	if req.IdempotencyKey != "key-123" {
		t.Errorf("idempotency key = %q, want key-123", req.IdempotencyKey)
	}
}

func TestSubmitCmd_IncludesLockfileSource(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitResp: &pb.SubmitWorkflowAssignmentResponse{
			RunId: "run-submit-3",
			State: pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		},
	}
	url := startSubmitFakeServer(t, handler)

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte(submitMinimalWorkflow), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	lockfilePath := filepath.Join(dir, ".criteria.lock.hcl")
	lockSrc := "adapter_pin \"noop\" \"default\" {\n  reference = \"v1\"\n}\n"
	if err := os.WriteFile(lockfilePath, []byte(lockSrc), 0o600); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}

	var out bytes.Buffer
	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("submit cmd: %v\n%s", err, out.String())
	}

	req := handler.submitReqs[0]
	if req.LockfileSource != lockSrc {
		t.Errorf("lockfile source = %q, want %q", req.LockfileSource, lockSrc)
	}
}

func TestSubmitCmd_InvalidWorkflow_ParseError_Exit2(t *testing.T) {
	handler := &fakeSubmitServerService{}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, `not valid hcl {`)

	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid workflow")
	}
	var se *submitError
	if !errors.As(err, &se) || se.code != exitInvalidWorkflow {
		t.Fatalf("expected exit code %d, got error: %v", exitInvalidWorkflow, err)
	}
	if OSErrorCode(err) != exitInvalidWorkflow {
		t.Fatalf("OSErrorCode = %d, want %d", OSErrorCode(err), exitInvalidWorkflow)
	}
	if len(handler.submitReqs) != 0 {
		t.Fatalf("expected no server request for invalid workflow")
	}
}

func TestSubmitCmd_InvalidWorkflow_CompileError_Exit2(t *testing.T) {
	handler := &fakeSubmitServerService{}
	url := startSubmitFakeServer(t, handler)
	// Missing terminal state makes compilation fail.
	workflowPath := writeWorkflowFile(t, `
workflow {
  name = "bad"
  initial_state = "missing"
  target_state = "missing"
}
`)

	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid workflow")
	}
	var se *submitError
	if !errors.As(err, &se) || se.code != exitInvalidWorkflow {
		t.Fatalf("expected exit code %d, got error: %v", exitInvalidWorkflow, err)
	}
}

func TestSubmitCmd_RejectedResponse_Exit4(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitResp: &pb.SubmitWorkflowAssignmentResponse{
			RunId:           "run-rejected",
			State:           pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED,
			RejectionReason: "no matching agent",
		},
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	var out bytes.Buffer
	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for rejected submission")
	}
	var se *submitError
	if !errors.As(err, &se) || se.code != exitDuplicateSubmission {
		t.Fatalf("expected exit code %d, got error: %v", exitDuplicateSubmission, err)
	}
	if !strings.Contains(err.Error(), "no matching agent") {
		t.Errorf("error should include rejection reason: %v", err)
	}
}

func TestSubmitCmd_ServerAlreadyExists_Exit4(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitErr: connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("duplicate idempotency key")),
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for duplicate submission")
	}
	if OSErrorCode(err) != exitDuplicateSubmission {
		t.Fatalf("OSErrorCode = %d, want %d", OSErrorCode(err), exitDuplicateSubmission)
	}
}

func TestSubmitCmd_ServerPermissionDenied_Exit5(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitErr: connect.NewError(connect.CodePermissionDenied, fmt.Errorf("invalid token")),
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for permission denied")
	}
	if OSErrorCode(err) != exitAuthFailure {
		t.Fatalf("OSErrorCode = %d, want %d", OSErrorCode(err), exitAuthFailure)
	}
}

func TestSubmitCmd_UnreachableServer_Exit3(t *testing.T) {
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:1", workflowPath})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	if OSErrorCode(err) != exitServerUnreachable {
		t.Fatalf("OSErrorCode = %d, want %d", OSErrorCode(err), exitServerUnreachable)
	}
}

func TestSubmitCmd_Watch_PrintsRunIDAndTerminalStatus(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitResp: &pb.SubmitWorkflowAssignmentResponse{
			RunId: "run-watch-1",
			State: pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		},
		live: []*pb.Envelope{
			{RunId: "run-watch-1", Seq: 1, Payload: &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "submit_minimal"}}},
			{RunId: "run-watch-1", Seq: 2, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
		},
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	var out bytes.Buffer
	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, "--watch", "--output", "concise", workflowPath})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("submit cmd: %v\n%s", err, out.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected run id line + watch output, got:\n%s", out.String())
	}
	if lines[0] != "run-watch-1" {
		t.Errorf("first line should be run id, got %q", lines[0])
	}
	if !strings.Contains(out.String(), "run completed: done") {
		t.Errorf("expected terminal status in watch output:\n%s", out.String())
	}
}

func TestSubmitCmd_Watch_HistoricalTerminal_SkipsLive(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitResp: &pb.SubmitWorkflowAssignmentResponse{
			RunId: "run-watch-2",
			State: pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		},
		pages: []fakePage{
			{
				sinceSeq: 0,
				resp: &pb.ListRunEventsResponse{
					Events: []*pb.Envelope{
						{RunId: "run-watch-2", Seq: 1, Payload: &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{FinalState: "done", Success: true}}},
					},
				},
			},
		},
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	var out bytes.Buffer
	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, "--watch", "--output", "concise", workflowPath})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("submit cmd: %v\n%s", err, out.String())
	}

	if !strings.HasPrefix(strings.TrimSpace(out.String()), "run-watch-2\n") {
		t.Errorf("expected run id preserved before watch output:\n%s", out.String())
	}
}

func TestSubmitCmd_EmptyRunID_Exit2(t *testing.T) {
	handler := &fakeSubmitServerService{
		submitResp: &pb.SubmitWorkflowAssignmentResponse{
			RunId: "",
			State: pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		},
	}
	url := startSubmitFakeServer(t, handler)
	workflowPath := writeWorkflowFile(t, submitMinimalWorkflow)

	cmd := NewSubmitCmd()
	cmd.SetArgs([]string{"--server", url, workflowPath})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for empty run id")
	}
	if OSErrorCode(err) != exitInvalidWorkflow {
		t.Fatalf("OSErrorCode = %d, want %d", OSErrorCode(err), exitInvalidWorkflow)
	}
}

// unusedSubmitCompileCheck prevents the compiler from complaining if any of the
// required handler methods change signature; the fake must remain exhaustive.
func unusedSubmitCompileCheck() {
	var _ criteriav1connect.ServerServiceHandler = (*fakeSubmitServerService)(nil)
	_ = timestamppb.Now
	_ = io.Discard
}

var _ = unusedSubmitCompileCheck

package peer

// Serve-adapter child-role tests (ADR-0008): the phone-home server built by
// NewServeAdapterServer registers the in-process adapter unwrapped, streams
// supervision from the caller-supplied journal, routes CancelChildRun
// control to the adapter's ChildRunCanceler surface, and ends the loop —
// without reconnecting — when the adapter requests process exit.

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	adapterhost "github.com/brokenbots/criteria/internal/adapterhost"
)

// fakeWorkflowAdapter is the in-process adapter surface for serve-adapter
// server tests: only the methods under test are stubbed.
type fakeWorkflowAdapter struct {
	adapterhost.Client

	mu             sync.Mutex
	infoCalls      int
	openSessions   []string
	closedSessions []string
	cancelRequests []string
	acceptedRunID  string
}

func (f *fakeWorkflowAdapter) Info(ctx context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	return &v2.InfoResponse{Name: "workflow.child", Version: "1.0.0"}, nil
}

func (f *fakeWorkflowAdapter) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openSessions = append(f.openSessions, req.GetSessionId())
	return &v2.OpenSessionResponse{}, nil
}

func (f *fakeWorkflowAdapter) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedSessions = append(f.closedSessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

// CancelChildRun implements ChildRunCanceler: it accepts exactly the run id
// acceptedRunID (a concrete in-flight run); anything else is not in-flight.
func (f *fakeWorkflowAdapter) CancelChildRun(runID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelRequests = append(f.cancelRequests, runID)
	if f.acceptedRunID == "" || (runID != "" && runID != f.acceptedRunID) {
		return "", false
	}
	return f.acceptedRunID, true
}

// newServeAdapterFixture builds a serve-adapter-role server over the shared
// peerServeFixture transport helpers (rt/child stay nil/unused).
func newServeAdapterFixture(t *testing.T, journal *EventJournal) *peerServeFixture {
	t.Helper()
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "workflow.child"
	cfg.Token = "tok"
	cfg.Scope = "sc"
	cfg.Digest = "sha256:aa"
	impl := &fakeWorkflowAdapter{acceptedRunID: "child-run-1"}
	server := NewServeAdapterServer(&cfg, impl, journal, captureLogger(&bytes.Buffer{}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &peerServeFixture{t: t, cfg: cfg, server: server, impl: impl, ctx: ctx, cancel: cancel}
}

func TestServeAdapterServer_ServesInProcessAdapter(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)
	client := adapterhost.NewClientForConn(cc)
	defer f.cancel()

	// The in-process adapter answers Info/OpenSession unwrapped: no
	// serveChildClient layering (the child-run arms come from the CLI's own
	// journal, not the runtime's).
	resp, err := client.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if resp.GetName() != "workflow.child" || resp.GetVersion() != "1.0.0" {
		t.Errorf("Info = %q/%q, want workflow.child/1.0.0", resp.GetName(), resp.GetVersion())
	}
	if _, err := client.OpenSession(context.Background(), &v2.OpenSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	f.impl.mu.Lock()
	opens := len(f.impl.openSessions)
	f.impl.mu.Unlock()
	if opens != 1 {
		t.Errorf("adapter saw %d OpenSession calls, want 1", opens)
	}
}

func TestServeAdapterServer_SuperviseStreamsCallerJournal(t *testing.T) {
	f := newServeAdapterFixture(t, NewEventJournal(0))
	ev, err := f.server.Journal().Append(&criteriav1.SupervisionEvent_ChildRunStarted{
		ChildRunStarted: &criteriav1.ChildRunStarted{
			RunId:          "child-run-1",
			WorkflowDigest: "sha256:aa",
		},
	}, f.cfg.AdapterName, f.cfg.Scope, "wf")
	if err != nil {
		t.Fatalf("journal append: %v", err)
	}

	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)
	ss := f.openSupervise(cc, ev.GetEventSeq()-1)
	got := f.mustRecvSupervisionEvent(ss, 5*time.Second)
	if got.GetChildRunStarted() == nil || got.GetChildRunStarted().GetRunId() != "child-run-1" {
		t.Fatalf("supervision event = %+v, want child_run_started for child-run-1", got.GetKind())
	}
	f.cancel()
}

func TestServeAdapterServer_ControlCancelChildRun(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	ctx := context.Background()

	// Cancellation routes to the adapter's canceler: accepted when an
	// in-flight child run matches, rejected when nothing is in-flight.
	f.impl.mu.Lock()
	f.impl.acceptedRunID = "child-run-1"
	f.impl.mu.Unlock()

	resp, err := f.server.Control(ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{RunId: "child-run-1"}},
	})
	if err != nil {
		t.Fatalf("CancelChildRun: %v", err)
	}
	if !resp.GetAccepted() {
		t.Errorf("CancelChildRun accepted = %v, detail %q, want accepted", resp.GetAccepted(), resp.GetDetail())
	}
	f.impl.mu.Lock()
	reqs := append([]string(nil), f.impl.cancelRequests...)
	f.impl.mu.Unlock()
	if len(reqs) != 1 || reqs[0] != "child-run-1" {
		t.Errorf("cancel requests = %q, want one for \"child-run-1\"", reqs)
	}

	// An unknown run id is accepted=false with a named detail.
	resp, err = f.server.Control(ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{RunId: "missing"}},
	})
	if err != nil {
		t.Fatalf("CancelChildRun(missing): %v", err)
	}
	if resp.GetAccepted() {
		t.Errorf("CancelChildRun(missing) accepted = %v, want rejected", resp.GetAccepted())
	}
	if resp.GetDetail() == "" {
		t.Error("CancelChildRun(missing) detail empty, want a name")
	}

	// KillChild targets a spawned process; serve-adapter mode has none.
	resp, err = f.server.Control(ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	})
	if err != nil {
		t.Fatalf("KillChild: %v", err)
	}
	if resp.GetAccepted() {
		t.Errorf("KillChild accepted = %v, want rejected", resp.GetAccepted())
	}
}

func TestServeAdapterServer_ControlWithoutCanceler(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "workflow.child"
	// An adapter that does NOT implement ChildRunCanceler: control request
	// is rejected with a detail, not a Go-side error.
	server := NewServeAdapterServer(&cfg, &fakeWorkflowAdapter{}, nil, captureLogger(&bytes.Buffer{}))
	resp := server.controlServeAdapter(&criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{}},
	})
	if resp.GetAccepted() {
		t.Errorf("CancelChildRun on non-canceler adapter accepted = %v", resp.GetAccepted())
	}
	if resp.GetDetail() == "" {
		t.Error("CancelChildRun on non-canceler adapter detail empty")
	}
}

func TestServeAdapterServer_ControlWorkflowGate(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	// Simulate an older peer that did not negotiate workflow.v1.
	f.server.capabilities = []string{peerAdapterV2FullCapability}

	_, err := f.server.Control(context.Background(), &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{}},
	})
	if err == nil {
		t.Fatal("CancelChildRun without workflow.v1 negotiated: want Unimplemented error")
	}
}

func TestServeAdapterServer_RequestExitEndsRunWithoutReconnecting(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	conn, _, serveErr := f.startConn()
	defer conn.Close()

	// A CloseSession teardown requests process exit; the loop must stop
	// serving and return nil — no reconnect, no second dial (the fixture's
	// one-shot dialer would error a second attempt).
	f.server.RequestExit()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after RequestExit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after RequestExit")
	}
	if !f.server.ExitRequested() {
		t.Fatal("ExitRequested = false after RequestExit")
	}
}

func TestServeAdapterServer_ExitRequestedDefaultsFalse(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	if f.server.ExitRequested() {
		t.Fatal("ExitRequested = true before any request")
	}
}

func TestServeAdapterServer_ImplicitNilJournal(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "workflow.child"
	impl := &fakeWorkflowAdapter{}
	server := NewServeAdapterServer(&cfg, impl, nil, captureLogger(&bytes.Buffer{}))
	if server.Journal() == nil {
		t.Fatal("Journal() = nil, want the implicit empty journal")
	}
	if server.impl != impl {
		t.Fatal("NewServeAdapterServer did not hold the provided adapter")
	}
}

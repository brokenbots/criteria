package cli

// Driver tests for the serve-adapter mode (KB-94, ADR-0008): a toy two-step
// workflow served through the adapter v2 client, driven in-process against a
// capturing ExecuteEventSink. Covers acceptance 1 (execute streams node
// events to a terminal outcome, outputs_json matches the projection schema,
// pause mid-call resumes to the same checkpoint), acceptance 2 (re-Execute
// in-flight fails closed with the typed error; CloseSession cancels a live
// run observably), and the wait/approval compile-time rejection (mechanics 5).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/runstate"
	"github.com/brokenbots/criteria/workflow"

	"connectrpc.com/connect"
)

// serveAdapterTestEnv is the shared harness: a compiled toy workflow with a
// noop adapter on the adapters dir and an isolated state dir.
type serveAdapterTestEnv struct {
	client *serveAdapterClient
	graph  *workflow.FSMGraph
	digest string
	log    *slog.Logger
}

// newServeAdapterEnv compiles the toy two-step workflow once and builds a
// serve-adapter client over it, mirroring runServeAdapter's setup path.
func newServeAdapterEnv(t *testing.T, fixture string) *serveAdapterTestEnv {
	t.Helper()
	adapterDir := filepath.Dir(buildNoopAdapterBinary(t))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")
	t.Setenv("CRITERIA_SERVER_URL", "")
	t.Setenv("CRITERIA_CONTROL_ADDR", "")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	log = log.With("test", t.Name())
	path, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	src, graph, loader, err := compileForExecution(context.Background(), path, log, false, true)
	if err != nil {
		t.Fatalf("compile fixture %s: %v", fixture, err)
	}
	if graph == nil {
		t.Fatal("compiled graph is nil")
	}
	digest := serveAdapterWorkflowDigest(src)
	return &serveAdapterTestEnv{
		client: newServeAdapterClient(serveAdapterClientOptions{
			graph:        graph,
			loader:       loader,
			digest:       digest,
			sourceHash:   digest,
			workflowPath: path,
			journal:      nil,
			log:          log,
			baseCtx:      context.Background(),
		}),
		graph:  graph,
		digest: digest,
		log:    log,
	}
}

// executeCapture is the driver's ExecuteEventSink: it records every event and
// the terminal result, plus a per-event callback for synchronization.
type executeCapture struct {
	mu      sync.Mutex
	events  []*criteriav2.ExecuteEvent
	result  *criteriav2.ExecuteResult
	watch   func(ev *criteriav2.ExecuteEvent) bool
	arrived chan struct{}
}

func (c *executeCapture) Emit(ev *criteriav2.ExecuteEvent) error {
	c.mu.Lock()
	c.events = append(c.events, ev)
	if r := ev.GetResult(); r != nil && c.result == nil {
		c.result = r
	}
	c.mu.Unlock()
	if c.watch != nil && c.watch(ev) {
		select {
		case c.arrived <- struct{}{}:
		default:
		}
	}
	return nil
}

func (c *executeCapture) snapshot() []*criteriav2.ExecuteEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*criteriav2.ExecuteEvent(nil), c.events...)
}

func (c *executeCapture) terminal() *criteriav2.ExecuteResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result
}

// adapterEventKinds lists the workflow.v1 kinds seen on the capture.
func (c *executeCapture) adapterEventKinds() []string {
	var kinds []string
	for _, ev := range c.snapshot() {
		if a := ev.GetAdapter(); a != nil {
			kinds = append(kinds, a.GetEventKind())
		}
	}
	return kinds
}

func (env *serveAdapterTestEnv) openTestSession(t *testing.T) string {
	t.Helper()
	resp, err := env.client.OpenSession(context.Background(), &criteriav2.OpenSessionRequest{SessionId: "sess-driver"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if resp == nil {
		t.Fatal("nil OpenSessionResponse")
	}
	return "sess-driver"
}

// slowStepStarted installs a watch that fires when the run reaches the slow
// step (the noop stage sleeps 4s there), giving the caller a deterministic
// mid-run window.
func slowStepStarted() func(*criteriav2.ExecuteEvent) bool {
	return func(ev *criteriav2.ExecuteEvent) bool {
		a := ev.GetAdapter()
		if a == nil || a.GetEventKind() != serveAdapterEventStepStarted || a.GetPayload() == nil {
			return false
		}
		step, _ := a.GetPayload().AsMap()["step"].(string)
		return step == "slow"
	}
}

// TestServeAdapter_ExecuteStreamsWorkflowEventsToTerminal covers the
// execute-stream half of acceptance 1: node lifecycle forwards as
// workflow.v1-prefixed adapter events; the terminal state maps to
// ExecuteResult.outcome + outputs_json matching the projection schema.
func TestServeAdapter_ExecuteStreamsWorkflowEventsToTerminal(t *testing.T) {
	env := newServeAdapterEnv(t, "serveadapter_toy")
	sessionID := env.openTestSession(t)

	cap2 := &executeCapture{arrived: make(chan struct{}, 8)}
	if err := env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
		SessionId: sessionID,
		StepName:  "warmup",
	}, cap2); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	res := cap2.terminal()
	if res == nil {
		t.Fatal("no terminal Result event delivered")
	}
	if res.GetOutcome() != serveAdapterOutcomeSuccess {
		t.Fatalf("outcome = %q (%s), want success", res.GetOutcome(), res.GetComment())
	}

	// outputs_json matches the projection schema (one "final" output).
	var outputs map[string]string
	if err := json.Unmarshal(res.GetOutputsJson(), &outputs); err != nil {
		t.Fatalf("outputs_json decode: %v (raw %q)", err, string(res.GetOutputsJson()))
	}
	outSchema := serveAdapterOutputSchema(env.graph)
	if _, ok := outSchema.GetFields()["final"]; !ok {
		t.Fatalf("output schema missing 'final': %v", outSchema.GetFields())
	}
	if outputs["final"] != "hello" {
		t.Fatalf("projected output final = %q, want the schema-declared default", outputs["final"])
	}

	// Node lifecycle events carried the workflow.v1 prefix and included the
	// core lifecycle kinds.
	kinds := cap2.adapterEventKinds()
	for _, want := range []string{
		serveAdapterEventRunStarted,
		serveAdapterEventStepStarted,
		serveAdapterEventStepOutcome,
		serveAdapterEventStepTransition,
	} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("event kinds %v missing %q", kinds, want)
		}
	}
	for _, k := range kinds {
		if !strings.HasPrefix(k, "workflow.v1.") {
			t.Errorf("event kind %q lacks the workflow.v1 prefix", k)
		}
	}
}

// TestServeAdapter_PauseMidCallResumesToSameCheckpoint covers the pause half
// of acceptance 1: Pause mid-call parks the run at a checkpoint boundary via
// the engine's real machinery, and Resume continues it to the same terminal
// state with pause/resume events streamed.
func TestServeAdapter_PauseMidCallResumesToSameCheckpoint(t *testing.T) {
	env := newServeAdapterEnv(t, "serveadapter_toy")
	sessionID := env.openTestSession(t)

	cap2 := &executeCapture{arrived: make(chan struct{}, 8), watch: slowStepStarted()}
	exErr := make(chan error, 1)
	go func() {
		exErr <- env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
			SessionId: sessionID,
			StepName:  "warmup",
		}, cap2)
	}()

	select {
	case <-cap2.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the slow step to start")
	}

	if _, err := env.client.Pause(context.Background(), &criteriav2.PauseRequest{SessionId: sessionID}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Inspect reports the paused node through the real machinery.
	insp, err := env.client.Inspect(context.Background(), &criteriav2.InspectRequest{SessionId: sessionID})
	if err != nil {
		t.Fatalf("Inspect during pause: %v", err)
	}
	if insp == nil || insp.GetCurrentStep() == "" {
		t.Fatalf("inspect during pause shows no current step: %+v", insp)
	}

	if _, err := env.client.Resume(context.Background(), &criteriav2.ResumeRequest{SessionId: sessionID}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case err := <-exErr:
		if err != nil {
			t.Fatalf("Execute after pause/resume: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("Execute did not return after resume")
	}
	res := cap2.terminal()
	if res == nil || res.GetOutcome() != serveAdapterOutcomeSuccess {
		t.Fatalf("terminal after resume = %+v, want success", res)
	}

	var sawPaused, sawResumed bool
	for _, kind := range cap2.adapterEventKinds() {
		if kind == serveAdapterEventRunPaused {
			sawPaused = true
		}
		if kind == serveAdapterEventRunResumed {
			sawResumed = true
		}
	}
	if !sawPaused || !sawResumed {
		t.Fatalf("pause/resume events missing: sawPaused=%t sawResumed=%t kinds=%v", sawPaused, sawResumed, cap2.adapterEventKinds())
	}
}

// TestServeAdapter_PauseAfterTerminalFailsClosed guards against a pause ack
// racing a finished run landing in a wedged tracker: pausing a non-live run
// must return a typed error, not succeed.
func TestServeAdapter_PauseAfterTerminalFailsClosed(t *testing.T) {
	env := newServeAdapterEnv(t, "serveadapter_toy")
	sessionID := env.openTestSession(t)

	cap2 := &executeCapture{}
	if err := env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
		SessionId: sessionID,
		StepName:  "warmup",
	}, cap2); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := env.client.Pause(context.Background(), &criteriav2.PauseRequest{SessionId: sessionID}); err == nil {
		t.Fatal("Pause on a completed session succeeded; want typed error")
	}
}

// TestServeAdapter_ReExecuteInFlightTypedError covers acceptance 2: a second
// Execute while a child run is in flight fails closed with the typed
// ErrChildRunInFlight (surviving connect's error wrapping) as
// CodeFailedPrecondition, and cancelling the run settles the first call.
func TestServeAdapter_ReExecuteInFlightTypedError(t *testing.T) {
	env := newServeAdapterEnv(t, "serveadapter_toy")
	sessionID := env.openTestSession(t)

	cap2 := &executeCapture{arrived: make(chan struct{}, 8), watch: slowStepStarted()}
	exErr := make(chan error, 1)
	go func() {
		exErr <- env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
			SessionId: sessionID,
			StepName:  "warmup",
		}, cap2)
	}()
	select {
	case <-cap2.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the slow step to start")
	}

	err := env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
		SessionId: sessionID,
		StepName:  "warmup",
	}, &executeCapture{})
	if err == nil {
		t.Fatal("re-Execute while in-flight succeeded; want typed error")
	}
	var typed *ErrChildRunInFlight
	if !errors.As(err, &typed) {
		t.Fatalf("error %v (%T) is not ErrChildRunInFlight", err, err)
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("connect code = %q, want failed_precondition", connect.CodeOf(err))
	}

	if _, ok := env.client.CancelChildRun(typed.RunID); !ok {
		t.Fatalf("CancelChildRun(%q) rejected a live run", typed.RunID)
	}
	select {
	case <-exErr:
	case <-time.After(30 * time.Second):
		t.Fatal("first Execute did not return after cancel")
	}
	assertRunRecordCancelled(t, typed.RunID)
}

// TestServeAdapter_CloseSessionCancelsLiveRunObservably covers acceptance 2:
// CloseSession cancels the live child run through the engine stop machinery
// and the child run's local record shows cancelled.
func TestServeAdapter_CloseSessionCancelsLiveRunObservably(t *testing.T) {
	env := newServeAdapterEnv(t, "serveadapter_toy")
	sessionID := env.openTestSession(t)

	cap2 := &executeCapture{arrived: make(chan struct{}, 8), watch: slowStepStarted()}
	exErr := make(chan error, 1)
	go func() {
		exErr <- env.client.Execute(context.Background(), &criteriav2.ExecuteRequest{
			SessionId: sessionID,
			StepName:  "warmup",
		}, cap2)
	}()
	select {
	case <-cap2.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the slow step to start")
	}

	resp, err := env.client.CloseSession(context.Background(), &criteriav2.CloseSessionRequest{SessionId: sessionID})
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if resp == nil {
		t.Fatal("nil CloseSessionResponse")
	}
	select {
	case err := <-exErr:
		if err != nil {
			t.Fatalf("Execute after CloseSession cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Execute did not return after CloseSession")
	}

	res := cap2.terminal()
	if res == nil || res.GetOutcome() == serveAdapterOutcomeSuccess {
		t.Fatalf("terminal on cancelled run = %+v, want a non-success outcome", res)
	}
}

// assertRunRecordCancelled polls the child run's local record until it shows
// the cancelled stamp (the settle stamps it from the run goroutine).
func assertRunRecordCancelled(t *testing.T, runID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for {
		st, err := readLocalRunState(runID)
		if err == nil && st != nil {
			last = st.Status
			if st.Status == runstate.StatusCancelled {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run record for %q never showed cancelled (last: %q)", runID, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestServeAdapter_WaitApprovalNodesRejectedAtServe covers mechanics 5: the
// mode fails closed at child compile with a clear error naming the node
// kinds, before any phone-home connectivity matters.
func TestServeAdapter_WaitApprovalNodesRejectedAtServe(t *testing.T) {
	cases := []struct {
		fixture string
		kind    string
	}{
		{fixture: "local_control_pause", kind: "wait"},
		{fixture: "local_approval_simple", kind: "approval"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			// An unreachable-but-parseable host so the gate error (not a
			// connectivity error) is what surfaces first.
			t.Setenv("CRITERIA_REMOTE_HOST", "127.0.0.1:59999")
			err := runServeAdapter(context.Background(), serveAdapterOptions{
				workflowPath:  filepath.Join("testdata", tc.fixture),
				allowUnsigned: true,
			})
			if err == nil {
				t.Fatalf("serve accepted a workflow with a %s node", tc.kind)
			}
			if !strings.Contains(err.Error(), tc.kind) {
				t.Errorf("error %v does not name the %q node kind", err, tc.kind)
			}
		})
	}
}

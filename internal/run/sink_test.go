package run

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// fakePublisher records all envelopes passed to Publish so tests can assert
// envelope types without needing a live server transport.
type fakePublisher struct {
	published []*pb.Envelope
}

func (fp *fakePublisher) Publish(_ context.Context, env *pb.Envelope) {
	fp.published = append(fp.published, env)
}

// newTestClient constructs a Client pointed at a non-existent server.
// No actual connections are made at construction time; Publish puts
// envelopes into the internal send buffer.
func newTestClient(t *testing.T) *servertrans.Client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := servertrans.NewClient("http://localhost:1", log)
	if err != nil {
		t.Fatalf("newTestClient: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func newTestSink(t *testing.T) *Sink {
	t.Helper()
	return &Sink{
		RunID:  "test-run-1",
		Client: newTestClient(t),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestSink_PauseLifecycle verifies OnRunPaused sets the paused node,
// IsPaused/PausedAt report it, and ClearPaused resets the state.
func TestSink_PauseLifecycle(t *testing.T) {
	s := newTestSink(t)

	if s.IsPaused() {
		t.Fatal("sink should not be paused initially")
	}
	if got := s.PausedAt(); got != "" {
		t.Fatalf("PausedAt should be empty initially, got %q", got)
	}

	s.OnRunPaused("gate", "signal", "ready")

	if !s.IsPaused() {
		t.Fatal("sink should be paused after OnRunPaused")
	}
	if got := s.PausedAt(); got != "gate" {
		t.Fatalf("PausedAt: got %q want %q", got, "gate")
	}

	s.ClearPaused()

	if s.IsPaused() {
		t.Fatal("sink should not be paused after ClearPaused")
	}
	if got := s.PausedAt(); got != "" {
		t.Fatalf("PausedAt should be empty after ClearPaused, got %q", got)
	}
}

// TestSink_CheckpointFn_NotCalledOnTerminalEvents asserts that CheckpointFn is
// NOT called by OnRunCompleted or OnRunFailed — only OnStepEntered triggers it.
func TestSink_CheckpointFn_NotCalledOnTerminalEvents(t *testing.T) {
	s := newTestSink(t)
	called := false
	s.CheckpointFn = func(_ string, _ int) { called = true }

	s.OnRunCompleted("done", true)
	if called {
		t.Error("CheckpointFn must NOT be called by OnRunCompleted")
	}

	s.OnRunFailed("boom", "step1")
	if called {
		t.Error("CheckpointFn must NOT be called by OnRunFailed")
	}
}

// TestSink_OnRunCompleted_PublishesRunCompletedEnvelope asserts that
// OnRunCompleted publishes a RunCompleted envelope with the expected fields.
func TestSink_OnRunCompleted_PublishesRunCompletedEnvelope(t *testing.T) {
	fp := &fakePublisher{}
	s := &Sink{RunID: "test-run", Client: fp, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	s.OnRunCompleted("done", true)

	if len(fp.published) != 1 {
		t.Fatalf("expected 1 published envelope, got %d", len(fp.published))
	}
	rc := fp.published[0].GetRunCompleted()
	if rc == nil {
		t.Fatal("expected RunCompleted payload in envelope")
	}
	if rc.GetFinalState() != "done" {
		t.Errorf("FinalState: got %q want %q", rc.GetFinalState(), "done")
	}
	if !rc.GetSuccess() {
		t.Error("Success: got false want true")
	}
}

// TestSink_OnRunFailed_PublishesRunFailedEnvelope asserts that OnRunFailed
// publishes a RunFailed envelope with the expected reason and step fields.
func TestSink_OnRunFailed_PublishesRunFailedEnvelope(t *testing.T) {
	fp := &fakePublisher{}
	s := &Sink{RunID: "test-run", Client: fp, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	s.OnRunFailed("max retries exceeded", "compile")

	if len(fp.published) != 1 {
		t.Fatalf("expected 1 published envelope, got %d", len(fp.published))
	}
	rf := fp.published[0].GetRunFailed()
	if rf == nil {
		t.Fatal("expected RunFailed payload in envelope")
	}
	if rf.GetReason() != "max retries exceeded" {
		t.Errorf("Reason: got %q want %q", rf.GetReason(), "max retries exceeded")
	}
	if rf.GetStep() != "compile" {
		t.Errorf("Step: got %q want %q", rf.GetStep(), "compile")
	}
}

// TestSink_CheckpointFnCalledOnStepEntered asserts that CheckpointFn is
// invoked with the step name and attempt number before the event is published.
func TestSink_CheckpointFnCalledOnStepEntered(t *testing.T) {
	s := newTestSink(t)
	var capturedStep string
	var capturedAttempt int
	s.CheckpointFn = func(step string, attempt int) {
		capturedStep = step
		capturedAttempt = attempt
	}

	s.OnStepEntered("compile", "noop", 2)

	if capturedStep != "compile" {
		t.Errorf("CheckpointFn step: got %q want %q", capturedStep, "compile")
	}
	if capturedAttempt != 2 {
		t.Errorf("CheckpointFn attempt: got %d want %d", capturedAttempt, 2)
	}
}

// TestSink_CheckpointFnNilSafe verifies OnStepEntered does not panic
// when CheckpointFn is nil.
func TestSink_CheckpointFnNilSafe(t *testing.T) {
	s := newTestSink(t)
	s.CheckpointFn = nil
	s.OnStepEntered("step", "demo", 1) // must not panic
}

// TestSink_PublishMethodsDoNotPanic exercises all event-publishing methods
// against a live (but offline) client to verify they do not panic.
func TestSink_PublishMethodsDoNotPanic(t *testing.T) {
	s := newTestSink(t)

	s.OnRunStarted("wf", "first")
	s.OnRunCompleted("done", true)
	s.OnRunFailed("boom", "step1")
	s.OnStepEntered("step1", "noop", 1)
	s.OnStepOutcome("step1", "success", 10*time.Millisecond, nil)
	s.OnStepOutcome("step1", "failure", 5*time.Millisecond, &stringErr{msg: "err"})
	s.OnStepTransition("step1", "done", "success")
	s.OnStepResumed("step1", 2, "criteria_restart")
	s.OnVariableSet("x", "1", "default")
	s.OnStepOutputCaptured("step1", map[string]string{"out": "val"})
	s.OnRunPaused("gate", "signal", "ready")
	s.OnWaitEntered("gate", "signal", "", "ready")
	s.OnWaitResumed("gate", "signal", "ready", map[string]string{"k": "v"})
	s.OnApprovalRequested("review", []string{"alice"}, "ship it")
	s.OnApprovalDecision("review", "approved", "alice", nil)
	s.OnBranchEvaluated("branch1", "arm[0]", "step2", "x == 1")
	s.OnForEachEntered("each", 3)
	s.OnStepIterationStarted("each", 0, "a", false)
	s.OnStepIterationItem("each", 0, "review")
	s.OnStepIterationCompleted("each", "all_succeeded", "done")
	s.OnScopeIterCursorSet(`{"index":1}`)
	s.OnAdapterLifecycle("step1", "noop", "started", "")
}

// TestSink_StepEventSinkLogAndAdapter exercises the step-level event sink
// returned by StepEventSink, verifying Log and Adapter calls do not panic.
func TestSink_StepEventSinkLogAndAdapter(t *testing.T) {
	s := newTestSink(t)
	ss := s.StepEventSink("step1")
	ss.Log("stdout", []byte("hello\n"))
	ss.Log("stderr", []byte("err\n"))
	ss.Adapter("agent.message", map[string]any{"content": "hi"})
	ss.Adapter("tool.invocation", map[string]any{"name": "write"})
	ss.Adapter("nil_data_event", nil) // nil data should not panic
}

// TestSink_PublishAfterClientClose_DoesNotPanic verifies that calling publish
// methods after the underlying client is closed does not panic (the event is
// silently dropped via the closed channel select arm).
func TestSink_PublishAfterClientClose_DoesNotPanic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := servertrans.NewClient("http://localhost:1", log)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	s := &Sink{RunID: "closed-run", Client: c, Log: log}
	c.Close() // close before publishing
	s.OnRunCompleted("done", true)
	s.OnRunFailed("closed", "step1")
}

// TestEncodeAdapterData_Object verifies that a JSON object passes through
// without wrapping.
func TestEncodeAdapterData_Object(t *testing.T) {
	st := encodeAdapterData(map[string]any{"key": "value"})
	if st == nil {
		t.Fatal("expected non-nil Struct for object input")
	}
	if _, ok := st.Fields["key"]; !ok {
		t.Error("expected 'key' field in encoded struct")
	}
}

// TestEncodeAdapterData_Scalar verifies that non-object JSON values are
// wrapped under a "value" key.
func TestEncodeAdapterData_Scalar(t *testing.T) {
	st := encodeAdapterData("hello")
	if st == nil {
		t.Fatal("expected non-nil Struct for scalar input")
	}
	if _, ok := st.Fields["value"]; !ok {
		t.Errorf("scalar should be wrapped under 'value', got fields: %v", st.Fields)
	}
}

// TestEncodeAdapterData_Array verifies arrays are wrapped under "value".
func TestEncodeAdapterData_Array(t *testing.T) {
	st := encodeAdapterData([]int{1, 2, 3})
	if st == nil {
		t.Fatal("expected non-nil Struct for array input")
	}
	if _, ok := st.Fields["value"]; !ok {
		t.Errorf("array should be wrapped under 'value', got fields: %v", st.Fields)
	}
}

// TestEncodeAdapterData_MarshalError verifies that marshal failures produce
// an error struct rather than nil.
func TestEncodeAdapterData_MarshalError(t *testing.T) {
	// channels cannot be marshalled to JSON.
	st := encodeAdapterData(make(chan int))
	if st == nil {
		t.Fatal("expected error Struct for unmarshalable input")
	}
	if _, ok := st.Fields["_encode_error"]; !ok {
		t.Errorf("expected _encode_error field, got: %v", st.Fields)
	}
}

// TestLogStreamFromString verifies all stream name → enum mappings.
func TestLogStreamFromString(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"stdout", "LOG_STREAM_STDOUT"},
		{"stderr", "LOG_STREAM_STDERR"},
		{"agent", "LOG_STREAM_AGENT"},
		{"unknown", "LOG_STREAM_UNSPECIFIED"},
		{"", "LOG_STREAM_UNSPECIFIED"},
	}
	for _, tc := range cases {
		got := logStreamFromString(tc.input).String()
		if got != tc.want {
			t.Errorf("logStreamFromString(%q): got %q want %q", tc.input, got, tc.want)
		}
	}
}

// TestErrorStruct_WithRaw verifies the error struct helper includes the
// reason and the raw field when provided.
func TestErrorStruct_WithRaw(t *testing.T) {
	st := errorStruct("bad input", `{"x":1}`)
	if st == nil {
		t.Fatal("expected non-nil Struct")
	}
	if _, ok := st.Fields["_encode_error"]; !ok {
		t.Error("missing _encode_error field")
	}
	if _, ok := st.Fields["raw"]; !ok {
		t.Error("missing raw field")
	}
}

// TestErrorStruct_NoRaw verifies the raw field is absent when empty.
func TestErrorStruct_NoRaw(t *testing.T) {
	st := errorStruct("reason", "")
	if _, ok := st.Fields["raw"]; ok {
		t.Error("raw field should be absent when empty")
	}
}

// TestLocalSink_AllRemainingEvents exercises the LocalSink methods not covered
// by the existing test, asserting each emits a correctly-typed ND-JSON line.
func TestLocalSink_AllRemainingEvents(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "r1", Out: &buf}

	sink.OnRunFailed("boom", "step1")
	sink.OnStepResumed("step1", 2, "restart")
	sink.OnVariableSet("x", "1", "default")
	sink.OnStepOutputCaptured("step1", map[string]string{"out": "val"})
	sink.OnRunPaused("gate", "signal", "ready") // no-op on LocalSink
	sink.OnWaitEntered("gate", "signal", "5s", "")
	sink.OnWaitResumed("gate", "signal", "ready", nil)
	sink.OnApprovalRequested("review", []string{"alice"}, "approve?")
	sink.OnApprovalDecision("review", "approved", "alice", nil)
	sink.OnBranchEvaluated("b", "arm[0]", "step2", "true")
	sink.OnForEachEntered("each", 2)
	sink.OnStepIterationStarted("each", 0, "a", false)
	sink.OnStepIterationItem("each", 0, "review")
	sink.OnStepIterationCompleted("each", "all_succeeded", "done")
	sink.OnScopeIterCursorSet(`{"index":1}`)
	sink.OnAdapterLifecycle("each", "noop", "started", "") // no-op on LocalSink

	// OnRunPaused is a no-op on LocalSink — no ND-JSON line is emitted.
	wantTypes := []string{
		"RunFailed", "StepResumed", "VariableSet", "StepOutputCaptured",
		"WaitEntered", "WaitResumed", "ApprovalRequested",
		"ApprovalDecision", "BranchEvaluated", "ForEachEntered",
		"StepIterationStarted", "StepIterationItem", "StepIterationCompleted", "ScopeIterCursorSet",
	}
	gotTypes := decodeSinkPayloadTypes(t, buf.String())
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event count: got %d want %d\ngot:  %v\nwant: %v",
			len(gotTypes), len(wantTypes), gotTypes, wantTypes)
	}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Errorf("event[%d]: got %q want %q", i, gotTypes[i], want)
		}
	}
}

// TestLocalSink_EscapeJSONString tests internal JSON string escaping used
// in the emit error path.
func TestLocalSink_EscapeJSONString(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`normal`, `normal`},
		{`has"quotes`, `has\"quotes`},
		{`has\backslash`, `has\\backslash`},
	}
	for _, tc := range cases {
		got := escapeJSONString(tc.input)
		if got != tc.want {
			t.Errorf("escapeJSONString(%q): got %q want %q", tc.input, got, tc.want)
		}
	}
}

// TestMultiSink_AllRemainingMethods exercises the MultiSink methods not
// covered by the existing test, verifying every event fans to all children.
func TestMultiSink_AllRemainingMethods(t *testing.T) {
	var a, b recordingSink
	sink := NewMultiSink(&a, &b)

	sink.OnRunFailed("boom", "step1")
	sink.OnStepResumed("step1", 2, "restart")
	sink.OnVariableSet("x", "1", "default")
	sink.OnStepOutputCaptured("step1", map[string]string{"k": "v"})
	sink.OnRunPaused("gate", "signal", "ready")
	sink.OnWaitEntered("gate", "signal", "5s", "")
	sink.OnWaitResumed("gate", "signal", "ready", nil)
	sink.OnApprovalRequested("review", []string{"alice"}, "reason")
	sink.OnApprovalDecision("review", "approved", "alice", nil)
	sink.OnBranchEvaluated("b", "arm[0]", "step2", "true")
	sink.OnForEachEntered("each", 2)
	sink.OnStepIterationStarted("each", 0, "a", false)
	sink.OnStepIterationItem("each", 0, "review")
	sink.OnStepIterationCompleted("each", "all_succeeded", "done")
	sink.OnScopeIterCursorSet(`{"index":1}`)

	const want = 15
	if got := a.calls.Load(); got != want {
		t.Errorf("child a calls: got %d want %d", got, want)
	}
	if got := b.calls.Load(); got != want {
		t.Errorf("child b calls: got %d want %d", got, want)
	}
}

// decodeSinkPayloadTypes parses ND-JSON output and returns the payload_type
// values in order.
func decodeSinkPayloadTypes(t *testing.T, ndjson string) []string {
	t.Helper()
	type line struct {
		PayloadType string `json:"payload_type"`
	}
	lines := strings.Split(strings.TrimSpace(ndjson), "\n")
	out := make([]string, 0, len(lines))
	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("decode ND-JSON line: %v\nraw: %s", err, raw)
		}
		out = append(out, l.PayloadType)
	}
	return out
}

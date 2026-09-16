package main

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// toolTestHost is an in-memory host double for the noop fixture's tool-call
// mode: it captures ExecuteEvents, surfaces permission.request payloads to a
// channel, and lets a test script drive the Permissions stream (allow-grant,
// cancel, tool_call_result) exactly like the criteria host does.
type toolTestHost struct {
	mu        sync.Mutex
	events    []*v2.ExecuteEvent
	decisions []*v2.PermissionDecision

	permCh     chan *v2.PermissionEvent
	permMu     sync.Mutex
	permClosed bool
	reqCh      chan map[string]any

	closeOnce sync.Once
}

func newToolTestHost() *toolTestHost {
	return &toolTestHost{
		permCh: make(chan *v2.PermissionEvent, 8),
		reqCh:  make(chan map[string]any, 8),
	}
}

// fakeExecuteSink implements adapterhost.ExecuteEventSender.
type fakeExecuteSink struct{ h *toolTestHost }

func (s *fakeExecuteSink) Send(ev *v2.ExecuteEvent) error {
	s.h.mu.Lock()
	s.h.events = append(s.h.events, ev)
	s.h.mu.Unlock()
	if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "permission.request" {
		select {
		case s.h.reqCh <- a.GetPayload().AsMap():
		default:
		}
	}
	return nil
}

// fakePermStream implements adapterhost.PermissionsStream.
type fakePermStream struct{ h *toolTestHost }

func (s *fakePermStream) Recv() (*v2.PermissionEvent, error) {
	ev, ok := <-s.h.permCh
	if !ok {
		return nil, io.EOF
	}
	return ev, nil
}

func (s *fakePermStream) Send(d *v2.PermissionDecision) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.decisions = append(s.h.decisions, d)
	return nil
}

func (s *fakePermStream) Context() context.Context { return context.Background() }

func (h *toolTestHost) snapshot() []*v2.ExecuteEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*v2.ExecuteEvent, len(h.events))
	copy(out, h.events)
	return out
}

func (h *toolTestHost) decisionsSnapshot() []*v2.PermissionDecision {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*v2.PermissionDecision, len(h.decisions))
	copy(out, h.decisions)
	return out
}

// awaitRequest blocks for the payload of the next permission.request event.
func (h *toolTestHost) awaitRequest(t *testing.T) map[string]any {
	t.Helper()
	select {
	case payload := <-h.reqCh:
		return payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for permission.request")
		return nil
	}
}

// sendPerm delivers one PermissionEvent to the dispatch loop unless the
// stream is already closed. It is safe to race with closeStream: a late event
// after cleanup is dropped, mirroring a torn-down stream, instead of
// panicking on a send to a closed channel.
func (h *toolTestHost) sendPerm(ev *v2.PermissionEvent) bool {
	h.permMu.Lock()
	defer h.permMu.Unlock()
	if h.permClosed {
		return false
	}
	h.permCh <- ev
	return true
}

// grant sends the allow-grant for the call the host detected.
func (h *toolTestHost) grant(t *testing.T, payload map[string]any) {
	t.Helper()
	id, _ := payload["request_id"].(string)
	if id == "" {
		t.Fatalf("permission.request payload without request_id: %v", payload)
	}
	h.sendPerm(&v2.PermissionEvent{
		Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: id}},
	})
}

// cancel sends a policy denial for id.
func (h *toolTestHost) cancel(id, reason string) {
	h.sendPerm(&v2.PermissionEvent{
		Event: &v2.PermissionEvent_Cancel{Cancel: &v2.PermissionCancel{RequestId: id, Reason: reason}},
	})
}

// reply sends the correlated typed result for id.
func (h *toolTestHost) reply(id string, res *v2.ToolCallResult) {
	res.RequestId = id
	h.sendPerm(&v2.PermissionEvent{
		Event: &v2.PermissionEvent_ToolCallResult{ToolCallResult: res},
	})
}

// closeStream ends the Permissions stream once (tests may close it early to
// simulate a stream cut; the cleanup closes it at test end).
func (h *toolTestHost) closeStream() {
	h.closeOnce.Do(func() {
		h.permMu.Lock()
		defer h.permMu.Unlock()
		h.permClosed = true
		close(h.permCh)
	})
}

// startBridge runs the fixture's dispatch loop for the host, mirroring the
// SDK Service Permissions call. The returned func ends the stream and waits
// for the loop to exit; it is idempotent and also registered as cleanup, so
// tests that assert on state only reachable after the loop exits can call it
// directly.
func startBridge(t *testing.T, h *toolTestHost, svc *noopService) func() {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- svc.bridge().Permissions(context.Background(), &fakePermStream{h: h}) }()
	var joinOnce sync.Once
	join := func() {
		joinOnce.Do(func() {
			h.closeStream()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("bridge dispatch loop did not exit")
			}
		})
	}
	t.Cleanup(join)
	return join
}

// newTestService returns a fixture service with the test session pre-opened,
// as a real host would.
func newTestService() *noopService {
	return &noopService{sessions: map[string]struct{}{"sess": {}}}
}

// runToolCall drives one Execute through the fixture with the scripted host.
func runToolCall(t *testing.T, h *toolTestHost, input map[string]string) error {
	t.Helper()
	svc := newTestService()
	startBridge(t, h, svc)
	return svc.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "sess", Input: input}, &fakeExecuteSink{h: h})
}

// resultOf returns the terminal ExecuteResult the fixture sent.
func resultOf(t *testing.T, h *toolTestHost) *v2.ExecuteResult {
	t.Helper()
	for _, ev := range h.snapshot() {
		if r := ev.GetResult(); r != nil {
			return r
		}
	}
	t.Fatal("no terminal ExecuteResult event captured")
	return nil
}

// resultOutputs decodes the result's outputs_json.
func resultOutputs(t *testing.T, res *v2.ExecuteResult) map[string]any {
	t.Helper()
	if len(res.GetOutputsJson()) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(res.GetOutputsJson(), &out); err != nil {
		t.Fatalf("decode result outputs: %v", err)
	}
	return out
}

func TestToolCallRoundTrip(t *testing.T) {
	h := newToolTestHost()
	args := map[string]any{"outputs": `{"rows":2}`}

	go func() {
		payload := h.awaitRequest(t)
		wantDigest, err := v2.ArgsDigest(args)
		if err != nil {
			t.Errorf("ArgsDigest: %v", err)
			return
		}
		if got := payload["kind"]; got != "adapter_tool" {
			t.Errorf("payload kind = %v, want adapter_tool", got)
		}
		if got, _ := payload["request_id"].(string); !strings.HasPrefix(got, "noop-tool-") {
			t.Errorf("request_id = %q, want noop-tool- prefix", got)
		}
		if got := payload["target"]; got != "adapter.callee.default.tools.echo_data" {
			t.Errorf("payload target = %v, want adapter.callee.default.tools.echo_data", got)
		}
		if got := payload["tool"]; got != "echo_data" {
			t.Errorf("payload tool = %v, want echo_data", got)
		}
		if got, _ := payload["args"].(map[string]any); !reflect.DeepEqual(got, args) {
			t.Errorf("payload args = %v, want %v", got, args)
		}
		if got := payload["args_digest"]; got != wantDigest {
			t.Errorf("payload args_digest = %v, want %v", got, wantDigest)
		}
		h.grant(t, payload)
		id, _ := payload["request_id"].(string)
		h.reply(id, &v2.ToolCallResult{Outcome: "success", OutputsJson: []byte(`{"echo":"data"}`)})
	}()

	err := runToolCall(t, h, map[string]string{
		inputToolTarget: "adapter.callee.default.tools.echo_data",
		inputToolArgs:   `{"outputs":"{\"rows\":2}"}`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The allow-grant was ACKed with an allow decision on the same request id.
	if ds := h.decisionsSnapshot(); len(ds) != 1 || ds[0].GetDecision() != "allow" || ds[0].GetRequestId() == "" {
		t.Fatalf("decisions = %+v, want one allow ACK", ds)
	}

	res := resultOf(t, h)
	if got := res.GetOutcome(); got != "success" {
		t.Fatalf("result outcome = %q, want success", got)
	}
	out := resultOutputs(t, res)
	if got, _ := out[calleeOutcomeKey].(string); got != "success" {
		t.Errorf("callee.outcome = %v, want success", got)
	}
	want := map[string]any{"echo": "data"}
	if got, _ := out[calleeOutputsKey].(map[string]any); !reflect.DeepEqual(got, want) {
		t.Errorf("callee.outputs = %v, want %v", got, want)
	}
	if _, ok := out[calleeErrorKey]; ok {
		t.Errorf("callee.error set on successful round-trip: %v", out)
	}
}

func TestToolCallCalleeFailureOutcome(t *testing.T) {
	h := newToolTestHost()
	go func() {
		payload := h.awaitRequest(t)
		h.grant(t, payload)
		id, _ := payload["request_id"].(string)
		h.reply(id, &v2.ToolCallResult{Outcome: "failure", OutputsJson: []byte(`{"error":"boom"}`)})
	}()

	if err := runToolCall(t, h, map[string]string{
		inputToolTarget: "adapter.callee.default.tools.echo_data",
		inputToolArgs:   "{}",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	res := resultOf(t, h)
	if got := res.GetOutcome(); got != "success" {
		t.Fatalf("result outcome = %q, want success (the call ran; the callee's own failure passes through)", got)
	}
	out := resultOutputs(t, res)
	if got, _ := out[calleeOutcomeKey].(string); got != "failure" {
		t.Errorf("callee.outcome = %v, want failure", got)
	}
	want := map[string]any{"error": "boom"}
	if got, _ := out[calleeOutputsKey].(map[string]any); !reflect.DeepEqual(got, want) {
		t.Errorf("callee.outputs = %v, want %v", got, want)
	}
	if _, ok := out[calleeErrorKey]; ok {
		t.Errorf("callee.error set on a callee-failure outcome: %v", out)
	}
}

func TestToolCallRoundTripCallError(t *testing.T) {
	h := newToolTestHost()
	go func() {
		payload := h.awaitRequest(t)
		h.grant(t, payload)
		id, _ := payload["request_id"].(string)
		h.reply(id, &v2.ToolCallResult{CallError: "callee_crash"})
	}()

	if err := runToolCall(t, h, map[string]string{
		inputToolTarget: "adapter.callee.default.tools.echo_data",
		inputToolArgs:   "{}",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	res := resultOf(t, h)
	if got := res.GetOutcome(); got != "failure" {
		t.Fatalf("result outcome = %q, want failure", got)
	}
	out := resultOutputs(t, res)
	if got, _ := out[calleeErrorKey].(string); !strings.Contains(got, "callee_crash") {
		t.Errorf("callee.error = %v, want it to name callee_crash", out)
	}
	if _, ok := out[calleeOutcomeKey]; ok {
		t.Errorf("callee.outcome set alongside callee.error: %v", out)
	}
	if _, ok := out[calleeOutputsKey]; ok {
		t.Errorf("callee.outputs set alongside callee.error: %v", out)
	}
}

func TestToolCallDenied(t *testing.T) {
	h := newToolTestHost()
	go func() {
		payload := h.awaitRequest(t)
		id, _ := payload["request_id"].(string)
		h.cancel(id, "policy denied")
		// A late result for the denied id must be dropped, not panic — and
		// it may legitimately arrive after the stream was torn down, where
		// sendPerm drops it.
		h.reply(id, &v2.ToolCallResult{Outcome: "success"})
	}()

	if err := runToolCall(t, h, map[string]string{
		inputToolTarget: "adapter.callee.default.tools.echo_data",
		inputToolArgs:   "{}",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	res := resultOf(t, h)
	if got := res.GetOutcome(); got != "failure" {
		t.Fatalf("result outcome = %q, want failure", got)
	}
	out := resultOutputs(t, res)
	if got, _ := out[calleeErrorKey].(string); got != "tool call to adapter.callee.default.tools.echo_data: denied: policy denied" {
		t.Errorf("callee.error = %v", out)
	}
}

func TestPermissionsSkipsEmptyRequestID(t *testing.T) {
	h := newToolTestHost()
	svc := newTestService()
	join := startBridge(t, h, svc)

	// Plain auto-allow traffic can arrive without a correlation id; there is
	// no pending call to grant and nothing to ACK, so the bridge must skip
	// the event without wedging the stream.
	h.sendPerm(&v2.PermissionEvent{
		Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{}},
	})
	join()
	if ds := h.decisionsSnapshot(); len(ds) != 0 {
		t.Errorf("decisions = %+v, want none for an empty request_id", ds)
	}
}

func TestToolCallHostUnsupported(t *testing.T) {
	h := newToolTestHost()
	old := defaultToolCallTimeout
	defaultToolCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { defaultToolCallTimeout = old })

	go func() {
		payload := h.awaitRequest(t)
		// A host predating adapter tools grants the call and never sends a
		// typed result.
		h.grant(t, payload)
	}()

	err := runToolCall(t, h, map[string]string{
		inputToolTarget: "adapter.callee.default.tools.echo_data",
		inputToolArgs:   "{}",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	res := resultOf(t, h)
	if got := res.GetOutcome(); got != "failure" {
		t.Fatalf("result outcome = %q, want failure", got)
	}
	out := resultOutputs(t, res)
	if got, _ := out[calleeErrorKey].(string); got != "adapter tool call failed: host_unsupported" {
		t.Errorf("callee.error = %v, want host_unsupported", out)
	}
}

func TestToolCallStreamClosed(t *testing.T) {
	h := newToolTestHost()
	svc := newTestService()
	startBridge(t, h, svc)
	execDone := make(chan error, 1)
	go func() {
		execDone <- svc.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: "sess",
			Input: map[string]string{
				inputToolTarget: "adapter.callee.default.tools.echo_data",
				inputToolArgs:   "{}",
			},
		}, &fakeExecuteSink{h: h})
	}()
	// Cut the stream while the call is in flight: no reply can ever arrive,
	// so the caller must fail instead of wedging.
	h.awaitRequest(t)
	h.closeStream()
	select {
	case err := <-execDone:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return after the stream was cut")
	}
	res := resultOf(t, h)
	if got := res.GetOutcome(); got != "failure" {
		t.Fatalf("result outcome = %q, want failure", got)
	}
	out := resultOutputs(t, res)
	if got, _ := out[calleeErrorKey].(string); got != "tool call to adapter.callee.default.tools.echo_data: permissions stream closed while tool call in flight" {
		t.Errorf("callee.error = %v", out)
	}
}

func TestToolCallBareTargetWithToolName(t *testing.T) {
	h := newToolTestHost()
	go func() {
		payload := h.awaitRequest(t)
		if got := payload["target"]; got != "adapter.callee.default.tools" {
			t.Errorf("payload target = %v, want the bare target", got)
		}
		if got := payload["tool"]; got != "echo_data" {
			t.Errorf("payload tool = %v, want echo_data from tool_name", got)
		}
		h.grant(t, payload)
		id, _ := payload["request_id"].(string)
		h.reply(id, &v2.ToolCallResult{Outcome: "success", OutputsJson: []byte(`{}`)})
	}()

	if err := runToolCall(t, h, map[string]string{
		inputToolTarget: "adapter.callee.default.tools",
		inputToolName:   "echo_data",
		inputToolArgs:   "{}",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	resultOf(t, h)
}

func TestToolCallInvalidInputs(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]string
		wantErr string
	}{
		{"target too short", map[string]string{inputToolTarget: "adapter.callee.default"}, "want adapter.<type>.<name>.tools[.<tool>]"},
		{"target too long", map[string]string{inputToolTarget: "adapter.callee.default.tools.echo_data.extra"}, "want adapter.<type>.<name>.tools[.<tool>]"},
		{"non-bareword label", map[string]string{inputToolTarget: "adapter.callee.1x.tools"}, "is not a bareword"},
		{"target without tool_name", map[string]string{inputToolTarget: "adapter.callee.default.tools"}, "requires tool_name"},
		{"tool_name conflict", map[string]string{inputToolTarget: "adapter.callee.default.tools.echo", inputToolName: "other"}, "conflicts with tool target"},
		{"bad tool_args JSON", map[string]string{inputToolTarget: "adapter.callee.default.tools.echo", inputToolArgs: "{nope"}, "invalid tool_args"},
		{"non-object tool_args", map[string]string{inputToolTarget: "adapter.callee.default.tools.echo", inputToolArgs: "[]"}, "invalid tool_args"},
		{"mutually exclusive modes", map[string]string{inputToolTarget: "adapter.callee.default.tools.echo", inputOutputs: "{}"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newToolTestHost()
			if err := runToolCall(t, h, tc.input); err == nil {
				t.Fatal("Execute succeeded, want validation error")
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestNoopPassthrough(t *testing.T) {
	t.Run("typed object", func(t *testing.T) {
		h := newToolTestHost()
		if err := runPassthrough(t, h, map[string]string{inputOutputs: `{"count":2,"items":["a","b"]}`}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		res := resultOf(t, h)
		if res.GetOutcome() != "success" {
			t.Fatalf("outcome = %q, want success", res.GetOutcome())
		}
		want := map[string]any{"count": float64(2), "items": []any{"a", "b"}}
		if got := resultOutputs(t, res); !reflect.DeepEqual(got, want) {
			t.Errorf("outputs = %v, want %v", got, want)
		}
	})
	t.Run("no input falls through to the default noop path", func(t *testing.T) {
		h := newToolTestHost()
		if err := runPassthrough(t, h, map[string]string{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		res := resultOf(t, h)
		if res.GetOutcome() != "success" {
			t.Fatalf("outcome = %q, want success", res.GetOutcome())
		}
		if got := resultOutputs(t, res); len(got) != 0 {
			t.Errorf("outputs = %v, want empty", got)
		}
	})
	t.Run("bad JSON", func(t *testing.T) {
		h := newToolTestHost()
		if err := runPassthrough(t, h, map[string]string{inputOutputs: "{nope"}); err == nil || !strings.Contains(err.Error(), "invalid outputs") {
			t.Fatalf("Execute error = %v, want invalid outputs", err)
		}
	})
	t.Run("non-object JSON", func(t *testing.T) {
		for _, raw := range []string{"[]", `"text"`, "3", "null"} {
			h := newToolTestHost()
			if err := runPassthrough(t, h, map[string]string{inputOutputs: raw}); err == nil || !strings.Contains(err.Error(), "invalid outputs") {
				t.Fatalf("outputs %q: Execute error = %v, want invalid outputs", raw, err)
			}
		}
	})
}

// runPassthrough drives one Execute through the outputs-passthrough mode.
func runPassthrough(t *testing.T, h *toolTestHost, input map[string]string) error {
	t.Helper()
	svc := newTestService()
	return svc.Execute(context.Background(), &v2.ExecuteRequest{SessionId: "sess", Input: input}, &fakeExecuteSink{h: h})
}

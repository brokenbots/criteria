package run

// local_sink_cri163_test.go — CRI-163 exit criteria, end to end through the
// real engine and LocalSink ND-JSON wire: tool.call / tool.call_result events
// around a nested call, per-layer audit entries (caller layer 0 vs callee
// layer 1), and redaction of the sensitive callee output in events and audit
// entries alike. Lives in internal/run because internal/run imports
// internal/engine (console sink), so the engine package's own test binary
// cannot exercise LocalSink.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

// cri163Redacted is the mask literal the redaction registry substitutes for
// registered sensitive values.
const cri163Redacted = "[REDACTED]"

const (
	cri163Target     = "adapter.callee.default.tools.helper_task"
	cri163BareTarget = "adapter.callee.default.tools"
	cri163CallerSess = "caller.default"
	cri163CalleeSess = "callee.default"
	cri163CallerStep = "call"
)

// cri163CalleeFake is the callee fake: it marks its lifecycle on the execute
// sink, emits one callee-side allow decision, sleeps so the tool.call_result
// duration is observable, and returns a sensitive output that the host
// registers into the redaction registry.
type cri163CalleeFake struct {
	mu          sync.Mutex
	started     bool
	finished    bool
	tokenOutput string
}

func (a *cri163CalleeFake) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Capabilities: []string{"execute"},
		AdapterInfo: workflow.AdapterInfo{
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"token": {CtyType: cty.String, Sensitive: true},
			},
		},
	}, nil
}
func (a *cri163CalleeFake) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *cri163CalleeFake) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
	sink.Adapter("callee.started", map[string]any{"task": "fetch-token"})
	sink.Adapter("permission.request", map[string]any{
		"request_id": "callee-perm-1",
		"tool":       "callee.helpers.read_file", // allowed by the callee's own policy
	})
	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return adapter.Result{Outcome: "failure"}, ctx.Err()
	}
	a.mu.Lock()
	a.finished = true
	a.mu.Unlock()
	sink.Adapter("callee.finished", map[string]any{"task": "fetch-token"})
	return adapter.Result{
		Outcome: "success",
		Outputs: map[string]cty.Value{"token": cty.StringVal(a.tokenOutput)},
	}, nil
}
func (a *cri163CalleeFake) CloseSession(context.Context, string) error { return nil }
func (a *cri163CalleeFake) Kill()                                      {}
func (a *cri163CalleeFake) Pause(context.Context, string) error        { return nil }
func (a *cri163CalleeFake) Resume(context.Context, string) error       { return nil }
func (a *cri163CalleeFake) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *cri163CalleeFake) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *cri163CalleeFake) Restore(context.Context, string, []byte, uint32) error { return nil }

// cri163CallerFake is the caller fake: it dispatches the nested tool call,
// waits for the typed result, re-exports the (sensitive) callee output as its
// own step output, and then makes a plain permission request whose tool
// string carries the raw secret — the real-world case the redaction surface
// must cover in events and audit alike.
type cri163CallerFake struct {
	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	token    string
}

func (a *cri163CallerFake) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Capabilities: []string{"adapter_tools", "execute"}}, nil
}
func (a *cri163CallerFake) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (a *cri163CallerFake) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}
func (a *cri163CallerFake) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     cri163Target,
		"args":       map[string]any{"task": "fetch-token"},
	})
	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		return adapter.Result{Outcome: "failure"}, errors.New("permission stream not started")
	}
	deadline := time.After(5 * time.Second)

	token, err := cri163AwaitToolCallResult(ctx, requests, deadline, "call-1")
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}
	a.mu.Lock()
	a.token = token
	a.mu.Unlock()

	// Second nested call whose target embeds the (by now registered)
	// sensitive value as the parsed tool label — the tool.call /
	// tool.call_result payloads and the granted decision must mask it.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     cri163BareTarget + "." + token,
		"args":       map[string]any{"task": "fetch-token"},
	})
	if _, err := cri163AwaitToolCallResult(ctx, requests, deadline, "call-2"); err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	// Re-export the sensitive callee output as the caller's own step output
	// (this is what the engine's redacting sink must mask in ND-JSON), then
	// echo the same value back in a plain permission request tool string.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "echo-1",
		"tool":       "shell:echo " + token,
	})
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				return adapter.Result{Outcome: "failure"}, errors.New("permission stream closed")
			}
			switch {
			case ev.GetRequest() != nil:
				return adapter.Result{
					Outcome: "success",
					Outputs: map[string]cty.Value{"report": cty.StringVal(token)},
				}, nil
			case ev.GetCancel() != nil:
				return adapter.Result{Outcome: "failure"}, errors.New("echo request denied")
			}
		case <-ctx.Done():
			return adapter.Result{Outcome: "failure"}, ctx.Err()
		case <-deadline:
			return adapter.Result{Outcome: "failure"}, errors.New("timed out waiting for echo decision")
		}
	}
}
func (a *cri163CallerFake) CloseSession(context.Context, string) error { return nil }
func (a *cri163CallerFake) Kill()                                      {}
func (a *cri163CallerFake) Pause(context.Context, string) error        { return nil }
func (a *cri163CallerFake) Resume(context.Context, string) error       { return nil }
func (a *cri163CallerFake) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *cri163CallerFake) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *cri163CallerFake) Restore(context.Context, string, []byte, uint32) error { return nil }

// cri163AwaitToolCallResult drains the permission stream until the typed
// result for the named nested call arrives, returning the decoded sensitive
// output.
func cri163AwaitToolCallResult(ctx context.Context, requests <-chan *v2.PermissionEvent, deadline <-chan time.Time, reqID string) (string, error) {
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				return "", errors.New("permission stream closed")
			}
			tcr := ev.GetToolCallResult()
			if tcr == nil || tcr.RequestId != reqID {
				continue
			}
			if tcr.CallError != "" {
				return "", fmt.Errorf("nested call error: %s", tcr.CallError)
			}
			if len(tcr.OutputsJson) == 0 {
				return "", errors.New("tool_call_result has no outputs")
			}
			typed, err := ctyjson.Unmarshal(tcr.OutputsJson, cty.Object(map[string]cty.Type{"token": cty.String}))
			if err != nil {
				return "", fmt.Errorf("decode outputs_json: %w", err)
			}
			token := typed.GetAttr("token").AsString()
			if token == "" {
				return "", errors.New("callee returned empty token")
			}
			return token, nil
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", errors.New("timed out waiting for tool_call_result")
		}
	}
}

// cri163AuditCollector implements adapterhost.AuditWriter.
type cri163AuditCollector struct {
	mu      sync.Mutex
	entries []*adapterhost.DecisionLogEntry
}

func (w *cri163AuditCollector) Write(e *adapterhost.DecisionLogEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, e)
}

func (w *cri163AuditCollector) all() []*adapterhost.DecisionLogEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*adapterhost.DecisionLogEntry(nil), w.entries...)
}

// cri163FakeLoader resolves the in-test adapter handles.
type cri163FakeLoader struct {
	adapters map[string]adapterhost.Handle
}

func (l *cri163FakeLoader) Resolve(_ context.Context, name string) (adapterhost.Handle, error) {
	h, ok := l.adapters[name]
	if !ok {
		return nil, fmt.Errorf("no adapter named %q", name)
	}
	return h, nil
}
func (l *cri163FakeLoader) Shutdown(context.Context) error { return nil }

// cri163WorkflowHCL is the CRI-163 engine workflow: the caller step's
// step-level policy allows the nested callee tool surface plus the echoed
// shell invocation; the workflow-level (callee-side) policy allows the
// callee's helper tool. The callee adapter uses dynamic_tools so the nested
// surface resolves to a synthetic step.
const cri163WorkflowHCL = `
workflow {
  name = "nested_cri163"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

environment "shell" "prod" {
  os = "linux"
}

adapter "caller" "default" {}
adapter "callee" "default" {
  environment   = shell.prod
  dynamic_tools = true
}

step "call" {
  target = adapter.caller.default
  allow_tools = ["adapter.callee.default.tools.*", "caller.only.*", "shell:echo *"]
  outcome "success" { next = step.done }
}
state "done" { terminal = true }

permissions {
  allow_tools = ["callee.helpers.*"]
}
`

// cri163NDJSONEnvelope mirrors LocalSink's wire shape.
type cri163NDJSONEnvelope struct {
	PayloadType string          `json:"payload_type"`
	Payload     json.RawMessage `json:"payload"`
}

type cri163AdapterEventPayload struct {
	Step string         `json:"step"`
	Kind string         `json:"kind"`
	Data map[string]any `json:"data"`
}

type cri163StepOutputPayload struct {
	Step    string            `json:"step"`
	Outputs map[string]string `json:"outputs"`
}

// parseCri163NDJSON decodes every LocalSink line into envelope+payload.
func parseCri163NDJSON(t *testing.T, buf []byte) []cri163NDJSONEnvelope {
	t.Helper()
	var envelopes []cri163NDJSONEnvelope
	sc := bufio.NewScanner(strings.NewReader(string(buf)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env cri163NDJSONEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("decode ND-JSON line %q: %v", line, err)
		}
		envelopes = append(envelopes, env)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan ND-JSON: %v", err)
	}
	return envelopes
}

// cri163AdapterEvent extracts the AdapterEvent payload from an envelope, if
// it is one.
func cri163AdapterEvent(t *testing.T, env cri163NDJSONEnvelope) (*cri163AdapterEventPayload, bool) {
	t.Helper()
	if env.PayloadType != "AdapterEvent" {
		return nil, false
	}
	var p cri163AdapterEventPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("decode AdapterEvent payload: %v", err)
	}
	return &p, true
}

// TestLocalSink_CRI163NestedCallEventsAuditRedaction is the end-to-end CRI-163
// test: ND-JSON events for tool.call / tool.call_result around the nested
// call, per-layer audit entries (caller layer 0, callee layer 1), and
// redaction of the sensitive callee output in events and audit entries alike.
func TestLocalSink_CRI163NestedCallEventsAuditRedaction(t *testing.T) {
	spec, diags := workflow.Parse("cri163.hcl", []byte(cri163WorkflowHCL))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller.default": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{"report": {CtyType: cty.String}},
		},
		"callee.default": {
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"token": {CtyType: cty.String, Sensitive: true},
			},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	const secretToken = "engine-secret-token-42"
	callee := &cri163CalleeFake{tokenOutput: secretToken}
	caller := &cri163CallerFake{}
	audit := &cri163AuditCollector{}
	loader := &cri163FakeLoader{adapters: map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	}}

	var ndjson bytes.Buffer
	sink := &LocalSink{RunID: "cri163", Out: &ndjson}

	if err := engine.New(g, loader, sink, engine.WithAuditWriter(audit)).Run(context.Background()); err != nil {
		for _, e := range audit.all() {
			t.Logf("DBG audit: sess=%s layer=%d decision=%s tool=%q reason=%q req=%s", e.SessionID, e.Layer, e.Decision, e.Tool, e.Reason, e.RequestID)
		}
		t.Logf("DBG ndjson:\n%s", ndjson.String())
		t.Fatalf("Run: %v", err)
	}

	envelopes := parseCri163NDJSON(t, ndjson.Bytes())
	if len(envelopes) == 0 {
		t.Fatal("no ND-JSON events emitted")
	}

	// --- tool.call appears before the callee starts; tool.call_result after
	// the callee finishes; payloads carry target, tool, depth, request_id,
	// outcome, duration. Two sequential nested calls run (call-1 plain,
	// call-2 with the sensitive value riding the target as the parsed tool
	// label), so events are located by request id and the two calls must
	// not interleave.
	call1Idx, result1Idx, call2Idx, result2Idx := -1, -1, -1, -1
	var calleeStartedIdxs, calleeFinishedIdxs []int
	for i, env := range envelopes {
		ev, ok := cri163AdapterEvent(t, env)
		if !ok {
			continue
		}
		reqID, _ := ev.Data["request_id"].(string)
		switch ev.Kind {
		case "tool.call":
			if ev.Step != cri163CallerStep {
				continue
			}
			switch reqID {
			case "call-1":
				if call1Idx < 0 {
					call1Idx = i
				}
			case "call-2":
				if call2Idx < 0 {
					call2Idx = i
				}
			}
		case "tool.call_result":
			if ev.Step != cri163CallerStep {
				continue
			}
			switch reqID {
			case "call-1":
				if result1Idx < 0 {
					result1Idx = i
				}
			case "call-2":
				if result2Idx < 0 {
					result2Idx = i
				}
			}
		case "callee.started":
			calleeStartedIdxs = append(calleeStartedIdxs, i)
		case "callee.finished":
			calleeFinishedIdxs = append(calleeFinishedIdxs, i)
		}
	}
	if call1Idx < 0 || result1Idx < 0 || call2Idx < 0 || result2Idx < 0 {
		t.Fatalf("nested-call events missing: call1=%d result1=%d call2=%d result2=%d", call1Idx, result1Idx, call2Idx, result2Idx)
	}
	if len(calleeStartedIdxs) != 2 || len(calleeFinishedIdxs) != 2 {
		t.Fatalf("callee markers: started=%v finished=%v, want two of each (two nested calls)", calleeStartedIdxs, calleeFinishedIdxs)
	}
	// Call-1: start before the callee's first marker; result after the first
	// callee finished and before the second call begins.
	if !(call1Idx < calleeStartedIdxs[0]) {
		t.Errorf("tool.call (idx %d) must appear before the nested call starts (callee.started idx %d)", call1Idx, calleeStartedIdxs[0])
	}
	if !(result1Idx > calleeFinishedIdxs[0] && result1Idx < calleeStartedIdxs[1]) {
		t.Errorf("call-1 tool.call_result (idx %d) must follow the first callee.finished (idx %d) and precede the second nested call (callee.started idx %d)", result1Idx, calleeFinishedIdxs[0], calleeStartedIdxs[1])
	}
	// Call-2: start after call-1's result, before the second callee's first
	// marker; result after the second callee finished.
	if !(call2Idx > result1Idx && call2Idx < calleeStartedIdxs[1]) {
		t.Errorf("call-2 tool.call (idx %d) must follow call-1's result (idx %d) and precede the second callee start (idx %d)", call2Idx, result1Idx, calleeStartedIdxs[1])
	}
	if !(result2Idx > calleeFinishedIdxs[1]) {
		t.Errorf("call-2 tool.call_result (idx %d) must appear after the nested call finishes (callee.finished idx %d)", result2Idx, calleeFinishedIdxs[1])
	}

	callEv, _ := cri163AdapterEvent(t, envelopes[call1Idx])
	if callEv.Step != cri163CallerStep {
		t.Errorf("tool.call step = %q, want caller step %q (attributed under the caller)", callEv.Step, cri163CallerStep)
	}
	assertCri163PayloadFields(t, "tool.call", callEv.Data, map[string]any{
		"target":     cri163Target,
		"tool":       "helper_task",
		"depth":      float64(1),
		"request_id": "call-1",
	})

	resultEv, _ := cri163AdapterEvent(t, envelopes[result1Idx])
	assertCri163PayloadFields(t, "tool.call_result", resultEv.Data, map[string]any{
		"target":     cri163Target,
		"tool":       "helper_task",
		"depth":      float64(1),
		"request_id": "call-1",
		"outcome":    "success",
	})
	if dur, ok := resultEv.Data["duration"].(float64); !ok {
		t.Errorf("tool.call_result duration missing or not a number: %v", resultEv.Data["duration"])
	} else if dur < 80 {
		t.Errorf("tool.call_result duration = %v ms, want >= 80 (callee sleeps 100ms)", dur)
	}

	// --- Redaction of the event payloads: call-2's target embeds the
	// (registered) sensitive value as the parsed tool label; the
	// tool.call / tool.call_result payloads must carry the masked form.
	secretTargetMasked := cri163BareTarget + "." + cri163Redacted
	call2Ev, _ := cri163AdapterEvent(t, envelopes[call2Idx])
	assertCri163PayloadFields(t, "tool.call (call-2)", call2Ev.Data, map[string]any{
		"target":     secretTargetMasked,
		"tool":       cri163Redacted,
		"depth":      float64(1),
		"request_id": "call-2",
	})
	result2Ev, _ := cri163AdapterEvent(t, envelopes[result2Idx])
	assertCri163PayloadFields(t, "tool.call_result (call-2)", result2Ev.Data, map[string]any{
		"target":     secretTargetMasked,
		"tool":       cri163Redacted,
		"depth":      float64(1),
		"request_id": "call-2",
		"outcome":    "success",
	})

	// --- Redaction in events: the caller re-exported the sensitive callee
	// output as its own step output; the engine's redacting sink must mask it
	// in the ND-JSON stream, and the raw value must not leak anywhere.
	sawRedactedReport := false
	for _, env := range envelopes {
		if env.PayloadType != "StepOutputCaptured" {
			continue
		}
		var p cri163StepOutputPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("decode StepOutputCaptured payload: %v", err)
		}
		if p.Step != cri163CallerStep {
			continue
		}
		if got := p.Outputs["report"]; got == secretToken {
			t.Error("StepOutputCaptured carries the raw sensitive callee output; redaction registry not applied")
		} else if got == cri163Redacted {
			sawRedactedReport = true
		}
	}
	if !sawRedactedReport {
		t.Errorf("no StepOutputCaptured with redacted report %q for caller step", cri163Redacted)
	}
	if idx := strings.Index(ndjson.String(), secretToken); idx >= 0 {
		t.Errorf("raw sensitive value appears in the ND-JSON event stream at offset %d: %s", idx, ndjson.String())
	}

	// --- Per-layer audit: caller decisions at layer 0, callee decisions at
	// layer 1, distinguishable by session and layer. The echoed secret must
	// arrive masked in the audit entry too.
	entries := audit.all()
	type auditWant struct {
		session  string
		layer    int
		decision string
		tool     string
	}
	wants := []auditWant{
		{session: cri163CallerSess, layer: 0, decision: "allow", tool: cri163Target},
		{session: cri163CallerSess, layer: 0, decision: "allow", tool: cri163BareTarget + "." + cri163Redacted},
		{session: cri163CalleeSess, layer: 1, decision: "allow", tool: "callee.helpers.read_file"},
		{session: cri163CallerSess, layer: 0, decision: "allow", tool: "shell:echo " + cri163Redacted},
	}
	for _, want := range wants {
		found := false
		for _, e := range entries {
			if e.SessionID == want.session && e.Decision == want.decision && e.Tool == want.tool && e.Layer == want.layer {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("audit entry missing: session=%q layer=%d decision=%q tool=%q (entries: %+v)", want.session, want.layer, want.decision, want.tool, entries)
		}
	}
	for i, e := range entries {
		if strings.Contains(e.Tool, secretToken) || strings.Contains(e.Reason, secretToken) {
			t.Errorf("audit entry %d carries the raw sensitive value: %+v", i, e)
		}
	}
}

func assertCri163PayloadFields(t *testing.T, kind string, got, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s payload field %q = %#v, want %#v (payload: %#v)", kind, k, got[k], v, got)
		}
	}
}

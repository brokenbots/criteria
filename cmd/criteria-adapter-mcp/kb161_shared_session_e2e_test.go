package main

// kb161_shared_session_e2e_test.go — KB-161 acceptance for the adapter_tools
// e2e lane: an agent tool-call reaching an mcp tool-resource runs through
// ONE shared session with the REAL adapter binary and the REAL echo fixture
// server (the KB-156 engine tests pin the same contract with in-memory
// probes; this pins the process-level evidence). Two concurrent callers
// inside a for_each fan onto one fixture process:
//   - a single server process serves every call (pid/call logs);
//   - interleaved calls keep exact per-request_id attribution (no leakage);
//   - a lease-holder that completes first cannot tear the shared session
//     out from under the still-running lease holder;
//   - overlap markers prove the calls genuinely shared the session.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// compileMCPSharedSessionGraph compiles the shared-session fixture workflow:
// identical to compileMCPToolsGraph plus an adapter-level env pass-through
// pointing the echo fixture's pid/call logs at temp files, so the tests can
// verify process-level session behavior. allowTools overrides the caller
// step's tool grant; the default keeps the broad tools.* grant.
func quotedList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, fmt.Sprintf("%q", item))
	}
	return strings.Join(quoted, ", ")
}

func compileMCPSharedSessionGraph(t *testing.T, echoBin, env string, allowTools ...string) *workflow.FSMGraph {
	t.Helper()
	if len(allowTools) == 0 {
		allowTools = []string{"adapter.mcp.tools.tools.*"}
	}
	src := fmt.Sprintf(`workflow {
  name          = "mcp_shared_session_e2e"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

permissions {
  allow_tools = ["echo", "structured"]
}

adapter "mcp" "tools" {
  config {
    command = %q
    env     = %q
  }
  dynamic_tools = true
}

adapter "caller" "default" {}

step "call" {
  target      = adapter.caller.default
  allow_tools = [%s]

  outcome "handled" {
    next = state.done
  }

  outcome "unhandled" {
    next = state.unhandled
  }
}

state "done" {
  terminal = true
}

state "unhandled" {
  terminal = true
}
`, echoBin, env, quotedList(allowTools))
	spec, diags := workflow.Parse("mcp_shared_session_e2e.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse shared-session workflow: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"caller": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{},
			Capabilities: []string{"adapter_tools"},
		},
		"mcp": {
			ConfigSchema: map[string]workflow.ConfigField{
				"command": {Required: true, Type: workflow.ConfigFieldString},
				"env":     {Type: workflow.ConfigFieldString},
			},
			Capabilities: []string{"adapter_tools"},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile shared-session workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("shared-session workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// readLogLines reads a fixture log; an absent file means nothing was ever
// appended (the assertions decide whether that is acceptable).
func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fixture log %s: %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// logPids extracts the distinct "pid=N" suffixes of log lines.
func logPids(lines []string) map[string]int {
	pids := map[string]int{}
	for _, line := range lines {
		if i := strings.Index(line, "pid="); i >= 0 {
			fields := strings.Fields(line[i:])
			if len(fields) > 0 {
				pids[strings.TrimPrefix(fields[0], "pid=")]++
			}
		}
	}
	return pids
}

// TestMCPAdapterTools_SharedSessionSurvivesLeaseDrop pins the KB-161
// shared-session acceptance with real processes: two concurrent tool calls
// issued by one caller execution share ONE fixture server process. The short
// call's lease drops mid-flight of the long call; the session must stay
// alive for the long call (no tear-out), every answer must carry only its
// own request's message, and the pid log must show the single process.
// TestMCPAdapterTools_ForEachFanOutSharesOneSession extends this to the
// engine's parallel for_each fan-out, where each iteration is its own caller
// execution with its own lease.
func TestMCPAdapterTools_SharedSessionSurvivesLeaseDrop(t *testing.T) {
	if testEchoBin == "" {
		t.Fatal("echo-mcp binary not available (TestMain not run)")
	}
	pidLog := filepath.Join(t.TempDir(), "pids.log")
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := fmt.Sprintf("MCP_PIDLOG=%s,MCP_CALLLOG=%s", pidLog, callLog)

	caller := newMCPConcurrentToolsCaller("handled",
		toolsCall{
			requestID: "call-a",
			target:    "adapter.mcp.tools.tools.echo",
			args:      map[string]any{"tool": "echo", "message": "msg-a", "sleep_ms": "150"},
		},
		toolsCall{
			requestID: "call-b",
			target:    "adapter.mcp.tools.tools.echo",
			args:      map[string]any{"tool": "echo", "message": "msg-b", "sleep_ms": "900"},
		},
	)
	sink := runMCPToolsGraphCase(t, compileMCPSharedSessionGraph(t, testEchoBin, env), caller, nil)

	results := caller.gotResults()
	if len(results) != 2 {
		t.Fatalf("typed replies = %d (%+v), want one per scripted call", len(results), results)
	}
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("cancels received = %d, want 0 (a sibling lease-drop must not cancel in-flight calls): %+v", len(cancels), cancels)
	}
	byID := map[string]toolsReply{}
	for _, reply := range results {
		byID[reply.requestID] = reply
	}
	for id, ownMessage := range map[string]string{"call-a": "msg-a", "call-b": "msg-b"} {
		reply, ok := byID[id]
		if !ok {
			t.Fatalf("no typed reply for %q; results = %+v", id, results)
		}
		if reply.outcome != "success" || reply.callError != "" {
			t.Fatalf("reply for %q = %+v, want success with no call_error", id, reply)
		}
		text, _ := reply.outputs["text"].(string)
		if !strings.Contains(text, ownMessage) {
			t.Fatalf("reply for %q text %q lacks its own message %q", id, text, ownMessage)
		}
		other := "msg-b"
		if id == "call-b" {
			other = "msg-a"
		}
		if strings.Contains(text, other) {
			t.Fatalf("reply for %q text %q carries the sibling's message %q: cross-call leakage", id, text, other)
		}
	}

	// Single server process: exactly one fixture pid serves both calls, and
	// the call log attributes both calls to that one pid.
	pidLines := readLogLines(t, pidLog)
	if pids := logPids(pidLines); len(pids) != 1 {
		t.Fatalf("fixture pid log lines %v produce %d distinct pids; want exactly 1 for a single shared server process", pidLines, len(pids))
	}
	calls := readLogLines(t, callLog)
	if len(calls) != 2 {
		t.Fatalf("fixture call log = %v, want exactly one entry per call", calls)
	}
	if got := len(logPids(calls)); got != 1 {
		t.Fatalf("call log lines %v carry %d distinct pids; want all calls served by one process", calls, got)
	}

	// Genuine sharing: both calls were in flight on the callee session at
	// the same time (overlap marker >= 2), each with its own progress token.
	maxOverlap := 0.0
	tokens := map[string]bool{}
	progress := 0
	for _, ev := range sink.stepEvents() {
		if ev.kind != "mcp.progress" {
			continue
		}
		progress++
		if token, _ := ev.payload["progressToken"].(string); token != "" {
			tokens[token] = true
		}
		if overlap, ok := numeric(ev.payload["criteria_overlap"]); ok && overlap > maxOverlap {
			maxOverlap = overlap
		}
	}
	if progress == 0 {
		t.Fatalf("no mcp.progress reached the callee stream: %v", kindsOf(sink.stepEvents()))
	}
	if maxOverlap < 2 {
		t.Fatalf("calls did not overlap on the shared session: peak criteria_overlap=%.0f want >= 2", maxOverlap)
	}
	if len(tokens) != progress {
		t.Fatalf("progress tokens %v are not one distinct token per call (progress events = %d)", tokens, progress)
	}

	assertRunContinued(t, sink, "done")
}

// TestMCPAdapterTools_DeniedToolNeverRuns pins the KB-161 permission-path
// lane at the real seam: a DISCOVERED fixture tool whose target the caller's
// grant does not cover is never invoked — the fixture's call log stays
// silent for it — the host answers with a policy deny: a cancel on the
// caller's permission stream plus an audited deny decision, NOT the typed
// unknown_tool signature; the session stays healthy for allowed calls, and
// the run continues.
func TestMCPAdapterTools_DeniedToolNeverRuns(t *testing.T) {
	if testEchoBin == "" {
		t.Fatal("echo-mcp binary not available (TestMain not run)")
	}
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := fmt.Sprintf("MCP_CALLLOG=%s", callLog)

	caller := newMCPToolsCaller("handled", toolsCall{
		requestID: "call-structured",
		target:    "adapter.mcp.tools.tools.structured",
		args:      map[string]any{"tool": "structured"},
	})
	audit := &mcpToolsAuditCollector{}
	sink := runMCPToolsGraphCase(t,
		compileMCPSharedSessionGraph(t, testEchoBin, env, "adapter.mcp.tools.tools.echo"),
		caller, audit)

	// The denied call surfaces as a host-side denial to the caller: a
	// cancel on the permission stream, never a typed tool_call_result —
	// the typed unknown_tool signature is reserved for undiscovered tools
	// (pinned by TestMCPAdapterTools_CallUnknownTool).
	cancels := caller.gotCancels()
	if len(cancels) != 1 || cancels[0].requestID != "call-structured" {
		t.Fatalf("cancels = %+v, want one cancel for \"call-structured\"", cancels)
	}
	if results := caller.gotResults(); len(results) != 0 {
		t.Fatalf("typed results = %+v, want none (a policy deny is not a tool result)", results)
	}

	// The denied tool never ran: the fixture call log must carry no
	// structured line.
	for _, line := range readLogLines(t, callLog) {
		if strings.Contains(line, "call=structured") {
			t.Fatalf("denied tool ran: fixture call log line %q", line)
		}
	}

	// The host audited the deny with the denied tool and a non-empty reason.
	denied := false
	for _, e := range audit.all() {
		if e != nil && e.Decision == "deny" && strings.Contains(e.Tool, "structured") && e.Reason != "" {
			denied = true
		}
	}
	if !denied {
		t.Errorf("audit carries no deny decision for \"structured\"; entries = %+v", audit.all())
	}

	// The deny decision reached the stream as a permission.denied event.
	if !sink.permissionDenied() {
		t.Error("no permission.denied event streamed; want the host's deny decision surfaced")
	}

	// The run itself continued: the deny is data for the caller, and the
	// caller's own outcome routing still reached done.
	if got, want := sink.terminalState(), "done"; got != want {
		t.Fatalf("terminal state = %q, want %q (failure=%q)", got, want, sink.runFailure())
	}
	if got, want := sink.stepsRun(), []string{mcpToolsCallerStep}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("steps run = %v, want %v", got, want)
	}
}

// compileMCPFanSessionGraph compiles the for_each fan-out variant of the
// shared-session fixture workflow: the caller step is an iterating step fanned
// out with parallel = [...] (the engine's concurrent for_each fan-out — for_each
// alone is sequential; the concurrency contract requires the parallel form) with
// two items and parallel_max = 2, so TWO independent caller executions — each
// holding its own lease on the callee session — run concurrently. The echo
// fixture's pid/call logs are pointed at temp files as in
// compileMCPSharedSessionGraph.
func compileMCPFanSessionGraph(t *testing.T, echoBin, env string) *workflow.FSMGraph {
	t.Helper()
	src := fmt.Sprintf(`workflow {
  name          = "mcp_shared_session_fanout_e2e"
  version       = "0.1"
  initial_state = "fan"
  target_state  = "done"
}

permissions {
  allow_tools = ["echo", "structured"]
}

adapter "mcp" "tools" {
  config {
    command = %q
    env     = %q
  }
  dynamic_tools = true
}

adapter "caller" "default" {}

step "fan" {
  target       = adapter.caller.default
  parallel     = ["call-a", "call-b"]
  parallel_max = 2
  allow_tools  = ["adapter.mcp.tools.tools.*"]

  outcome "all_succeeded" {
    next = state.done
  }

  outcome "any_failed" {
    next = state.failed
  }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`, echoBin, env)
	spec, diags := workflow.Parse("mcp_shared_session_fanout_e2e.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse fan-out workflow: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		// The caller fans out concurrently, so it must declare parallel_safe
		// (compile-time gate) — its fakes are concurrency-safe by contract.
		"caller": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{},
			Capabilities: []string{"adapter_tools", "execute", "parallel_safe"},
		},
		"mcp": {
			ConfigSchema: map[string]workflow.ConfigField{
				"command": {Required: true, Type: workflow.ConfigFieldString},
				"env":     {Type: workflow.ConfigFieldString},
			},
			Capabilities: []string{"adapter_tools"},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile fan-out workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("fan-out workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// mcpFanToolsCaller is the fan-out caller: each parallel for_each iteration is
// its own Execute invocation — ONE scripted call per iteration, dispatched by
// the engine's own iteration goroutine (unlike mcpConcurrentToolsCaller, which
// fans every scripted call out of a single Execute). A central reader consumes
// the shared permission stream and attributes each typed reply to its owner by
// request_id, so iterations wait only for their own call no matter how the
// sibling calls interleave.
type mcpFanToolsCaller struct {
	mcpToolsCaller
	next   atomic.Uint32
	routes map[string]chan *v2.PermissionEvent
}

func newMCPFanToolsCaller(outcome string, script ...toolsCall) *mcpFanToolsCaller {
	c := &mcpFanToolsCaller{
		mcpToolsCaller: mcpToolsCaller{
			capabilities: []string{"adapter_tools", "execute", "parallel_safe"},
			outcome:      outcome,
			script:       script,
		},
		routes: map[string]chan *v2.PermissionEvent{},
	}
	for _, call := range script {
		c.routes[call.requestID] = make(chan *v2.PermissionEvent, 1)
	}
	return c
}

// StartPermissionStream stores the shared requests stream and spawns the
// central reader that routes settled replies/cancels to their owner. A
// second start is refused: the whole point of the fan-out is ONE session
// multiplexing ONE permission stream, so a re-start would mean the fake
// drifted from the single-active-sink contract.
func (a *mcpFanToolsCaller) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	if a.requests != nil {
		a.mu.Unlock()
		return func() {}, errors.New("fan caller: permission stream already started")
	}
	a.requests = requests
	a.mu.Unlock()
	go a.readPermissionStream(requests)
	return func() {}, nil
}

// readPermissionStream is the request_id router: ONE reader on the shared
// stream (the multiplexing contract — replies arrive in any order), recording
// every settled reply and cancel and unblocking its owning iteration.
func (a *mcpFanToolsCaller) readPermissionStream(requests <-chan *v2.PermissionEvent) {
	for ev := range requests {
		if reply, settled := toolsTypedReply(ev); settled {
			a.recordResult(reply)
			a.route(reply.requestID, ev)
			continue
		}
		if cancel := ev.GetCancel(); cancel != nil {
			a.mu.Lock()
			a.cancels = append(a.cancels, toolsReply{requestID: cancel.GetRequestId()})
			a.mu.Unlock()
			a.route(cancel.GetRequestId(), ev)
		}
		// Other stream traffic is not part of the call path; skip it.
	}
	a.routeClosed()
}

// route delivers a settled PermissionEvent to the waiting iteration. The
// buffered channel absorbs a reply that lands before the waiter starts.
func (a *mcpFanToolsCaller) route(requestID string, ev *v2.PermissionEvent) {
	a.mu.Lock()
	route, ok := a.routes[requestID]
	a.mu.Unlock()
	if !ok {
		return
	}
	select {
	case route <- ev:
	default:
	}
}

// routeClosed unblocks every iteration still waiting when the stream closes.
func (a *mcpFanToolsCaller) routeClosed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, route := range a.routes {
		select {
		case route <- nil:
		default:
		}
	}
}

// Execute dispatches this invocation's own scripted call (index-based) and
// returns once its OWN reply settles; sibling iterations — and their leases —
// are managed by the engine's parallel fan-out.
func (a *mcpFanToolsCaller) Execute(_ context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	idx := int(a.next.Add(1)) - 1
	if idx < len(a.script) {
		call := a.script[idx]
		sink.Adapter("permission.request", map[string]any{
			"request_id": call.requestID,
			"target":     call.target,
			"args":       call.args,
		})
		a.awaitOwnReply(call)
	}
	return adapter.Result{Outcome: a.outcome}, nil
}

// awaitOwnReply blocks until the reader routes this call's settle event.
func (a *mcpFanToolsCaller) awaitOwnReply(call toolsCall) {
	a.mu.Lock()
	route, ok := a.routes[call.requestID]
	a.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ev := <-route:
		if ev == nil {
			a.recordResult(toolsReply{requestID: call.requestID, callError: "test-harness-error: permission stream closed"})
		}
	case <-time.After(mcpToolsAwaitReplyTimeout):
		a.recordResult(toolsReply{requestID: call.requestID, callError: "test-harness-error: timed out waiting for reply"})
	}
}

// TestMCPAdapterTools_ForEachFanOutSharesOneSession pins the KB-161 for_each
// acceptance with real processes: TWO independent caller executions fanned out
// by the engine's parallel iteration machinery (the concurrent form of the
// for_each contract — a plain for_each is sequential and never produces
// concurrent callers) each hold their own lease on the ONE shared fixture
// session. The short iteration's lease drops while the long iteration is still
// in flight; the long call must complete untouched by that drop (no tear-out,
// no cancels), every iteration must wait only for its own request_id, and the
// pid log must show the single process serving both iterations.
func TestMCPAdapterTools_ForEachFanOutSharesOneSession(t *testing.T) {
	if testEchoBin == "" {
		t.Fatal("echo-mcp binary not available (TestMain not run)")
	}
	pidLog := filepath.Join(t.TempDir(), "pids.log")
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := fmt.Sprintf("MCP_PIDLOG=%s,MCP_CALLLOG=%s", pidLog, callLog)

	caller := newMCPFanToolsCaller("success",
		toolsCall{
			requestID: "call-a",
			target:    "adapter.mcp.tools.tools.echo",
			args:      map[string]any{"tool": "echo", "message": "msg-a", "sleep_ms": "150"},
		},
		toolsCall{
			requestID: "call-b",
			target:    "adapter.mcp.tools.tools.echo",
			args:      map[string]any{"tool": "echo", "message": "msg-b", "sleep_ms": "900"},
		},
	)
	sink := runMCPToolsGraphCase(t, compileMCPFanSessionGraph(t, testEchoBin, env), caller, nil)

	// The parallel fan-out genuinely fanned: two items entered and two
	// independent caller executions started.
	if total := sink.fanOutTotal(); total != 2 {
		t.Fatalf("iterating step reported total = %d, want 2 items", total)
	}
	started := sink.iterationsStartedList()
	if len(started) != 2 {
		t.Fatalf("iterations started = %v, want one per fan item", started)
	}

	// Per-request_id attribution: each iteration's reply carries only its own
	// call's message — correlation is by request_id, never by arrival order.
	results := caller.gotResults()
	if len(results) != 2 {
		t.Fatalf("typed replies = %d (%+v), want one per scripted call", len(results), results)
	}
	byID := map[string]toolsReply{}
	for _, reply := range results {
		byID[reply.requestID] = reply
	}
	for id, ownMessage := range map[string]string{"call-a": "msg-a", "call-b": "msg-b"} {
		reply, ok := byID[id]
		if !ok {
			t.Fatalf("no typed reply for %q; results = %+v", id, results)
		}
		if reply.outcome != "success" || reply.callError != "" {
			t.Fatalf("reply for %q = %+v, want success with no call_error", id, reply)
		}
		text, _ := reply.outputs["text"].(string)
		if !strings.Contains(text, ownMessage) {
			t.Fatalf("reply for %q text %q lacks its own message %q", id, text, ownMessage)
		}
		other := "msg-b"
		if id == "call-b" {
			other = "msg-a"
		}
		if strings.Contains(text, other) {
			t.Fatalf("reply for %q text %q carries the sibling's message %q: cross-call leakage", id, text, other)
		}
	}

	// No lease-holder torn the shared session out from under the other: the
	// sibling leases survive (the long call succeeds after the short
	// iteration's lease dropped) and no cancel reached either iteration.
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("cancels received = %d, want 0 (a sibling lease-drop must not cancel in-flight calls): %+v", len(cancels), cancels)
	}
	if done := sink.iterationsCompletedList(); len(done) != 1 || done[0] != "fan=all_succeeded" {
		t.Fatalf("aggregate outcome = %v, want [fan=all_succeeded]", done)
	}

	// Single server process: exactly one fixture pid serves both iterations,
	// and the call log attributes both calls to that one pid.
	pidLines := readLogLines(t, pidLog)
	if pids := logPids(pidLines); len(pids) != 1 {
		t.Fatalf("fixture pid log lines %v produce %d distinct pids; want exactly 1 for a single shared server process", pidLines, len(pids))
	}
	calls := readLogLines(t, callLog)
	if len(calls) != 2 {
		t.Fatalf("fixture call log = %v, want exactly one entry per call", calls)
	}
	if got := len(logPids(calls)); got != 1 {
		t.Fatalf("call log lines %v carry %d distinct pids; want all calls served by one process", calls, got)
	}

	// Genuine sharing: both iterations' calls were in flight on the callee
	// session at the same time (overlap marker >= 2) — the short iteration's
	// lease dropped before the long call settled, so the long call ran across
	// that drop on the shared session.
	maxOverlap := 0.0
	tokens := map[string]bool{}
	progress := 0
	for _, ev := range sink.stepEvents() {
		if ev.kind != "mcp.progress" {
			continue
		}
		progress++
		if token, _ := ev.payload["progressToken"].(string); token != "" {
			tokens[token] = true
		}
		if overlap, ok := numeric(ev.payload["criteria_overlap"]); ok && overlap > maxOverlap {
			maxOverlap = overlap
		}
	}
	if progress == 0 {
		t.Fatalf("no mcp.progress reached the callee stream: %v", kindsOf(sink.stepEvents()))
	}
	if maxOverlap < 2 {
		t.Fatalf("fan-out calls did not overlap on the shared session: peak criteria_overlap=%.0f want >= 2", maxOverlap)
	}
	if len(tokens) != progress {
		t.Fatalf("progress tokens %v are not one distinct token per call (progress events = %d)", tokens, progress)
	}

	// The run continued: the fan step itself entered, the aggregate outcome
	// routed to done, and the run completed successfully.
	if got, want := sink.terminalState(), "done"; got != want {
		t.Fatalf("terminal state = %q, want %q (failure=%q)", got, want, sink.runFailure())
	}
	if !sink.runOK() {
		t.Fatalf("run did not complete successfully: failure=%q", sink.runFailure())
	}
	if got := sink.stepsRun(); len(got) != 2 || got[0] != "fan" || got[1] != "fan" {
		t.Fatalf("steps run = %v, want two \"fan\" entries (one per parallel caller execution)", got)
	}
}

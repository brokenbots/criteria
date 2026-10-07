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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// shared-session acceptance with real processes: two concurrent callers —
// the for_each fan-in shape — share ONE fixture server process. The short
// call's lease drops mid-flight of the long call; the session must stay
// alive for the long call (no tear-out), every answer must carry only its
// own request's message, and the pid log must show the single process.
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

package main

// End-to-end adapter-tools integration for the MCP adapter (CRI-172): a
// workflow step tool-calls adapter.mcp.<name>.tools.<tool> through the real
// engine, the host's adapter-tools seam dispatches it as a nested Execute on
// a real mcp adapter binary (TestMain-built), and the bridge answers with an
// MCP tools/call against the scripted echo fixture server.
//
// Covered here, per the CRI-172 exit criteria:
//
//   - a known-tool call succeeds: the MCP tools/call result content is
//     mapped into the tool result's outputs_json (text as the primary
//     payload) and delivered to the caller as a typed tool_call_result;
//   - a call naming a tool outside the discovered tools/list surface fails
//     typed call_error "unknown_tool" — never a policy deny (no cancel);
//   - structuredContent, when the MCP server provides it, passes through
//     into outputs_json under "structured";
//   - the adapter's Info surface advertises the tools discovered at
//     OpenSession (CRI-171) plus the adapter_tools capability.
//
// The caller is a scripted in-memory fake mirroring the conformance matrix's
// caller: its Execute emits the §8 permission.request AdapterEvent, the host
// dispatches the nested call, and the fake reads the typed reply off its
// Permissions stream. The permission event IS the adapter-tool call: the
// nested MCP call proceeds on the host's grant with no second permission
// hop, preserving the bridge's existing awaitPermission ordering.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

const (
	// mcpToolsCallerStep is the caller step name every integration case uses.
	mcpToolsCallerStep = "call"

	// mcpToolsAwaitReplyTimeout bounds how long the scripted caller waits
	// for a typed reply. Generous: every failure path is delivered by the
	// host well before this fires.
	mcpToolsAwaitReplyTimeout = 15 * time.Second
)

// toolsCall is one scripted adapter-tool call the caller fake issues.
type toolsCall struct {
	requestID string
	target    string
	args      map[string]any
}

// toolsReply records one typed tool_call_result reply (or cancel) the caller
// received on its Permissions stream.
type toolsReply struct {
	requestID string
	outcome   string
	callError string
	outputs   map[string]any
}

// mcpToolsCaller is the scripted caller fake. Each script entry is emitted as
// an adapter-tool permission.request; the fake blocks for the typed reply on
// its permission stream before issuing the next call, then returns its own
// configured outcome.
type mcpToolsCaller struct {
	capabilities []string
	script       []toolsCall
	outcome      string

	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	results  []toolsReply
	cancels  []toolsReply
}

func newMCPToolsCaller(outcome string, script ...toolsCall) *mcpToolsCaller {
	return &mcpToolsCaller{
		capabilities: []string{"adapter_tools", "execute"},
		outcome:      outcome,
		script:       script,
	}
}

func (a *mcpToolsCaller) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "mcp-tools-caller",
		Version:      "0.0.0-test",
		Capabilities: append([]string(nil), a.capabilities...),
	}, nil
}

func (a *mcpToolsCaller) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (a *mcpToolsCaller) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}

func (a *mcpToolsCaller) Execute(_ context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	for _, call := range a.script {
		sink.Adapter("permission.request", map[string]any{
			"request_id": call.requestID,
			"target":     call.target,
			"args":       call.args,
		})
		if !a.awaitReply(call) {
			break
		}
	}
	a.mu.Lock()
	outcome := a.outcome
	a.mu.Unlock()
	return adapter.Result{Outcome: outcome}, nil
}

// toolsTypedReply converts a typed tool_call_result stream event into the
// reply the caller records; settled=false for other stream traffic.
func toolsTypedReply(ev *v2.PermissionEvent) (toolsReply, bool) {
	tcr := ev.GetToolCallResult()
	if tcr == nil {
		return toolsReply{}, false
	}
	reply := toolsReply{
		requestID: tcr.GetRequestId(),
		outcome:   tcr.GetOutcome(),
		callError: tcr.GetCallError(),
	}
	if len(tcr.GetOutputsJson()) > 0 {
		var outputs map[string]any
		if err := json.Unmarshal(tcr.GetOutputsJson(), &outputs); err != nil {
			reply.callError = "test-harness-error: decoding outputs: " + err.Error()
			reply.outcome = ""
		}
		reply.outputs = outputs
	}
	return reply, true
}

// awaitReply blocks until the host settles the call: a typed
// tool_call_result is recorded and reported as delivered; a cancel is
// recorded and reported as a denial (which terminates the script).
func (a *mcpToolsCaller) awaitReply(call toolsCall) bool {
	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		a.recordResult(toolsReply{requestID: call.requestID, callError: "test-harness-error: permission stream not started"})
		return false
	}
	deadline := time.After(mcpToolsAwaitReplyTimeout)
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				a.recordResult(toolsReply{requestID: call.requestID, callError: "test-harness-error: permission stream closed"})
				return false
			}
			if reply, settled := toolsTypedReply(ev); settled {
				a.recordResult(reply)
				return true
			}
			if cancel := ev.GetCancel(); cancel != nil {
				a.mu.Lock()
				a.cancels = append(a.cancels, toolsReply{requestID: cancel.GetRequestId()})
				a.mu.Unlock()
				return false
			}
			// Other stream traffic is not part of the call path; skip it.
		case <-deadline:
			a.recordResult(toolsReply{requestID: call.requestID, callError: "test-harness-error: timed out waiting for reply"})
			return false
		}
	}
}

func (a *mcpToolsCaller) recordResult(reply toolsReply) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.results = append(a.results, reply)
}

func (a *mcpToolsCaller) gotResults() []toolsReply {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]toolsReply(nil), a.results...)
}

func (a *mcpToolsCaller) gotCancels() []toolsReply {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]toolsReply(nil), a.cancels...)
}

func (a *mcpToolsCaller) CloseSession(context.Context, string) error { return nil }
func (a *mcpToolsCaller) Kill()                                      {}
func (a *mcpToolsCaller) Pause(context.Context, string) error        { return nil }
func (a *mcpToolsCaller) Resume(context.Context, string) error       { return nil }
func (a *mcpToolsCaller) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *mcpToolsCaller) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *mcpToolsCaller) Restore(context.Context, string, []byte, uint32) error { return nil }

// mcpToolsLoader resolves the caller fake in-memory and delegates the mcp
// adapter to a real binary loader, so the nested call runs against the actual
// adapter process.
type mcpToolsLoader struct {
	fakes map[string]adapterhost.Handle
	real  adapterhost.Loader
}

func (l *mcpToolsLoader) Resolve(ctx context.Context, name string) (adapterhost.Handle, error) {
	if h, ok := l.fakes[name]; ok {
		return h, nil
	}
	return l.real.Resolve(ctx, name)
}

func (l *mcpToolsLoader) Shutdown(ctx context.Context) error { return l.real.Shutdown(ctx) }

// mcpToolsEvent is one recorded adapter event on a step's event sink.
type mcpToolsEvent struct {
	kind    string
	payload map[string]any
}

// mcpToolsSink is a full engine.Sink recording the run-level facts the cases
// assert: which steps ran, the terminal state, the outcome routing, and the
// adapter outputs captured per step.
type mcpToolsSink struct {
	mu       sync.Mutex
	entered  []string
	outcomes []string
	terminal string
	ok       bool
	failure  string
	events   []mcpToolsEvent
	outputs  map[string]map[string]string

	// Iteration fan-out facts (recorded via the lockedSink from the parallel
	// iteration goroutines; mu keeps them race-free).
	iterationsStarted []string
	iterationsDone    []string
	fanTotal          int
}

// stepOutputs returns the outputs captured for one step.
func (s *mcpToolsSink) stepOutputs(step string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.outputs[step]
	cp := make(map[string]string, len(out))
	for k, v := range out {
		cp[k] = v
	}
	return cp
}

func (s *mcpToolsSink) OnRunStarted(string, string) {}
func (s *mcpToolsSink) OnRunCompleted(finalState string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminal = finalState
	s.ok = success
}
func (s *mcpToolsSink) OnRunFailed(reason, step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = reason + " (step " + step + ")"
}
func (s *mcpToolsSink) OnStepEntered(step, _ string, _ int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered = append(s.entered, step)
}
func (s *mcpToolsSink) OnStepOutcome(step, outcome string, _ time.Duration, _ error, comment string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = append(s.outcomes, step+"="+outcome)
}
func (s *mcpToolsSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputs == nil {
		s.outputs = map[string]map[string]string{}
	}
	s.outputs[step] = outputs
}
func (s *mcpToolsSink) OnStepTransition(from, to, _ string)                          {}
func (s *mcpToolsSink) OnStepResumed(string, int, string)                            {}
func (s *mcpToolsSink) OnVariableSet(string, string, string)                         {}
func (s *mcpToolsSink) OnRunPaused(string, string, string)                           {}
func (s *mcpToolsSink) OnRunResumed(string)                                          {}
func (s *mcpToolsSink) OnWaitEntered(string, string, string, string)                 {}
func (s *mcpToolsSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *mcpToolsSink) OnApprovalRequested(string, []string, string)                 {}
func (s *mcpToolsSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *mcpToolsSink) OnBranchEvaluated(string, string, string, string)             {}
func (s *mcpToolsSink) OnForEachEntered(step string, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fanTotal = total
}
func (s *mcpToolsSink) OnStepIterationStarted(step string, _ int, item string, _ bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.iterationsStarted = append(s.iterationsStarted, step+"="+item)
}
func (s *mcpToolsSink) OnStepIterationCompleted(step, outcome string, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.iterationsDone = append(s.iterationsDone, step+"="+outcome)
}
func (s *mcpToolsSink) OnStepIterationItem(string, int, string)                      {}
func (s *mcpToolsSink) OnScopeIterCursorSet(string)                                  {}
func (s *mcpToolsSink) OnAdapterLifecycle(string, string, string, string)            {}
func (s *mcpToolsSink) OnAdapterLifecycleEvent(*engine.AdapterLifecycleEvent)        {}
func (s *mcpToolsSink) OnRunOutputs([]map[string]string)                             {}
func (s *mcpToolsSink) OnStepOutcomeDefaulted(string, string, string)                {}
func (s *mcpToolsSink) OnStepOutcomeUnknown(string, string)                          {}
func (s *mcpToolsSink) OnStepOutcomeInvalid(string, string, []string, int)           {}

func (s *mcpToolsSink) OnAgentPromptInjected(string, string, string, string, time.Time) {}

func (s *mcpToolsSink) OnCheckpointPointer(*engine.CheckpointPointerEvent) {}

func (s *mcpToolsSink) StepEventSink(string) adapter.EventSink {
	return &mcpToolsEventRecorder{sink: s}
}

func (s *mcpToolsSink) terminalState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *mcpToolsSink) runOK() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ok
}

func (s *mcpToolsSink) runFailure() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *mcpToolsSink) stepOutcomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.outcomes...)
}

func (s *mcpToolsSink) stepsRun() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.entered...)
}

// permissionDenied reports whether any permission.denied event was emitted.
func (s *mcpToolsSink) permissionDenied() bool {
	for _, ev := range s.stepEvents() {
		if ev.kind == "permission.denied" {
			return true
		}
	}
	return false
}

// stepEvents returns a copy of the recorded step events.
func (s *mcpToolsSink) stepEvents() []mcpToolsEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mcpToolsEvent(nil), s.events...)
}

// iterationsStarted returns the recorded item fan-outs ("step=item") for
// iterating steps.
func (s *mcpToolsSink) iterationsStartedList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.iterationsStarted...)
}

// fanOutTotal returns the item count the last entered iterating step
// reported, or -1 when no iterating step was entered.
func (s *mcpToolsSink) fanOutTotal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fanTotal == 0 {
		return -1
	}
	return s.fanTotal
}

// iterationsCompletedList returns the recorded iteration completions
// ("step=outcome") for iterating steps; on parallel steps each entry carries
// the aggregate outcome.
func (s *mcpToolsSink) iterationsCompletedList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.iterationsDone...)
}

// mcpToolsEventRecorder records adapter events attributed to the caller step.
type mcpToolsEventRecorder struct {
	sink *mcpToolsSink
}

func (r *mcpToolsEventRecorder) Log(string, []byte) {}
func (r *mcpToolsEventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	cp := make(map[string]any, len(payload))
	for k, v := range payload {
		cp[k] = v
	}
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	r.sink.events = append(r.sink.events, mcpToolsEvent{kind: kind, payload: cp})
}

// compileMCPToolsGraph parses and compiles the integration workflow. The
// compile-time schemas mirror the runtime handshakes: the caller declares the
// adapter_tools capability so the step's tools surface compiles without
// warnings, and the mcp adapter declares only its adapter-level config schema
// — its step-input surface is dynamic (MCP tool arguments are not statically
// declarable).
func compileMCPToolsGraph(t *testing.T, echoBin, targetState string) *workflow.FSMGraph {
	t.Helper()
	src := fmt.Sprintf(`workflow {
  name          = "mcp_adapter_tools_integration"
  version       = "0.1"
  initial_state = "call"
  target_state  = %q
}

permissions {
  allow_tools = ["echo", "structured"]
}

adapter "mcp" "tools" {
  config {
    command = %q
  }
  dynamic_tools = true
}

adapter "caller" "default" {}

step "call" {
  target      = adapter.caller.default
  allow_tools = ["adapter.mcp.tools.tools.*"]

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
`, targetState, echoBin)
	spec, diags := workflow.Parse("mcp_tools_integration.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse mcp tools workflow: %s", diags)
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
			},
			Capabilities: []string{"adapter_tools"},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile mcp tools workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("mcp tools workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// compileMCPToolsTypedGraph compiles the KB-59 variant: the same fixture
// workflow, but the mcp adapter declares named types and a contract on the
// echo tool. The callee stays schema-less (no input schema in the compile
// schemas — the dynamic surface); the contract is the declared schema at the
// seam.
func compileMCPToolsTypedGraph(t *testing.T, echoBin, targetState string) *workflow.FSMGraph {
	t.Helper()
	src := fmt.Sprintf(`workflow {
  name          = "mcp_adapter_tools_typed"
  version       = "0.1"
  initial_state = "call"
  target_state  = %q
}

type "echo_request" {
  schema = object({
    tool    = string
    message = optional(string)
  })
}

type "echo_response" {
  schema = object({
    text = optional(string)
  })
}

permissions {
  allow_tools = ["echo", "structured"]
}

adapter "mcp" "tools" {
  config {
    command = %q
  }
  dynamic_tools = true

  tool "echo" {
    in  = type.echo_request
    out = type.echo_response
  }
}

adapter "caller" "default" {}

step "call" {
  target      = adapter.caller.default
  allow_tools = ["adapter.mcp.tools.tools.*"]

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
`, targetState, echoBin)
	spec, diags := workflow.Parse("mcp_tools_typed.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse mcp tools workflow: %s", diags)
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
			},
			Capabilities: []string{"adapter_tools"},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile mcp tools typed workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("mcp tools typed workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// compileMCPDirectTypedGraph compiles the KB-59 direct-target variant: a step
// targeting adapter.mcp.tools whose typed input block is validated against
// the declared contract instead of the (absent) step-input schema.
func compileMCPDirectTypedGraph(t *testing.T, echoBin string) *workflow.FSMGraph {
	t.Helper()
	src := fmt.Sprintf(`workflow {
  name          = "mcp_direct_typed"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

type "echo_request" {
  schema = object({
    tool    = string
    message = optional(string)
  })
}

type "echo_response" {
  schema = object({
    text = optional(string)
  })
}

permissions {
  allow_tools = ["echo", "structured"]
}

adapter "mcp" "tools" {
  config {
    command = %q
  }
  dynamic_tools = true

  tool "echo" {
    in  = type.echo_request
    out = type.echo_response
  }
}

step "call" {
  target = adapter.mcp.tools

  input {
    tool    = "echo"
    message = "direct typed echo"
  }

  outcome "success" {
    next = state.done
  }

  outcome "failure" {
    next = state.failed
  }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success = false
}
`, echoBin)
	spec, diags := workflow.Parse("mcp_direct_typed.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse mcp direct typed workflow: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"mcp": {
			ConfigSchema: map[string]workflow.ConfigField{
				"command": {Required: true, Type: workflow.ConfigFieldString},
			},
			Capabilities: []string{"adapter_tools"},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile mcp direct typed workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("mcp direct typed workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// runMCPToolsCase drives one case through the real engine: the caller fake
// issues its scripted call, the nested callee is the real mcp adapter binary
// talking to the echo fixture server.
func runMCPToolsCase(t *testing.T, echoBin, targetState string, caller adapterhost.Handle) *mcpToolsSink {
	t.Helper()
	return runMCPToolsGraphCase(t, compileMCPToolsGraph(t, echoBin, targetState), caller, nil)
}

// runMCPToolsGraphCase runs a pre-compiled graph through the real engine;
// audit, when non-nil, captures the host's decision log (the typed issue
// lists KB-59 asserts live there). A nil caller runs no caller fake — the
// direct-target cases whose step executes the mcp adapter itself.
func runMCPToolsGraphCase(t *testing.T, graph *workflow.FSMGraph, caller adapterhost.Handle, audit adapterhost.AuditWriter) *mcpToolsSink {
	t.Helper()
	sink := &mcpToolsSink{}
	realLoader := adapterhost.NewLoaderWithDiscovery(func(name string) (string, error) {
		if name == "mcp" {
			return testAdapterBin, nil
		}
		return "", fmt.Errorf("no adapter binary for %q", name)
	})
	loader := &mcpToolsLoader{real: realLoader}
	if caller != nil {
		loader.fakes = map[string]adapterhost.Handle{"caller": caller}
	}
	defer func() {
		if err := realLoader.Shutdown(context.Background()); err != nil {
			t.Logf("loader shutdown: %v", err)
		}
	}()
	opts := []engine.Option{}
	if audit != nil {
		opts = append(opts, engine.WithAuditWriter(audit))
	}
	if err := engine.New(graph, loader, sink, opts...).Run(context.Background()); err != nil {
		t.Fatalf("engine run: %v", err)
	}
	return sink
}

// assertRunContinued asserts the run reached the terminal state the caller's
// own outcome routing selected, only the caller step ran, and no
// permission.denied event was emitted anywhere in the run.
func assertRunContinued(t *testing.T, sink *mcpToolsSink, wantTerminal string) {
	t.Helper()
	if got, want := sink.terminalState(), wantTerminal; got != want {
		t.Fatalf("terminal state = %q, want %q (failure=%q)", got, want, sink.runFailure())
	}
	if !sink.runOK() {
		t.Fatalf("run did not complete successfully: failure=%q", sink.runFailure())
	}
	if got, want := sink.stepsRun(), []string{mcpToolsCallerStep}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("steps run = %v, want %v", got, want)
	}
	if sink.permissionDenied() {
		t.Fatal("permission.denied event emitted, want none: an adapter-tool result is data for the caller, not a policy deny")
	}
}

// assertTypedReply asserts exactly one typed reply arrived for the scripted
// request with no cancel, and returns it.
func assertTypedReply(t *testing.T, caller *mcpToolsCaller) toolsReply {
	t.Helper()
	results := caller.gotResults()
	if len(results) != 1 {
		t.Fatalf("results received = %d, want 1: %+v", len(results), results)
	}
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("cancels received = %d, want 0: %+v", len(cancels), cancels)
	}
	return results[0]
}

// TestMCPAdapterTools_CallKnownTool covers the happy path: the caller calls
// adapter.mcp.tools.tools.echo; the bridge maps the call to MCP tools/call
// "echo", the text content lands in the tool result's outputs_json, and the
// run continues through the caller's own outcome routing.
func TestMCPAdapterTools_CallKnownTool(t *testing.T) {
	caller := newMCPToolsCaller("handled", toolsCall{
		requestID: "call-1",
		target:    "adapter.mcp.tools.tools.echo",
		args:      map[string]any{"tool": "echo", "message": "hello from tools"},
	})
	sink := runMCPToolsCase(t, testEchoBin, "done", caller)

	reply := assertTypedReply(t, caller)
	if reply.requestID != "call-1" || reply.outcome != "success" || reply.callError != "" {
		t.Fatalf("typed reply = %+v, want request \"call-1\" outcome \"success\" no call_error", reply)
	}
	text, _ := reply.outputs["text"].(string)
	if !strings.Contains(text, `"message":"hello from tools"`) {
		t.Fatalf("outputs text = %q, want the MCP tools/call text content echoing the arguments", text)
	}
	assertRunContinued(t, sink, "done")
	if got, want := sink.stepOutcomes(), []string{"call=handled"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("step outcomes = %v, want %v", got, want)
	}
	found := false
	for _, ev := range sink.stepEvents() {
		// The session-scoped permission interceptor consumes the caller's
		// raw permission.request and re-emits the policy decision, so the
		// recorded event is the grant — proving the §8 adapter-tool call
		// itself was the permission event.
		if ev.kind == "permission.granted" {
			found = true
		}
	}
	if !found {
		t.Fatal("no permission.granted event recorded on the caller step, want the adapter-tool permission decision")
	}
}

// TestMCPAdapterTools_CallUnknownTool covers the unknown-tool call: the tool
// is not in the set discovered via tools/list, so the adapter reports the
// typed failure result carrying the reserved call_error output; the host
// delivers call_error "unknown_tool" to the caller — not a policy deny — and
// the run continues.
func TestMCPAdapterTools_CallUnknownTool(t *testing.T) {
	caller := newMCPToolsCaller("unhandled", toolsCall{
		requestID: "call-1",
		target:    "adapter.mcp.tools.tools.no_such",
		args:      map[string]any{"tool": "no_such"},
	})
	sink := runMCPToolsCase(t, testEchoBin, "unhandled", caller)

	reply := assertTypedReply(t, caller)
	if reply.requestID != "call-1" || reply.callError != "unknown_tool" || reply.outcome != "" {
		t.Fatalf("typed reply = %+v, want request \"call-1\" call_error \"unknown_tool\" outcome empty", reply)
	}
	assertRunContinued(t, sink, "unhandled")
	if got, want := sink.stepOutcomes(), []string{"call=unhandled"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("step outcomes = %v, want %v", got, want)
	}
}

// TestMCPAdapterTools_StructuredContent covers structured content passthrough:
// an MCP tool result carrying structuredContent maps it into outputs_json
// under "structured" alongside the primary text payload.
func TestMCPAdapterTools_StructuredContent(t *testing.T) {
	caller := newMCPToolsCaller("handled", toolsCall{
		requestID: "call-1",
		target:    "adapter.mcp.tools.tools.structured",
		args:      map[string]any{"tool": "structured"},
	})
	sink := runMCPToolsCase(t, testEchoBin, "done", caller)

	reply := assertTypedReply(t, caller)
	if reply.requestID != "call-1" || reply.outcome != "success" || reply.callError != "" {
		t.Fatalf("typed reply = %+v, want request \"call-1\" outcome \"success\" no call_error", reply)
	}
	structured, ok := reply.outputs["structured"].(map[string]any)
	if !ok {
		t.Fatalf("outputs structured = %#v, want the MCP structuredContent passthrough", reply.outputs["structured"])
	}
	if count, _ := structured["count"].(float64); count != 2 {
		t.Fatalf("structured count = %v, want 2", structured["count"])
	}
	if text, _ := reply.outputs["text"].(string); text != "structured payload" {
		t.Fatalf("outputs text = %q, want the MCP text content", text)
	}
	assertRunContinued(t, sink, "done")
}

// TestMCPAdapterTools_InfoSurfacesDiscoveredTools covers the CRI-171 Info
// contract on the real binary: after an OpenSession against the fixture
// server, the adapter's Info surface lists the tools discovered from
// tools/list and declares the adapter_tools capability.
func TestMCPAdapterTools_InfoSurfacesDiscoveredTools(t *testing.T) {
	ctx := context.Background()
	loader := adapterhost.NewLoaderWithDiscovery(func(name string) (string, error) {
		if name == "mcp" {
			return testAdapterBin, nil
		}
		return "", fmt.Errorf("no adapter binary for %q", name)
	})
	defer func() {
		if err := loader.Shutdown(ctx); err != nil {
			t.Logf("loader shutdown: %v", err)
		}
	}()

	handle, err := loader.Resolve(ctx, "mcp")
	if err != nil {
		t.Fatalf("resolve mcp adapter: %v", err)
	}
	const sessionID = "mcp-tools-info-test"
	if err := handle.OpenSession(ctx, sessionID, map[string]string{"command": testEchoBin}, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		if err := handle.CloseSession(ctx, sessionID); err != nil {
			t.Logf("close session: %v", err)
		}
	}()

	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	hasCapability := false
	for _, c := range info.Capabilities {
		if c == "adapter_tools" {
			hasCapability = true
		}
	}
	if !hasCapability {
		t.Fatalf("capabilities %v must include adapter_tools", info.Capabilities)
	}
	names := make(map[string]string, len(info.Tools))
	for _, tool := range info.Tools {
		names[tool.Name] = tool.Description
	}
	for _, want := range []string{"echo", "structured"} {
		desc, ok := names[want]
		if !ok {
			t.Fatalf("info tools %v must contain the discovered tool %q", names, want)
		}
		if desc == "" {
			t.Fatalf("info tool %q description is empty, want the tools/list description", want)
		}
	}
}

// mcpToolsAuditCollector captures the host side's decision log entries.
type mcpToolsAuditCollector struct {
	mu      sync.Mutex
	entries []*adapterhost.DecisionLogEntry
}

func (w *mcpToolsAuditCollector) Write(e *adapterhost.DecisionLogEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, e)
}

func (w *mcpToolsAuditCollector) all() []*adapterhost.DecisionLogEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*adapterhost.DecisionLogEntry(nil), w.entries...)
}

// TestMCPAdapterTools_TypedContractRejectsBadArgs is the KB-59 Gap 2 repro at
// the real seam: the echo tool's args violate the declared contract (message
// as a JSON number, plus an undeclared key) on a schema-less dynamic
// adapter — pre-KB-59, anything passed; now the host rejects the call typed
// `invalid_args` before the callee runs, and the audit carries the typed
// issue list.
func TestMCPAdapterTools_TypedContractRejectsBadArgs(t *testing.T) {
	caller := newMCPToolsCaller("unhandled", toolsCall{
		requestID: "call-1",
		target:    "adapter.mcp.tools.tools.echo",
		args:      map[string]any{"tool": 6, "ghost": "boo"},
	})
	audit := &mcpToolsAuditCollector{}
	sink := runMCPToolsGraphCase(t, compileMCPToolsTypedGraph(t, testEchoBin, "unhandled"), caller, audit)

	reply := assertTypedReply(t, caller)
	if reply.requestID != "call-1" || reply.callError != "invalid_args" || reply.outcome != "" {
		t.Fatalf("typed reply = %+v, want request \"call-1\" call_error \"invalid_args\"", reply)
	}
	assertRunContinued(t, sink, "unhandled")
	for _, needle := range []string{
		`payload_schema: property "tool": expected "string", got "number"`,
		`payload_schema: property "ghost": undeclared property`,
	} {
		found := false
		for _, e := range audit.all() {
			if e != nil && strings.Contains(e.Reason, needle) {
				found = true
			}
		}
		if !found {
			t.Errorf("audit missing reason %q; entries = %+v", needle, audit.all())
		}
	}
}

// TestMCPAdapterTools_TypedContractValidatesGoodArgs covers the same typed
// seam on a contract-respecting call: the arguments pass the declared
// contract, the callee executes, and the response decodes against
// type.echo_response.
func TestMCPAdapterTools_TypedContractValidatesGoodArgs(t *testing.T) {
	caller := newMCPToolsCaller("handled", toolsCall{
		requestID: "call-1",
		target:    "adapter.mcp.tools.tools.echo",
		args:      map[string]any{"tool": "echo", "message": "typed and validated"},
	})
	sink := runMCPToolsGraphCase(t, compileMCPToolsTypedGraph(t, testEchoBin, "done"), caller, nil)

	reply := assertTypedReply(t, caller)
	if reply.requestID != "call-1" || reply.outcome != "success" || reply.callError != "" {
		t.Fatalf("typed reply = %+v, want request \"call-1\" outcome \"success\"", reply)
	}
	text, _ := reply.outputs["text"].(string)
	if !strings.Contains(text, `"message":"typed and validated"`) {
		t.Fatalf("outputs text = %q, want the echo response the contract validates", text)
	}
	assertRunContinued(t, sink, "done")
}

// TestMCPAdapterTools_DirectTypedTarget runs the direct-target half of
// KB-59's repro: a step targeting adapter.mcp.tools (no caller hop) whose
// typed input block is validated against the declared contract at compile
// time, then executed for real by the engine — the unknown-field check that
// would have rejected the dynamic adapter's input keys is bypassed in favor
// of the contract.
func TestMCPAdapterTools_DirectTypedTarget(t *testing.T) {
	sink := runMCPToolsGraphCase(t, compileMCPDirectTypedGraph(t, testEchoBin), nil, nil)

	if got, want := sink.terminalState(), "done"; got != want {
		t.Fatalf("terminal state = %q, want %q (failure=%q)", got, want, sink.runFailure())
	}
	if !sink.runOK() {
		t.Fatalf("run did not complete successfully: failure=%q", sink.runFailure())
	}
	if got, want := sink.stepOutcomes(), []string{"call=success"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("step outcomes = %v, want %v", got, want)
	}
	// The adapter's Execute input became the step's outputs: the echo
	// response text, satisfying type.echo_response.
	outs := sink.stepOutputs("call")
	if text := outs["text"]; !strings.Contains(text, `"message":"direct typed echo"`) {
		t.Fatalf("step outputs = %v, want the echo response text", outs)
	}
}

// mcpConcurrentToolsCaller fans all scripted calls out at once: every call is
// emitted on its own goroutine so the nested calls overlap on the shared
// callee session (KB-155), while ONE reader consumes the shared permission
// stream and attributes each typed reply to its owner by request_id — the
// caller-side half of the multiplexing contract.
type mcpConcurrentToolsCaller struct {
	mcpToolsCaller
	wg sync.WaitGroup

	expectedMu sync.Mutex
	expected   map[string]bool
}

func newMCPConcurrentToolsCaller(outcome string, calls ...toolsCall) *mcpConcurrentToolsCaller {
	c := &mcpConcurrentToolsCaller{
		mcpToolsCaller: mcpToolsCaller{
			capabilities: []string{"adapter_tools", "execute"},
			outcome:      outcome,
			script:       calls,
		},
		expected: map[string]bool{},
	}
	for _, call := range calls {
		c.expected[call.requestID] = true
	}
	return c
}

// outstanding reports how many scripted calls still wait for their reply.
func (a *mcpConcurrentToolsCaller) outstanding() int {
	a.expectedMu.Lock()
	defer a.expectedMu.Unlock()
	n := 0
	for _, pending := range a.expected {
		if pending {
			n++
		}
	}
	return n
}

func (a *mcpConcurrentToolsCaller) markSettled(requestID string) {
	a.expectedMu.Lock()
	defer a.expectedMu.Unlock()
	a.expected[requestID] = false
}

// recordMissing types the well-known harness error for every call that never
// settled (stream closed or deadline), mirroring awaitReply's failure paths.
func (a *mcpConcurrentToolsCaller) recordMissing(reason string) {
	a.expectedMu.Lock()
	defer a.expectedMu.Unlock()
	for id, pending := range a.expected {
		if !pending {
			continue
		}
		a.expected[id] = false
		a.mcpToolsCaller.recordResult(toolsReply{requestID: id, callError: reason})
	}
}

func (a *mcpConcurrentToolsCaller) Execute(_ context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	for _, call := range a.script {
		a.wg.Add(1)
		go func(call toolsCall) {
			defer a.wg.Done()
			sink.Adapter("permission.request", map[string]any{
				"request_id": call.requestID,
				"target":     call.target,
				"args":       call.args,
			})
		}(call)
	}

	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests != nil {
		deadline := time.After(mcpToolsAwaitReplyTimeout)
		for a.outstanding() > 0 {
			var drained bool
			select {
			case ev, ok := <-requests:
				if !ok {
					a.recordMissing("test-harness-error: permission stream closed")
					drained = true
				} else if reply, settled := toolsTypedReply(ev); settled {
					a.recordResult(reply)
					a.markSettled(reply.requestID)
				} else if cancel := ev.GetCancel(); cancel != nil {
					a.mu.Lock()
					a.cancels = append(a.cancels, toolsReply{requestID: cancel.GetRequestId()})
					a.mu.Unlock()
					a.markSettled(cancel.GetRequestId())
				}
				// Other stream traffic is not part of the call path; skip it.
			case <-deadline:
				a.recordMissing("test-harness-error: timed out waiting for reply")
				drained = true
			}
			if drained {
				break
			}
		}
	}
	a.wg.Wait()

	a.mu.Lock()
	outcome := a.outcome
	a.mu.Unlock()
	return adapter.Result{Outcome: outcome}, nil
}

// numeric reads a payload number regardless of how the seam decoded it.
func numeric(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// TestMCPAdapterTools_ConcurrentCallsShareSession covers the KB-155 goal end
// to end: two calls issued concurrently from the caller fan onto ONE shared
// mcp callee session; each typed reply is correlated to its own
// request_id carrying only its own message; the fixture's peak-concurrency
// marker proves the calls ran concurrently on that shared session; and the
// run continues to its terminal state with no deny anywhere.
func TestMCPAdapterTools_ConcurrentCallsShareSession(t *testing.T) {
	caller := newMCPConcurrentToolsCaller("handled",
		toolsCall{
			requestID: "call-a",
			target:    "adapter.mcp.tools.tools.echo",
			args:      map[string]any{"tool": "echo", "message": "msg-a", "sleep_ms": "400"},
		},
		toolsCall{
			requestID: "call-b",
			target:    "adapter.mcp.tools.tools.echo",
			args:      map[string]any{"tool": "echo", "message": "msg-b", "sleep_ms": "400"},
		},
	)
	sink := runMCPToolsCase(t, testEchoBin, "done", caller)

	results := caller.gotResults()
	if len(results) != 2 {
		t.Fatalf("typed replies = %d (%+v), want one per scripted call", len(results), results)
	}
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("cancels received = %d, want 0: %+v", len(cancels), cancels)
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
	}
	if text := textOf(byID["call-a"].outputs); strings.Contains(text, "msg-b") {
		t.Fatalf("reply for call-a text %q carries the sibling's message: cross-call leakage", text)
	}
	if text := textOf(byID["call-b"].outputs); strings.Contains(text, "msg-a") {
		t.Fatalf("reply for call-b text %q carries the sibling's message: cross-call leakage", text)
	}

	// Shared-session overlap: the callee session's peak in-flight count
	// reaches 2 only if both calls multiplexed over one bridge session.
	progress := 0
	maxOverlap := 0.0
	tokens := map[string]bool{}
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

// textOf extracts the primary text output of a typed reply, for leakage checks.
func textOf(outputs map[string]any) string {
	text, _ := outputs["text"].(string)
	return text
}

// kindsOf lists the event kinds of a recorded stream for failure messages.
func kindsOf(events []mcpToolsEvent) string {
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.kind)
	}
	return strings.Join(kinds, ",")
}

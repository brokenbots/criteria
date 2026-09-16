package conformance

// conformance_adapter_tools_agent.go — the CRI-183 adapter-tools AGENT-CALLER
// matrix, the agent-shaped twin of the CRI-167 failure matrix in
// conformance_adapter_tools.go.
//
// ADR-0004 (docs/adrs/ADR-0004-adapter-tools.md):
//   - §4 Caller-side grammar: adapter tool calls originate from the CALLER's
//     adapter, and the caller is by definition an agent runtime — an
//     interactive agent adapter whose Execute is one conversation turn, not a
//     single input-keyed action. The caller-side grammar is a wire grammar:
//     it says nothing about WHERE inside the caller the call originates.
//   - §5 Return semantics: a denied or failed tool call is DATA to the
//     caller. The agent turn continues after a deny or typed failure — it can
//     issue further calls and submit any outcome afterward — and the caller's
//     step does not fail unless the agent itself submits a failing outcome.
//   - §8 Wire decision: every tool call rides the session's permissions
//     stream as a permission.request AdapterEvent carrying the full
//     {kind, request_id, target, tool, args, args_digest} payload, and every
//     reply is correlated on the same stream by request_id — regardless of
//     who inside the adapter initiated the call.
//
// The CRI-167 matrix holds only for the noop test double's INPUT-KEYED path
// (testdata/noop/toolcall.go): its bridge issues tool calls because a caller
// step's input keys (tool_target, tool_name, tool_args) select the mode.
// These cases are the same matrix from the other origin: the caller is an
// agent-shaped adapter whose simulated turn HANDLER issues a
// tool-handler-shaped, non-input-keyed call mid-Execute, riding the exact
// same §8 wire payload the input-keyed bridge sends. The point is the wire's
// agnosticism: the host cannot tell who inside the adapter initiated the
// call and must not need to. Only the agent-shaped variants of the matrix
// entries are covered here — the input-keyed cases already exist in the
// CRI-167 matrix and in the noop fixture path, and are not duplicated.
//
// Matrix (agent-caller additions; deny/unknown_tool/concurrency per CRI-167
// and the CRI-161 engine interleave test):
//
//	Deny mid-conversation: the agent's tool call is denied by the caller's
//	  own policy; the deny is data to the turn, which continues with a
//	  follow-up call that succeeds and then submits its own outcome. The
//	  caller's step does not fail, and the host's observable behavior is
//	  indistinguishable from the input-keyed deny case.
//	Unknown tool mid-conversation: a typed unknown_tool failure (the
//	  callee's static surface does not declare the target's tool) is data to
//	  the turn, which continues with a follow-up call that succeeds.
//	Concurrent calls from one agent turn: two calls issued by the same
//	  turn's handlers (one goroutine each) are in flight together and the
//	  fast call's reply overtakes the slow call's — reply order is NOT
//	  assumed on the Permissions stream; correlation is by request_id.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// agentToolCallTargetOther is the undeclared tool target the agent-caller
// cases use for a deny (the caller's policy excludes it) and for the
// unknown_tool typed failure (the callee's static surface does not declare
// it) — the same undeclared-tool shape the CRI-167 matrix cases use.
const agentToolCallTargetOther = "adapter.callee.default.tools.other_task"

// agentToolInvocation is one tool call the simulated agent's turn handler
// issues mid-Execute. The call is NOT keyed on the caller step's input —
// that shape is the input-keyed noop fixture path's, and these cases exist
// to prove the wire is agnostic to the call's origin.
type agentToolInvocation struct {
	requestID string
	target    string
	tool      string
	args      map[string]any
}

// agentTurnBatch is one scripted beat of the simulated agent turn: the tool
// calls the turn's handlers issue (one handler per call, one goroutine per
// handler, so several calls from one beat are in flight together), or the
// outcome the agent submits to end the turn. Exactly one of the fields is
// set.
type agentTurnBatch struct {
	calls  []agentToolInvocation
	submit string
}

// agentCall scripts one tool call from the turn's tool handler to the named
// tool on the given target.
func agentCall(requestID, target, tool string, args map[string]any) agentTurnBatch {
	return agentTurnBatch{calls: []agentToolInvocation{{
		requestID: requestID,
		target:    target,
		tool:      tool,
		args:      args,
	}}}
}

// agentConcurrentTurn scripts one beat of the agent turn whose tool handlers
// issue several calls at once.
func agentConcurrentTurn(invocations ...agentToolInvocation) agentTurnBatch {
	return agentTurnBatch{calls: invocations}
}

// agentSubmit scripts the outcome the agent submits to end the turn.
func agentSubmit(outcome string) agentTurnBatch {
	return agentTurnBatch{submit: outcome}
}

// agentCallerAdapter is the scripted caller-side fake: an adapter whose
// Execute is one simulated agent conversation turn. The turn's script is a
// sequence of batches — tool calls issued mid-Execute by the turn's tool
// handlers through the bridge, and the outcome the agent submits to end the
// turn. The fake records the step input it was handed (to prove no
// input-keyed tool-call selectors) and every reply the handlers observed.
type agentCallerAdapter struct {
	turn []agentTurnBatch

	mu         sync.Mutex
	stepInputs []map[string]string
	replies    []agentToolReply

	bridge agentToolBridge
}

func newAgentCaller(turn ...agentTurnBatch) *agentCallerAdapter {
	return &agentCallerAdapter{turn: turn}
}

func (a *agentCallerAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "agent-caller",
		Version:      "0.0.0-matrix",
		Capabilities: []string{"adapter_tools", "execute"},
	}, nil
}

func (a *agentCallerAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (a *agentCallerAdapter) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	go a.bridge.serve(requests)
	return func() {}, nil
}

func (a *agentCallerAdapter) Execute(_ context.Context, _ string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	a.recordStepInput(step.Input)
	return adapter.Result{Outcome: a.runTurn(sink)}, nil
}

// runTurn drives the scripted conversation: batches run in order; a call
// batch runs the turn's tool handlers — one goroutine per handler, so
// several calls from one turn are in flight together — and a submit batch
// ends the turn with the agent's own outcome. Every handler reply is data
// to the turn; a denied or failed call never fails the turn.
func (a *agentCallerAdapter) runTurn(sink adapter.EventSink) string {
	outcome := ""
	for _, batch := range a.turn {
		if batch.submit != "" {
			outcome = batch.submit
			continue
		}
		var wg sync.WaitGroup
		for _, call := range batch.calls {
			wg.Add(1)
			go func(inv agentToolInvocation) {
				defer wg.Done()
				a.toolHandler(sink, inv)
			}(call)
		}
		wg.Wait()
	}
	return outcome
}

// toolHandler is one agent tool handler invocation: the call rides the §8
// wire mid-Execute and its correlated reply — the callee's result, a policy
// deny, or a typed failure — is data to the turn (ADR-0004 §5).
func (a *agentCallerAdapter) toolHandler(sink adapter.EventSink, call agentToolInvocation) {
	reply := a.bridge.issue(sink, call)
	a.recordReply(&reply)
}

func (a *agentCallerAdapter) recordStepInput(input map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make(map[string]string, len(input))
	for k, v := range input {
		copied[k] = v
	}
	a.stepInputs = append(a.stepInputs, copied)
}

// recordReply records one tool handler's observed reply.
func (a *agentCallerAdapter) recordReply(reply *agentToolReply) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replies = append(a.replies, *reply)
}

func (a *agentCallerAdapter) gotReplies() []agentToolReply {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agentToolReply(nil), a.replies...)
}

func (a *agentCallerAdapter) gotStepInputs() []map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]string(nil), a.stepInputs...)
}

func (a *agentCallerAdapter) CloseSession(context.Context, string) error { return nil }
func (a *agentCallerAdapter) Kill()                                      {}
func (a *agentCallerAdapter) Pause(context.Context, string) error        { return nil }
func (a *agentCallerAdapter) Resume(context.Context, string) error       { return nil }
func (a *agentCallerAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *agentCallerAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *agentCallerAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// agentToolReply is the agent turn's view of one tool call's reply: the
// callee's typed result (outcome/call_error/outputs), or a denial (denied
// set, reason carrying the policy reason, outcome/call_error empty). A
// harness error (stream closed before the reply, timeout) is surfaced as a
// call_error so it fails the case's reply assertions instead of wedging the
// turn.
type agentToolReply struct {
	requestID string
	outcome   string
	callError string
	outputs   map[string]any
	denied    bool
	reason    string
}

// agentPendingCall is one in-flight call awaiting its correlated reply.
type agentPendingCall struct {
	reply agentToolReply
	done  chan struct{}
}

// agentToolBridge is the agent caller's adapter-tools dispatch loop: the
// in-memory twin of the noop conformance fixture's toolcall.go bridge. The
// call payload is the full §8 shape the input-keyed noop path sends, so the
// host sees a wire-identical permission.request regardless of who inside
// the adapter initiated the call; replies resolve off the same permissions
// stream, correlated by request_id.
type agentToolBridge struct {
	mu      sync.Mutex
	pending map[string]*agentPendingCall
}

// issue issues one adapter tool call and blocks for the correlated reply.
// The pending entry is captured before the request is sent (the noop
// fixture's bridge does the same): the host may settle the reply before
// issue resumes, so the entry must be held, not re-looked-up.
func (b *agentToolBridge) issue(sink adapter.EventSink, call agentToolInvocation) agentToolReply {
	payload, err := agentToolCallPayload(call)
	if err != nil {
		return agentToolReply{requestID: call.requestID, callError: "agent-harness-error: " + err.Error()}
	}
	entry := b.register(call.requestID)
	sink.Adapter("permission.request", payload)
	select {
	case <-entry.done:
		return entry.reply
	case <-time.After(matrixAwaitReplyTimeout):
		return agentToolReply{requestID: call.requestID, callError: "agent-harness-error: timed out waiting for reply to " + call.requestID}
	}
}

// agentToolCallPayload renders the §8 call payload — the exact shape the
// noop fixture's input-keyed bridge (testdata/noop/toolcall.go) sends — so
// the host sees a wire-identical permission.request regardless of the call's
// origin inside the adapter.
func agentToolCallPayload(call agentToolInvocation) (map[string]any, error) {
	digest, err := v2.ArgsDigest(call.args)
	if err != nil {
		return nil, fmt.Errorf("tool call args digest: %w", err)
	}
	argsStruct, err := structpb.NewStruct(call.args)
	if err != nil {
		return nil, fmt.Errorf("tool call args: %w", err)
	}
	return map[string]any{
		"kind":        "adapter_tool",
		"request_id":  call.requestID,
		"target":      call.target,
		"tool":        call.tool,
		"args":        argsStruct.AsMap(),
		"args_digest": digest,
	}, nil
}

// register tracks the call before the request is sent, so a fast reply
// cannot race the pending registration, and returns the entry for issue to
// wait on.
func (b *agentToolBridge) register(requestID string) *agentPendingCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		b.pending = make(map[string]*agentPendingCall)
	}
	entry := &agentPendingCall{done: make(chan struct{})}
	b.pending[requestID] = entry
	return entry
}

// resolve settles the pending call the reply correlates to; other stream
// traffic (plain permission requests, late replies) is not a call reply.
func (b *agentToolBridge) resolve(requestID string, reply *agentToolReply) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.pending[requestID]
	if !ok {
		return
	}
	delete(b.pending, requestID)
	reply.requestID = requestID
	entry.reply = *reply
	close(entry.done)
}

// failAllPending fails every still-pending call when the permissions stream
// closes, so a wedged caller is visible in the reply assertions instead of
// hanging the turn.
func (b *agentToolBridge) failAllPending() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for requestID, entry := range b.pending {
		entry.reply = agentToolReply{
			requestID: requestID,
			callError: "agent-harness-error: permissions stream closed while tool call in flight",
		}
		close(entry.done)
		delete(b.pending, requestID)
	}
}

// serve is the bridge's reply loop over the session's permissions stream:
// typed tool_call_result replies and permission cancels settle the pending
// calls they correlate by request_id; allow-grant request events and other
// permission traffic are not the agent's reply path. The loop ends when the
// host closes the stream at session close.
func (b *agentToolBridge) serve(requests <-chan *v2.PermissionEvent) {
	for ev := range requests {
		if tcr := ev.GetToolCallResult(); tcr != nil {
			typed := b.typedReply(tcr)
			b.resolve(tcr.GetRequestId(), &typed)
			continue
		}
		if cancel := ev.GetCancel(); cancel != nil {
			denied := agentToolReply{
				denied: true,
				reason: cancel.GetReason(),
			}
			b.resolve(cancel.GetRequestId(), &denied)
		}
	}
	b.failAllPending()
}

// typedReply converts a typed tool_call_result stream event into the reply
// the agent turn records; the outputs decode mirrors the matrix caller's
// (a decode failure is a harness error, not a callee failure).
func (b *agentToolBridge) typedReply(tcr *v2.ToolCallResult) agentToolReply {
	reply := agentToolReply{outcome: tcr.GetOutcome(), callError: tcr.GetCallError()}
	if len(tcr.GetOutputsJson()) > 0 {
		var outputs map[string]any
		err := json.Unmarshal(tcr.GetOutputsJson(), &outputs)
		reply.outputs = outputs
		if err != nil {
			reply.callError = "agent-harness-error: decoding outputs: " + err.Error()
			reply.outcome = ""
		}
	}
	return reply
}

// assertAgentReplies asserts the agent turn observed exactly the given
// replies, in arrival order — the same data the input-keyed path receives:
// typed results for completed calls, the policy reason for a denied one.
func assertAgentReplies(t *testing.T, caller *agentCallerAdapter, want ...agentToolReply) {
	t.Helper()
	replies := caller.gotReplies()
	if len(replies) != len(want) {
		t.Fatalf("agent turn replies = %+v, want %d reply(ies)", replies, len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(replies[i], want[i]) {
			t.Fatalf("agent turn reply %d = %+v, want %+v", i, replies[i], want[i])
		}
	}
}

// assertNoToolCallInputKeys asserts the agent caller's step input carries
// none of the input-keyed tool-call selectors: the exercised calls originate
// inside the turn's tool handler, not from the step's input (the input-keyed
// noop fixture path's shape, which these cases must not duplicate).
func assertNoToolCallInputKeys(t *testing.T, caller *agentCallerAdapter) {
	t.Helper()
	for i, input := range caller.gotStepInputs() {
		for _, key := range []string{"tool_target", "tool_name", "tool_args"} {
			if _, has := input[key]; has {
				t.Fatalf("agent caller step input %d carries %q (input-keyed calls are the noop fixture's path, not the agent's)", i, key)
			}
		}
	}
}

// RunAdapterToolsAgentCallerMatrix runs the CRI-183 adapter-tools agent-caller
// matrix: three sub-tests, one per matrix entry, each asserting the
// agent-turn continuation invariant and the run-continuation invariant
// through the real engine.
func RunAdapterToolsAgentCallerMatrix(t *testing.T) {
	t.Run("deny_mid_conversation", agentCaseDenyMidConversation)
	t.Run("unknown_tool_mid_conversation", agentCaseUnknownToolMidConversation)
	t.Run("concurrent_calls", agentCaseConcurrentCalls)
}

// agentCaseDenyMidConversation covers the denied call from the agent side
// (ADR-0004 §5): the turn's tool call is denied by the caller's own policy;
// the deny is data to the turn, which continues — issues a follow-up call
// that succeeds — and then submits its own "denied" outcome. The caller's
// step does not fail: the run ends on the terminal the agent's own outcome
// routing selected. (The agent does not submit "success" after the deny: the
// host overrides a post-denial success to needs_review, which would route
// the run outside the agent's own choice — see matrixCaseDeny.) The wire is
// identical to the input-keyed deny case: the host cannot tell who inside
// the adapter initiated the denied call.
func agentCaseDenyMidConversation(t *testing.T) {
	caller := newAgentCaller(
		agentCall("call-1", agentToolCallTargetOther, "other_task", map[string]any{"task": "do-thing"}),
		agentCall("call-2", matrixToolCallTarget, "helper_task", map[string]any{"task": "recover"}),
		agentSubmit("denied"),
	)
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_agent_deny", "denied", "\n"+
		"  allow_tools = [\"adapter.callee.default.tools.helper_task\"]\n"+
		"\n"+
		"  outcome \"denied\" {\n"+
		"    next = step.denied\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	// The turn's view of the deny: the denied call's reply carries the
	// policy reason (a cancel-shaped reply, not a typed result), and the
	// turn continued — the follow-up call settled cleanly afterwards.
	assertAgentReplies(t, caller,
		agentToolReply{requestID: "call-1", denied: true, reason: "no matching allow_tools entry"},
		agentToolReply{requestID: "call-2", outcome: "success", outputs: map[string]any{"report": "recover", "count": float64(7)}},
	)
	assertNoToolCallInputKeys(t, caller)

	assertAgentDenyDispatch(t, sink)
	assertAgentDenyPolicy(t, audit, callee)

	// The run-continuation invariant: the deny did not fail the caller's
	// step — the agent's own "denied" outcome routed the run to the
	// terminal.
	assertMatrixRunContinued(t, sink, "denied")
}

// assertAgentDenyDispatch asserts the host's view of the denied call is
// indistinguishable from the input-keyed deny case (CRI-167): the inner
// permission.denied carries the request id and the policy reason, only the
// follow-up call is granted and dispatched, and the denied call never
// reaches the callee.
func assertAgentDenyDispatch(t *testing.T, sink *matrixEngineSink) {
	assertCallerStepEvent(t, sink, "permission.denied", map[string]string{
		"request_id": "call-1",
		"reason":     "no matching allow_tools entry",
	})
	if grants := sink.stepEventCount("permission.granted"); grants != 1 {
		t.Fatalf("permission.granted events = %d, want 1 (only the follow-up call is granted)", grants)
	}
	assertCallerStepEvent(t, sink, "permission.granted", map[string]string{
		"request_id": "call-2",
		"pattern":    "adapter.callee.default.tools.helper_task",
	})
	if calls := sink.stepEventCount("tool.call"); calls != 1 {
		t.Fatalf("tool.call events = %d, want 1 (the denied call must never be dispatched)", calls)
	}
	assertCallerStepEvent(t, sink, "tool.call", map[string]string{
		"request_id": "call-2",
		"target":     matrixToolCallTarget,
		"tool":       "helper_task",
	})
	if results := sink.stepEventCount("tool.call_result"); results != 1 {
		t.Fatalf("tool.call_result events = %d, want 1", results)
	}
	assertCallerStepEvent(t, sink, "tool.call_result", map[string]string{
		"request_id": "call-2",
		"target":     matrixToolCallTarget,
		"tool":       "helper_task",
		"outcome":    "success",
	})
}

// assertAgentDenyPolicy asserts the audit trail and callee reach for the
// deny case: the caller's own policy deny plus the follow-up allow, both at
// layer 0 — identical layering to the input-keyed deny case — and exactly
// one callee execution, in the callee's own session.
func assertAgentDenyPolicy(t *testing.T, audit *matrixAuditCollector, callee *matrixCalleeAdapter) {
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "no matching allow_tools entry" {
		t.Fatalf("deny audit entries = %+v, want exactly one policy deny \"no matching allow_tools entry\"", denies)
	}
	if denies[0].Layer != 0 || denies[0].SessionID != "caller.default" || denies[0].RequestID != "call-1" {
		t.Fatalf("deny audit entry = layer %d session %q request %q, want layer 0 session \"caller.default\" request \"call-1\"", denies[0].Layer, denies[0].SessionID, denies[0].RequestID)
	}
	allows := audit.auditDecisions("allow")
	if len(allows) != 1 || allows[0].RequestID != "call-2" {
		t.Fatalf("allow audit entries = %+v, want exactly one allow for the follow-up call", allows)
	}
	assertSessionCloseSummary(t, audit, 2)

	// The denied call never reached the callee; only the follow-up ran, in
	// the callee's own session.
	execs := callee.executions()
	if len(execs) != 1 {
		t.Fatalf("callee executions = %d, want 1 (the denied call must never reach the callee): %+v", len(execs), execs)
	}
	if execs[0].sessionID != "callee.default" || execs[0].task != "recover" {
		t.Fatalf("callee execution = session %q task %q, want session \"callee.default\" task \"recover\"", execs[0].sessionID, execs[0].task)
	}
}

// agentCaseUnknownToolMidConversation covers the typed unknown_tool failure
// from the agent side: the target names a tool the callee's static surface
// does not declare. The caller's own policy grants the call, the graph gate
// fails it typed as unknown_tool, and the typed failure is data to the turn,
// which continues with a follow-up call that succeeds and then submits its
// own success outcome. The caller's step does not fail.
func agentCaseUnknownToolMidConversation(t *testing.T) {
	caller := newAgentCaller(
		agentCall("call-1", agentToolCallTargetOther, "other_task", map[string]any{"task": "do-thing"}),
		agentCall("call-2", matrixToolCallTarget, "helper_task", map[string]any{"task": "recover"}),
		agentSubmit("handled"),
	)
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_agent_unknown_tool", "handled", "\n"+
		"  allow_tools = [\"adapter.callee.default.tools.*\"]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	// The turn's view: the failed call arrives as a typed unknown_tool
	// reply with no outcome, and the turn continued — the follow-up call
	// settled cleanly afterwards.
	assertAgentReplies(t, caller,
		agentToolReply{requestID: "call-1", callError: "unknown_tool"},
		agentToolReply{requestID: "call-2", outcome: "success", outputs: map[string]any{"report": "recover", "count": float64(7)}},
	)
	assertNoToolCallInputKeys(t, caller)

	assertAgentUnknownToolDispatch(t, sink)
	assertAgentUnknownToolPolicy(t, audit, callee)

	// The run-continuation invariant: the typed failure did not fail the
	// caller's step — the agent's own success outcome routed the run.
	assertMatrixRunContinued(t, sink, "handled")
}

// assertAgentUnknownToolDispatch asserts the host's dispatch view for the
// unknown_tool case: the policy granted both calls before the graph gate
// rejected the first (same layering as the input-keyed unknown_tool case),
// no deny event fired, and only the follow-up call was dispatched.
func assertAgentUnknownToolDispatch(t *testing.T, sink *matrixEngineSink) {
	if grants := sink.stepEventCount("permission.granted"); grants != 2 {
		t.Fatalf("permission.granted events = %d, want 2 (the policy allows both calls; the graph gate rejects the first)", grants)
	}
	assertCallerStepEvent(t, sink, "permission.granted", map[string]string{
		"request_id": "call-1",
		"pattern":    "adapter.callee.default.tools.*",
	})
	assertNoStepEvents(t, sink, "permission.denied")
	if calls := sink.stepEventCount("tool.call"); calls != 1 {
		t.Fatalf("tool.call events = %d, want 1 (the typed-gate-rejected call must never be dispatched)", calls)
	}
	assertCallerStepEvent(t, sink, "tool.call", map[string]string{
		"request_id": "call-2",
		"target":     matrixToolCallTarget,
		"tool":       "helper_task",
	})
	if results := sink.stepEventCount("tool.call_result"); results != 1 {
		t.Fatalf("tool.call_result events = %d, want 1", results)
	}
	assertCallerStepEvent(t, sink, "tool.call_result", map[string]string{
		"request_id": "call-2",
		"target":     matrixToolCallTarget,
		"tool":       "helper_task",
		"outcome":    "success",
	})
}

// assertAgentUnknownToolPolicy asserts the audit trail and callee reach for
// the unknown_tool case: the caller's own policy allow for the failed call,
// the typed graph-gate reject, and the follow-up call's allow, plus exactly
// one callee execution in the callee's own session.
func assertAgentUnknownToolPolicy(t *testing.T, audit *matrixAuditCollector, callee *matrixCalleeAdapter) {
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "adapter tool call rejected: unknown_tool" {
		t.Fatalf("deny audit entries = %+v, want exactly one \"adapter tool call rejected: unknown_tool\"", denies)
	}
	allows := audit.auditDecisions("allow")
	if len(allows) != 2 {
		t.Fatalf("allow audit entries = %d, want 2 (one per policy-allowed call)", len(allows))
	}
	assertSessionCloseSummary(t, audit, 2)

	// The rejected call never reached the callee; only the follow-up ran,
	// in the callee's own session.
	execs := callee.executions()
	if len(execs) != 1 {
		t.Fatalf("callee executions = %d, want 1 (a typed-gate-rejected call must never reach the callee): %+v", len(execs), execs)
	}
	if execs[0].sessionID != "callee.default" || execs[0].task != "recover" {
		t.Fatalf("callee execution = session %q task %q, want session \"callee.default\" task \"recover\"", execs[0].sessionID, execs[0].task)
	}
}

// agentCaseConcurrentCalls covers two tool calls issued by the same agent
// turn's handlers (one goroutine each): both are in flight together and the
// fast call's reply overtakes the slow call's — reply order is NOT assumed
// on the Permissions stream; correlation is by request_id (parity with the
// CRI-161 engine interleave test). Both replies carry their own call's
// outputs and the run continues through the agent's own outcome routing.
func agentCaseConcurrentCalls(t *testing.T) {
	caller := newAgentCaller(
		agentConcurrentTurn(
			agentToolInvocation{requestID: "call-1", target: matrixToolCallTarget, tool: "helper_task", args: map[string]any{"task": "slow"}},
			agentToolInvocation{requestID: "call-2", target: matrixToolCallTarget, tool: "helper_task", args: map[string]any{"task": "fast"}},
		),
		agentSubmit("handled"),
	)
	callee := newMatrixCallee()
	src := matrixGraph("adapter_tools_matrix_agent_concurrent", "handled", "\n"+
		"  allow_tools = [\"adapter.callee.default.tools.*\"]\n"+
		"\n"+
		"  outcome \"handled\" {\n"+
		"    next = step.handled\n"+
		"  }\n"+
		"\n")
	sink, audit := runMatrixCase(t, src, caller, callee)

	// The turn's view: both calls settled cleanly, each reply correlated to
	// its own call's outputs; the fast call's reply overtook the slow
	// call's (the replies arrive in completion order, not issue order).
	assertAgentReplies(t, caller,
		agentToolReply{requestID: "call-2", outcome: "success", outputs: map[string]any{"report": "fast", "count": float64(4)}},
		agentToolReply{requestID: "call-1", outcome: "success", outputs: map[string]any{"report": "slow", "count": float64(4)}},
	)
	assertNoToolCallInputKeys(t, caller)

	assertAgentConcurrentDispatch(t, sink)
	assertAgentConcurrentPolicy(t, audit, callee)

	// The run-continuation invariant: the agent's own success outcome
	// routed the run to the terminal.
	assertMatrixRunContinued(t, sink, "handled")
}

// assertAgentConcurrentDispatch asserts the host's dispatch view for the
// concurrent case: both calls were granted and dispatched, and no deny or
// typed failure fired.
func assertAgentConcurrentDispatch(t *testing.T, sink *matrixEngineSink) {
	if grants := sink.stepEventCount("permission.granted"); grants != 2 {
		t.Fatalf("permission.granted events = %d, want 2", grants)
	}
	assertNoStepEvents(t, sink, "permission.denied")
	if calls := sink.stepEventCount("tool.call"); calls != 2 {
		t.Fatalf("tool.call events = %d, want 2 (both in-flight calls dispatched)", calls)
	}
	if results := sink.stepEventCount("tool.call_result"); results != 2 {
		t.Fatalf("tool.call_result events = %d, want 2", results)
	}
}

// assertAgentConcurrentPolicy asserts the audit trail and callee reach for
// the concurrent case: one policy allow per call, no deny, and both nested
// Executes in the callee's own session.
func assertAgentConcurrentPolicy(t *testing.T, audit *matrixAuditCollector, callee *matrixCalleeAdapter) {
	if allows := audit.auditDecisions("allow"); len(allows) != 2 {
		t.Fatalf("allow audit entries = %d, want 2 (one per concurrent call)", len(allows))
	}
	if denies := audit.auditDecisions("deny"); len(denies) != 0 {
		t.Fatalf("deny audit entries = %+v, want none", denies)
	}
	assertSessionCloseSummary(t, audit, 2)

	execs := callee.executions()
	if len(execs) != 2 {
		t.Fatalf("callee executions = %d, want 2: %+v", len(execs), execs)
	}
	seen := map[string]string{}
	for _, exec := range execs {
		if exec.sessionID != "callee.default" {
			t.Fatalf("callee execution session = %q, want \"callee.default\"", exec.sessionID)
		}
		seen[exec.task] = exec.sessionID
	}
	for _, task := range []string{"slow", "fast"} {
		if _, ok := seen[task]; !ok {
			t.Fatalf("callee never executed task %q: %+v", task, execs)
		}
	}
}

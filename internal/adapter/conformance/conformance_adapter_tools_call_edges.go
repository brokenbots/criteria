package conformance

// conformance_adapter_tools_call_edges.go — M6.4 conformance (CRI-170): the
// three invariants that govern caller→callee adapter call edges, asserted
// against the real engine with in-memory fakes rather than documented:
//
//  1. callee_output_redacted — redaction survives call edges. The caller's
//     secret input rides its freely-chosen call arguments, the callee echoes
//     a value derived from that input back as a Sensitive-declared output
//     (so the runtime redaction registry built by the engine, CRI-163,
//     registers the echoed value — and only that registration can mask it,
//     since it is not byte-identical to anything registered at run start),
//     and the caller then embeds the echoed value in the next call's target
//     as the bareword tool label and re-exports it as its own step output.
//     Every host surface the value touches must carry the masked form: the
//     permission.granted event payload (redactEventValue), the allow and
//     deny audit entries (RedactingAuditWriter), and the re-exported outputs
//     on the engine's redacting sink. A marshaled sweep over every captured
//     event and audit entry fails if the raw value leaks anywhere else. The
//     undeclared tool label is typed-rejected (unknown_tool) by the graph
//     gate after the policy gate granted it — which is what puts the echoed
//     value on the granted/denied audit path without dispatching a second
//     nested call.
//  2. deny_by_default_survives_call_edges — callee allow_tools are its own.
//     The caller step's allow_tools is deliberately broad enough to cover
//     not only the first callee but also the callee's own nested target (so
//     a host that leaked the caller's patterns into the callee's session
//     would grant it); the declaring workflow carries NO permissions block,
//     so the nested callee session's policy is empty and its own call to
//     the next hop is denied with "no matching allow_tools entry". The
//     caller's patterns never auto-grant: the third hop never executes, the
//     denial is a permission cancel (not a typed reply), and the audit
//     carries the callee-side deny at the callee's own nesting layer.
//
// The compile-time half of the ticket (call edges are non-data-flow for
// workflow/compile_taint.go) lives in workflow/compile_taint_call_edges_test.go:
// a differential asserting a cross-adapter tools list adds no taint
// diagnostic to a workflow whose step input derives from a secret.
//
// This suite is host-side and unconditional: the redaction and deny-by-default
// behaviors under test live in the host runtime, not in adapters, so a
// per-adapter matrix suite (matrix.yaml gates suites on adapter capabilities)
// would skip in CI forever. It runs as its own always-on conformance entry
// point, mirroring the CRI-167 matrix and the CRI-168 depth/cycle suite.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

const (
	// callEdgesSecretValue is the caller's secret input: registered in the
	// run's redaction registry at run start (the workflow declares it as a
	// secret variable).
	callEdgesSecretValue = "sk_live_call_edges_secret_value"

	// callEdgesEchoSuffix derives the callee's echo from the task argument,
	// so the value surfacing as the callee's output is NOT byte-identical to
	// any value registered at run start: the only thing that can mask it is
	// the callee-output registration itself (registerSensitiveOutputs on the
	// Sensitive-declared report).
	callEdgesEchoSuffix = "-echoed"

	// callEdgesEchoValue is the callee's echoed sensitive output.
	callEdgesEchoValue = callEdgesSecretValue + callEdgesEchoSuffix

	// callEdgesRedacted is the mask literal the redaction registry
	// substitutes for registered sensitive values.
	callEdgesRedacted = "[REDACTED]"

	// callEdgesEchoTarget is call-1's target: the callee's declared helper
	// tool (matrixToolCallTarget, shared with the CRI-167 matrix).
	callEdgesEchoTarget = matrixToolCallTarget

	// callEdgesEchoTargetBase is the target prefix up to the bareword tool
	// label; call-2 appends the echoed sensitive value as the label.
	callEdgesEchoTargetBase = "adapter.callee.default.tools"

	// callEdgesMaskedEchoTarget is the only acceptable form of call-2's
	// target on any host surface: the secret must be masked in the tool
	// label it rides.
	callEdgesMaskedEchoTarget = callEdgesEchoTargetBase + "." + callEdgesRedacted

	// callEdgesHopBTarget/Glob, callEdgesHopCTarget, and callEdgesHopCGlob
	// alias the depth-cycle suite's shared hop-chain surface constants: the
	// deny-by-default scenario reuses the matrix's three-hop chain verbatim.
	callEdgesHopBTarget = depthCycleHopBTarget
	callEdgesHopBGlob   = depthCycleHopBGlob
	callEdgesHopCTarget = depthCycleHopCTarget
	callEdgesHopCGlob   = depthCycleHopCGlob
)

// callEdgesSink extends the matrix engine sink with step-output capture, so
// the redaction scenario can assert the sensitive callee output is masked on
// the engine's step-output surface too (the engine's redacting sink masks it
// through the runtime redaction registry, CRI-163).
type callEdgesSink struct {
	*matrixEngineSink

	mu      sync.Mutex
	outputs map[string]map[string]string
}

func (s *callEdgesSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	cp := make(map[string]string, len(outputs))
	for k, v := range outputs {
		cp[k] = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputs == nil {
		s.outputs = map[string]map[string]string{}
	}
	s.outputs[step] = cp
}

// stepOutputs returns a copy of the outputs captured for a step (nil when
// none were), so callers cannot race the sink's writer.
func (s *callEdgesSink) stepOutputs(step string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	captured := s.outputs[step]
	if captured == nil {
		return nil
	}
	out := make(map[string]string, len(captured))
	for k, v := range captured {
		out[k] = v
	}
	return out
}

// snapshotEvents copies every recorded event, grouped by step, for the
// suite's marshaled no-leak sweep.
func (s *callEdgesSink) snapshotEvents() map[string][]matrixEvent {
	s.matrixEngineSink.mu.Lock()
	defer s.matrixEngineSink.mu.Unlock()
	out := make(map[string][]matrixEvent, len(s.stepEvents))
	for step, events := range s.stepEvents {
		out[step] = append([]matrixEvent(nil), events...)
	}
	return out
}

// callEdgesEchoCaller models the redaction invariant's leak chain: the
// caller's secret input (the engine resolved it into the step's secret
// inputs) rides the call arguments the caller freely chooses; after the
// callee echoes it back, the caller embeds the echoed value in the next
// call's target as the bareword tool label — the adapter-supplied surface
// the host must mask in events and audit alike (CRI-163). Every other
// behavior (Permissions stream capture, typed reply correlation and
// recording, lifecycle methods) is inherited from the matrix caller harness.
type callEdgesEchoCaller struct {
	*matrixCallerAdapter

	mu   sync.Mutex
	echo string
}

func (a *callEdgesEchoCaller) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "call-edges-echo-caller",
		Version:      "0.0.0-call-edges",
		Capabilities: []string{"adapter_tools", "execute"},
	}, nil
}

func (a *callEdgesEchoCaller) Execute(_ context.Context, _ string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	secret := step.SecretInputs["token"]
	if secret == "" {
		return adapter.Result{Outcome: "failure"}, errors.New("call-edges caller: secret input token did not resolve")
	}

	// Call 1: the secret rides the caller-chosen call arguments — the
	// compile time cannot see this data flow (call arguments are
	// caller-chosen, never auto-propagated), and the runtime redaction must
	// mask whatever the callee echoes back.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     callEdgesEchoTarget,
		"args":       map[string]any{"task": secret},
	})
	if !a.awaitReply(matrixCall{requestID: "call-1", target: callEdgesEchoTarget}) {
		return adapter.Result{Outcome: "failure"}, errors.New("call-edges caller: call-1 did not settle")
	}
	replies := a.gotResults()
	if len(replies) != 1 || replies[0].outcome != "success" {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("call-edges caller: call-1 reply %+v, want a successful echo", replies)
	}
	echo, _ := replies[0].outputs["report"].(string)
	if echo == "" {
		return adapter.Result{Outcome: "failure"}, errors.New("call-edges caller: callee echoed an empty report")
	}
	a.mu.Lock()
	a.echo = echo
	a.mu.Unlock()

	// Call 2: the echoed sensitive output rides the next call's target. The
	// policy gate grants it (the step's glob matches the whole surface), the
	// graph gate rejects the undeclared tool label typed unknown_tool — and
	// every surface the value touched (granted event tool, audit Tool) must
	// carry the masked form.
	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-2",
		"target":     callEdgesEchoTargetBase + "." + echo,
		"args":       map[string]any{"task": "echo-again"},
	})
	if !a.awaitReply(matrixCall{requestID: "call-2", target: callEdgesEchoTargetBase + "." + echo}) {
		return adapter.Result{Outcome: "failure"}, errors.New("call-edges caller: call-2 did not settle")
	}
	// The caller re-exports the echoed callee output as its own step output
	// (the CRI-163 leak chain): this is what puts the value on the engine's
	// step-output surface, where the redacting sink must mask it via the
	// runtime redaction registry.
	return adapter.Result{Outcome: "success", Outputs: map[string]cty.Value{"report": cty.StringVal(echo)}}, nil
}

// echoed returns the sensitive value the caller observed from the callee's
// echo (the setup invariant for the redaction assertions).
func (a *callEdgesEchoCaller) echoed() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.echo
}

// callEdgesEchoCallee is the redaction scenario's callee fake: the matrix
// callee behavior with two deliberate differences — its report output echoes
// the task argument (derived, so the surfaced value is not byte-identical to
// anything registered at run start) and is declared Sensitive, so the echoed
// value lands in the runtime redaction registry (CRI-163) the moment the
// nested step's outputs render (registerSensitiveOutputs).
type callEdgesEchoCallee struct {
	*matrixCalleeAdapter
}

func (a *callEdgesEchoCallee) Info(ctx context.Context) (adapterhost.Info, error) {
	info, err := a.matrixCalleeAdapter.Info(ctx)
	if err != nil {
		return info, err
	}
	info.AdapterInfo.OutputSchema = map[string]workflow.ConfigField{
		"report": {CtyType: cty.String, Sensitive: true},
		"count":  {CtyType: cty.Number},
	}
	return info, nil
}

func (a *callEdgesEchoCallee) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	res, err := a.matrixCalleeAdapter.Execute(ctx, sessionID, step, sink)
	if err != nil {
		return res, err
	}
	res.Outputs["report"] = cty.StringVal(step.Input["task"] + callEdgesEchoSuffix)
	return res, nil
}

// callEdgesRedactionWorkflowHCL is the redaction scenario workflow: the
// caller step declares its secret input (the secret variable's value the
// engine registers at run start) and a broad allow_tools glob for the
// callee's whole tool surface; the callee adapter declares one static helper
// tool. No compile-time tools grants exist — the calls ride the step's
// allow_tools policy alone.
func callEdgesRedactionWorkflowHCL() string {
	return `
workflow {
  name          = "adapter_tools_call_edges_redaction"
  version       = "0.1"
  initial_state = "call"
  target_state  = "success"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "` + callEdgesSecretValue + `"
}

adapter "callee" "default" {
  tool "helper_task" {}
}

adapter "caller" "default" {}

step "call" {
  target = adapter.caller.default

  secret_input {
    token = var.api_key
  }
  allow_tools = ["adapter.callee.default.tools.*"]

  outcome "success" { next = step.success }
}

state "success" { terminal = true }
`
}

// callEdgesRedactionSchemas mirrors the runtime handshake of the redaction
// scenario's fakes: the caller advertises adapter_tools, the callee declares
// its helper input and — as at runtime — a Sensitive report output.
func callEdgesRedactionSchemas() map[string]workflow.AdapterInfo {
	return map[string]workflow.AdapterInfo{
		"caller": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{},
			Capabilities: []string{"adapter_tools"},
		},
		"callee": {
			InputSchema: map[string]workflow.ConfigField{"task": {Required: true}},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String, Sensitive: true},
				"count":  {CtyType: cty.Number},
			},
		},
	}
}

// callEdgesDenyWorkflowHCL is the deny-by-default scenario workflow: the
// caller step's allow_tools covers both the first callee AND the callee's
// own nested target (a pattern-leak regression would therefore grant the
// nested call and fail the case), while the declaring workflow carries NO
// permissions block — so the nested callee session's own policy is empty and
// its call to the next hop must be denied by default.
func callEdgesDenyWorkflowHCL() string {
	return `
workflow {
  name          = "adapter_tools_call_edges_deny_default"
  version       = "0.1"
  initial_state = "call"
  target_state  = "success"
}

adapter "caller" "default" {}
adapter "hopb" "default" {
  tool "helper_task" {}
}
adapter "hopc" "default" {
  tool "helper_task" {}
}

step "call" {
  target = adapter.caller.default
  allow_tools = ["` + callEdgesHopBGlob + `", "` + callEdgesHopCGlob + `"]

  outcome "success" { next = step.success }
}

state "success" { terminal = true }
`
}

// callEdgesDenySchemas mirrors the deny scenario's runtime handshake.
func callEdgesDenySchemas() map[string]workflow.AdapterInfo {
	hop := workflow.AdapterInfo{
		InputSchema: map[string]workflow.ConfigField{"task": {Required: true}},
	}
	return map[string]workflow.AdapterInfo{
		"caller": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{},
			Capabilities: []string{"adapter_tools"},
		},
		"hopb": hop,
		"hopc": hop,
	}
}

// compileCallEdgesGraph parses and compiles a call-edges conformance
// workflow with the scenario's schemas (keyed by adapter TYPE, the
// compiler's adapterInfo lookup key). Runtime scenarios use only allow_tools
// (no step-level tools grants, so no compile edges and no cycle warning) and
// must compile cleanly — errors AND warnings — keeping a noisy compile out
// of the runtime signal.
func compileCallEdgesGraph(t *testing.T, filename, src string, schemas map[string]workflow.AdapterInfo) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse(filename, []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse call-edges workflow: %s", diags)
	}
	g, diags := workflow.Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("compile call-edges workflow: %s", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("call-edges workflow compiled with warnings, want clean: %s", diags)
	}
	return g
}

// runCallEdgesCase drives one call-edges scenario through the real engine
// with the scenario's own fakes (the redaction case needs a caller that reads
// its resolved secret input and a callee whose echo is declared Sensitive;
// the deny-by-default case reuses the depth-cycle chain-hop fakes).
func runCallEdgesCase(t *testing.T, filename, src string, schemas map[string]workflow.AdapterInfo, handles map[string]adapterhost.Handle) (*callEdgesSink, *matrixAuditCollector) {
	t.Helper()
	sink := &callEdgesSink{matrixEngineSink: &matrixEngineSink{}}
	audit := &matrixAuditCollector{}
	if err := engine.New(compileCallEdgesGraph(t, filename, src, schemas), &matrixLoader{handles: handles}, sink, engine.WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("engine run: %v", err)
	}
	return sink, audit
}

// RunAdapterToolsCallEdgesConformance runs the CRI-170 M6.4 call-edges
// conformance suite: sub-tests locking the caller→callee invariants the
// runtime must hold — redaction across call edges and deny-by-default
// surviving call edges. The compile-time half (call edges are non-data-flow
// for the taint pass) is asserted in workflow/compile_taint_call_edges_test.go.
func RunAdapterToolsCallEdgesConformance(t *testing.T) {
	t.Run("callee_output_redacted", callEdgesCaseCalleeOutputRedacted)
	t.Run("deny_by_default_survives_call_edges", callEdgesCaseDenyByDefault)
}

// callEdgesCaseCalleeOutputRedacted covers invariant 1: a secret the caller
// feeds through a call edge comes back masked wherever the host surfaces it.
// The callee echoes a value derived from the caller's secret input as its
// Sensitive report output — derived, so the only registration that can mask
// it is the callee-output registration (registerSensitiveOutputs), the
// CRI-163 path this invariant exercises. The caller then embeds the echoed
// value in call-2's target as the bareword tool label and re-exports it as
// its own step output. Every host surface the value touches must carry the
// masked form: the permission.granted event payload (redactEventValue), the
// allow and deny audit entries (RedactingAuditWriter), and the re-exported
// outputs on the engine's redacting sink — and nowhere in any captured event
// or audit entry may the raw value appear.
func callEdgesCaseCalleeOutputRedacted(t *testing.T) {
	caller := &callEdgesEchoCaller{matrixCallerAdapter: newMatrixCaller([]string{"adapter_tools", "execute"}, "success")}
	callee := &callEdgesEchoCallee{matrixCalleeAdapter: newMatrixCallee()}
	sink, audit := runCallEdgesCase(t, "call_edges_redaction.hcl", callEdgesRedactionWorkflowHCL(), callEdgesRedactionSchemas(), map[string]adapterhost.Handle{
		"caller": caller,
		"callee": callee,
	})
	assertMatrixRunContinued(t, sink.matrixEngineSink, "success")

	// Setup invariants: the caller's secret rode the call args (caller-chosen,
	// not auto-propagated — compile_taint must never see this as data-flow),
	// the callee executed exactly once with that task, and the caller got the
	// echo back so call-2 could embed it in the target.
	execs := callee.executions()
	if len(execs) != 1 {
		t.Fatalf("callee executions = %+v, want exactly one (the caller-chosen echo call)", execs)
	}
	if execs[0].sessionID != "callee.default" || execs[0].task != callEdgesSecretValue {
		t.Fatalf("callee execution = session %q task %q, want session %q task %q (the caller-chosen argument)",
			execs[0].sessionID, execs[0].task, "callee.default", callEdgesSecretValue)
	}
	if got := caller.echoed(); got != callEdgesEchoValue {
		t.Fatalf("caller observed echo %q, want the derived sensitive output %q (leak-chain setup)", got, callEdgesEchoValue)
	}

	// Typed replies: call-1 settled with the echoed value; call-2 was
	// typed-rejected by the graph gate (undeclared tool label) after the
	// policy gate granted it — a typed gate reject is data for the caller,
	// not a deny.
	results := caller.gotResults()
	if len(results) != 2 {
		t.Fatalf("caller typed replies = %+v, want 2 (echo + typed reject)", results)
	}
	if results[0].requestID != "call-1" || results[0].outcome != "success" || results[0].outputs["report"] != callEdgesEchoValue {
		t.Fatalf("call-1 reply = %+v, want outcome success with the echoed report", results[0])
	}
	if results[1].requestID != "call-2" || results[1].callError != "unknown_tool" || results[1].outcome != "" {
		t.Fatalf("call-2 reply = %+v, want typed unknown_tool with an empty outcome", results[1])
	}
	if cancels := caller.gotCancels(); len(cancels) != 0 {
		t.Fatalf("caller received cancels = %+v, want none (a typed gate reject is not a deny)", cancels)
	}

	callEdgesAssertRedactionEvents(t, sink)
	callEdgesAssertRedactionStepOutputs(t, sink)
	callEdgesAssertRedactionAudit(t, audit)

	// The no-leak sweep: no captured event payload and no audit entry may
	// carry the raw sensitive value anywhere.
	callEdgesAssertNoRawSecret(t, sink, audit)
}

// callEdgesAssertRedactionEvents asserts the redaction scenario's event
// surface (CRI-163): exactly two policy grants and one dispatched call.
// Call-2's grant is where the echoed value rides the tool label — the
// payload must carry the masked form. A typed gate reject records no
// permission deny and dispatches nothing.
func callEdgesAssertRedactionEvents(t *testing.T, sink *callEdgesSink) {
	t.Helper()
	grants := sink.stepEventsOf("permission.granted")
	if len(grants) != 2 {
		t.Fatalf("permission.granted events = %d, want 2 (both calls policy-allowed): %+v", len(grants), grants)
	}
	if matrixEventString(grants[0], "request_id") != "call-1" || matrixEventString(grants[0], "tool") != callEdgesEchoTarget {
		t.Fatalf("call-1 grant = %+v, want tool %q", grants[0].payload, callEdgesEchoTarget)
	}
	if matrixEventString(grants[1], "request_id") != "call-2" || matrixEventString(grants[1], "tool") != callEdgesMaskedEchoTarget {
		t.Fatalf("call-2 grant = %+v, want tool %q (the echoed sensitive value must arrive masked)", grants[1].payload, callEdgesMaskedEchoTarget)
	}
	for _, grant := range grants {
		if pattern := matrixEventString(grant, "pattern"); pattern != "adapter.callee.default.tools.*" {
			t.Fatalf("grant %s pattern = %q, want the step's matched glob", matrixEventString(grant, "request_id"), pattern)
		}
	}
	assertNoStepEvents(t, sink.matrixEngineSink, "permission.denied")
	assertCallerStepEvent(t, sink.matrixEngineSink, "tool.call", map[string]string{
		"target":     callEdgesEchoTarget,
		"tool":       "helper_task",
		"request_id": "call-1",
	})
	assertCallerStepEvent(t, sink.matrixEngineSink, "tool.call_result", map[string]string{
		"target":     callEdgesEchoTarget,
		"tool":       "helper_task",
		"request_id": "call-1",
		"outcome":    "success",
	})
	if n := sink.stepEventCount("tool.call"); n != 1 {
		t.Fatalf("tool.call events = %d, want 1 (the typed-rejected call-2 must never dispatch)", n)
	}
}

// callEdgesAssertRedactionStepOutputs asserts the caller's re-exported step
// outputs (the echoed sensitive output) arrive masked on the engine's
// step-output surface (the caller step "call"; the registry registered the
// value from the callee's Sensitive output).
func callEdgesAssertRedactionStepOutputs(t *testing.T, sink *callEdgesSink) {
	t.Helper()
	outputs := sink.stepOutputs("call")
	if outputs == nil {
		t.Fatalf("no step outputs captured for the caller step %q", "call")
	}
	if got := outputs["report"]; got != callEdgesRedacted {
		t.Errorf("caller step output report = %q, want %q (the runtime redaction registry must mask the re-exported sensitive output)", got, callEdgesRedacted)
	}
}

// callEdgesAssertRedactionAudit asserts the redaction scenario's audit
// surface (CRI-163): both calls' allows carry the matched glob, call-2's
// allow and the typed-reject deny carry the masked Tool, and the caller's
// session closes with the two policy decisions it recorded (the typed gate
// reject contributes none).
func callEdgesAssertRedactionAudit(t *testing.T, audit *matrixAuditCollector) {
	t.Helper()
	assertAllowedLayers(t, audit.auditDecisions("allow"), map[string]int{"caller.default@0": 2})
	allows := audit.auditDecisions("allow")
	for i := range allows {
		allow := &allows[i]
		switch allow.RequestID {
		case "call-1":
			if allow.Tool != callEdgesEchoTarget {
				t.Errorf("call-1 allow audit Tool = %q, want %q", allow.Tool, callEdgesEchoTarget)
			}
		case "call-2":
			if allow.Tool != callEdgesMaskedEchoTarget {
				t.Errorf("call-2 allow audit Tool = %q, want %q (the echoed value must be masked in audit too)", allow.Tool, callEdgesMaskedEchoTarget)
			}
		default:
			t.Errorf("unexpected allow audit entry request %q: %+v", allow.RequestID, allow)
		}
	}
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "adapter tool call rejected: unknown_tool" {
		t.Fatalf("deny audit entries = %+v, want exactly one \"adapter tool call rejected: unknown_tool\"", denies)
	}
	if denies[0].RequestID != "call-2" || denies[0].Tool != callEdgesMaskedEchoTarget || denies[0].SessionID != "caller.default" || denies[0].Layer != 0 {
		t.Fatalf("deny audit entry = %+v, want request call-2 with masked Tool on caller.default layer 0", denies[0])
	}
	assertSessionCloseSummaries(t, audit, map[string]int{"caller.default": 2})
}

// callEdgesAssertNoRawSecret fails the case when the raw sensitive value
// appears anywhere in the marshaled form of every captured event payload or
// audit entry — the sweep that fails any redaction regression, not just the
// fields asserted above.
func callEdgesAssertNoRawSecret(t *testing.T, sink *callEdgesSink, audit *matrixAuditCollector) {
	t.Helper()
	callEdgesAssertValueAbsent(t, sink.snapshotEvents(), audit.all())
}

// callEdgesAssertValueAbsent fails when either raw sensitive value — the
// run-start secret or the callee's derived echo (a superset of the secret,
// checked separately for a precise failure message) — appears anywhere in
// the marshaled form of the captured event payloads or audit entries.
func callEdgesAssertValueAbsent(t *testing.T, events map[string][]matrixEvent, entries []adapterhost.DecisionLogEntry) {
	t.Helper()
	for _, raw := range []string{callEdgesSecretValue, callEdgesEchoValue} {
		for step, stepEvents := range events {
			for _, ev := range stepEvents {
				blob, err := json.Marshal(ev.payload)
				if err != nil {
					t.Fatalf("marshal event %q under step %q: %v", ev.kind, step, err)
				}
				callEdgesAssertAbsent(t, string(blob), raw, fmt.Sprintf("event %q under step %q", ev.kind, step))
			}
		}
		for i := range entries {
			blob, err := json.Marshal(entries[i])
			if err != nil {
				t.Fatalf("marshal audit entry %d: %v", i, err)
			}
			callEdgesAssertAbsent(t, string(blob), raw, fmt.Sprintf("audit entry %d", i))
		}
	}
}

func callEdgesAssertAbsent(t *testing.T, blob, raw, where string) {
	t.Helper()
	if strings.Contains(blob, raw) {
		t.Errorf("%s carries the raw sensitive value: %s", where, blob)
	}
}

// callEdgesCaseDenyByDefault covers invariant 3: deny-by-default survives
// call edges. The caller step's allow_tools is broad enough to cover the
// first callee and the callee's own nested target, but the declaring
// workflow carries no permissions block, so the nested callee session's own
// policy is empty and its call to the next hop is denied with "no matching
// allow_tools entry". The caller's patterns never auto-grant — the deny
// holds even though the caller's patterns would have matched the nested
// call had the host leaked them into the callee's session — the denial is a
// permission cancel on the callee's own session (audit at the callee's own
// nesting layer), the denied call is never dispatched, and the third hop
// never executes at all.
func callEdgesCaseDenyByDefault(t *testing.T) {
	caller := newChainHop("call-edges-deny-caller", matrixCall{
		requestID: "call-1",
		target:    callEdgesHopBTarget,
		args:      map[string]any{"task": "one"},
	})
	// hopb runs as the nested callee and issues its own call to hopc — the
	// request its own (empty) policy must deny. Its outcome is the
	// non-success "handled": a callee that observed a policy deny must not
	// claim success (the run's needs_review override only rewrites a
	// success), so the case stays on the caller's own outcome routing.
	hopb := &chainHopAdapter{
		matrixCallerAdapter: newMatrixCaller([]string{"adapter_tools", "execute"}, "handled", matrixCall{
			requestID: "hopb-call-1",
			target:    callEdgesHopCTarget,
			args:      map[string]any{"task": "nested"},
		}),
		identity: "call-edges-deny-hopb",
	}
	hopc := newChainHop("call-edges-deny-hopc")

	sink, audit := runCallEdgesCase(t, "call_edges_deny_default.hcl", callEdgesDenyWorkflowHCL(), callEdgesDenySchemas(), map[string]adapterhost.Handle{
		"caller": caller,
		"hopb":   hopb,
		"hopc":   hopc,
	})
	assertMatrixRunContinued(t, sink.matrixEngineSink, "success")

	// The callee's own request was denied by its own policy; the caller's
	// patterns never auto-granted, and hopc never executed.
	assertDenyCancel(t, hopb.matrixCallerAdapter, "hopb-call-1", "no matching allow_tools entry")
	hopc.assertNeverExecuted(t)

	// The caller's own call settled on the hop's non-success outcome: the
	// deny affected only the denied call.
	caller.assertTypedReplies(t, []matrixReply{{
		requestID: "call-1",
		outcome:   "handled",
		outputs:   map[string]any{"report": "one"},
	}})
	caller.assertNoCancels(t)
	caller.assertExecutions(t, "caller.default", "")
	hopb.assertExecutions(t, "hopb.default", "one")

	callEdgesAssertDenyEvents(t, sink)
	callEdgesAssertDenyAudit(t, audit)
}

// callEdgesAssertDenyEvents asserts the deny-by-default scenario's event
// surface (ADR-0004 §8): the caller's call was granted and dispatched; the
// callee's denied request emitted the permission.denied event with the
// policy reason and never dispatched.
func callEdgesAssertDenyEvents(t *testing.T, sink *callEdgesSink) {
	t.Helper()
	assertGrantedRequests(t, sink.matrixEngineSink, [][2]string{{"call-1", callEdgesHopBGlob}})
	denied := sink.stepEventsOf("permission.denied")
	if len(denied) != 1 {
		t.Fatalf("permission.denied events = %d, want 1: %+v", len(denied), denied)
	}
	if matrixEventString(denied[0], "request_id") != "hopb-call-1" ||
		matrixEventString(denied[0], "reason") != "no matching allow_tools entry" ||
		matrixEventString(denied[0], "tool") != callEdgesHopCTarget {
		t.Fatalf("permission.denied = %+v, want the callee's denied call with the deny-by-default reason", denied[0].payload)
	}
	calls := sink.stepEventsOf("tool.call")
	if len(calls) != 1 || matrixEventString(calls[0], "request_id") != "call-1" ||
		matrixEventString(calls[0], "target") != callEdgesHopBTarget || matrixEventDepth(calls[0]) != 1 {
		t.Fatalf("tool.call events = %+v, want exactly call-1 at depth 1 (a denied call is never dispatched)", calls)
	}
	assertCallerStepEvent(t, sink.matrixEngineSink, "tool.call_result", map[string]string{
		"target":     callEdgesHopBTarget,
		"request_id": "call-1",
		"outcome":    "handled",
	})
}

// callEdgesAssertDenyAudit asserts the deny-by-default scenario's audit
// surface (ADR-0004 §6): the caller's allow at its own layer 0, the callee's
// deny at ITS OWN nesting layer 1 with the deny-by-default reason, and one
// close summary per session carrying the calls its policy actually
// evaluated.
func callEdgesAssertDenyAudit(t *testing.T, audit *matrixAuditCollector) {
	t.Helper()
	assertAllowedLayers(t, audit.auditDecisions("allow"), map[string]int{"caller.default@0": 1})
	denies := audit.auditDecisions("deny")
	if len(denies) != 1 || denies[0].Reason != "no matching allow_tools entry" {
		t.Fatalf("deny audit entries = %+v, want exactly one \"no matching allow_tools entry\"", denies)
	}
	if denies[0].SessionID != "hopb.default" || denies[0].Layer != 1 || denies[0].RequestID != "hopb-call-1" {
		t.Fatalf("deny audit entry = %+v, want the callee session at layer 1 for hopb-call-1", denies[0])
	}
	assertSessionCloseSummaries(t, audit, map[string]int{"caller.default": 1, "hopb.default": 1})
}

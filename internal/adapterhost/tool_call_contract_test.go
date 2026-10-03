package adapterhost

// KB-59: declared-type validation at the adapter-tools seam. The callee
// adapter is schema-less on purpose (the dynamic-surface posture, CRI-172):
// its Info handshake declares no input or output schema, so the only real
// validation the nested tool call gets is the typed contract compiled onto
// the callee's `tool` block (workflow.ToolContract for the named tool).
//
// Covered here:
//
//   - a call whose arguments satisfy the declared in-contract executes
//     normally, with the synthetic step carrying no fabricated schema keys;
//   - a call whose arguments violate the contract (wrong type, unknown key,
//     missing required property) is rejected typed `invalid_args` before the
//     callee runs, with the ordered issue list in the audit's deny reason;
//   - the contract takes precedence over a declared handshake schema: the
//     contract's issue vocabulary wins when both surfaces are present;
//   - callee outputs that violate the declared out-contract are refused at
//     the boundary with typed `invalid_response`;
//   - a callee with no declared contract and no declared schema behaves
//     byte-identically to the pre-KB-59 seam: untyped arguments pass
//     through and results are not gated.

import (
	"context"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/brokenbots/criteria/workflow"
)

// contractCalleeAdapter is the schema-less callee used by the contract
// fixtures: the handshake carries no AdapterInfo at all, exactly like the
// dynamic mcp adapter whose tool surface is discovered at session open.
type contractCalleeAdapter struct{ *nestedCalleeAdapter }

func (a *contractCalleeAdapter) Info(ctx context.Context) (Info, error) {
	info, err := a.nestedCalleeAdapter.Info(ctx)
	info.AdapterInfo = workflow.AdapterInfo{}
	return info, err
}

// nestedToolCallContractWorkflowHCL extends the fixture workflow with named
// types and a contract on the callee's tool block. The callee adapter itself
// is dynamic (dynamic_tools, no schemas): the contract is the declared
// surface.
const nestedToolCallContractWorkflowHCL = `
workflow {
  name            = "x"
  version         = "0.1"
  initial_state   = "call"
  target_state    = "done"
}

type "probe_request" {
  schema = object({
    task = string
  })
}

type "probe_response" {
  schema = object({
    report = string
    count  = optional(number)
  })
}

environment "shell" "prod" {
  os = "linux"
}

adapter "caller" "instance" {}
adapter "callee" "helper" {
  environment   = shell.prod
  dynamic_tools = true

  tool "helper_task" {
    in  = type.probe_request
    out = type.probe_response
  }
}

step "call" {
  target = adapter.caller.instance
  allow_tools = ["adapter.callee.helper.tools.*"]

  outcome "success" { next = step.done }
}
state "done" { terminal = true }

permissions {
  allow_tools = ["callee.helpers.*"]
}
`

// compileNestedToolCallContractGraph compiles the contracted fixture and
// asserts the contract landed on the callee's node as the seam will read it.
func compileNestedToolCallContractGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("nested_contract.hcl", []byte(nestedToolCallContractWorkflowHCL))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	callee := g.Adapters[nestedCalleeSession]
	if callee == nil {
		t.Fatalf("callee node %q missing from compiled graph", nestedCalleeSession)
	}
	contract, ok := callee.ToolContractFor("helper_task")
	if !ok || contract.InType == cty.NilType || contract.OutType == cty.NilType {
		t.Fatalf("contract for helper_task not compiled: %+v", contract)
	}
	return g
}

// openFixtureSessions verifies the graph and opens both sessions the way the
// pre-KB-59 success fixture does.
func openFixtureSessions(t *testing.T, sm *SessionManager, graph *workflow.FSMGraph) context.Context {
	t.Helper()
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	return ctx
}

// contractIssuesContains reports whether any audit entry for the caller's
// tool call carries every needle in its reason.
func contractIssuesContains(t *testing.T, audit *sliceAuditWriter, needles ...string) bool {
	t.Helper()
	entries := audit.all()
	for i := range entries {
		entry := &entries[i]
		if entry.SessionID != nestedCallerSession || entry.RequestID != "call-1" {
			continue
		}
		all := true
		for _, needle := range needles {
			if !strings.Contains(entry.Reason, needle) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// TestNestedToolCall_ContractValidatesArgs: a schema-less callee whose tool
// declares an in-contract accepts a well-typed call — the callee runs with
// exactly the rendered input, and the typed result decodes against the
// declared out-contract.
func TestNestedToolCall_ContractValidatesArgs(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: nestedToolCallArgs()}
	callee := &contractCalleeAdapter{&nestedCalleeAdapter{rec: calleeRec}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	graph := compileNestedToolCallContractGraph(t)
	sm.SetGraph(graph)
	ctx := openFixtureSessions(t, sm, graph)
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("caller outcome = %q, want success", res.Outcome)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.RequestId != "call-1" || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Fatalf("tool_call_result = %+v, want call-1/success/no error", tcr)
	}
	objType := cty.Object(map[string]cty.Type{"report": cty.String, "count": cty.Number})
	vals, decErr := ctyjson.Unmarshal(tcr.OutputsJson, objType)
	if decErr != nil {
		t.Fatalf("decode outputs_json %q: %v", tcr.OutputsJson, decErr)
	}
	if got := vals.GetAttr("report").AsString(); got != "do-thing" {
		t.Errorf("outputs.report = %q, want do-thing", got)
	}

	if got := calleeRec.calleeSession(); got != nestedCalleeSession {
		t.Fatalf("callee executed in session %q, want %q", got, nestedCalleeSession)
	}
	step := calleeRec.calleeStep()
	if step == nil {
		t.Fatal("callee never recorded a step")
	}
	if got := step.Input["task"]; got != "do-thing" || len(step.Input) != 1 {
		t.Errorf("callee step input = %v, want only task=do-thing", step.Input)
	}
	// The synthetic step carries the callee's declared handshake schema only
	// when the handshake declares one; a contract alone does not fabricate
	// one (the out-contract gates at the seam instead).
	if len(step.OutputSchema) != 0 {
		t.Errorf("callee step output schema = %+v, want none for a schema-less callee", step.OutputSchema)
	}
}

// TestNestedToolCall_ContractRejectsArgs: a schema-less callee whose tool
// declares an in-contract rejects a call whose arguments are mistyped (task
// as a JSON number) and carry an undeclared key, with the typed issue list
// in the audit reason and the callee never executed.
func TestNestedToolCall_ContractRejectsArgs(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: map[string]any{
		"task":  6,
		"ghost": "boo",
	}}
	callee := &contractCalleeAdapter{&nestedCalleeAdapter{rec: calleeRec}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	graph := compileNestedToolCallContractGraph(t)
	sm.SetGraph(graph)
	ctx := openFixtureSessions(t, sm, graph)
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	if _, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.RequestId != "call-1" || tcr.CallError != callErrorInvalidArgs || tcr.Outcome != "" {
		t.Fatalf("tool_call_result = %+v, want invalid_args reply", tcr)
	}
	if got := calleeRec.calleeSession(); got != "" {
		t.Errorf("callee executed for rejected args (session %q)", got)
	}
	for _, needle := range []string{
		`payload_schema: property "task": expected "string", got "number"`,
		`payload_schema: property "ghost": undeclared property`,
	} {
		if !contractIssuesContains(t, audit, needle) {
			t.Errorf("audit reason missing %q; entries = %+v", needle, audit.all())
		}
	}
}

// TestNestedToolCall_ContractRejectsMissingRequired: an empty argument set
// violates the declared in-contract's required property and is rejected
// before the callee runs. Pre-KB-59, the same call on a schema-less callee
// executed with no validation — this is the Gap 2 repro.
func TestNestedToolCall_ContractRejectsMissingRequired(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: nil}
	callee := &contractCalleeAdapter{&nestedCalleeAdapter{rec: calleeRec}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	graph := compileNestedToolCallContractGraph(t)
	sm.SetGraph(graph)
	ctx := openFixtureSessions(t, sm, graph)
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	if _, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.CallError != callErrorInvalidArgs {
		t.Fatalf("tool_call_result = %+v, want invalid_args reply", tcr)
	}
	if got := calleeRec.calleeSession(); got != "" {
		t.Errorf("callee executed for rejected args (session %q)", got)
	}
	if !contractIssuesContains(t, audit, `payload_schema: property "task": required property is missing`) {
		t.Errorf("audit reason missing required-property issue; entries = %+v", audit.all())
	}
}

// TestNestedToolCall_ContractPrecedesSchema: when the callee's handshake
// declares an input schema AND its tool declares a contract, the contract
// wins — an undeclared key is reported in the contract's payload-schema
// vocabulary, not the handshake's undeclared-input-key error.
func TestNestedToolCall_ContractPrecedesSchema(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: map[string]any{
		"task":  "do-thing",
		"ghost": "boo",
	}}
	callee := &nestedCalleeAdapter{rec: calleeRec}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	graph := compileNestedToolCallContractGraph(t)
	sm.SetGraph(graph)
	ctx := openFixtureSessions(t, sm, graph)
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	if _, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.RequestId != "call-1" || tcr.CallError != callErrorInvalidArgs || tcr.Outcome != "" {
		t.Fatalf("tool_call_result = %+v, want invalid_args reply", tcr)
	}
	if got := calleeRec.calleeSession(); got != "" {
		t.Errorf("callee executed for rejected args (session %q)", got)
	}
	if !contractIssuesContains(t, audit, `payload_schema: property "ghost": undeclared property`) {
		t.Errorf("audit reason missing contract issue; entries = %+v", audit.all())
	}
	for _, entry := range audit.all() {
		if entry.SessionID == nestedCallerSession && entry.RequestID == "call-1" &&
			strings.Contains(entry.Reason, `undeclared input key "ghost" for callee`) {
			t.Errorf("handshake vocabulary applied under a declared contract; entries = %+v", audit.all())
		}
	}
}

// TestNestedToolCall_ContractRejectsResponse: callee outputs that violate
// the declared out-contract (report as a JSON number) are refused at the
// boundary — the caller receives typed `invalid_response`, the callee did
// run, and the audit carries the validation reason.
func TestNestedToolCall_ContractRejectsResponse(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: nestedToolCallArgs()}
	callee := &contractCalleeAdapter{&nestedCalleeAdapter{rec: calleeRec, outputs: map[string]cty.Value{
		"report": cty.NumberIntVal(3),
		"count":  cty.NumberIntVal(1),
	}}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	graph := compileNestedToolCallContractGraph(t)
	sm.SetGraph(graph)
	ctx := openFixtureSessions(t, sm, graph)
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	if _, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.RequestId != "call-1" || tcr.CallError != callErrorInvalidResponse || tcr.Outcome != "" {
		t.Fatalf("tool_call_result = %+v, want invalid_response reply", tcr)
	}
	if got := calleeRec.calleeSession(); got != nestedCalleeSession {
		t.Errorf("callee session = %q, want %q (outputs are produced, then gated)", got, nestedCalleeSession)
	}
	if !contractIssuesContains(t, audit,
		"nested callee outputs rejected by contract validation",
		`payload_schema: property "report": expected "string", got "number"`) {
		t.Errorf("audit reason missing out-contract issues; entries = %+v", audit.all())
	}
}

// TestNestedToolCall_UncontractedPassthrough: a callee with NO declared
// contract and NO declared schema — the pre-KB-59 dynamic posture — lets
// untyped arguments through and does not gate the result: the seam behaves
// byte-identically when nothing is declared.
func TestNestedToolCall_UncontractedPassthrough(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: nestedCallTarget, args: map[string]any{
		"task":  "do-thing",
		"ghost": "boo",
	}}
	callee := &contractCalleeAdapter{&nestedCalleeAdapter{rec: calleeRec}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	// Hand-built graph: no tool contracts anywhere.
	sm.SetGraph(nestedToolCallGraph())
	ctx := context.Background()
	// The callee is verified for lazy binding but never opened up front.
	if err := sm.Verify(ctx, nestedCalleeSession, "callee", "", nil, nil, nil, "", "wf", "inst-1"); err != nil {
		t.Fatalf("Verify callee: %v", err)
	}
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()

	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, nestedCallerSession, nestedCallerStep(), inner, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("caller outcome = %q, want success", res.Outcome)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Fatalf("tool_call_result = %+v, want success", tcr)
	}
	if got := calleeRec.calleeSession(); got != nestedCalleeSession {
		t.Fatalf("callee executed in session %q, want %q", got, nestedCalleeSession)
	}
	step := calleeRec.calleeStep()
	if step == nil {
		t.Fatal("callee never recorded a step")
	}
	if got := step.Input["task"]; got != "do-thing" {
		t.Errorf("callee input task = %q, want do-thing", got)
	}
	if got, ok := step.Input["ghost"]; !ok || got != "boo" {
		t.Errorf("callee input ghost = %q/%v, want boo/unchecked passthrough", got, ok)
	}
}

// bareTargetContractWorkflowHCL is the bare-target twin of the contract
// fixture: the caller's step carries a whole-surface tools grant
// (adapter.callee.helper.tools, the bare 4-label form whose Tool segment is
// empty) and the callee's helper_task contract declares the routing key
// itself (`tool = string`) as a validated payload attribute. A call through
// the bare target carries the tool name as an args key — the same routing
// key the mcp bridge reads at dispatch — so the seam must still route the
// in-contract.
const bareTargetContractWorkflowHCL = `
workflow {
  name            = "x"
  version         = "0.1"
  initial_state   = "call"
  target_state    = "done"
}

type "bare_request" {
  schema = object({
    tool = string
    task = string
  })
}

type "probe_response" {
  schema = object({
    report = string
    count  = optional(number)
  })
}

environment "shell" "prod" {
  os = "linux"
}

adapter "caller" "instance" {}
adapter "callee" "helper" {
  environment   = shell.prod
  dynamic_tools = true

  tool "helper_task" {
    in  = type.bare_request
    out = type.probe_response
  }
}

step "call" {
  target = adapter.caller.instance
  tools  = [adapter.callee.helper.tools]

  outcome "success" { next = step.done }
}
state "done" { terminal = true }

permissions {
  allow_tools = ["callee.helpers.*"]
}
`

const bareCallTarget = "adapter.callee.helper.tools"

// compileNestedToolCallBareGraph compiles the bare-target fixture and asserts
// both that the contract landed on the callee node and that the step's
// whole-surface grant compiled to the bare-ref shape the seam's grant gate
// matches (CalleeRef set, empty Tool).
func compileNestedToolCallBareGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("bare_contract.hcl", []byte(bareTargetContractWorkflowHCL))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	callee := g.Adapters[nestedCalleeSession]
	if callee == nil {
		t.Fatalf("callee node %q missing from compiled graph", nestedCalleeSession)
	}
	if contract, ok := callee.ToolContractFor("helper_task"); !ok || contract.InType == cty.NilType {
		t.Fatalf("contract for helper_task not compiled: %+v", contract)
	}
	step := g.Steps["call"]
	if step == nil || len(step.Tools) != 1 || step.Tools[0].CalleeRef != "callee.helper" || step.Tools[0].Tool != "" {
		t.Fatalf("bare whole-surface grant not compiled: %+v", step)
	}
	return g
}

// TestNestedToolCall_BareTargetRoutesContractViaArgs: a call to the bare
// whole-surface target adapter.callee.helper.tools carries the tool name as
// the args routing key. Pre-fix, the seam resolved the contract from the
// target's tool segment and the request tool field only — both empty here —
// so the call skipped the typed contract entirely and executed with bad
// arguments. Post-fix the args["tool"] fallback routes the contract and the
// violating args are rejected typed invalid_args before the callee runs.
func TestNestedToolCall_BareTargetRoutesContractViaArgs(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: bareCallTarget, args: map[string]any{
		"tool": "helper_task",
		"task": 6,
		"bad":  1,
	}}
	callee := &contractCalleeAdapter{&nestedCalleeAdapter{rec: calleeRec}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	graph := compileNestedToolCallBareGraph(t)
	sm.SetGraph(graph)
	ctx := openFixtureSessions(t, sm, graph)
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	if _, err := sm.Execute(ctx, nestedCallerSession, graph.Steps["call"], inner, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.RequestId != "call-1" || tcr.CallError != callErrorInvalidArgs || tcr.Outcome != "" {
		t.Fatalf("tool_call_result = %+v, want invalid_args reply", tcr)
	}
	if got := calleeRec.calleeSession(); got != "" {
		t.Errorf("callee executed for rejected args (session %q)", got)
	}
	for _, needle := range []string{
		`payload_schema: property "task": expected "string", got "number"`,
		`payload_schema: property "bad": undeclared property`,
	} {
		if !contractIssuesContains(t, audit, needle) {
			t.Errorf("audit reason missing %q; entries = %+v", needle, audit.all())
		}
	}
}

// TestNestedToolCall_BareTargetNoRoutingKeyPassthrough preserves the
// no-routing-key posture: a bare-target call whose args name no tool has no
// contract to route, so nothing is declared at the seam and the call stays
// untyped — the args pass through unvalidated and the callee runs, exactly
// like the pre-KB-59 seam.
func TestNestedToolCall_BareTargetNoRoutingKeyPassthrough(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	caller := &nestedCallerAdapter{target: bareCallTarget, args: map[string]any{
		"task":  "do-thing",
		"ghost": "boo",
	}}
	callee := &contractCalleeAdapter{&nestedCalleeAdapter{rec: calleeRec}}
	sm := newNestedToolCallManager(t, caller, callee)
	sm.Audit = audit

	graph := compileNestedToolCallBareGraph(t)
	sm.SetGraph(graph)
	ctx := openFixtureSessions(t, sm, graph)
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, nestedCallerSession, graph.Steps["call"], inner, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("caller outcome = %q, want success", res.Outcome)
	}

	tcr := caller.gotResult()
	if tcr == nil || tcr.CallError != "" || tcr.Outcome != "success" {
		t.Fatalf("tool_call_result = %+v, want success/no error", tcr)
	}
	if got := calleeRec.calleeSession(); got != nestedCalleeSession {
		t.Fatalf("callee executed in session %q, want %q", got, nestedCalleeSession)
	}
	step := calleeRec.calleeStep()
	if step == nil {
		t.Fatal("callee never recorded a step")
	}
	if got := step.Input["task"]; got != "do-thing" {
		t.Errorf("callee input task = %q, want do-thing", got)
	}
	if got, ok := step.Input["ghost"]; !ok || got != "boo" {
		t.Errorf("callee input ghost = %q/%v, want boo/unchecked passthrough", got, ok)
	}
}

// TestNestedToolCall_ContractInputArgs_DynamicRootSkipped pins the seam's
// defensive guard: validateContractInputArgs skips (never panics on
// AttributeTypes()) when the contract leaks an unconstrained root or any
// non-object type, even though compileToolContractSide now rejects such
// contracts at compile time.
func TestNestedToolCall_ContractInputArgs_DynamicRootSkipped(t *testing.T) {
	for name, contract := range map[string]*workflow.ToolContract{
		"unconstrained": {InType: cty.DynamicPseudoType, InSchemaJSON: []byte(`{}`)},
		"non-object":    {InType: cty.String, InSchemaJSON: []byte(`{"type":"string"}`)},
		"nil-type":      {InSchemaJSON: []byte(`{}`)},
	} {
		issues := validateContractInputArgs(contract, map[string]any{
			"tool":  "helper_task",
			"ghost": "boo",
			"task":  6,
		})
		if issues != nil {
			t.Errorf("%s: want no issues (contract skipped), got %v", name, issues)
		}
	}
}

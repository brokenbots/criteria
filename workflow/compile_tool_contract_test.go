package workflow

// compile_tool_contract_test.go — typed tool contracts on adapter tool
// blocks (KB-59, MCP-probe Gap 2 seam): `in = type.<name>` / `out =
// type.<name>` on a tool declaration resolve through the shared type
// namespace (KB-45) and drive (a) seam validation of nested tool-call
// arguments and surfaced outputs (engine tests) and (b) compile-time input{}
// validation for direct step targets of schema-less adapters
// (validateStepInputToolContract). Adapters without contracts compile
// byte-identically.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"

	"github.com/zclconf/go-cty/cty"
)

// TestKB59_ToolContractCompile_NamedInOut verifies in/out contract sides
// resolve named types, produce schema JSON on both sides, and carry the
// diagnostic location.
func TestKB59_ToolContractCompile_NamedInOut(t *testing.T) {
	g, diags := compileTypedToolWorkflow(t, contractWorkflowWithInOut("type.probe_request", "type.probe_response"))
	if g == nil {
		t.Fatalf("compile failed: %s", diags.Error())
	}
	node := g.Adapters["mcp.probe"]
	contract, ok := node.ToolContractFor("echo")
	if !ok {
		t.Fatalf("tool contract for echo not compiled: %v", node.ToolContracts)
	}
	wantIn := cty.ObjectWithOptionalAttrs(map[string]cty.Type{"tool": cty.String, "message": cty.String}, []string{"message"})
	wantOut := cty.Object(map[string]cty.Type{"report": cty.String})
	if !contract.InType.Equals(wantIn) {
		t.Errorf("InType = %s; want object({tool: string, message: string})", contract.InType.FriendlyName())
	}
	if !contract.OutType.Equals(wantOut) {
		t.Errorf("OutType = %s; want object({report: string})", contract.OutType.FriendlyName())
	}
	if len(contract.InSchemaJSON) == 0 || len(contract.OutSchemaJSON) == 0 {
		t.Errorf("schema JSON missing: in=%s out=%s", contract.InSchemaJSON, contract.OutSchemaJSON)
	}
	if !bytes.Contains(contract.InSchemaJSON, []byte(`"tool"`)) || !bytes.Contains(contract.InSchemaJSON, []byte(`"message"`)) {
		t.Errorf("InSchemaJSON lacks declared properties: %s", contract.InSchemaJSON)
	}
	if contract.Loc != `adapter "mcp.probe" tool "echo"` {
		t.Errorf("Loc = %q", contract.Loc)
	}
}

// TestKB59_ToolContractCompile_InlineTwin verifies the byte-equality
// property shared with the type namespace: a tool contract authored inline
// and one referencing the named type produce identical schema bytes.
func TestKB59_ToolContractCompile_InlineTwin(t *testing.T) {
	compileOne := func(in, out string) ToolContract {
		t.Helper()
		g, diags := compileTypedToolWorkflow(t, contractWorkflowWithInOut(in, out))
		if g == nil {
			t.Fatalf("compile failed: %s", diags.Error())
		}
		return g.Adapters["mcp.probe"].ToolContracts["echo"]
	}
	inline := compileOne(`object({tool = string, message = optional(string)})`, `object({report = string})`)
	named := compileOne("type.probe_request", "type.probe_response")
	if string(inline.InSchemaJSON) != string(named.InSchemaJSON) {
		t.Errorf("in schema bytes differ inline vs named:\n inline: %s\n named:  %s", inline.InSchemaJSON, named.InSchemaJSON)
	}
	if string(inline.OutSchemaJSON) != string(named.OutSchemaJSON) {
		t.Errorf("out schema bytes differ inline vs named:\n inline: %s\n named:  %s", inline.OutSchemaJSON, named.OutSchemaJSON)
	}
}

// TestKB59_ToolContractCompile_UnknownTypeRef verifies a named ref to an
// undeclared type is a compile error at the tool-contract position.
func TestKB59_ToolContractCompile_UnknownTypeRef(t *testing.T) {
	_, diags := compileErrToolWorkflow(t, contractWorkflowWithInOut("type.noprobe", "type.probe_response"))
	if !strings.Contains(diags.Error(), `tool "echo": in: unknown workflow type "noprobe"`) {
		t.Errorf("expected unknown workflow type diagnostic at the tool contract, got: %s", diags.Error())
	}
}

// TestKB59_ToolContractCompile_NonObjectType verifies the payload gate: a
// non-object constraint can never be satisfied by a tool-call payload, so it
// is rejected.
func TestKB59_ToolContractCompile_NonObjectType(t *testing.T) {
	_, diags := compileErrToolWorkflow(t, contractWorkflowWithInOut("list(string)", "type.probe_response"))
	if !strings.Contains(diags.Error(), `tool "echo": in must be an object(...) type constraint; got list of string`) {
		t.Errorf("expected non-object contract diagnostic, got: %s", diags.Error())
	}
	_, diags2 := compileErrToolWorkflow(t, contractWorkflowWithInOut("type.probe_request", "string"))
	if !strings.Contains(diags2.Error(), `tool "echo": out must be an object(...) type constraint; got string`) {
		t.Errorf("expected non-object out-contract diagnostic, got: %s", diags2.Error())
	}
}

// TestKB59_ToolContractCompile_ExtraAttrsRejected mirrors the type-block
// posture: tool blocks accept only the contract sides plus reserved body.
func TestKB59_ToolContractCompile_ExtraAttrsRejected(t *testing.T) {
	_, diags := compileErrToolWorkflow(t, `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" {
    in      = object({tool = string})
    garbage = true
  }
}
step "a" {
  target = adapter.mcp.probe
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if !strings.Contains(diags.Error(), `tool "echo": unsupported attribute "garbage"`) {
		t.Errorf("expected unsupported-attribute diagnostic on the tool block, got: %s", diags.Error())
	}
}

// TestKB59_ToolContractCompile_DuplicateTool verifies duplicate tool names on
// one adapter are rejected (bodies used to be discarded silently).
func TestKB59_ToolContractCompile_DuplicateTool(t *testing.T) {
	_, diags := compileErrToolWorkflow(t, `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state  = "a"
  target_state   = "done"
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" { }
  tool "echo" { }
}
step "a" {
  target = adapter.mcp.probe
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if !strings.Contains(diags.Error(), `adapter "mcp.probe": duplicate tool "echo"`) {
		t.Errorf("expected duplicate tool diagnostic, got: %s", diags.Error())
	}
}

// TestKB59_ToolContractCompile_NoContractUnaffected verifies the
// byte-identical rule: tool blocks without in/out compile exactly as before
// KB-59 (no entry in ToolContracts, StaticTools unchanged).
func TestKB59_ToolContractCompile_NoContractUnaffected(t *testing.T) {
	g, diags := compileTypedToolWorkflow(t, `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
adapter "noop" "callee" {
  tool "echo_data" { }
}
step "a" {
  target = adapter.noop.callee
  tools = [adapter.noop.callee.tools.echo_data]
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if g == nil {
		t.Fatalf("compile failed: %s", diags.Error())
	}
	node := g.Adapters["noop.callee"]
	if node.HasToolContracts() {
		t.Errorf("tool blocks without in/out must not register contracts: %v", node.ToolContracts)
	}
	if len(node.StaticTools) != 1 || node.StaticTools[0] != "echo_data" {
		t.Errorf("StaticTools = %v; want [echo_data]", node.StaticTools)
	}
}

// TestKB59_DirectTargetTypedInput_GoldenDiagnostics verifies the
// direct-mcp-target input path: with a declared in-type the static
// unknown-field probe is bypassed in favor of the contract (unknown keys are
// now REJECTED against the type), wrong-typed values fail, and missing
// required fields fail with the schema path's vocabulary.
func TestKB59_DirectTargetTypedInput_GoldenDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		want string
		src  string
	}{
		{
			name: "unknown key rejected against the declared type",
			want: `step "probe" input: unknown field "ghost"`,
			src: directTargetTypedInputSource(`
    tool    = "echo"
    message = "ping"
    ghost   = true
`),
		},
		{
			name: "wrong-typed value rejected",
			want: `step "probe" input: field "message": string required`,
			src: directTargetTypedInputSource(`
    tool    = "echo"
    message = 12345
`),
		},
		{
			name: "missing required field rejected",
			want: `step "probe" input: required field "tool" is missing`,
			src: directTargetTypedInputSource(`
    message = "ping"
`),
		},
		{
			name: "uncontracted tool passes with extra keys",
			want: "",
			src: `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "probe"
  target_state  = "done"
}
type "probe_request" {
  schema = object({
    tool = string
  })
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" {
    in = type.probe_request
  }
  tool "other" { }
}
step "probe" {
  target = adapter.mcp.probe
  input {
    tool  = "other"
    loose = "keys pass for uncontracted tools"
  }
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == "" {
				g, diags := compileTypedToolWorkflow(t, tc.src)
				if g == nil {
					t.Fatalf("compile failed: %s", diags.Error())
				}
				return
			}
			_, diags := compileErrToolWorkflow(t, tc.src)
			if !strings.Contains(diags.Error(), tc.want) {
				t.Errorf("diagnostics missing %q:\n%s", tc.want, diags.Error())
			}
		})
	}
}

// TestKB59_DirectTargetTypedInput_SchemaFulUnaffected verifies the
// handshake-first posture is untouched for schema-ful adapters: input
// validation still uses the adapter handshake, and the tool contract adds
// no compile-time input check there.
func TestKB59_DirectTargetTypedInput_SchemaFulUnaffected(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"noop": {
			InputSchema: map[string]ConfigField{
				"tool":     {Type: ConfigFieldString},
				"nonsense": {Type: ConfigFieldNumber},
			},
		},
	}
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
adapter "noop" "callee" {
  tool "echo" {
    in = object({tool = string, message = string})
  }
}
step "a" {
  target = adapter.noop.callee
  input {
    tool     = "echo"
    nonsense = 12345
  }
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, pdiags := Parse("t.hcl", []byte(src))
	if pdiags.HasErrors() {
		t.Fatalf("parse: %s", pdiags.Error())
	}
	if _, diags := Compile(spec, schemas); diags.HasErrors() {
		t.Fatalf("schema-ful adapter input must validate via the handshake only: %s", diags.Error())
	}
}

// TestKB59_DirectTargetTypedInput_DynamicToolRefUnverified verifies that a
// dynamically-routed tool reference (non-literal input.tool) stays unchecked
// at compile time even on a contract-bearing adapter: it is validated by the
// callee at runtime.
func TestKB59_DirectTargetTypedInput_DynamicToolRefUnverified(t *testing.T) {
	g, diags := compileTypedToolWorkflow(t, `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "probe"
  target_state  = "done"
}
type "probe_request" {
  schema = object({
    tool    = string
    message = optional(string)
  })
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" {
    in = type.probe_request
  }
}
variable "route" {
  type = string
}
step "probe" {
  target = adapter.mcp.probe
  input {
    tool  = var.route
    ghost = true
  }
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if g == nil {
		t.Fatalf("compile failed: %s", diags.Error())
	}
}

// TestKB59_DirectTargetTypedInput_ObjectAttr verifies a structured input
// attribute (e.g. tool_args) validates against its declared object type and
// folds into the string map by JSON encoding, exactly like the permissive
// decode.
func TestKB59_DirectTargetTypedInput_ObjectAttr(t *testing.T) {
	g, diags := compileTypedToolWorkflow(t, `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" {
    in = object({tool = string, tool_args = object({message = string})})
  }
}
step "a" {
  target = adapter.mcp.probe
  input {
    tool      = "echo"
    tool_args = { message = "ping" }
  }
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if g == nil {
		t.Fatalf("compile failed: %s", diags.Error())
	}
	step := g.Steps["a"]
	if step.Input["tool"] != "echo" {
		t.Errorf("tool input = %q; want echo", step.Input["tool"])
	}
	got := step.Input["tool_args"]
	if !strings.Contains(got, `"message"`) || !strings.Contains(got, `"ping"`) {
		t.Errorf("tool_args input = %q; want JSON-encoded object", got)
	}
}

// TestKB59_DirectTargetTypedInput_UnknownVarDeferred verifies that a
// required attribute whose value is unknown at compile time (a step input
// referencing a var with no default) is not rejected as missing — presence
// in the input block is authoritative, mirroring the schema path's deferred
// placeholder handling.
func TestKB59_DirectTargetTypedInput_UnknownVarDeferred(t *testing.T) {
	g, diags := compileTypedToolWorkflow(t, `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "probe"
  target_state  = "done"
}
type "probe_request" {
  schema = object({
    tool    = string
    message = string
  })
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" {
    in = type.probe_request
  }
}
variable "runtime_message" {
  type = string
}
step "probe" {
  target = adapter.mcp.probe
  input {
    tool    = "echo"
    message = var.runtime_message
  }
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`)
	if g == nil {
		t.Fatalf("compile failed: %s", diags.Error())
	}
}

// directTargetTypedInputSource builds the direct-target workflow for the
// golden-diagnostics table with a parametrized input block body.
func directTargetTypedInputSource(inputBody string) string {
	return `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "probe"
  target_state  = "done"
}
type "probe_request" {
  schema = object({
    tool    = string
    message = optional(string)
  })
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" {
    in = type.probe_request
  }
}
step "probe" {
  target = adapter.mcp.probe
  input {` + inputBody + `
  }
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`
}

// contractWorkflowWithInOut builds a probe workflow whose tool contract sides
// are parametrized for constraint-resolution diagnostics.
func contractWorkflowWithInOut(in, out string) string {
	return `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "probe"
  target_state  = "done"
}
type "probe_request" {
  schema = object({
    tool    = string
    message = optional(string)
  })
}
type "probe_response" {
  schema = object({
    report = string
  })
}
adapter "mcp" "probe" {
  dynamic_tools = true
  tool "echo" {
    in  = ` + in + `
    out = ` + out + `
  }
}
step "probe" {
  target = adapter.mcp.probe
  input {
    tool    = "echo"
    message = "ping"
  }
  outcome "success" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`
}

// compileTypedToolWorkflow parses and compiles a workflow expected to
// succeed, failing the test on diagnostics.
func compileTypedToolWorkflow(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	return g, diags
}

// compileErrToolWorkflow compiles a workflow expected to fail, returning the
// aggregated diagnostics for substring assertions.
func compileErrToolWorkflow(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatalf("expected compile diagnostics, got none")
	}
	return g, diags
}

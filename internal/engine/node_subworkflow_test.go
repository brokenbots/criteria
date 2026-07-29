package engine

// node_subworkflow_test.go — unit tests for runSubworkflow (W13, Phase 3).
//
// These tests verify the runtime entry point without requiring W14's step
// target wiring. They call runSubworkflow directly and assert on the returned
// output map (matching the workstream-specified signature).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// minimalSubworkflowNode builds a SubworkflowNode with the simplest possible
// callee FSMGraph: a single terminal state and no declared outputs.
func minimalSubworkflowNode(name string) *workflow.SubworkflowNode {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Variables: map[string]*workflow.VariableNode{},
	}
	return &workflow.SubworkflowNode{
		Name:         name,
		SourcePath:   "/test/" + name,
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
}

// traversalExpr builds an hcl.Expression for a dotted traversal like "var.x"
// or "each.value" without requiring HCL text parsing.
func traversalExpr(root string, attrs ...string) hcl.Expression {
	t := hcl.Traversal{hcl.TraverseRoot{Name: root}}
	for _, a := range attrs {
		t = append(t, hcl.TraverseAttr{Name: a})
	}
	return &hclsyntax.ScopeTraversalExpr{Traversal: t}
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	sessions := adapterhost.NewSessionManager(adapterhost.NewLoader())
	t.Cleanup(func() { sessions.Shutdown(context.Background()) })
	return Deps{
		Sessions: sessions,
		Sink:     &fakeSink{},
	}
}

// TestRunSubworkflow_ReachesTerminalState verifies that runSubworkflow executes
// a minimal callee FSMGraph to completion without error, returning nil outputs
// when no output blocks are declared.
func TestRunSubworkflow_ReachesTerminalState(t *testing.T) {
	node := minimalSubworkflowNode("simple")
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	if len(outputs) != 0 {
		t.Errorf("expected no outputs, got %v", outputs)
	}
}

// TestRunSubworkflow_OutputsEvaluated verifies that a callee's declared output
// expressions are evaluated against the final child state and returned to the
// caller. This is the core of Step 6's runtime contract.
func TestRunSubworkflow_OutputsEvaluated(t *testing.T) {
	// Callee declares a literal output: output "status" { value = "ok" }
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Outputs: map[string]*workflow.OutputNode{
			"status": {Name: "status", Value: &hclsyntax.LiteralValueExpr{Val: cty.StringVal("ok")}},
		},
		OutputOrder: []string{"status"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "status-test",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["status"]
	if !ok {
		t.Fatal("output 'status' not present in returned map")
	}
	if got.AsString() != "ok" {
		t.Errorf("output 'status': want %q, got %q", "ok", got.AsString())
	}
}

// TestRunSubworkflow_InputBoundToOutput verifies the full data-flow path:
// parent input expression → callee var.* → callee output → returned output map.
func TestRunSubworkflow_InputBoundToOutput(t *testing.T) {
	// Callee: variable "greeting" (no default) + output "result" = var.greeting
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{"greeting": {Name: "greeting", Type: cty.String}},
		Outputs: map[string]*workflow.OutputNode{
			"result": {Name: "result", Value: traversalExpr("var", "greeting")},
		},
		OutputOrder: []string{"result"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "greeter",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{"greeting": &hclsyntax.LiteralValueExpr{Val: cty.StringVal("hello")}},
		DeclaredVars: map[string]*workflow.VariableNode{"greeting": {Name: "greeting", Type: cty.String}},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["result"]
	if !ok {
		t.Fatal("output 'result' not present")
	}
	if got.AsString() != "hello" {
		t.Errorf("output 'result': want %q, got %q", "hello", got.AsString())
	}
}

// TestRunSubworkflow_ComplexInputConverted verifies that real HCL expressions
// in a subworkflow input block are converted to the child's declared variable
// type. A parent tuple([string,string]) becomes a child list(string), and a
// parent object becomes the child object type.
func TestRunSubworkflow_ComplexInputConverted(t *testing.T) {
	listVarType := cty.List(cty.String)
	mapVarType := cty.Map(cty.String)
	objVarType := cty.Object(map[string]cty.Type{
		"enabled": cty.Bool,
		"retries": cty.Number,
	})

	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables: map[string]*workflow.VariableNode{
			"tags":   {Name: "tags", Type: listVarType},
			"labels": {Name: "labels", Type: mapVarType},
			"config": {Name: "config", Type: objVarType},
		},
		Outputs: map[string]*workflow.OutputNode{
			"tags":   {Name: "tags", Value: traversalExpr("var", "tags")},
			"labels": {Name: "labels", Value: traversalExpr("var", "labels")},
			"config": {Name: "config", Value: traversalExpr("var", "config")},
		},
		OutputOrder: []string{"tags", "labels", "config"},
	}

	node := &workflow.SubworkflowNode{
		Name:      "complex-input",
		Body:      body,
		BodyEntry: "done",
		Inputs: map[string]hcl.Expression{
			"tags":   parseExpr(t, `["a", "b"]`),
			"labels": parseExpr(t, `{env = "prod"}`),
			"config": parseExpr(t, `{enabled = true, retries = 3}`),
		},
		DeclaredVars: map[string]*workflow.VariableNode{
			"tags":   {Name: "tags", Type: listVarType},
			"labels": {Name: "labels", Type: mapVarType},
			"config": {Name: "config", Type: objVarType},
		},
	}

	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}

	tags := outputs["tags"]
	if !tags.Type().IsListType() {
		t.Errorf("tags type = %s, want list", tags.Type().FriendlyName())
	}
	wantList := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	if !tags.RawEquals(wantList) {
		t.Errorf("tags output = %#v, want %#v", tags, wantList)
	}

	labels := outputs["labels"]
	if !labels.Type().IsMapType() {
		t.Errorf("labels type = %s, want map", labels.Type().FriendlyName())
	}
	wantMap := cty.MapVal(map[string]cty.Value{"env": cty.StringVal("prod")})
	if !labels.RawEquals(wantMap) {
		t.Errorf("labels output = %#v, want %#v", labels, wantMap)
	}

	cfg := outputs["config"]
	if !cfg.Type().IsObjectType() {
		t.Errorf("config type = %s, want object", cfg.Type().FriendlyName())
	}
	wantObj := cty.ObjectVal(map[string]cty.Value{
		"enabled": cty.True,
		"retries": cty.NumberIntVal(3),
	})
	if !cfg.RawEquals(wantObj) {
		t.Errorf("config output = %#v, want %#v", cfg, wantObj)
	}
}

// TestRunSubworkflow_ObjectOptionalDefaultsApplied verifies that object
// variables with optional() attributes in the child workflow receive their
// declared defaults when the parent input omits those attributes.
func TestRunSubworkflow_ObjectOptionalDefaultsApplied(t *testing.T) {
	objType := cty.Object(map[string]cty.Type{
		"a": cty.String,
		"b": cty.String,
	})

	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables: map[string]*workflow.VariableNode{
			"cfg": {
				Name:         "cfg",
				Type:         objType,
				TypeDefaults: nil, // set below after construction
			},
		},
		Outputs: map[string]*workflow.OutputNode{
			"cfg": {Name: "cfg", Value: traversalExpr("var", "cfg")},
		},
		OutputOrder: []string{"cfg"},
	}

	// Build real type defaults for object({a=string, b=optional(string,"x")}).
	expr := parseExpr(t, `object({ a = string, b = optional(string, "x") })`)
	typ, defs, typeDiags := typeexpr.TypeConstraintWithDefaults(expr)
	if typeDiags.HasErrors() {
		t.Fatalf("resolve type: %s", typeDiags.Error())
	}
	body.Variables["cfg"].Type = typ
	body.Variables["cfg"].TypeDefaults = defs

	node := &workflow.SubworkflowNode{
		Name:      "optional-defaults",
		Body:      body,
		BodyEntry: "done",
		Inputs: map[string]hcl.Expression{
			"cfg": parseExpr(t, `{a = "1"}`),
		},
		DeclaredVars: map[string]*workflow.VariableNode{
			"cfg": {
				Name:         "cfg",
				Type:         typ,
				TypeDefaults: defs,
			},
		},
	}

	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}

	cfg := outputs["cfg"]
	if got := cfg.GetAttr("a").AsString(); got != "1" {
		t.Errorf("cfg.a = %q, want 1", got)
	}
	if got := cfg.GetAttr("b").AsString(); got != "x" {
		t.Errorf("cfg.b = %q, want x", got)
	}
}

// TestRunSubworkflow_NullInputFallsBackToDefault verifies that a null parent
// binding for a child variable that has a declared default leaves the child
// with that default.
func TestRunSubworkflow_NullInputFallsBackToDefault(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables: map[string]*workflow.VariableNode{
			"greeting": {
				Name:    "greeting",
				Type:    cty.String,
				Default: cty.StringVal("hello"),
			},
		},
		Outputs: map[string]*workflow.OutputNode{
			"result": {Name: "result", Value: traversalExpr("var", "greeting")},
		},
		OutputOrder: []string{"result"},
	}
	node := &workflow.SubworkflowNode{
		Name:      "null-default",
		Body:      body,
		BodyEntry: "done",
		Inputs: map[string]hcl.Expression{
			"greeting": &hclsyntax.LiteralValueExpr{Val: cty.NullVal(cty.String)},
		},
		DeclaredVars: map[string]*workflow.VariableNode{
			"greeting": {Name: "greeting", Type: cty.String, Default: cty.StringVal("hello")},
		},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got := outputs["result"]
	if got.AsString() != "hello" {
		t.Errorf("output 'result': want %q, got %q", "hello", got.AsString())
	}
}

// TestRunSubworkflow_NullRequiredInputReportsMissing verifies that a null parent
// binding for a required child variable is caught by checkRequiredVars and
// reported as a missing required input, not as a conversion error.
func TestRunSubworkflow_NullRequiredInputReportsMissing(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables: map[string]*workflow.VariableNode{
			"greeting": {Name: "greeting", Type: cty.String},
		},
	}
	node := &workflow.SubworkflowNode{
		Name:      "null-required",
		Body:      body,
		BodyEntry: "done",
		Inputs: map[string]hcl.Expression{
			"greeting": &hclsyntax.LiteralValueExpr{Val: cty.NullVal(cty.String)},
		},
		DeclaredVars: map[string]*workflow.VariableNode{
			"greeting": {Name: "greeting", Type: cty.String},
		},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err == nil {
		t.Fatal("expected error for null required input")
	}
	if !strings.Contains(err.Error(), "required input(s): greeting") {
		t.Errorf("error = %q, want required input(s): greeting", err.Error())
	}
}

// TestRunSubworkflow_StringBindingToNumberRejectsGarbage verifies that a string
// parent binding passed to a child number variable is converted strictly (via
// convert.Convert) and rejects trailing garbage instead of the lenient Sscanf
// used for raw CLI values.
func TestRunSubworkflow_StringBindingToNumberRejectsGarbage(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables: map[string]*workflow.VariableNode{
			"retries": {Name: "retries", Type: cty.Number},
		},
	}
	node := &workflow.SubworkflowNode{
		Name:      "string-to-number",
		Body:      body,
		BodyEntry: "done",
		Inputs: map[string]hcl.Expression{
			"retries": &hclsyntax.LiteralValueExpr{Val: cty.StringVal("3abc")},
		},
		DeclaredVars: map[string]*workflow.VariableNode{
			"retries": {Name: "retries", Type: cty.Number},
		},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err == nil {
		t.Fatal("expected error for invalid number string")
	}
	if !strings.Contains(err.Error(), "subworkflow input \"retries\"") {
		t.Errorf("error = %q, want it to name subworkflow input retries", err.Error())
	}
}

// TestRunSubworkflow_EachThreadedToOutput verifies that each.* from the parent
// scope is visible inside the subworkflow and can be captured via an output.
func TestRunSubworkflow_EachThreadedToOutput(t *testing.T) {
	// Callee has output "item" = each.value
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Outputs: map[string]*workflow.OutputNode{
			"item": {Name: "item", Value: traversalExpr("each", "value")},
		},
		OutputOrder: []string{"item"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "each-test",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	parentSt := &RunState{
		Vars: map[string]cty.Value{
			"var": cty.EmptyObjectVal,
			"each": cty.ObjectVal(map[string]cty.Value{
				"value": cty.StringVal("item-x"),
				"_idx":  cty.NumberIntVal(0),
			}),
		},
		WorkflowDir: t.TempDir(),
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["item"]
	if !ok {
		t.Fatal("output 'item' not present")
	}
	if got.AsString() != "item-x" {
		t.Errorf("output 'item': want %q, got %q", "item-x", got.AsString())
	}
}

// TestRunSubworkflow_MissingRequiredInput verifies that a missing required input
// variable produces a descriptive error (not a panic).
func TestRunSubworkflow_MissingRequiredInput(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{"required_var": {Name: "required_var", Type: cty.String}},
	}
	node := &workflow.SubworkflowNode{
		Name:         "missing-input",
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{"required_var": {Name: "required_var", Type: cty.String}},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err == nil {
		t.Fatal("expected error for missing required input, got none")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention 'required', got: %v", err)
	}
}

// TestRunSubworkflow_FileFromCalleeDir is a regression test that verifies the
// callee's runtime functions resolve relative paths against the subworkflow's
// source directory (node.SourcePath), not the parent workflow directory.
//
// A subworkflow with output "msg" { value = file("msg.txt") } should succeed
// when msg.txt exists in the subworkflow directory even if the parent workflow
// lives in a completely different directory.
func TestRunSubworkflow_FileFromCalleeDir(t *testing.T) {
	calleeDir := t.TempDir()
	parentDir := t.TempDir()

	// Write msg.txt only in the callee directory, not in the parent directory.
	msgPath := filepath.Join(calleeDir, "msg.txt")
	if err := os.WriteFile(msgPath, []byte("hello from callee"), 0o600); err != nil {
		t.Fatalf("write msg.txt: %v", err)
	}

	// Build a file("msg.txt") expression via HCL parsing.
	fileExpr, diags := hclsyntax.ParseExpression([]byte(`file("msg.txt")`), "test", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parse file expr: %s", diags.Error())
	}

	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Outputs: map[string]*workflow.OutputNode{
			"msg": {Name: "msg", Value: fileExpr},
		},
		OutputOrder: []string{"msg"},
	}
	node := &workflow.SubworkflowNode{
		Name:         "file-test",
		SourcePath:   calleeDir,
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	// Parent lives in a separate directory — msg.txt does NOT exist there.
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: parentDir,
	}

	outputs, _, err := runSubworkflow(context.Background(), node, parentSt, nil, testDeps(t))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	got, ok := outputs["msg"]
	if !ok {
		t.Fatal("output 'msg' not present")
	}
	if got.AsString() != "hello from callee" {
		t.Errorf("output 'msg': want %q, got %q", "hello from callee", got.AsString())
	}
}

// ctxCheckAdapter is a test adapter whose Execute returns ctx.Err() immediately
// when the context is already cancelled, allowing deterministic cancellation tests.
type ctxCheckAdapter struct {
	fakeAdapter
}

func (p *ctxCheckAdapter) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if err := ctx.Err(); err != nil {
		return adapter.Result{}, err
	}
	return adapter.Result{Outcome: "success"}, nil
}

// calleeBodyWithAdapter builds a callee FSMGraph that declares a single adapter
// and has an immediate terminal state. Adapter lifecycle (open/close) happens
// regardless of whether any step uses the adapter.
func calleeBodyWithAdapter(adapterType string) *workflow.FSMGraph {
	instanceID := adapterType + ".default"
	return &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Adapters:     map[string]*workflow.AdapterNode{instanceID: {Type: adapterType, Name: "default"}},
		AdapterOrder: []string{instanceID},
		Policy:       workflow.DefaultPolicy,
	}
}

// calleeBodyWithStep builds a callee FSMGraph with a single step that uses an
// adapter. The step transitions to terminal state on "success" outcome.
func calleeBodyWithStep(adapterType string) *workflow.FSMGraph {
	instanceID := adapterType + ".default"
	return &workflow.FSMGraph{
		InitialState: "work",
		Steps: map[string]*workflow.StepNode{
			"work": {
				Name:       "work",
				TargetKind: workflow.StepTargetAdapter,
				AdapterRef: instanceID,
				Outcomes: map[string]*workflow.CompiledOutcome{
					"success": {Next: "done"},
				},
			},
		},
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Adapters:     map[string]*workflow.AdapterNode{instanceID: {Type: adapterType, Name: "default"}},
		AdapterOrder: []string{instanceID},
		Policy:       workflow.DefaultPolicy,
	}
}

// subworkflowNodeFor wraps a body FSMGraph in a SubworkflowNode.
func subworkflowNodeFor(name string, body *workflow.FSMGraph) *workflow.SubworkflowNode {
	return &workflow.SubworkflowNode{
		Name:         name,
		SourcePath:   "/test/" + name,
		Body:         body,
		BodyEntry:    body.InitialState,
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
}

// depsWithLoader builds a Deps whose SessionManager uses the given loader.
func depsWithLoader(t *testing.T, loader adapterhost.Loader) Deps {
	t.Helper()
	sessions := adapterhost.NewSessionManager(loader)
	t.Cleanup(func() { sessions.Shutdown(context.Background()) })
	return Deps{Sessions: sessions, Sink: &fakeSink{}}
}

// TestRunSubworkflow_AdaptersIsolatedFromParent verifies that a callee-declared
// adapter is opened at the start of the subworkflow scope and closed when it
// returns — proving that adapter lifecycle is fully contained within the
// subworkflow and does not leak into the parent scope.
//
// A broken teardown (missing deferred tearDownScopeAdapters) would leave
// closes==0 after runSubworkflow returns, failing the test.
func TestRunSubworkflow_AdaptersIsolatedFromParent(t *testing.T) {
	tracker := &lifecycleTrackingAdapter{fakeAdapter: fakeAdapter{name: "noop"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": tracker}}

	node := subworkflowNodeFor("isolated", calleeBodyWithAdapter("noop"))
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, _, err := runSubworkflow(context.Background(), node, parentSt, nil, depsWithLoader(t, loader))
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}

	tracker.mu.Lock()
	opens := tracker.opensCount
	closes := tracker.closesCount
	tracker.mu.Unlock()

	if opens != 1 {
		t.Errorf("callee adapter opens: want 1, got %d", opens)
	}
	if closes != 1 {
		t.Errorf("callee adapter closes: want 1, got %d (adapter leaked past subworkflow boundary)", closes)
	}
}

// TestRunSubworkflow_ErrorPropagatesToParent verifies that a runtime failure
// inside the callee (adapter Execute returning an error) propagates back to
// the caller of runSubworkflow rather than being silently swallowed.
//
// A broken implementation that converts callee errors to empty/nil outputs
// without returning an error would fail this test.
func TestRunSubworkflow_ErrorPropagatesToParent(t *testing.T) {
	errAdapter := &fakeAdapter{name: "noop", err: fmt.Errorf("simulated step failure")}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": errAdapter}}

	node := subworkflowNodeFor("fail-test", calleeBodyWithStep("noop"))
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	_, _, err := runSubworkflow(context.Background(), node, parentSt, nil, depsWithLoader(t, loader))
	if err == nil {
		t.Fatal("expected error from failing callee step, got nil")
	}
	if !strings.Contains(err.Error(), "simulated step failure") {
		t.Errorf("error should contain step failure message, got: %v", err)
	}
}

// TestRunSubworkflow_CalleeCancellation verifies that cancelling the context
// while the callee is executing causes runSubworkflow to return a
// context-related error rather than completing normally.
//
// A broken implementation that ignored ctx would execute to completion and
// return nil error, failing this test.
func TestRunSubworkflow_CalleeCancellation(t *testing.T) {
	checkPlugin := &ctxCheckAdapter{fakeAdapter: fakeAdapter{name: "noop"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": checkPlugin}}

	node := subworkflowNodeFor("cancel-test", calleeBodyWithStep("noop"))
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	// Pre-cancel the context: ctxCheckAdapter.Execute returns ctx.Err() immediately,
	// which propagates up through runWorkflowBody as a non-terminal error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := runSubworkflow(ctx, node, parentSt, nil, depsWithLoader(t, loader))
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error should mention context cancellation, got: %v", err)
	}
}

// TestRunSubworkflow_ReturnSentinelWithNilOutputs verifies that when a
// subworkflow exits via next = step.return with no output projection, runSubworkflow
// returns (nil, nil) rather than falling through to evalRunOutputsAsValues.
// Prior to the fix, `_ = terminal` and `if returnOutputs != nil` caused the
// nil-output return path to silently evaluate the callee's output blocks instead.
func TestRunSubworkflow_ReturnSentinelWithNilOutputs(t *testing.T) {
	// Callee: single step with next = step.return but no output = {...} projection.
	// The callee also declares an output block so we can detect a fall-through:
	// if evalRunOutputsAsValues is called it would populate "leaked" in the output.
	returnStep := &workflow.StepNode{
		Name:       "inner",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "fake.default",
		Input:      map[string]string{},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success", Next: workflow.ReturnSentinel},
		},
	}
	calleeGraph := &workflow.FSMGraph{
		Name:         "callee",
		InitialState: "inner",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"inner": returnStep},
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Adapters:     map[string]*workflow.AdapterNode{"fake.default": {Type: "fake", Name: "default"}},
		AdapterOrder: []string{"fake.default"},
		Subworkflows: map[string]*workflow.SubworkflowNode{},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}
	swNode := &workflow.SubworkflowNode{Name: "callee", Body: calleeGraph}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         &fakeAdapter{name: "fake", outcome: "success"},
		"fake.default": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{},
		WorkflowDir: t.TempDir(),
	}
	deps := depsWithLoader(t, loader)

	outputs, _, err := runSubworkflow(context.Background(), swNode, parentSt, nil, deps)
	if err != nil {
		t.Fatalf("runSubworkflow: %v", err)
	}
	// Nil outputs is the correct result for a no-projection return.
	if outputs != nil {
		t.Errorf("expected nil outputs on no-projection return, got %v", outputs)
	}
}

// TestRunSubworkflow_NullStringOutput verifies that a subworkflow whose declared
// output evaluates to a null string does not panic during the parent step's
// cty-to-string conversion. The null guard in evaluateSubworkflowStep causes the
// value to fall through to renderCtyValue, which returns "null".
func TestRunSubworkflow_NullStringOutput(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Outputs: map[string]*workflow.OutputNode{
			"result": {Name: "result", Value: &hclsyntax.LiteralValueExpr{Val: cty.NullVal(cty.String)}},
		},
		OutputOrder: []string{"result"},
	}
	swNode := &workflow.SubworkflowNode{
		Name:         "null-out",
		SourcePath:   t.TempDir(),
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}

	g := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "null-out",
				Outcomes:       map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Subworkflows: map[string]*workflow.SubworkflowNode{"null-out": swNode},
		Variables:    map[string]*workflow.VariableNode{},
	}

	sink := &captureOutputSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
	sink.mu.Lock()
	got := sink.outputs["call"]
	sink.mu.Unlock()
	if got == nil {
		t.Fatal("step 'call' outputs not captured")
	}
	if got["result"] != "null" {
		t.Errorf("null string output rendered: want %q, got %q", "null", got["result"])
	}
}

// TestRunSubworkflow_TerminalStateFailure verifies that when a subworkflow reaches
// a terminal state with success=false, the parent step receives outcome="failure"
// and routes accordingly.
func TestRunSubworkflow_TerminalStateFailure(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "fail",
		States:       map[string]*workflow.StateNode{"fail": {Name: "fail", Terminal: true, Success: false}},
	}
	swNode := &workflow.SubworkflowNode{
		Name:         "failing-callee",
		SourcePath:   t.TempDir(),
		Body:         body,
		BodyEntry:    "fail",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}

	g := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "failing-callee",
				Outcomes: map[string]*workflow.CompiledOutcome{
					"success": {Next: "done"},
					"failure": {Next: "failed"},
				},
			},
		},
		States: map[string]*workflow.StateNode{
			"done":   {Name: "done", Terminal: true, Success: true},
			"failed": {Name: "failed", Terminal: true, Success: false},
		},
		Subworkflows: map[string]*workflow.SubworkflowNode{"failing-callee": swNode},
		Variables:    map[string]*workflow.VariableNode{},
	}

	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want failed/false", sink.terminal, sink.terminalOK)
	}
}

// TestRunSubworkflow_TerminalStateSuccess is a regression guard that verifies a
// subworkflow reaching a success=true terminal state still produces outcome="success"
// in the parent step.
func TestRunSubworkflow_TerminalStateSuccess(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
	}
	swNode := &workflow.SubworkflowNode{
		Name:         "happy-callee",
		SourcePath:   t.TempDir(),
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}

	g := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "happy-callee",
				Outcomes: map[string]*workflow.CompiledOutcome{
					"success": {Next: "done"},
					"failure": {Next: "failed"},
				},
			},
		},
		States: map[string]*workflow.StateNode{
			"done":   {Name: "done", Terminal: true, Success: true},
			"failed": {Name: "failed", Terminal: true, Success: false},
		},
		Subworkflows: map[string]*workflow.SubworkflowNode{"happy-callee": swNode},
		Variables:    map[string]*workflow.VariableNode{},
	}

	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestMergeLockfiles verifies the mergeLockfiles helper that unions two lockfiles,
// with overlay entries taking precedence on (type, name) collisions.
func TestMergeLockfiles(t *testing.T) {
	baseAdapter := lockfile.LockedAdapter{
		Type:           "shell",
		Name:           "main",
		ResolvedDigest: "aaa",
		Reference:      "oci://example.com/shell:1",
		SourceURL:      "https://example.com/shell",
	}
	overlayAdapter := lockfile.LockedAdapter{
		Type:           "claude-agent",
		Name:           "reviewer",
		ResolvedDigest: "bbb",
		Reference:      "oci://example.com/claude-agent:1",
		SourceURL:      "https://example.com/claude-agent",
	}
	conflictingAdapter := lockfile.LockedAdapter{
		Type:           "shell",
		Name:           "main",
		ResolvedDigest: "bbb", // overlay wins with digest "bbb"
		Reference:      "oci://example.com/shell:2",
		SourceURL:      "https://example.com/shell",
	}

	t.Run("nil_base", func(t *testing.T) {
		overlay := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{overlayAdapter},
		}
		got := mergeLockfiles(nil, overlay)
		if got != overlay {
			t.Errorf("nil base: expected overlay returned unchanged")
		}
	})

	t.Run("nil_overlay", func(t *testing.T) {
		base := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{baseAdapter},
		}
		// The function only nil-checks base, not overlay. Passing nil overlay currently
		// panics on overlay.Adapters field access. Use defer/recover so this subtest
		// fails gracefully rather than crashing the test binary.
		var got *lockfile.Lockfile
		panicked := func() (panicked bool) {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			got = mergeLockfiles(base, nil)
			return false
		}()
		if panicked {
			t.Error("nil overlay: mergeLockfiles(base, nil) panicked — the function should handle nil overlay without panicking")
			return
		}
		if got == nil {
			t.Error("nil overlay: expected non-nil result")
		}
	})

	t.Run("overlay_wins", func(t *testing.T) {
		base := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{baseAdapter},
		}
		overlay := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{overlayAdapter},
		}
		got := mergeLockfiles(base, overlay)
		if got == nil {
			t.Fatal("expected non-nil merged lockfile")
		}
		if len(got.Adapters) != 2 {
			t.Errorf("expected 2 adapters in merged lockfile, got %d: %v", len(got.Adapters), got.Adapters)
		}
		// Both adapters should be present.
		types := make(map[string]string) // "type.name" -> digest
		for _, a := range got.Adapters {
			types[a.Type+"."+a.Name] = a.ResolvedDigest
		}
		if _, ok := types["shell.main"]; !ok {
			t.Error("shell.main missing from merged lockfile")
		}
		if _, ok := types["claude-agent.reviewer"]; !ok {
			t.Error("claude-agent.reviewer missing from merged lockfile")
		}
	})

	t.Run("overlay_replaces_same_key", func(t *testing.T) {
		base := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{baseAdapter},
		}
		overlay := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{conflictingAdapter},
		}
		got := mergeLockfiles(base, overlay)
		if got == nil {
			t.Fatal("expected non-nil merged lockfile")
		}
		if len(got.Adapters) != 1 {
			t.Errorf("expected exactly 1 adapter (overlay replaces base), got %d: %v", len(got.Adapters), got.Adapters)
		}
		if got.Adapters[0].ResolvedDigest != "bbb" {
			t.Errorf("expected overlay digest %q, got %q", "bbb", got.Adapters[0].ResolvedDigest)
		}
	})
}

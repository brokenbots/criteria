package engine

import (
"testing"

"github.com/hashicorp/hcl/v2"
"github.com/zclconf/go-cty/cty"

"github.com/brokenbots/criteria/workflow"
)

// TestEvalRunOutputs_StepOutputAccessible tests that output expressions can access step outputs.
func TestEvalRunOutputs_StepOutputAccessible(t *testing.T) {
g := &workflow.FSMGraph{
Outputs:     make(map[string]*workflow.OutputNode),
OutputOrder: []string{},
}

st := &RunState{
Vars: map[string]cty.Value{
"steps": cty.ObjectVal(map[string]cty.Value{
"my_step": cty.ObjectVal(map[string]cty.Value{
"output_field": cty.StringVal("hello"),
}),
}),
},
}

val := cty.StringVal("hello from step")
expr := hcl.StaticExpr(val, hcl.Range{})

g.Outputs["result"] = &workflow.OutputNode{
Name:         "result",
Description:  "step output",
DeclaredType: cty.String,
Value:        expr,
}
g.OutputOrder = append(g.OutputOrder, "result")

outputs, err := evalRunOutputs(g, st)
if err != nil {
t.Fatalf("evalRunOutputs failed: %v", err)
}

if len(outputs) != 1 {
t.Fatalf("expected 1 output, got %d", len(outputs))
}

if outputs[0]["name"] != "result" {
t.Fatalf("expected output name 'result', got %q", outputs[0]["name"])
}

if outputs[0]["declared_type"] != "string" {
t.Fatalf("expected declared_type 'string', got %q", outputs[0]["declared_type"])
}
}

// TestEvalRunOutputs_TypeMismatch tests that runtime type mismatches produce descriptive errors.
func TestEvalRunOutputs_TypeMismatch(t *testing.T) {
g := &workflow.FSMGraph{
Outputs:     make(map[string]*workflow.OutputNode),
OutputOrder: []string{},
}

st := &RunState{
Vars: make(map[string]cty.Value),
}

// Use a map which cannot be converted to string - a genuine mismatch
val := cty.MapVal(map[string]cty.Value{"key": cty.StringVal("value")})
expr := hcl.StaticExpr(val, hcl.Range{})

g.Outputs["typed_output"] = &workflow.OutputNode{
Name:         "typed_output",
Description:  "mismatched type",
DeclaredType: cty.String,
Value:        expr,
}
g.OutputOrder = append(g.OutputOrder, "typed_output")

_, err := evalRunOutputs(g, st)
if err == nil {
t.Fatal("expected type mismatch error, got none")
}

errStr := err.Error()
if !contains(errStr, "typed_output") {
t.Fatalf("expected error to mention output name 'typed_output', got: %s", errStr)
}
if !contains(errStr, "string") {
t.Fatalf("expected error to mention declared type 'string', got: %s", errStr)
}
if !contains(errStr, "map") {
t.Fatalf("expected error to mention actual type 'map', got: %s", errStr)
}
}

// TestEvalRunOutputs_EmptyOutputs tests that runs with no outputs return empty list.
func TestEvalRunOutputs_EmptyOutputs(t *testing.T) {
g := &workflow.FSMGraph{
Outputs:     make(map[string]*workflow.OutputNode),
OutputOrder: []string{},
}

st := &RunState{
Vars: make(map[string]cty.Value),
}

outputs, err := evalRunOutputs(g, st)
if err != nil {
t.Fatalf("evalRunOutputs failed for empty outputs: %v", err)
}

if outputs != nil {
t.Fatalf("expected nil for empty outputs, got %v", outputs)
}
}

// TestEvalRunOutputs_TypeCoercion tests that compatible types are coerced (tuple -> list).
func TestEvalRunOutputs_TypeCoercion(t *testing.T) {
g := &workflow.FSMGraph{
Outputs:     make(map[string]*workflow.OutputNode),
OutputOrder: []string{},
}

st := &RunState{
Vars: make(map[string]cty.Value),
}

tupleVal := cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
expr := hcl.StaticExpr(tupleVal, hcl.Range{})

g.Outputs["list_output"] = &workflow.OutputNode{
Name:         "list_output",
Description:  "tuple to list coercion",
DeclaredType: cty.List(cty.String),
Value:        expr,
}
g.OutputOrder = append(g.OutputOrder, "list_output")

outputs, err := evalRunOutputs(g, st)
if err != nil {
t.Fatalf("evalRunOutputs failed for type coercion: %v", err)
}

if len(outputs) != 1 {
t.Fatalf("expected 1 output, got %d", len(outputs))
}

if outputs[0]["name"] != "list_output" {
t.Fatalf("expected output name 'list_output', got %q", outputs[0]["name"])
}
}

func contains(s, substr string) bool {
for i := 0; i <= len(s)-len(substr); i++ {
if s[i:i+len(substr)] == substr {
return true
}
}
return false
}

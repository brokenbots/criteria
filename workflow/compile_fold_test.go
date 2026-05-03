package workflow_test

// compile_fold_test.go — unit tests for FoldExpr in compile_fold.go.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// parseExpr is a test helper that parses a single HCL expression from src.
func parseExpr(t *testing.T, src string) hcl.Expression {
	t.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parseExpr(%q): %s", src, diags.Error())
	}
	return expr
}

func TestFoldExpr_PureLiteral(t *testing.T) {
	expr := parseExpr(t, `"hello"`)
	val, foldable, diags := workflow.FoldExpr(expr, nil, nil, "")
	if !foldable {
		t.Fatal("expected foldable=true for a string literal")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val.Type() != cty.String || val.AsString() != "hello" {
		t.Errorf("expected cty.StringVal(\"hello\"), got %s", val.GoString())
	}
}

func TestFoldExpr_VarReference_Resolved(t *testing.T) {
	expr := parseExpr(t, "var.x")
	vars := map[string]cty.Value{"x": cty.NumberIntVal(42)}
	val, foldable, diags := workflow.FoldExpr(expr, vars, nil, "")
	if !foldable {
		t.Fatal("expected foldable=true for var.x with known vars")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val.Type() != cty.Number {
		t.Errorf("expected cty.Number, got %s", val.Type().FriendlyName())
	}
}

func TestFoldExpr_VarReference_Missing(t *testing.T) {
	expr := parseExpr(t, "var.does_not_exist")
	val, foldable, diags := workflow.FoldExpr(expr, nil, nil, "")
	if !foldable {
		t.Fatal("expected foldable=true (var.* is not a runtime-only namespace)")
	}
	_ = val
	if !diags.HasErrors() {
		t.Fatal("expected an error diagnostic for unknown var; got none")
	}
}

func TestFoldExpr_LocalReference_Resolved(t *testing.T) {
	expr := parseExpr(t, "local.greeting")
	locals := map[string]cty.Value{"greeting": cty.StringVal("hi")}
	val, foldable, diags := workflow.FoldExpr(expr, nil, locals, "")
	if !foldable {
		t.Fatal("expected foldable=true for local.greeting with known locals")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val.Type() != cty.String || val.AsString() != "hi" {
		t.Errorf("expected cty.StringVal(\"hi\"), got %s", val.GoString())
	}
}

func TestFoldExpr_RuntimeOnly_StepsRef(t *testing.T) {
	expr := parseExpr(t, "steps.foo.out")
	val, foldable, diags := workflow.FoldExpr(expr, nil, nil, "")
	if foldable {
		t.Fatal("expected foldable=false for steps.* reference")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val != cty.NilVal {
		t.Errorf("expected cty.NilVal for runtime-deferred expression, got %s", val.GoString())
	}
}

func TestFoldExpr_RuntimeOnly_EachRef(t *testing.T) {
	expr := parseExpr(t, "each.value")
	val, foldable, diags := workflow.FoldExpr(expr, nil, nil, "")
	if foldable {
		t.Fatal("expected foldable=false for each.* reference")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val != cty.NilVal {
		t.Errorf("expected cty.NilVal for runtime-deferred expression, got %s", val.GoString())
	}
}

func TestFoldExpr_FileFunc_Literal_Resolves(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expr := parseExpr(t, `file("fixture.txt")`)
	val, foldable, diags := workflow.FoldExpr(expr, nil, nil, dir)
	if !foldable {
		t.Fatal("expected foldable=true for file(literal)")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val.Type() != cty.String {
		t.Errorf("expected string, got %s", val.Type().FriendlyName())
	}
}

func TestFoldExpr_FileFunc_VarPath_Resolves(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expr := parseExpr(t, "file(var.path)")
	vars := map[string]cty.Value{"path": cty.StringVal("prompt.txt")}
	val, foldable, diags := workflow.FoldExpr(expr, vars, nil, dir)
	if !foldable {
		t.Fatal("expected foldable=true for file(var.path) with known var")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val.Type() != cty.String {
		t.Errorf("expected string, got %s", val.Type().FriendlyName())
	}
}

func TestFoldExpr_FileFunc_VarPath_Missing(t *testing.T) {
	dir := t.TempDir()
	// "nope.txt" does not exist in dir.
	expr := parseExpr(t, "file(var.path)")
	vars := map[string]cty.Value{"path": cty.StringVal("nope.txt")}
	_, foldable, diags := workflow.FoldExpr(expr, vars, nil, dir)
	if !foldable {
		t.Fatal("expected foldable=true (var.path is a fold-time reference)")
	}
	if !diags.HasErrors() {
		t.Fatal("expected a file-not-found diagnostic; got none")
	}
}

func TestFoldExpr_FileFunc_RuntimeRef_Skipped(t *testing.T) {
	dir := t.TempDir()
	expr := parseExpr(t, "file(steps.foo.path)")
	val, foldable, diags := workflow.FoldExpr(expr, nil, nil, dir)
	if foldable {
		t.Fatal("expected foldable=false for file(steps.*) — steps.* is runtime-only")
	}
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostic: %s", diags.Error())
	}
	if val != cty.NilVal {
		t.Errorf("expected cty.NilVal for runtime-deferred expression, got %s", val.GoString())
	}
}

// TestFoldExpr_VarNoDefault_FileCall_NoError verifies that file(var.path) where
// var.path has no default (cty.UnknownVal) does not produce a compile error.
// The expression is foldable (var is declared) but path validation is deferred
// because the argument is unknown at compile time.
func TestFoldExpr_VarNoDefault_FileCall_NoError(t *testing.T) {
dir := t.TempDir()
expr := parseExpr(t, "file(var.path)")
// var.path has no default — should be represented as UnknownVal, not NullVal.
vars := map[string]cty.Value{"path": cty.UnknownVal(cty.String)}
_, foldable, diags := workflow.FoldExpr(expr, vars, nil, dir)
if !foldable {
t.Fatal("expected foldable=true for file(var.path) with declared (but unknown) var")
}
if diags.HasErrors() {
t.Fatalf("expected no error for file(unknown_var); got: %s", diags.Error())
}
}

// TestFoldExpr_NoWorkflowDir_LiteralFile_NoError verifies that file("path")
// with workflowDir="" does not error — the function is stubbed to return
// unknown so var/local reference checks can still run.
func TestFoldExpr_NoWorkflowDir_LiteralFile_NoError(t *testing.T) {
expr := parseExpr(t, `file("any_path.txt")`)
_, foldable, diags := workflow.FoldExpr(expr, nil, nil, "")
if !foldable {
t.Fatal("expected foldable=true for file() with literal string")
}
if diags.HasErrors() {
t.Fatalf("expected no error when workflowDir is empty; got: %s", diags.Error())
}
}

// TestFoldExpr_NoWorkflowDir_UndeclaredVar_StillErrors verifies that even when
// workflowDir is "", a reference to an undeclared var still produces an error.
func TestFoldExpr_NoWorkflowDir_UndeclaredVar_StillErrors(t *testing.T) {
expr := parseExpr(t, "var.does_not_exist")
_, foldable, diags := workflow.FoldExpr(expr, nil, nil, "")
if !foldable {
t.Fatal("expected foldable=true (var.* is a fold-time namespace)")
}
if !diags.HasErrors() {
t.Fatal("expected a compile error for undeclared var reference; got none")
}
}

package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compile_fold_typed_test.go — KB-266 acceptance fixtures.
//
// Compile-time deferral exists for VALUE evaluation; it must not waive TYPE
// checking. The fixtures reproduce the live defect tree shape (KB-240 lesson:
// the exact collision): one number-typed variable piped through shellquote()
// inside a templatefile() var binding, ALONGSIDE a data.* reference in the
// SAME step input expression. The data.* ref makes FoldExpr defer the whole
// expression (runtime-only namespace); the typed fold and the template
// placeholder render must still surface the type mismatch as a compile error
// at the expression site.

// kb266Workflow builds a minimal workflow around the step input expression
// with a variable and an internal data block present. A prior step "pri"
// (declared, but with no adapter output contract in these fixtures) lets
// tests exercise the steps.* binding surface.
func kb266Workflow(varBlock, dataBlock, inputExpr string) string {
	return `workflow {
  name = "kb266"
  version       = "0.1"
  initial_state = "pri"
  target_state  = "done"
}
` + varBlock + `
` + dataBlock + `
adapter "noop" "default" {
  config {}
}

step "pri" {
  target = adapter.noop.default
  outcome "success" { next = step.work }
}

step "work" {
  target = adapter.noop.default
  input {
    cmd = ` + inputExpr + `
  }
  outcome "success" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`
}

const kb266NumberVar = `
variable "port" {
  type = number
}
`

const kb266StringVar = `
variable "name" {
  type = string
}
`

const kb266DataStr = `
data "internal" "cfg" {
  type = string
  value = "prod"
}
`

// TestKB266_ShellquoteNumberThroughDeferredTemplatefile reproduces the live
// defect: kb-264-1792105840 — a number-typed variable piped into shellquote()
// (templatefile var binding) inside a step input that also binds a data.*
// member. The data.* reference defers the whole expression, so the type
// mismatch previously surfaced at arm execution ("expected string; got
// int64"). Compile must fail at the expression site with a diagnostic naming
// the operation, the variable, and the type mismatch.
func TestKB266_ShellquoteNumberThroughDeferredTemplatefile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy.tmpl"), []byte(`region {{ shellquote .port }} from {{ .env }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hclContent := kb266Workflow(
		kb266NumberVar,
		kb266DataStr,
		`templatefile("./deploy.tmpl", { port = var.port, env = data.internal.cfg.value })`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatalf("expected compile error for number variable through shellquote, got none: %v", diags)
	}
	msg := diags.Error()
	// The diagnostic names the binding-site variable with its declared type
	// and the static type mismatch (wrong type for value; expected string;
	// got int64) via the template execution context <.port>.
	for _, want := range []string{"port [number]", "wrong type for value", "expected string", "int64"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic should contain %q; got: %s", want, msg)
		}
	}
}

// TestKB266_TypeCorrectDeferredTemplatefileCompilesClean proves the unknown
// propagation path: the same tree shape with a type-correct string variable
// keeps deferring (renders at runtime) and compiles clean.
func TestKB266_TypeCorrectDeferredTemplatefileCompilesClean(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy.tmpl"), []byte(`region {{ shellquote .name }} from {{ .env }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hclContent := kb266Workflow(
		kb266StringVar,
		kb266DataStr,
		`templatefile("./deploy.tmpl", { name = var.name, env = data.internal.cfg.value })`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("type-correct deferred expression must compile clean: %s", diags.Error())
	}
}

// TestKB266_DynamicOperandStaysDeferred proves the skip path: an operand with
// no declared type (the legacy 4-segment steps.X.outputs.Y form, bound
// dynamic) sharing an expression with a data.* ref stays runtime-deferred and
// compiles with no error.
func TestKB266_DynamicOperandStaysDeferred(t *testing.T) {
	dir := t.TempDir()
	hclContent := kb266Workflow(
		kb266StringVar,
		kb266DataStr,
		`join(",", [var.name, data.internal.cfg.value, steps.pri.outputs.result])`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("fully-dynamic operand must stay runtime-deferred, not a compile error: %s", diags.Error())
	}
}

// TestKB266_DynamicOperandInTemplatefileVarStaysDeferred proves the skip path
// through the templatefile placeholder render: a dynamic unknown among the
// template vars aborts the placeholder render and keeps the expression
// deferred without a compile error.
func TestKB266_DynamicOperandInTemplatefileVarStaysDeferred(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy.tmpl"), []byte(`{{ .a }} {{ .b }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hclContent := kb266Workflow(
		kb266StringVar,
		kb266DataStr,
		`templatefile("./deploy.tmpl", { a = var.name, b = steps.pri.outputs.result })`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("dynamic template var must stay runtime-deferred: %s", diags.Error())
	}
}

// TestKB266_FoldLevelFunctionParamTypeMismatch checks the non-template fold
// path: a number-typed variable passed to a registered function whose
// parameter declares list(string) is a compile error at the expression site —
// the class of the runtime "expected string; got int64" failure, moved to
// compile. (number→string is a safe cty conversion, so the mismatch is
// exercised through an impossible conversion: number into list(string).)
func TestKB266_FoldLevelFunctionParamTypeMismatch(t *testing.T) {
	dir := t.TempDir()
	hclContent := kb266Workflow(
		kb266NumberVar,
		kb266DataStr,
		`join("-", var.port)`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatalf("expected compile error for number variable passed to join([string]), got none")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "Invalid function argument") {
		t.Fatalf("diagnostic should be the function-parameter type check; got: %s", msg)
	}
}

// TestKB266_FoldLevelTypeCorrectCompilesClean proves a type-correct deferred
// expression folds to unknown and compiles clean (no value evaluation).
func TestKB266_FoldLevelTypeCorrectCompilesClean(t *testing.T) {
	dir := t.TempDir()
	hclContent := kb266Workflow(
		kb266StringVar,
		kb266DataStr,
		`join("-", [var.name, data.internal.cfg.type])`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("type-correct deferred expression must compile clean: %s", diags.Error())
	}
}

// TestKB266_TupleVarRealDiagnosticNotPanic is the review regression: a
// heterogeneous list literal among the templatefile vars (tuple element
// iteration) plus a failing placeholder render must surface the real
// execute error. cty's tuple ElementIterator yields Number keys; calling
// k.AsString() on them panicked ("panic in function implementation: not a
// string"), replacing the diagnostic with a panic error that named neither
// the binding site, the operation, nor the mismatch.
func TestKB266_TupleVarRealDiagnosticNotPanic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy.tmpl"), []byte(`{{ shellquote .port }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hclContent := kb266Workflow(
		kb266NumberVar,
		kb266DataStr,
		`templatefile("./deploy.tmpl", { args = [var.port, "x"], port = var.port })`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatalf("expected compile error for number variable through shellquote, got none: %v", diags)
	}
	msg := diags.Error()
	if strings.Contains(msg, "panic") {
		t.Fatalf("diagnostic must be the real execute error, never a panic; got: %s", msg)
	}
	for _, want := range []string{"wrong type for value", "expected string", "int64"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic should contain %q; got: %s", want, msg)
		}
	}
}

// TestKB266_PathCompositionCompilesClean pins the path.* binding contract:
// path.workflow/root/cwd bind unknown-of-string, so path-composed filesystem
// args keep deferring and compile clean. (Known-wrong-dir bindings used to
// evaluate file() at compile and falsely reject the composed absolute path —
// the same error the runtime raises; unknown args short-circuit before Impl
// and the render stays runtime-owned.)
func TestKB266_PathCompositionCompilesClean(t *testing.T) {
	dir := t.TempDir()
	hclContent := kb266Workflow(
		kb266StringVar,
		kb266DataStr,
		`file(format("%s/sub/deploy.tmpl", path.workflow))`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("path-composed file() in a deferred input must compile clean: %s", diags.Error())
	}
}

// TestKB266_NumberVarThroughShellquoteWithoutDataRef pins that the pure
// templatefile placeholder render (no data.* ref at all) also rejects the
// type mismatch — the same failure that previously surfaced at arm execution.
func TestKB266_NumberVarThroughShellquoteWithoutDataRef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy.tmpl"), []byte(`region {{ shellquote .port }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hclContent := kb266Workflow(
		kb266NumberVar,
		``,
		`templatefile("./deploy.tmpl", { port = var.port })`,
	)
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatalf("expected compile error for number variable through shellquote (no data ref), got none")
	}
	msg := diags.Error()
	for _, want := range []string{"port [number]", "wrong type for value", "expected string", "int64"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic should contain %q; got: %s", want, msg)
		}
	}
}

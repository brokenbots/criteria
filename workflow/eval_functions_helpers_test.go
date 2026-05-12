package workflow_test

// eval_functions_helpers_test.go — shared helpers for hash/encoding/dynamic
// function tests.

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/brokenbots/criteria/workflow"
)

// funcFromContext retrieves a named function registered in the workflow eval
// context (no workflowDir; default options).
func funcFromContext(t *testing.T, name string) function.Function {
	t.Helper()
	ctx := workflow.BuildEvalContextWithOpts(nil, workflow.FunctionOptions{})
	fn, ok := ctx.Functions[name]
	if !ok {
		t.Fatalf("function %q not registered in workflow eval context", name)
	}
	return fn
}

// callFn invokes a cty function with the given args, failing the test on error.
func callFn(t *testing.T, fn function.Function, args ...cty.Value) cty.Value {
	t.Helper()
	v, err := fn.Call(args)
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	return v
}

// callFnError invokes a cty function and returns the error, failing the test
// if the call unexpectedly succeeds.
func callFnError(t *testing.T, fn function.Function, args ...cty.Value) error {
	t.Helper()
	_, err := fn.Call(args)
	if err == nil {
		t.Fatal("expected error; got none")
	}
	return err
}

// Package testdata provides sample eval definitions for spec-gen unit tests.
// This file intentionally mirrors the namespace-binding shape of workflow/eval.go.
package testdata

type testVal struct{}

func ctyObjectVal(m map[string]testVal) testVal { return testVal{} }

// BuildEvalContextWithOpts builds the test eval context.
func BuildEvalContextWithOpts() {
	ctxVars := map[string]testVal{
		"alpha": {},
		"beta":  {},
	}
	ctxVars["each"] = testVal{}
	_ = ctxVars
}

// WithEachBinding populates per-iteration bindings.
func WithEachBinding() {
	newVars := map[string]testVal{}
	newVars["each"] = ctyObjectVal(map[string]testVal{
		"item": {},
		"pos":  {},
	})
	_ = newVars
}

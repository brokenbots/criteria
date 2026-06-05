package adapterhost

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// benchEventSink discards all adapter events during benchmarks.
type benchEventSink struct{}

func (benchEventSink) Log(string, []byte)  {}
func (benchEventSink) Adapter(string, any) {}

var _ adapter.EventSink = benchEventSink{}

// noopAdapter is an in-process adapter that returns "success" immediately
// without spawning any subprocess. It is the canonical in-process builtin used
// to measure pure adapter-dispatch overhead. (Real adapters — including shell —
// run out-of-process via discovery; their cost is dominated by subprocess spawn
// and is not meaningful to micro-benchmark here.)
type noopAdapter struct{}

func (noopAdapter) Name() string               { return "noop" }
func (noopAdapter) Info() workflow.AdapterInfo { return workflow.AdapterInfo{} }
func (noopAdapter) Execute(_ context.Context, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "success"}, nil
}

// noopStep returns a minimal adapter step for the noop builtin.
func noopStep(name string) *workflow.StepNode {
	return &workflow.StepNode{
		Name:       name,
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "noop",
		Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
	}
}

// BenchmarkBuiltinAdapter_Execute measures the per-step dispatch cost
// (OpenSession → Execute → CloseSession) through an in-process builtin adapter.
func BenchmarkBuiltinAdapter_Execute(b *testing.B) {
	factory := BuiltinFactoryForAdapter(noopAdapter{})
	ctx := context.Background()
	step := noopStep("bench-step")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := factory()
		if err := p.OpenSession(ctx, "sess", nil, nil); err != nil {
			b.Fatalf("OpenSession: %v", err)
		}
		if _, err := p.Execute(ctx, "sess", step, benchEventSink{}); err != nil {
			b.Fatalf("Execute: %v", err)
		}
		if err := p.CloseSession(ctx, "sess"); err != nil {
			b.Fatalf("CloseSession: %v", err)
		}
	}
}

// BenchmarkAdapterExecuteNoop measures pure Execute throughput with an
// in-process noop adapter. The session is opened once before b.ResetTimer()
// so each iteration measures only dispatch overhead without session setup cost.
func BenchmarkAdapterExecuteNoop(b *testing.B) {
	factory := BuiltinFactoryForAdapter(noopAdapter{})
	ctx := context.Background()
	step := noopStep("noop-step")
	p := factory()
	if err := p.OpenSession(ctx, "sess", nil, nil); err != nil {
		b.Fatalf("OpenSession: %v", err)
	}
	b.Cleanup(func() { _ = p.CloseSession(ctx, "sess") })
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := p.Execute(ctx, "sess", step, benchEventSink{}); err != nil {
			b.Fatalf("Execute: %v", err)
		}
	}
}

// BenchmarkBuiltinAdapter_Info measures the Info() call overhead — called
// during schema collection before every workflow execution.
func BenchmarkBuiltinAdapter_Info(b *testing.B) {
	factory := BuiltinFactoryForAdapter(noopAdapter{})
	p := factory()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := p.Info(ctx); err != nil {
			b.Fatalf("Info: %v", err)
		}
	}
}

// BenchmarkLoaderResolveBuiltin measures how long it takes to resolve a
// builtin adapter from the DefaultLoader.
func BenchmarkLoaderResolveBuiltin(b *testing.B) {
	loader := NewLoader()
	loader.RegisterBuiltin("noop", BuiltinFactoryForAdapter(noopAdapter{}))
	ctx := context.Background()
	b.Cleanup(func() { _ = loader.Shutdown(ctx) })
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := loader.Resolve(ctx, "noop"); err != nil {
			b.Fatalf("Resolve: %v", err)
		}
	}
}

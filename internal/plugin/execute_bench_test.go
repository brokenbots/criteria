package plugin

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/workflow"
)

// benchEventSink discards all plugin events during benchmarks.
type benchEventSink struct{}

func (benchEventSink) Log(string, []byte)  {}
func (benchEventSink) Adapter(string, any) {}

var _ adapter.EventSink = benchEventSink{}

// noopAdapter is an in-process adapter that returns "success" immediately
// without spawning any subprocess. It lets BenchmarkPluginExecuteNoop
// measure pure plugin-dispatch overhead.
type noopAdapter struct{}

func (noopAdapter) Name() string               { return "noop" }
func (noopAdapter) Info() workflow.AdapterInfo { return workflow.AdapterInfo{} }
func (noopAdapter) Execute(_ context.Context, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "success"}, nil
}

// minimalStep returns a minimal StepNode for the shell adapter that runs a
// no-op command (true(1)) so process spawn dominates, not command duration.
func minimalStep(name string) *workflow.StepNode {
	return &workflow.StepNode{
		Name:     name,
		Adapter:  "shell",
		Input:    map[string]string{"command": "true"},
		Outcomes: map[string]string{"success": "done", "failure": "done"},
	}
}

// BenchmarkBuiltinPlugin_Execute measures the full per-step dispatch cost
// (OpenSession → Execute → CloseSession) through the shell builtin plugin.
// Subprocess spawn dominates the ~18 ms cost.
func BenchmarkBuiltinPlugin_Execute(b *testing.B) {
	factory := BuiltinFactoryForAdapter(shell.New())
	ctx := context.Background()
	step := minimalStep("bench-step")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := factory()
		if err := p.OpenSession(ctx, "sess", nil); err != nil {
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

// BenchmarkPluginExecuteNoop measures pure Execute throughput with an
// in-process noop adapter. The session is opened once before b.ResetTimer()
// so each iteration measures only dispatch overhead without session setup cost.
func BenchmarkPluginExecuteNoop(b *testing.B) {
	factory := BuiltinFactoryForAdapter(noopAdapter{})
	ctx := context.Background()
	step := &workflow.StepNode{
		Name:     "noop-step",
		Adapter:  "noop",
		Outcomes: map[string]string{"success": "done"},
	}
	p := factory()
	if err := p.OpenSession(ctx, "sess", nil); err != nil {
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

// BenchmarkBuiltinPlugin_Info measures the Info() call overhead — called
// during schema collection before every workflow execution.
func BenchmarkBuiltinPlugin_Info(b *testing.B) {
	factory := BuiltinFactoryForAdapter(shell.New())
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
	loader.RegisterBuiltin(shell.Name, BuiltinFactoryForAdapter(shell.New()))
	ctx := context.Background()
	b.Cleanup(func() { _ = loader.Shutdown(ctx) })
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := loader.Resolve(ctx, shell.Name); err != nil {
			b.Fatalf("Resolve: %v", err)
		}
	}
}

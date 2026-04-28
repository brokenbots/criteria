package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/workflow"
)

// benchEventSink discards all plugin events during benchmarks.
type benchEventSink struct{}

func (benchEventSink) Log(string, []byte)  {}
func (benchEventSink) Adapter(string, any) {}

var _ adapter.EventSink = benchEventSink{}

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

// BenchmarkBuiltinPlugin_Execute measures the overhead of invoking the shell
// adapter through the builtin plugin wrapper (OpenSession → Execute → CloseSession).
// This captures the full per-step plugin dispatch cost in local mode.
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

// BenchmarkBuiltinPlugin_Info measures the Info() call overhead — this is
// called during schema collection before every workflow execution.
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

// Ensure benchEventSink satisfies the interface at compile time (already done
// via var _ above; this constant keeps the import of "time" used in the
// interface method signature check below used elsewhere in this file).
var _ = time.Second

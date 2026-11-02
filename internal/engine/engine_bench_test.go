package engine

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

// benchSink discards all engine events so it does not perturb benchmark timing.
type benchSink struct{}

func (benchSink) OnCheckpointPointer(*CheckpointPointerEvent)                   {}
func (benchSink) OnRunStarted(string, string)                                   {}
func (benchSink) OnRunCompleted(string, bool)                                   {}
func (benchSink) OnRunFailed(string, string)                                    {}
func (benchSink) OnRunPaused(string, string, string)                            {}
func (benchSink) OnRunResumed(string)                                           {}
func (benchSink) OnStepEntered(string, string, int)                             {}
func (benchSink) OnStepOutcome(_, _ string, _ time.Duration, _ error, _ string) {}
func (benchSink) OnStepTransition(string, string, string)                       {}
func (benchSink) OnStepResumed(string, int, string)                             {}
func (benchSink) OnVariableSet(string, string, string)                          {}
func (benchSink) OnStepOutputCaptured(string, map[string]string)                {}
func (benchSink) OnWaitEntered(string, string, string, string)                  {}
func (benchSink) OnWaitResumed(string, string, string, map[string]string)       {}
func (benchSink) OnApprovalRequested(string, []string, string)                  {}
func (benchSink) OnApprovalDecision(string, string, string, map[string]string)  {}
func (benchSink) OnBranchEvaluated(string, string, string, string)              {}
func (benchSink) OnForEachEntered(string, int)                                  {}
func (benchSink) OnStepIterationStarted(string, int, string, bool)              {}
func (benchSink) OnStepIterationCompleted(string, string, string)               {}
func (benchSink) OnStepIterationItem(string, int, string)                       {}
func (benchSink) OnScopeIterCursorSet(string)                                   {}
func (benchSink) OnAdapterLifecycle(string, string, string, string)             {}
func (benchSink) OnAdapterLifecycleEvent(*AdapterLifecycleEvent)                {}
func (benchSink) OnRunOutputs([]map[string]string)                              {}
func (benchSink) OnStepOutcomeDefaulted(string, string, string)                 {}
func (benchSink) OnStepOutcomeUnknown(string, string)                           {}
func (benchSink) OnStepOutcomeInvalid(string, string, []string, int)            {}

func (benchSink) OnAgentPromptInjected(string, string, string, string, time.Time) {}
func (benchSink) StepEventSink(string) adapter.EventSink                          { return benchEventSink{} }

type benchEventSink struct{}

func (benchEventSink) Log(string, []byte)  {}
func (benchEventSink) Adapter(string, any) {}

// buildNStepWorkflow compiles an in-memory workflow with n sequential steps,
// each using adapter "fake" and always succeeding.
func buildNStepWorkflow(b *testing.B, n int) *workflow.FSMGraph {
	b.Helper()
	hcl := workflowtest.Linear(n,
		workflowtest.LinearName(fmt.Sprintf("bench_%d", n)),
		workflowtest.LinearAdapter("fake", "default"),
		workflowtest.LinearInput(func(i int) string {
			return fmt.Sprintf("input { prompt = %q }", fmt.Sprintf("step %d", i))
		}),
	).Build()

	spec, diags := workflow.Parse("bench.hcl", []byte(hcl))
	if diags.HasErrors() {
		b.Fatalf("parse: %s", diags.Error())
	}
	graph, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		b.Fatalf("compile: %s", diags.Error())
	}
	return graph
}

func runBenchEngine(b *testing.B, graph *workflow.FSMGraph) {
	b.Helper()
	loader := &fakeLoader{
		adapters: map[string]adapterhost.Handle{
			"fake":         &fakeAdapter{name: "fake", outcome: "success"},
			"fake.default": &fakeAdapter{name: "fake", outcome: "success"},
		},
	}
	sink := benchSink{}
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		eng := New(graph, loader, sink)
		if err := eng.Run(ctx); err != nil {
			b.Fatalf("engine run: %v", err)
		}
	}
}

// BenchmarkEngineRun_10Steps benchmarks a 10-step linear workflow.
func BenchmarkEngineRun_10Steps(b *testing.B) {
	runBenchEngine(b, buildNStepWorkflow(b, 10))
}

// BenchmarkEngineRun_100Steps benchmarks a 100-step linear workflow.
func BenchmarkEngineRun_100Steps(b *testing.B) {
	runBenchEngine(b, buildNStepWorkflow(b, 100))
}

// BenchmarkEngineRun_1000Steps benchmarks a 1000-step linear workflow to
// measure throughput and allocation patterns at scale.
func BenchmarkEngineRun_1000Steps(b *testing.B) {
	runBenchEngine(b, buildNStepWorkflow(b, 1000))
}

// discardWriter is an io.Writer that discards all bytes.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

var _ io.Writer = discardWriter{}

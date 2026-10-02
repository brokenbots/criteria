package engine

// KB-48: runtime behavior-neutrality for named type blocks. The inline twin
// and the named twin below read identically at run time: the same data +
// variable + output types, the same applied defaults, the same adapter-visible
// inputs, the same captured outputs, and the same terminal step stream.
// Refactoring a schema from inline to named must change nothing at run time.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

const typedConsumerTwinBody = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "fake" "default" {}

data "internal" "store" {
  type  = %s
  value = 0
}
variable "amount" {
  type    = %s
  default = 5
}
output "spent" {
  value = data.internal.store.value
  type  = %s
}

step "work" {
  target = adapter.fake.default
  input {
    initial  = data.internal.store.value
    declared = var.amount
  }
  outcome "success" {
    next   = step.done
    schema = object({ amount_out = number })
  }
  write {
    target = data.internal.store.value
    value  = var.amount
  }
}

state "done" {
  terminal = true
  success  = true
}
`

const consumerTwinNamed = `type.num`

// consumerTwinNamedBlocks: one shared type block feeds the data, variable, and
// output consumers in the named twin.
const consumerTwinNamedBlocks = `
type "num" {
  schema = number
}
`

// twinRunSink records the runtime observable surface on top of fakeSink:
// adapter-visible inputs, variable-set events, captured step outputs, and the
// final run outputs.
type twinRunSink struct {
	fakeSink

	mu         sync.Mutex
	inputs     string
	varSets    string
	captured   string
	runOutputs string
}

func (s *twinRunSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured += fmt.Sprintf("%s=%v;", step, outputs)
}

func (s *twinRunSink) OnVariableSet(name, value, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.varSets += fmt.Sprintf("%s=%s(%s);", name, value, source)
}

func (s *twinRunSink) OnRunOutputs(outputs []map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runOutputs = fmt.Sprintf("%v", outputs)
}

// compileTypedConsumerTwin builds one twin of the consumer workflow. withNamed
// switches data/variable/output constraints from the inline `number` to the
// shared named ref type.num and declares the type block.
func compileTypedConsumerTwin(t *testing.T, withNamed bool) *workflow.FSMGraph {
	t.Helper()

	dataT, varT, outT := "number", "number", "number"
	typeBlocks := ""
	if withNamed {
		dataT, varT, outT = consumerTwinNamed, consumerTwinNamed, consumerTwinNamed
		typeBlocks = consumerTwinNamedBlocks
	}
	src := fmt.Sprintf(typedConsumerTwinBody, dataT, varT, outT) + typeBlocks

	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())
	return g
}

func runTypedConsumerTwin(t *testing.T, g *workflow.FSMGraph) *twinRunSink {
	t.Helper()

	capt := ""
	plug := &adapterFunc{fn: func(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink, _ *criteriav2.ExecutionRejection) (adapter.Result, error) {
		capt = step.Input["initial"] + "|" + step.Input["declared"]
		return adapter.Result{Outcome: "success", Outputs: map[string]cty.Value{"amount_out": cty.NumberIntVal(7)}}, nil
	}}

	sink := &twinRunSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": plug}}

	eng := NewTestEngine(g, loader, sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.inputs = capt
	return sink
}

func TestTypedConsumers_TwinRunsProduceIdenticalEvents(t *testing.T) {
	inline := runTypedConsumerTwin(t, compileTypedConsumerTwin(t, false))
	named := runTypedConsumerTwin(t, compileTypedConsumerTwin(t, true))

	require.Equal(t, inline.inputs, named.inputs, "adapter-visible inputs")
	require.Equal(t, inline.stepsRun, named.stepsRun, "step order")
	require.Equal(t, inline.terminal, named.terminal, "terminal state")
	require.True(t, inline.terminalOK, "terminal ok")
	require.Equal(t, inline.varSets, named.varSets, "variable-set events")
	require.Equal(t, inline.captured, named.captured, "captured outputs")
	require.Equal(t, inline.runOutputs, named.runOutputs, "run outputs")

	require.Equal(t, []string{"work"}, inline.stepsRun)
	require.Equal(t, "done", inline.terminal)
	require.NotEmpty(t, inline.runOutputs)
	require.Contains(t, inline.varSets, "amount=5(default)")
}

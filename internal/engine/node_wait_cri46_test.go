package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

// cri46Graph constructs a signal wait node with the supplied outcomes and
// matching terminal target states.
func cri46Graph(outcomes map[string]string) *workflow.FSMGraph {
	const waitName = "gate"
	g := &workflow.FSMGraph{
		Name:      "cri46",
		Steps:     map[string]*workflow.StepNode{},
		States:    map[string]*workflow.StateNode{},
		Waits:     map[string]*workflow.WaitNode{},
		Approvals: map[string]*workflow.ApprovalNode{},
		Variables: map[string]*workflow.VariableNode{},
	}
	g.InitialState = waitName
	g.Waits[waitName] = &workflow.WaitNode{
		Name:     waitName,
		Signal:   "resume",
		Outcomes: outcomes,
	}
	for _, target := range outcomes {
		g.States[target] = &workflow.StateNode{
			Name:     target,
			Terminal: true,
			Success:  true,
		}
	}
	return g
}

func TestCRI46_SignalWait_MultiOutcome_MissingOutcome(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done_ok", "err": "done_err"})
	eng := engine.New(g, emptyLoader(), &pauseSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	resumed := engine.New(g, emptyLoader(), &pauseSink{},
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{}),
	)
	err := resumed.RunFrom(context.Background(), "gate", 1)
	if err == nil {
		t.Fatal("expected error for missing outcome selector, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "gate") {
		t.Errorf("error should include wait name, got: %s", msg)
	}
	if !strings.Contains(msg, "ok") || !strings.Contains(msg, "err") {
		t.Errorf("error should include valid outcomes, got: %s", msg)
	}
}

func TestCRI46_SignalWait_MultiOutcome_EmptyOutcome(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done_ok", "err": "done_err"})
	eng := engine.New(g, emptyLoader(), &pauseSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	resumed := engine.New(g, emptyLoader(), &pauseSink{},
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"outcome": ""}),
	)
	err := resumed.RunFrom(context.Background(), "gate", 1)
	if err == nil {
		t.Fatal("expected error for empty outcome selector, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "gate") {
		t.Errorf("error should include wait name, got: %s", msg)
	}
	if !strings.Contains(msg, "ok") || !strings.Contains(msg, "err") {
		t.Errorf("error should include valid outcomes, got: %s", msg)
	}
}

func TestCRI46_SignalWait_MultiOutcome_UnknownOutcome(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done_ok", "err": "done_err"})
	eng := engine.New(g, emptyLoader(), &pauseSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	resumed := engine.New(g, emptyLoader(), &pauseSink{},
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"outcome": "unknown"}),
	)
	err := resumed.RunFrom(context.Background(), "gate", 1)
	if err == nil {
		t.Fatal("expected error for unknown outcome selector, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "gate") {
		t.Errorf("error should include wait name, got: %s", msg)
	}
	if !strings.Contains(msg, "ok") || !strings.Contains(msg, "err") {
		t.Errorf("error should include valid outcomes, got: %s", msg)
	}
}

func TestCRI46_SignalWait_MultiOutcome_ValidOutcome(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done_ok", "err": "done_err"})
	eng := engine.New(g, emptyLoader(), &pauseSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	sink := &pauseSink{}
	resumed := engine.New(g, emptyLoader(), sink,
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"outcome": "ok"}),
	)
	if err := resumed.RunFrom(context.Background(), "gate", 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !sink.completed {
		t.Fatal("expected run completed")
	}
}

func TestCRI46_SignalWait_MultiOutcome_MissingSelector_Deterministic(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done_ok", "err": "done_err"})
	for i := 0; i < 1000; i++ {
		eng := engine.New(g, emptyLoader(), &pauseSink{})
		if err := eng.Run(context.Background()); err != nil {
			t.Fatalf("first run: %v", err)
		}
		resumed := engine.New(g, emptyLoader(), &pauseSink{},
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumePayload(map[string]string{}),
		)
		if err := resumed.RunFrom(context.Background(), "gate", 1); err == nil {
			t.Fatalf("iteration %d: expected error, got nil", i)
		}
	}
}

func TestCRI46_SignalWait_SingleOutcome_MissingOutcome(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done"})
	eng := engine.New(g, emptyLoader(), &pauseSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	sink := &pauseSink{}
	resumed := engine.New(g, emptyLoader(), sink,
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{}),
	)
	if err := resumed.RunFrom(context.Background(), "gate", 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !sink.completed {
		t.Fatal("expected run completed")
	}
}

func TestCRI46_SignalWait_SingleOutcome_EmptyOutcome(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done"})
	eng := engine.New(g, emptyLoader(), &pauseSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	sink := &pauseSink{}
	resumed := engine.New(g, emptyLoader(), sink,
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"outcome": ""}),
	)
	if err := resumed.RunFrom(context.Background(), "gate", 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !sink.completed {
		t.Fatal("expected run completed")
	}
}

func TestCRI46_SignalWait_SingleOutcome_UnknownOutcome(t *testing.T) {
	g := cri46Graph(map[string]string{"ok": "done"})
	eng := engine.New(g, emptyLoader(), &pauseSink{})
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	sink := &pauseSink{}
	resumed := engine.New(g, emptyLoader(), sink,
		engine.WithResumedVars(eng.VarScope()),
		engine.WithResumePayload(map[string]string{"outcome": "unknown"}),
	)
	if err := resumed.RunFrom(context.Background(), "gate", 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !sink.completed {
		t.Fatal("expected run completed")
	}
}

func TestCRI46_DurationWait_DeterministicOutcome(t *testing.T) {
	g := &workflow.FSMGraph{
		Name:      "cri46_duration",
		Steps:     map[string]*workflow.StepNode{},
		States:    map[string]*workflow.StateNode{},
		Waits:     map[string]*workflow.WaitNode{},
		Approvals: map[string]*workflow.ApprovalNode{},
		Variables: map[string]*workflow.VariableNode{},
	}
	g.InitialState = "gate"
	g.Waits["gate"] = &workflow.WaitNode{
		Name:     "gate",
		Duration: 1, // 1 nanosecond to keep the test fast
		Outcomes: map[string]string{"elapsed": "done"},
	}
	g.States["done"] = &workflow.StateNode{Name: "done", Terminal: true, Success: true}

	sink := &pauseSink{}
	eng := engine.New(g, emptyLoader(), sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.completed {
		t.Fatal("expected run completed")
	}
}

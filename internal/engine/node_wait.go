package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/workflow"
)

type waitNode struct {
	node *workflow.WaitNode
}

func (n *waitNode) Name() string { return n.node.Name }

// sortedOutcomeKeys returns the outcome names of a wait node in a stable,
// deterministic order for error messages and diagnostics.
func sortedOutcomeKeys(outcomes map[string]string) []string {
	return slices.Sorted(maps.Keys(outcomes))
}

func (n *waitNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	if n.node.Duration > 0 {
		return n.evaluateDuration(ctx, deps)
	}
	return n.evaluateSignal(st, deps)
}

// evaluateDuration sleeps for the configured duration and then resumes.
// ctx cancellation aborts the wait and propagates as run cancellation.
// Duration mode enforces a single outcome at compile time (workflow.compile.go),
// so we look up "elapsed" directly to avoid map-iteration non-determinism.
func (n *waitNode) evaluateDuration(ctx context.Context, deps Deps) (string, error) {
	deps.Sink.OnWaitEntered(n.node.Name, "duration", n.node.Duration.String(), "")

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(n.node.Duration):
	}

	// The compiler enforces exactly one outcome for duration waits named "elapsed".
	target, ok := n.node.Outcomes["elapsed"]
	if !ok {
		// Fallback: take the single outcome if it exists under any name.
		for _, t := range n.node.Outcomes {
			deps.Sink.OnWaitResumed(n.node.Name, "duration", "", nil)
			return t, nil
		}
		return "", fmt.Errorf("wait %q: no outcomes defined", n.node.Name)
	}
	deps.Sink.OnWaitResumed(n.node.Name, "duration", "", nil)
	return target, nil
}

// evaluateSignal handles signal-mode wait nodes.
//
// Three entry conditions:
//   - First entry (PendingSignal == "" and ResumePayload == nil):
//     sets PendingSignal, emits WaitEntered, returns ErrPaused.
//   - Crash-reattach (PendingSignal == signal, ResumePayload == nil):
//     re-emits WaitEntered, returns ErrPaused so the run stays blocked.
//   - Resume (ResumePayload != nil, PendingSignal cleared by orchestrator):
//     emits WaitResumed, returns the single outcome target.
func (n *waitNode) evaluateSignal(st *RunState, deps Deps) (string, error) {
	if st.ResumePayload != nil {
		// Resumed: the orchestrator delivered the signal.
		payload := st.ResumePayload
		st.ResumePayload = nil
		st.PendingSignal = ""
		deps.Sink.OnWaitResumed(n.node.Name, "signal", n.node.Signal, payload)

		// payload["outcome"] selects the branch; this is the documented contract
		// between the Resume RPC caller and the engine. Multi-outcome signal waits
		// require a valid selector to avoid Go map-iteration non-determinism.
		// Single-outcome waits retain the payload-free resume path.
		if len(n.node.Outcomes) == 1 {
			if outcomeName := payload["outcome"]; outcomeName != "" {
				if target, ok := n.node.Outcomes[outcomeName]; ok {
					return target, nil
				}
			}
			for _, target := range n.node.Outcomes {
				return target, nil
			}
		}

		outcomeName := payload["outcome"]
		if target, ok := n.node.Outcomes[outcomeName]; ok && outcomeName != "" {
			return target, nil
		}
		return "", fmt.Errorf("wait %q: missing or invalid outcome %q; valid outcomes: %v", n.node.Name, outcomeName, sortedOutcomeKeys(n.node.Outcomes))
	}

	// First entry or crash-reattach: pause and wait for the signal.
	st.PendingSignal = n.node.Signal
	deps.Sink.OnWaitEntered(n.node.Name, "signal", "", n.node.Signal)
	return "", engineruntime.ErrPaused
}

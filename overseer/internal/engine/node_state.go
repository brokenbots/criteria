package engine

import (
	"context"

	engineruntime "github.com/brokenbots/overlord/overseer/internal/engine/runtime"
	"github.com/brokenbots/overlord/workflow"
)

type stateNode struct {
	state *workflow.StateNode
}

func (n *stateNode) Name() string {
	return n.state.Name
}

func (n *stateNode) Evaluate(context.Context, *RunState, Deps) (string, error) {
	// In Phase 1.5, state nodes terminate interpreter execution.
	return "", engineruntime.ErrTerminal
}

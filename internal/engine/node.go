package engine

import (
	"context"
	"fmt"

	"github.com/brokenbots/overseer/internal/plugin"
	"github.com/brokenbots/overseer/workflow"
)

// Node executes a graph node and returns the next node name.
type Node interface {
	Name() string
	Evaluate(ctx context.Context, st *RunState, deps Deps) (next string, err error)
}

// Deps carries interpreter runtime dependencies shared by node implementations.
type Deps struct {
	Sessions            *plugin.SessionManager
	Sink                Sink
	SubWorkflowResolver SubWorkflowResolver
	BranchScheduler     BranchScheduler
}

// UnknownNodeError indicates the graph does not contain the requested node.
type UnknownNodeError struct {
	Name string
}

func (e *UnknownNodeError) Error() string {
	return fmt.Sprintf("unknown node %q", e.Name)
}

func nodeFor(graph *workflow.FSMGraph, name string) (Node, error) {
	if step, ok := graph.Steps[name]; ok {
		return &stepNode{graph: graph, step: step}, nil
	}
	if wait, ok := graph.Waits[name]; ok {
		return &waitNode{node: wait}, nil
	}
	if approval, ok := graph.Approvals[name]; ok {
		return &approvalNode{node: approval}, nil
	}
	if br, ok := graph.Branches[name]; ok {
		return &branchNode{node: br}, nil
	}
	if fe, ok := graph.ForEachs[name]; ok {
		return &forEachNode{node: fe}, nil
	}
	// TODO(1.6): parallelNode would call deps.BranchScheduler.Run(...).
	if state, ok := graph.States[name]; ok {
		return &stateNode{state: state}, nil
	}
	return nil, &UnknownNodeError{Name: name}
}

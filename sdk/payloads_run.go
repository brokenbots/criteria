package criteria

import pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

// RunStarted is emitted when a run begins execution.
type RunStarted = pb.RunStarted

// RunCompleted is emitted when a run finishes successfully.
type RunCompleted = pb.RunCompleted

// RunFailed is emitted when a run terminates with an error.
type RunFailed = pb.RunFailed

// SubworkflowGraph is one compiled subworkflow layer of a run's workflow
// (CRI-278). Body carries the layer's complete compiled body graph serialized
// as JSON.
type SubworkflowGraph = pb.SubworkflowGraph

// WorkflowGraphs is emitted by the agent exactly once per run, immediately
// after compilation and before the engine starts, carrying the compiled
// subworkflow layers (CRI-278). Resend-replaces: consumers use the most
// recent workflow.graphs event per run.
type WorkflowGraphs = pb.WorkflowGraphs

package criteria

import pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

// RunStarted is emitted when a run begins execution.
type RunStarted = pb.RunStarted

// RunCompleted is emitted when a run finishes successfully.
type RunCompleted = pb.RunCompleted

// RunFailed is emitted when a run terminates with an error.
type RunFailed = pb.RunFailed

// SubworkflowGraph is one compiled subworkflow layer of a run's workflow
// (CRI-278, CRI-299). Body carries ONLY that layer's own compiled body graph
// serialized as JSON — its subworkflows key is stripped, so no body contains
// another layer; nested subworkflow layers ride the flat subworkflows list
// as their own entries, and nesting is expressed by the parent's
// subworkflow.<name> step targets plus that flat list.
type SubworkflowGraph = pb.SubworkflowGraph

// WorkflowGraphs is emitted by the agent exactly once per run, immediately
// after compilation and before the engine starts, carrying the compiled
// subworkflow layers (CRI-278). Resend-replaces: consumers use the most
// recent workflow.graphs event per run.
type WorkflowGraphs = pb.WorkflowGraphs

// WorkflowGraphs emission at the post-compile seam (CRI-278).
//
// After compileForExecution returns, the apply path emits exactly one
// WorkflowGraphs envelope per run — in both local and server mode — before
// the engine starts, so the event lands at or before RunStarted. The payload
// is serialized from the already-compiled in-memory graph (no second
// compilation pass): one layer per compiled subworkflow, top-level layers
// only, each carrying name, source_path and the complete compiled body graph
// with nested layers inline — the same content criteria compile --format
// json produces in subworkflows[].
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/brokenbots/criteria/internal/run"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

// workflowGraphsLayersJSON marshals the compiled subworkflow layers into the
// compact JSON layers array. The array is never nil: workflows without
// subworkflows emit an empty (non-null) payload.
func workflowGraphsLayersJSON(graph *workflow.FSMGraph) (json.RawMessage, error) {
	cj := buildCompileJSON(graph)
	layers := cj.Subworkflows
	if layers == nil {
		layers = []compileSubworkflow{}
	}
	b, err := json.Marshal(layers)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow graphs layers: %w", err)
	}
	return b, nil
}

// buildWorkflowGraphsPayload builds the server-mode WorkflowGraphs message
// from the already-compiled in-memory graph. Top-level layers only; each
// body is the complete compiled body graph serialized as a JSON string, with
// nested subworkflow layers carried inline by body.subworkflows.
func buildWorkflowGraphsPayload(graph *workflow.FSMGraph) (*pb.WorkflowGraphs, error) {
	cj := buildCompileJSON(graph)
	msg := &pb.WorkflowGraphs{Subworkflows: make([]*pb.SubworkflowGraph, 0, len(cj.Subworkflows))}
	for i := range cj.Subworkflows {
		layer := &cj.Subworkflows[i]
		body, err := json.Marshal(layer.Body)
		if err != nil {
			return nil, fmt.Errorf("marshal subworkflow %q body: %w", layer.Name, err)
		}
		msg.Subworkflows = append(msg.Subworkflows, &pb.SubworkflowGraph{
			Name:       layer.Name,
			SourcePath: layer.SourcePath,
			Body:       string(body),
		})
	}
	return msg, nil
}

// emitWorkflowGraphsLocal writes the WorkflowGraphs envelope into the local
// ND-JSON stream via the sink's shared seq sequence. A payload build failure
// is logged and skipped — the event is best-effort metadata and must never
// fail the run.
func emitWorkflowGraphsLocal(log *slog.Logger, local *run.LocalSink, graph *workflow.FSMGraph) {
	layersJSON, err := workflowGraphsLayersJSON(graph)
	if err != nil {
		log.Warn("skipping workflow graphs event", "error", err)
		return
	}
	local.OnWorkflowGraphsLayers(layersJSON)
}

// emitWorkflowGraphsServer publishes the WorkflowGraphs event on the server
// stream and, when dual-write is active, mirrors it into the ND-JSON events
// file. A payload build failure is logged and skipped — the event is
// best-effort metadata and must never fail the run.
func emitWorkflowGraphsServer(ctx context.Context, log *slog.Logger, sink *run.Sink, mirror *run.LocalSink, graph *workflow.FSMGraph) {
	msg, err := buildWorkflowGraphsPayload(graph)
	if err != nil {
		log.Warn("skipping workflow graphs event", "error", err)
		return
	}
	sink.OnWorkflowGraphs(ctx, msg)
	mirror.OnWorkflowGraphs(msg)
}

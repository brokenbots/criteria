// WorkflowGraphs emission at the post-compile seam (CRI-278, CRI-298).
//
// After compileForExecution returns, the apply path emits exactly one
// WorkflowGraphs envelope per run — in both local and server mode — before
// the engine starts, so the event lands at or before RunStarted. The payload
// is serialized from the already-compiled in-memory graph (no second
// compilation pass): one layer per compiled subworkflow, top-level layers
// only, each carrying name, sourcePath and the complete compiled body graph
// serialized as a JSON string. The layer shape is uniform at every nesting
// depth (CRI-298): the same three keys and the string-body convention
// top-level and nested, recursively — nested layers ride inside the parsed
// body's subworkflows array in the same wire shape. The compile-JSON dialect
// (snake_case source_path, inline bodies) stays internal to `criteria
// compile --format json`, which keeps its own published shape.
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

// workflowGraphsLayer is one subworkflow layer entry in the uniform wire
// shape (CRI-298): the same three keys at every nesting depth, with body
// carrying the compiled body graph as a JSON string rather than an inline
// object, and sourcePath in the wire's camelCase.
type workflowGraphsLayer struct {
	Name       string `json:"name"`
	SourcePath string `json:"sourcePath"`
	Body       string `json:"body"`
}

// workflowGraphsBody is a compiled layer body as carried inside a wire
// layer's body string: the compile-JSON graph of that layer with its nested
// subworkflow entries re-keyed to the wire layer shape. The embedded
// compileJSON.Subworkflows field (compile-JSON dialect) is shadowed by the
// outer field for JSON marshaling.
type workflowGraphsBody struct {
	Subworkflows []workflowGraphsLayer `json:"subworkflows,omitempty"`
	compileJSON
}

// workflowGraphsLayers builds the compiled subworkflow layers in the uniform
// wire shape. The slice is never nil: workflows without subworkflows emit an
// empty (non-null) payload.
func workflowGraphsLayers(graph *workflow.FSMGraph) ([]workflowGraphsLayer, error) {
	return workflowGraphsWireLayers(buildCompileJSON(graph).Subworkflows)
}

// workflowGraphsLayersJSON marshals the compiled subworkflow layers into the
// compact JSON layers array, uniform at every depth.
func workflowGraphsLayersJSON(graph *workflow.FSMGraph) (json.RawMessage, error) {
	layers, err := workflowGraphsLayers(graph)
	if err != nil {
		return nil, err
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
// nested subworkflow layers riding inside the parsed body's subworkflows
// array in the same wire shape.
func buildWorkflowGraphsPayload(graph *workflow.FSMGraph) (*pb.WorkflowGraphs, error) {
	layers, err := workflowGraphsLayers(graph)
	if err != nil {
		return nil, err
	}
	msg := &pb.WorkflowGraphs{Subworkflows: make([]*pb.SubworkflowGraph, 0, len(layers))}
	for i := range layers {
		msg.Subworkflows = append(msg.Subworkflows, &pb.SubworkflowGraph{
			Name:       layers[i].Name,
			SourcePath: layers[i].SourcePath,
			Body:       layers[i].Body,
		})
	}
	return msg, nil
}

// workflowGraphsWireLayers converts compiled subworkflow entries into the
// uniform wire shape, recursively: each entry's body is its compiled body
// graph serialized as a JSON string whose own subworkflow entries are again
// wire-shaped.
func workflowGraphsWireLayers(subs []compileSubworkflow) ([]workflowGraphsLayer, error) {
	layers := make([]workflowGraphsLayer, 0, len(subs))
	for i := range subs {
		layer, err := workflowGraphsWireLayer(&subs[i])
		if err != nil {
			return nil, err
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

func workflowGraphsWireLayer(sw *compileSubworkflow) (workflowGraphsLayer, error) {
	body := workflowGraphsBody{compileJSON: sw.Body}
	nested, err := workflowGraphsWireLayers(sw.Body.Subworkflows)
	if err != nil {
		return workflowGraphsLayer{}, fmt.Errorf("subworkflow %q: %w", sw.Name, err)
	}
	body.Subworkflows = nested
	body.compileJSON.Subworkflows = nil
	raw, err := json.Marshal(&body)
	if err != nil {
		return workflowGraphsLayer{}, fmt.Errorf("marshal subworkflow %q body: %w", sw.Name, err)
	}
	return workflowGraphsLayer{Name: sw.Name, SourcePath: sw.SourcePath, Body: string(raw)}, nil
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

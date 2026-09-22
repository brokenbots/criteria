// WorkflowGraphs emission at the post-compile seam (CRI-278, CRI-298,
// CRI-299).
//
// After compileForExecution returns, the apply path emits exactly one
// WorkflowGraphs envelope per run — in both local and server mode — before
// the engine starts, so the event lands at or before RunStarted. The payload
// is serialized from the already-compiled in-memory graph (no second
// compilation pass) as one flat layers array: every subworkflow layer at
// every nesting depth is a sibling entry in the repeated field (proto
// events.proto field 37: "a layer may itself declare subworkflows, which
// appear as their own entries"). A layer body carries ONLY that layer's own
// compiled graph (steps/states/adapters) — its subworkflows key is stripped
// and never serialized, so no body contains another layer. Nesting is
// expressed by the parent's subworkflow.<name> step targets plus the flat
// entry list; consumers rebuild the tree with a plain name->layer map. The
// layer shape is uniform at every entry (CRI-298): the same three keys with
// body as a JSON string and sourcePath in the wire's camelCase. Both
// emitters serialize the same pb.WorkflowGraphs message with the same
// protojson codec, so the local ND-JSON payload is semantically identical to
// the server stream (the dual-write mirror and the castle consumer's
// WorkflowGraphsPayload shape: {"subworkflows":[...]}; raw bytes can differ
// only in protojson's build-randomized separator whitespace versus
// encoding/json compaction). The compile-JSON
// dialect (snake_case source_path, inline bodies) stays internal to
// `criteria compile --format json`, which keeps its own published shape —
// nesting stays internal to it as well.
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

// workflowGraphsLayer is one subworkflow layer entry in the wire shape
// (CRI-298/CRI-299): the same three keys at every nesting depth, with body
// carrying the compiled body graph as a JSON string rather than an inline
// object, and sourcePath in the wire's camelCase.
type workflowGraphsLayer struct {
	Name       string `json:"name"`
	SourcePath string `json:"sourcePath"`
	Body       string `json:"body"`
}

// workflowGraphsLayers builds the compiled subworkflow layers in the flat
// wire shape (CRI-299): every layer at every nesting depth is a sibling
// entry in the returned slice, in document (pre) order. A layer body
// carries ONLY that layer's own compiled graph — its subworkflows key is
// stripped, never serialized. The slice is never nil: workflows without
// subworkflows emit an empty (non-null) payload.
func workflowGraphsLayers(graph *workflow.FSMGraph) ([]workflowGraphsLayer, error) {
	return workflowGraphsFlatLayers(buildCompileJSON(graph).Subworkflows)
}

// buildWorkflowGraphsPayload builds the server-mode WorkflowGraphs message
// from the already-compiled in-memory graph: every layer at every depth is a
// sibling entry; each body is that layer's own compiled graph serialized as
// a JSON string, never carrying another layer.
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

// workflowGraphsFlatLayers converts the compiled subworkflow tree into the
// flat wire layer list (CRI-299), pre-order: every entry is appended as a
// sibling before its own nested entries are walked. Termination is
// structural — the compiled tree is finite (subworkflow cycles are rejected
// at parse time).
func workflowGraphsFlatLayers(subs []compileSubworkflow) ([]workflowGraphsLayer, error) {
	layers := make([]workflowGraphsLayer, 0, len(subs))
	for i := range subs {
		layer, err := workflowGraphsLeafLayer(&subs[i])
		if err != nil {
			return nil, err
		}
		layers = append(layers, layer)
		nested, err := workflowGraphsFlatLayers(subs[i].Body.Subworkflows)
		if err != nil {
			return nil, fmt.Errorf("subworkflow %q: %w", subs[i].Name, err)
		}
		layers = append(layers, nested...)
	}
	return layers, nil
}

// workflowGraphsLeafLayer serializes one compiled subworkflow as a flat wire
// entry (CRI-299): the body is that layer's own compiled graph with its
// subworkflows key stripped — bodies are leaf data, never containers of
// other layers. The nested entries ride the top-level repeated field as
// their own siblings instead.
func workflowGraphsLeafLayer(sw *compileSubworkflow) (workflowGraphsLayer, error) {
	body := sw.Body // shallow copy: stripping the copy's subworkflows leaves the compiled graph untouched
	body.Subworkflows = nil
	raw, err := json.Marshal(&body)
	if err != nil {
		return workflowGraphsLayer{}, fmt.Errorf("marshal subworkflow %q body: %w", sw.Name, err)
	}
	return workflowGraphsLayer{Name: sw.Name, SourcePath: sw.SourcePath, Body: string(raw)}, nil
}

// emitWorkflowGraphsLocal writes the WorkflowGraphs envelope into the local
// ND-JSON stream via the sink's shared seq sequence. The payload is the same
// pb.WorkflowGraphs message the server stream carries, serialized with the
// same protojson codec — semantically identical to the wire (CRI-299: the
// local viewer renders the flat layers array from either path; raw bytes can
// differ only in protojson's build-randomized separator whitespace versus
// encoding/json compaction of the envelope's raw payload). A payload build
// failure is logged and skipped — the event is
// best-effort metadata and must never fail the run.
func emitWorkflowGraphsLocal(log *slog.Logger, local *run.LocalSink, graph *workflow.FSMGraph) {
	msg, err := buildWorkflowGraphsPayload(graph)
	if err != nil {
		log.Warn("skipping workflow graphs event", "error", err)
		return
	}
	local.OnWorkflowGraphs(msg)
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

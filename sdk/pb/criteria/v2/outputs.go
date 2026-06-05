package criteriav2

import "encoding/json"

// outputs.go — helpers for building the typed ExecuteResult.outputs_json channel.
// Adapters emit step outputs as native JSON (values keep their JSON type:
// string/number/bool/object/array), and the host decodes them to native cty
// types. This is the sole output channel after the v2 typed-outputs cutover; the
// legacy map<string,string> field was removed.

// MarshalOutputs encodes a typed outputs map into the JSON bytes carried in
// ExecuteResult.outputs_json. Returns (nil, nil) for an empty map. Values may be
// any JSON-encodable Go value; structured values (maps, slices, numbers, bools)
// are preserved with their native type.
func MarshalOutputs(outputs map[string]any) ([]byte, error) {
	if len(outputs) == 0 {
		return nil, nil
	}
	return json.Marshal(outputs)
}

// NewExecuteResult builds a single (non-chunked) ExecuteResult carrying the
// outcome and natively-typed outputs. For payloads that may exceed the negotiated
// chunk size, marshal with MarshalOutputs and split with ChunkExecuteResultOutputs.
func NewExecuteResult(outcome string, outputs map[string]any) (*ExecuteResult, error) {
	oj, err := MarshalOutputs(outputs)
	if err != nil {
		return nil, err
	}
	return &ExecuteResult{Outcome: outcome, OutputsJson: oj}, nil
}

// NewExecuteResultEvent wraps NewExecuteResult in an ExecuteEvent, the terminal
// event an adapter sends from Execute.
func NewExecuteResultEvent(outcome string, outputs map[string]any) (*ExecuteEvent, error) {
	res, err := NewExecuteResult(outcome, outputs)
	if err != nil {
		return nil, err
	}
	return &ExecuteEvent{Event: &ExecuteEvent_Result{Result: res}}, nil
}

// copilot_util.go — event-construction helpers shared across the copilot adapter.

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func resultEvent(outcome string) *pb.ExecuteEvent {
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{
			Result: &pb.ExecuteResult{Outcome: outcome},
		},
	}
}

func logEvent(stream, chunk string) *pb.ExecuteEvent {
	if !strings.HasSuffix(chunk, "\n") {
		chunk += "\n"
	}
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Log{
			Log: &pb.LogEvent{Stream: stream, Chunk: []byte(chunk)},
		},
	}
}

func adapterEvent(kind string, data map[string]any) *pb.ExecuteEvent {
	s, _ := structpb.NewStruct(data)
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Adapter{
			Adapter: &pb.AdapterEvent{
				Kind: kind,
				Data: s,
			},
		},
	}
}

func stringifyAny(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

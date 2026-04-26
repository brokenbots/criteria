package run

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type sinkLine struct {
	Seq         int64           `json:"seq"`
	RunID       string          `json:"run_id"`
	PayloadType string          `json:"payload_type"`
	Payload     json.RawMessage `json:"payload"`
}

func TestLocalSink_EncodesNDJSONAndMonotonicSeq(t *testing.T) {
	var buf bytes.Buffer
	checkpointCalls := 0
	sink := &LocalSink{
		RunID: "run-local-1",
		Out:   &buf,
		CheckpointFn: func(step string, attempt int) {
			checkpointCalls++
			if step != "step1" || attempt != 1 {
				t.Fatalf("unexpected checkpoint call: step=%s attempt=%d", step, attempt)
			}
		},
	}

	sink.OnRunStarted("wf", "step1")
	sink.OnStepEntered("step1", "noop", 1)
	stepSink := sink.StepEventSink("step1")
	stepSink.Log("stdout", []byte("hello\n"))
	stepSink.Adapter("custom.event", map[string]any{"k": "v"})
	sink.OnStepOutcome("step1", "success", 17*time.Millisecond, nil)
	sink.OnStepTransition("step1", "done", "success")
	sink.OnRunCompleted("done", true)

	if checkpointCalls != 1 {
		t.Fatalf("checkpoint calls: got %d want 1", checkpointCalls)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("line count: got %d want 7", len(lines))
	}

	wantTypes := []string{"RunStarted", "StepEntered", "StepLog", "AdapterEvent", "StepOutcome", "StepTransition", "RunCompleted"}
	for i, line := range lines {
		var got sinkLine
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d json: %v", i+1, err)
		}
		if got.Seq != int64(i+1) {
			t.Fatalf("line %d seq: got %d want %d", i+1, got.Seq, i+1)
		}
		if got.RunID != "run-local-1" {
			t.Fatalf("line %d run_id: got %q", i+1, got.RunID)
		}
		if got.PayloadType != wantTypes[i] {
			t.Fatalf("line %d payload_type: got %q want %q", i+1, got.PayloadType, wantTypes[i])
		}
		if len(got.Payload) == 0 {
			t.Fatalf("line %d payload must not be empty", i+1)
		}
	}
}

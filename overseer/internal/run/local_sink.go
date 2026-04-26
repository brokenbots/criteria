package run

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

// LocalSink emits engine events as newline-delimited JSON envelopes.
// Sequence numbers are monotonic for this sink instance.
type LocalSink struct {
	RunID string
	Out   io.Writer

	// CheckpointFn, if non-nil, is called synchronously inside OnStepEntered
	// before the envelope is written.
	CheckpointFn func(step string, attempt int)

	mu  sync.Mutex
	seq int64
}

type localEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Seq           int64           `json:"seq"`
	RunID         string          `json:"run_id"`
	PayloadType   string          `json:"payload_type"`
	Payload       json.RawMessage `json:"payload"`
}

func (s *LocalSink) OnRunStarted(workflowName, initialStep string) {
	s.emit("RunStarted", &pb.RunStarted{WorkflowName: workflowName, InitialStep: initialStep})
}

func (s *LocalSink) OnRunCompleted(finalState string, success bool) {
	s.emit("RunCompleted", &pb.RunCompleted{FinalState: finalState, Success: success})
}

func (s *LocalSink) OnRunFailed(reason, step string) {
	s.emit("RunFailed", &pb.RunFailed{Reason: reason, Step: step})
}

func (s *LocalSink) OnStepEntered(step, adapterName string, attempt int) {
	if s.CheckpointFn != nil {
		s.CheckpointFn(step, attempt)
	}
	s.emit("StepEntered", &pb.StepEntered{Step: step, Adapter: adapterName, Attempt: int32(attempt)})
}

func (s *LocalSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	p := &pb.StepOutcome{Step: step, Outcome: outcome, DurationMs: duration.Milliseconds()}
	if err != nil {
		p.Error = err.Error()
	}
	s.emit("StepOutcome", p)
}

func (s *LocalSink) OnStepTransition(from, to, viaOutcome string) {
	s.emit("StepTransition", &pb.StepTransition{From: from, To: to, ViaOutcome: viaOutcome})
}

func (s *LocalSink) OnStepResumed(step string, attempt int, reason string) {
	s.emit("StepResumed", &pb.StepResumed{Step: step, Attempt: int32(attempt), Reason: reason})
}

// OnVariableSet emits a VariableSet event (W04).
func (s *LocalSink) OnVariableSet(name, value, source string) {
	s.emit("VariableSet", &pb.VariableSet{Name: name, Value: value, Source: source})
}

// OnStepOutputCaptured emits a StepOutputCaptured event (W04).
func (s *LocalSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.emit("StepOutputCaptured", &pb.StepOutputCaptured{Step: step, Outputs: outputs})
}

func (s *LocalSink) StepEventSink(step string) adapter.EventSink {
	return &localStepSink{parent: s, step: step}
}

func (s *LocalSink) emit(payloadType string, payload proto.Message) {
	if s == nil || s.Out == nil {
		return
	}
	payloadJSON, err := protojson.Marshal(payload)
	if err != nil {
		payloadJSON = []byte(`{"_encode_error":"` + escapeJSONString(err.Error()) + `"}`)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++

	env := localEnvelope{
		SchemaVersion: events.SchemaVersion,
		Seq:           s.seq,
		RunID:         s.RunID,
		PayloadType:   payloadType,
		Payload:       payloadJSON,
	}
	line, err := json.Marshal(env)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(s.Out, string(line))
}

type localStepSink struct {
	parent *LocalSink
	step   string
}

func (ss *localStepSink) Log(stream string, chunk []byte) {
	ss.parent.emit("StepLog", &pb.StepLog{Step: ss.step, Stream: logStreamFromString(stream), Chunk: string(chunk)})
}

func (ss *localStepSink) Adapter(kind string, data any) {
	msg := &pb.AdapterEvent{Step: ss.step, Kind: kind}
	if data != nil {
		msg.Data = encodeAdapterData(data)
	}
	ss.parent.emit("AdapterEvent", msg)
}

func escapeJSONString(v string) string {
	b, err := json.Marshal(v)
	if err != nil || len(b) < 2 {
		return "encode error"
	}
	return string(b[1 : len(b)-1])
}

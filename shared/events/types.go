// Package events defines the wire envelope and payload types exchanged between
// Overseer, Castle, and Parapet. The schema is mirrored in
// api/events.schema.json which is the source of truth.
package events

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the current event protocol version.
const SchemaVersion = 1

// Type is the envelope type discriminator.
type Type string

const (
	TypeRunStarted          Type = "run.started"
	TypeRunCompleted        Type = "run.completed"
	TypeRunFailed           Type = "run.failed"
	TypeStepEntered         Type = "step.entered"
	TypeStepOutcome         Type = "step.outcome"
	TypeStepTransition      Type = "step.transition"
	TypeStepLog             Type = "step.log"
	TypeAdapterEvent        Type = "adapter.event"
	TypeOverseerHeartbeat   Type = "overseer.heartbeat"
	TypeOverseerDisconnected Type = "overseer.disconnected"
)

// Envelope is the wire format for every event.
//
// `Seq` is assigned by Castle on ingest (monotonic per run). Overseer leaves it
// at 0 when publishing.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	Seq           uint64          `json:"seq"`
	Type          Type            `json:"type"`
	Timestamp     time.Time       `json:"ts"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// --- Payload types ---

type RunStarted struct {
	WorkflowName string `json:"workflow_name"`
	InitialStep  string `json:"initial_step"`
}

type RunCompleted struct {
	FinalState string `json:"final_state"`
	Success    bool   `json:"success"`
}

type RunFailed struct {
	Reason string `json:"reason"`
	Step   string `json:"step,omitempty"`
}

type StepEntered struct {
	Step    string `json:"step"`
	Adapter string `json:"adapter"`
	Attempt int    `json:"attempt"`
}

type StepOutcome struct {
	Step       string `json:"step"`
	Outcome    string `json:"outcome"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type StepTransition struct {
	From       string `json:"from"`
	To         string `json:"to"`
	ViaOutcome string `json:"via_outcome"`
}

type LogStream string

const (
	StreamStdout LogStream = "stdout"
	StreamStderr LogStream = "stderr"
	StreamAgent  LogStream = "agent"
)

type StepLog struct {
	Step   string    `json:"step"`
	Stream LogStream `json:"stream"`
	Chunk  string    `json:"chunk"`
}

type AdapterEvent struct {
	Step    string          `json:"step"`
	Adapter string          `json:"adapter"`
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// New builds an envelope with the given typed payload. Seq is left at zero —
// Castle assigns the real value on ingest.
func New(runID string, t Type, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		Type:          t,
		Timestamp:     time.Now().UTC(),
		Payload:       raw,
	}, nil
}

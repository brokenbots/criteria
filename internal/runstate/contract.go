// Package runstate implements the minimal run-state HTTP server that backs the
// embedded run viewer for local (non-orchestrator) runs, delivering the read
// half of the CRI-255 loopback seam early (CRI-279/CRI-279's read scope).
//
// The seam's plain-JSON contract is defined by its consumer
// (parapet/packages/run-viewer/src/api/localRunDataSource.ts); the shapes here
// mirror that contract exactly:
//
//	Run           {runId, criteriaId, workflowName, workflowHash, status,
//	               startedAt?, endedAt?, finalState, failureReason,
//	               ticket?, repoUrl?, prUrl?}
//	RunsPage      {runs: Run[], nextPageToken}
//	Agent         {criteriaId, name, labels, status, lastSeenAt?}
//	EventEnvelope {schemaVersion, runId, seq, type, correlationId, payload}
//	RunEventsPage {events: EventEnvelope[], lastSeq, nextSinceSeq}
//	RunInspection {runId, sessionId, adapter?, currentStep,
//	               pendingPermissions, lastActivityAt?, stateJson?}
//
// The data source is the run's NDJSON events file plus run-state.json under
// $CRITERIA_HOME/runs/<runID>/ — single-process, files the process owns. The
// event vocabulary is the seam's camelCase oneof-case names, identical to the
// castle path's Envelope rendering: the ND-JSON producer's payload_type
// discriminator is mapped through the shared table in eventvocab.go and the
// payload is the raw payload object verbatim.
package runstate

import "encoding/json"

// Run is the castle-mapped run record. Optional fields are omitted when the
// local data source has no equivalent (ticket/repoUrl/prUrl are castle
// enrichments; repoUrl is filled from run-metadata.json when present).
type Run struct {
	RunID         string `json:"runId"`
	CriteriaID    string `json:"criteriaId,omitempty"`
	WorkflowName  string `json:"workflowName"`
	WorkflowHash  string `json:"workflowHash,omitempty"`
	Status        string `json:"status"`
	StartedAt     string `json:"startedAt,omitempty"` // RFC 3339 UTC
	EndedAt       string `json:"endedAt,omitempty"`   // RFC 3339 UTC
	FinalState    string `json:"finalState,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	Ticket        string `json:"ticket,omitempty"`
	RepoURL       string `json:"repoUrl,omitempty"`
	PRURL         string `json:"prUrl,omitempty"`
}

// RunsPage is the paginated run-list response.
type RunsPage struct {
	Runs          []Run  `json:"runs"`
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// Agent is a stub run-agent record (the criteriaId when known). The full
// agent surface arrives with the orchestrator-backed path; a stub is
// contract-acceptable for the local seam (CRI-279). Labels is always present
// (an empty object for the stub) — the consumer type requires it.
type Agent struct {
	CriteriaID string            `json:"criteriaId,omitempty"`
	Name       string            `json:"name,omitempty"`
	Labels     map[string]string `json:"labels"`
	Status     string            `json:"status,omitempty"`
	LastSeenAt string            `json:"lastSeenAt,omitempty"` // RFC 3339 UTC
}

// EventEnvelope is the castle-mapped event shape the viewer consumes. Type is
// the payload discriminator (the ND-JSON payload_type value) and Payload is
// the raw payload JSON verbatim.
type EventEnvelope struct {
	SchemaVersion int             `json:"schemaVersion"`
	RunID         string          `json:"runId"`
	Seq           int64           `json:"seq"`
	Type          string          `json:"type"`
	CorrelationID string          `json:"correlationId,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// RunEventsPage is the paginated run-events response. LastSeq is the highest
// seq in the run at fetch time; NextSinceSeq is the continuation cursor for
// the next (newer) page — non-nil only on a full page (nil renders as JSON
// null, the consumer's "no continuation" signal).
type RunEventsPage struct {
	Events       []EventEnvelope `json:"events"`
	LastSeq      int64           `json:"lastSeq"`
	NextSinceSeq *int64          `json:"nextSinceSeq"`
}

// RunInspection mirrors the castle InspectRunResponse for a run (or, with the
// session parameter, a single adapter session within it). Local data has at
// most one live adapter session per step, so sessionId maps the requested
// session (or the run's most recent adapter session when empty).
type RunInspection struct {
	RunID              string `json:"runId"`
	SessionID          string `json:"sessionId"`
	Adapter            string `json:"adapter,omitempty"`
	CurrentStep        string `json:"currentStep,omitempty"`
	PendingPermissions int    `json:"pendingPermissions"`
	LastActivityAt     string `json:"lastActivityAt,omitempty"` // RFC 3339 UTC
	StateJSON          string `json:"stateJson,omitempty"`
}

// Run status vocabulary (identical to the castle Run record statuses).
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

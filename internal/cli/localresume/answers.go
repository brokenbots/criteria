package localresume

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ModeAnswers is the internal mode backing the --answers <file> path (CRI-256):
// pre-populated, non-interactive decisions resolved at the pause point. It is
// never selectable through CRITERIA_LOCAL_APPROVAL -- it is constructed
// programmatically by the apply command's selection logic.
const ModeAnswers Mode = "answers"

// ErrUnanswered is returned when an answers-file resumer is asked to resolve a
// node that has no entry in the file. Callers can detect it with errors.Is to
// decide between falling back to the interactive prompt (TTY session) and
// failing loudly naming the node.
var ErrUnanswered = errors.New("no entry in answers file")

// IsUnanswered reports whether err indicates a node missing from the answers
// file (as opposed to a malformed entry or a transport error).
func IsUnanswered(err error) bool { return errors.Is(err, ErrUnanswered) }

// --- answers mode ---

// resolveApprovalAnswers resolves an approval pause from the pre-populated
// entries. A node that is missing from the file returns an error wrapping
// ErrUnanswered so the caller can fall back to the interactive prompt (TTY
// session) or fail loudly naming the node.
func (r *resumer) resolveApprovalAnswers(name string) (map[string]string, error) {
	entry, ok := r.answers[name]
	if !ok {
		return nil, fmt.Errorf("%w: approval node %q", ErrUnanswered, name)
	}
	if entry.Decision == "" {
		return nil, fmt.Errorf("answers entry for approval node %q must set \"decision\" (approved or rejected); outcome %q is not an approval decision",
			name, entry.Outcome)
	}
	payload := map[string]string{"decision": entry.Decision}
	if entry.Reason != "" {
		payload["reason"] = entry.Reason
	}
	for k, v := range entry.Payload {
		payload[k] = v
	}
	return payload, nil
}

// resolveSignalAnswers resolves a signal-wait pause from the pre-populated
// entries. The returned outcome is validated against the node's declared
// outcomes by ResumeSignal after the mode switch.
func (r *resumer) resolveSignalAnswers(nodeName string) (map[string]string, error) {
	entry, ok := r.answers[nodeName]
	if !ok {
		return nil, fmt.Errorf("%w: signal wait %q", ErrUnanswered, nodeName)
	}
	if entry.Outcome == "" {
		return nil, fmt.Errorf("answers entry for signal wait %q must set \"outcome\"", nodeName)
	}
	payload := map[string]string{"outcome": entry.Outcome}
	if entry.Reason != "" {
		payload["reason"] = entry.Reason
	}
	for k, v := range entry.Payload {
		payload[k] = v
	}
	return payload, nil
}

// AnswerEntry is one pre-populated decision in a --answers file entry mapped
// by paused node name. Approval nodes take decision ("approved"|"rejected")
// with an optional reason; signal-wait nodes take outcome (a declared outcome
// of the wait node). Payload carries extra resume-payload keys the workflow
// consumes.
type AnswerEntry struct {
	Decision string            `json:"decision,omitempty"`
	Outcome  string            `json:"outcome,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	Payload  map[string]string `json:"payload,omitempty"`
}

// ParseAnswers decodes an answers-file document: a JSON object keyed by paused
// node name. Structure errors (malformed JSON, non-string values, unknown
// entry fields, empty node keys) are reported here; per-entry semantics
// (unknown nodes, ambiguous decision/outcome, undeclared outcomes) are
// validated by the caller against the compiled workflow graph, because the
// parser does not know the node kinds.
func ParseAnswers(data []byte) (map[string]AnswerEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse answers file: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse answers file: unexpected trailing data after the JSON object")
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("parse answers file: object is empty (name at least one paused node)")
	}
	entries := make(map[string]AnswerEntry, len(raw))
	for name, body := range raw {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("parse answers file: node name must not be empty")
		}
		var entry AnswerEntry
		strict := json.NewDecoder(bytes.NewReader(body))
		strict.DisallowUnknownFields()
		if err := strict.Decode(&entry); err != nil {
			return nil, fmt.Errorf("parse answers file: node %q: %w", name, err)
		}
		if strict.More() {
			return nil, fmt.Errorf("parse answers file: node %q: unexpected trailing data in entry", name)
		}
		if entry.Decision == "" && entry.Outcome == "" && entry.Reason == "" && len(entry.Payload) == 0 {
			return nil, fmt.Errorf("parse answers file: node %q: entry is empty (want a decision, an outcome, or a payload)", name)
		}
		entries[name] = entry
	}
	return entries, nil
}

// NewAnswers builds a resumer that resolves pauses from pre-populated entries
// (the --answers <file> path). Node semantics are validated by the caller
// against the compiled graph before the run starts; the resumer still fails
// loudly for a node that is missing from the map instead of guessing.
func NewAnswers(entries map[string]AnswerEntry, opts Options) LocalResumer { //nolint:gocritic // Options is a config struct; callers pass by value intentionally
	opts.applyDefaults()
	return &resumer{mode: ModeAnswers, opts: opts, answers: entries}
}

// Interactive reports whether r prompts an operator interactively (the stdin
// path). The apply command uses this to decide whether an unanswered answers
// entry can fall back to the prompt and to dismiss abandoned prompts cleanly.
func Interactive(r LocalResumer) bool {
	if res, ok := r.(*resumer); ok {
		return res.mode == ModeStdin
	}
	return false
}

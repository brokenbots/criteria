package localresume_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/cli/localresume"
)

// --- ParseAnswers (structure) ---

func TestParseAnswers_Valid(t *testing.T) {
	entries, err := localresume.ParseAnswers([]byte(`{
		"review": {"decision": "approved"},
		"reject-node": {"decision": "rejected", "reason": "not ready"},
		"gate": {"outcome": "proceeded", "payload": {"ticket": "123"}}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries["review"].Decision != "approved" {
		t.Errorf("review decision = %q, want approved", entries["review"].Decision)
	}
	if entries["reject-node"].Reason != "not ready" {
		t.Errorf("reject-node reason = %q, want %q", entries["reject-node"].Reason, "not ready")
	}
	if entries["gate"].Outcome != "proceeded" || entries["gate"].Payload["ticket"] != "123" {
		t.Errorf("gate entry = %+v", entries["gate"])
	}
}

func TestParseAnswers_Errors(t *testing.T) {
	cases := map[string]string{
		"not an object":       `["review"]`,
		"malformed JSON":      `{"review": {`,
		"unknown field":       `{"review": {"decision":"approved","extra":1}}`,
		"non-string payload":  `{"gate": {"outcome":"ok","payload":{"k":2}}}`,
		"empty entry":         `{"review": {}}`,
		"empty node name":     `{"": {"decision":"approved"}}`,
		"trailing data":       `{"review": {"decision":"approved"}} {"gate": {}}`,
		"trailing in entry":   `{"review": {"decision":"approved"} } trailing`,
		"non-string decision": `{"review": {"decision": 3}}`,
	}
	for name, doc := range cases {
		_, err := localresume.ParseAnswers([]byte(doc))
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// --- NewAnswers (resolution) ---

func TestAnswersMode_Approval_Approved(t *testing.T) {
	stateDir := t.TempDir()
	entries, err := localresume.ParseAnswers([]byte(`{"review": {"decision": "approved"}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeApproval(context.Background(), "run-a1", "review", []string{"alice"}, "ship it")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "approved" {
		t.Errorf("decision = %v, want approved", payload)
	}
	// Decisions persist exactly like every other mode (reattach safety).
	assertDecisionPersisted(t, stateDir, "run-a1", "review", "approved", "")
}

func TestAnswersMode_Approval_RejectedWithReasonAndPayload(t *testing.T) {
	stateDir := t.TempDir()
	entries, err := localresume.ParseAnswers([]byte(
		`{"review": {"decision": "rejected", "reason": "fix the tests", "payload": {"actor": "alice"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeApproval(context.Background(), "run-a2", "review", nil, "ship it")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "rejected" || payload["reason"] != "fix the tests" || payload["actor"] != "alice" {
		t.Errorf("payload = %v", payload)
	}
	assertDecisionPersisted(t, stateDir, "run-a2", "review", "rejected", "")
}

func TestAnswersMode_Approval_MissingNode_Unanswered(t *testing.T) {
	entries, err := localresume.ParseAnswers([]byte(`{"other": {"decision": "approved"}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: t.TempDir()})
	_, err = r.ResumeApproval(context.Background(), "run-a3", "review", nil, "")
	if err == nil {
		t.Fatal("expected ErrUnanswered for a node without an entry, got nil")
	}
	if !localresume.IsUnanswered(err) {
		t.Errorf("error should wrap ErrUnanswered, got: %v", err)
	}
	if !strings.Contains(err.Error(), "review") {
		t.Errorf("error should name the node, got: %v", err)
	}
}

func TestAnswersMode_Approval_EntryWithoutDecision(t *testing.T) {
	entries, err := localresume.ParseAnswers([]byte(`{"review": {"payload": {"k": "v"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: t.TempDir()})
	_, err = r.ResumeApproval(context.Background(), "run-a4", "review", nil, "")
	if err == nil || localresume.IsUnanswered(err) {
		t.Fatalf("expected a non-Unanswered malformed-entry error, got: %v", err)
	}
	if !strings.Contains(err.Error(), `must set "decision"`) {
		t.Errorf("error should require a decision, got: %v", err)
	}
}

func TestAnswersMode_Signal_Outcome(t *testing.T) {
	stateDir := t.TempDir()
	entries, err := localresume.ParseAnswers([]byte(`{"gate": {"outcome": "failure", "payload": {"code": "42"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeSignal(context.Background(), "run-a5", "gate", "proceed", []string{"success", "failure"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["outcome"] != "failure" || payload["code"] != "42" {
		t.Errorf("payload = %v", payload)
	}
	assertDecisionPersisted(t, stateDir, "run-a5", "gate", "", "failure")
}

func TestAnswersMode_Signal_MissingNode_Unanswered(t *testing.T) {
	entries, err := localresume.ParseAnswers([]byte(`{"review": {"decision": "approved"}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: t.TempDir()})
	_, err = r.ResumeSignal(context.Background(), "run-a6", "gate", "proceed", []string{"success"})
	if !localresume.IsUnanswered(err) {
		t.Fatalf("expected ErrUnanswered, got: %v", err)
	}
	if !strings.Contains(err.Error(), `gate`) {
		t.Errorf("error should name the node, got: %v", err)
	}
}

// A wait entry without an outcome is malformed: it fails at the pause point
// (the start-of-run graph validation normally catches it earlier).
func TestAnswersMode_Signal_EntryWithoutOutcome(t *testing.T) {
	entries, err := localresume.ParseAnswers([]byte(`{"gate": {"payload": {"k": "v"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: t.TempDir()})
	_, err = r.ResumeSignal(context.Background(), "run-a7", "gate", "proceed", []string{"success", "failure"})
	if err == nil || localresume.IsUnanswered(err) {
		t.Fatalf("expected a non-Unanswered error, got: %v", err)
	}
	if !strings.Contains(err.Error(), `must set "outcome"`) {
		t.Errorf("error should require an outcome, got: %v", err)
	}
}

// A persisted decision (reattach) wins over the answers entry, as for every
// other mode.
func TestAnswersMode_PersistedDecisionWins(t *testing.T) {
	stateDir := t.TempDir()
	persistDecisionForTest(t, stateDir, "run-a8", "review", "rejected")
	entries, err := localresume.ParseAnswers([]byte(`{"review": {"decision": "approved"}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := localresume.NewAnswers(entries, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeApproval(context.Background(), "run-a8", "review", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "rejected" {
		t.Errorf("persisted decision should win, got %v", payload)
	}
}

func TestInteractive(t *testing.T) {
	if !localresume.Interactive(localresume.New(localresume.ModeStdin, localresume.Options{})) {
		t.Error("stdin mode must be interactive")
	}
	entries, err := localresume.ParseAnswers([]byte(`{"review": {"decision": "approved"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if localresume.Interactive(localresume.NewAnswers(entries, localresume.Options{})) {
		t.Error("answers mode must not be interactive")
	}
	if localresume.Interactive(localresume.New(localresume.ModeAutoApprove, localresume.Options{})) {
		t.Error("auto-approve must not be interactive")
	}
}

func TestParseMode_RejectsAnswers(t *testing.T) {
	if _, err := localresume.ParseMode(string(localresume.ModeAnswers)); err == nil {
		t.Fatal("answers mode must not be selectable via CRITERIA_LOCAL_APPROVAL")
	}
}

// persistDecisionForTest seeds a prior decision at the shared decision path so
// answers mode can be checked against reattach semantics.
func persistDecisionForTest(t *testing.T, stateDir, runID, nodeName, decision string) {
	t.Helper()
	dir := filepath.Join(stateDir, "runs", runID, "approvals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir decision dir: %v", err)
	}
	body := fmt.Sprintf(`{"decision": %q, "decided_at": "seeded"}`, decision)
	if err := os.WriteFile(filepath.Join(dir, nodeName+".json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
}

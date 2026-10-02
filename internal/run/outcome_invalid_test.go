package run

// outcome_invalid_test.go — KB-45 coverage for the StepOutcomeInvalid event
// across the sink stack: Sink publishes the pb payload verbatim, MultiSink
// fans it out, LocalSink encodes it as ND-JSON, and ConsoleSink renders the
// rejection reason with per-issue lines. Also covers the ExecuteResult
// comment surface (KB-45 item 6): non-empty comments render, empty ones stay
// invisible.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSink_OnStepOutcomeInvalid_PublishesPayload(t *testing.T) {
	fp := &fakePublisher{}
	sink := &Sink{RunID: "run-invalid-1", Client: fp}

	issues := []string{"field \"summary\": required but missing", "unmapped outcome \"weird\""}
	sink.OnStepOutcomeInvalid("audit_step", "success", issues, 2)

	if got := len(fp.published); got != 1 {
		t.Fatalf("published envelopes: got %d want 1", got)
	}
	invalid := fp.published[0].GetStepOutcomeInvalid()
	if invalid == nil {
		t.Fatalf("payload type: want StepOutcomeInvalid, got %T", fp.published[0].GetPayload())
	}
	if invalid.GetStep() != "audit_step" || invalid.GetOutcome() != "success" {
		t.Errorf("step/outcome: got %q/%q", invalid.GetStep(), invalid.GetOutcome())
	}
	if invalid.GetAttempt() != 2 {
		t.Errorf("attempt: got %d want 2", invalid.GetAttempt())
	}
	if got := strings.Join(invalid.GetIssues(), "|"); got != strings.Join(issues, "|") {
		t.Errorf("issues: got %q want %q", got, strings.Join(issues, "|"))
	}
	if fp.published[0].GetRunId() != "run-invalid-1" {
		t.Errorf("envelope run_id: got %q", fp.published[0].GetRunId())
	}
}

func TestSink_OnStepOutcome_CarriesComment(t *testing.T) {
	fp := &fakePublisher{}
	sink := &Sink{RunID: "run-comment-1", Client: fp}

	sink.OnStepOutcome("audit_step", "success", 5*time.Millisecond, nil, "done from adapter")
	if got := len(fp.published); got != 1 {
		t.Fatalf("published envelopes: got %d want 1", got)
	}
	outcome := fp.published[0].GetStepOutcome()
	if outcome == nil {
		t.Fatalf("payload type: want StepOutcome, got %T", fp.published[0].GetPayload())
	}
	if outcome.GetComment() != "done from adapter" {
		t.Errorf("comment: got %q", outcome.GetComment())
	}
}

func TestMultiSink_OnStepOutcomeInvalid_FansOut(t *testing.T) {
	first, second := &recordingSink{}, &recordingSink{}
	ms := NewMultiSink(first, nil, second)

	ms.OnStepOutcomeInvalid("audit_step", "success", []string{"issue-1"}, 3)

	for name, s := range map[string]*recordingSink{"first": first, "second": second} {
		if got := s.calls.Load(); got != 1 {
			t.Errorf("%s child calls: got %d want 1", name, got)
		}
	}
}

func TestLocalSink_OnStepOutcomeInvalid_NDJSON(t *testing.T) {
	var buf bytes.Buffer
	sink := &LocalSink{RunID: "run-invalid-2", Out: &buf}

	sink.OnStepOutcomeInvalid("audit_step", "success", []string{"required but missing"}, 2)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("line count: got %d want 1", len(lines))
	}
	var got sinkLine
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.PayloadType != "StepOutcomeInvalid" {
		t.Errorf("payload_type: got %q", got.PayloadType)
	}
	if !strings.Contains(string(got.Payload), `"attempt":2`) {
		t.Errorf("attempt not encoded: %s", got.Payload)
	}
	if !strings.Contains(string(got.Payload), `required but missing`) {
		t.Errorf("issues not encoded: %s", got.Payload)
	}
}

func TestConsoleSink_OnStepOutcomeInvalid_RendersIssues(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"audit_step"}, false, nil)
	sink.OnRunStarted("wf", "audit_step")
	sink.OnStepEntered("audit_step", "noop", 1)

	sink.OnStepOutcomeInvalid("audit_step", "success", []string{"field \"summary\": required", "unmapped outcome \"weird\""}, 2)

	out := stripANSI(buf.String())
	if !strings.Contains(out, "attempt 2 rejected by outcome contract (outcome \"success\")") {
		t.Errorf("missing rejection header:\n%s", out)
	}
	for _, want := range []string{"field \"summary\": required", "unmapped outcome \"weird\""} {
		if !strings.Contains(out, want) {
			t.Errorf("missing issue line %q:\n%s", want, out)
		}
	}
}

func TestConsoleSink_OnStepOutcome_CommentVisibility(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"only"}, false, nil)
	sink.OnRunStarted("wf", "only")
	sink.OnStepEntered("only", "demo", 1)
	sink.OnStepOutcome("only", "success", 10*time.Millisecond, nil, "adapter said done")
	sink.OnStepOutcome("only", "success", 10*time.Millisecond, nil, "")

	out := stripANSI(buf.String())
	if !strings.Contains(out, `comment="adapter said done"`) {
		t.Errorf("non-empty comment not rendered:\n%s", out)
	}
	// The second (empty-comment) outcome line must not carry a comment tag.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	emptyLine := lines[len(lines)-1]
	if strings.Contains(emptyLine, "comment=") {
		t.Errorf("empty comment rendered as tag: %q", emptyLine)
	}
}

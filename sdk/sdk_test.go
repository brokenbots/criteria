package criteria_test

import (
	"testing"

	"github.com/brokenbots/criteria/events"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestPayloadAliasIdentity verifies that the SDK type aliases are truly
// identical to the underlying pb types — no conversion needed.
func TestPayloadAliasIdentity(t *testing.T) {
	// Assign *pb.RunStarted to *criteria.RunStarted and back.
	orig := &pb.RunStarted{WorkflowName: "test-workflow"}
	var sdk *criteria.RunStarted = orig
	var roundtrip *pb.RunStarted = sdk
	if roundtrip != orig {
		t.Fatal("round-trip pointer identity broken for RunStarted")
	}

	// Envelope alias
	env := &pb.Envelope{RunId: "run-1"}
	var sdkEnv *criteria.Envelope = env
	if sdkEnv.RunId != "run-1" {
		t.Fatalf("Envelope alias field mismatch: got %q", sdkEnv.RunId)
	}
}

// TestNewEnvelopeAcceptsAllSDKPayloads checks that NewEnvelope does not panic
// for any payload type exported by the SDK.
func TestNewEnvelopeAcceptsAllSDKPayloads(t *testing.T) {
	const runID = "run-test"
	cases := []struct {
		name    string
		payload any
	}{
		{"RunStarted", &criteria.RunStarted{}},
		{"RunCompleted", &criteria.RunCompleted{}},
		{"RunFailed", &criteria.RunFailed{}},
		{"StepEntered", &criteria.StepEntered{}},
		{"StepOutcome", &criteria.StepOutcome{}},
		{"StepTransition", &criteria.StepTransition{}},
		{"StepLog", &criteria.StepLog{}},
		{"StepResumed", &criteria.StepResumed{}},
		{"StepOutputCaptured", &criteria.StepOutputCaptured{}},
		{"WaitEntered", &criteria.WaitEntered{}},
		{"WaitResumed", &criteria.WaitResumed{}},
		{"ApprovalRequested", &criteria.ApprovalRequested{}},
		{"ApprovalDecision", &criteria.ApprovalDecision{}},
		{"BranchEvaluated", &criteria.BranchEvaluated{}},
		{"ForEachEntered", &criteria.ForEachEntered{}},
		{"StepIterationStarted", &criteria.StepIterationStarted{}},
		{"StepIterationCompleted", &criteria.StepIterationCompleted{}},
		{"StepIterationItem", &criteria.StepIterationItem{}},
		{"ScopeIterCursorSet", &criteria.ScopeIterCursorSet{}},
		{"VariableSet", &criteria.VariableSet{}},
		{"CriteriaHeartbeat", &criteria.CriteriaHeartbeat{}},
		{"CriteriaDisconnected", &criteria.CriteriaDisconnected{}},
		{"WatchReady", &criteria.WatchReady{}},
		{"AdapterEvent", &criteria.AdapterEvent{}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewEnvelope panicked for %s: %v", tc.name, r)
				}
			}()
			env := criteria.NewEnvelope(runID, tc.payload)
			if env == nil {
				t.Fatal("NewEnvelope returned nil")
			}
			if env.RunId != runID {
				t.Fatalf("expected RunId %q, got %q", runID, env.RunId)
			}
			if criteria.TypeString(env) == "" {
				t.Fatalf("TypeString empty for %s — payload likely not set", tc.name)
			}
		})
	}
}

// TestTypeStringMatchesEvents verifies that criteria.TypeString delegates
// faithfully to the underlying events.TypeString implementation.
func TestTypeStringMatchesEvents(t *testing.T) {
	cases := []struct {
		payload  any
		wantType string
	}{
		{&criteria.RunStarted{}, "run.started"},
		{&criteria.RunCompleted{}, "run.completed"},
		{&criteria.RunFailed{}, "run.failed"},
		{&criteria.StepLog{}, "step.log"},
		{&criteria.WaitEntered{}, "wait.entered"},
		{&criteria.BranchEvaluated{}, "branch.evaluated"},
		{&criteria.ForEachEntered{}, "for_each.entered"},
		{&criteria.AdapterEvent{}, "adapter.event"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.wantType, func(t *testing.T) {
			env := criteria.NewEnvelope("run-1", tc.payload)
			got := criteria.TypeString(env)
			if got != tc.wantType {
				t.Errorf("TypeString = %q, want %q", got, tc.wantType)
			}
			// Also verify that the SDK wrapper matches the underlying implementation.
			underlying := events.TypeString(env)
			if got != underlying {
				t.Errorf("SDK TypeString %q != events.TypeString %q", got, underlying)
			}
		})
	}
}

// TestSchemaVersionExportedAsConstant ensures SchemaVersion is exported as a
// package-level constant equal to the underlying events.SchemaVersion.
func TestSchemaVersionExportedAsConstant(t *testing.T) {
	if criteria.SchemaVersion != events.SchemaVersion {
		t.Errorf("criteria.SchemaVersion = %d, events.SchemaVersion = %d",
			criteria.SchemaVersion, events.SchemaVersion)
	}
	if criteria.SchemaVersion != 1 {
		t.Errorf("expected SchemaVersion = 1, got %d", criteria.SchemaVersion)
	}
}

// TestIsTerminalDelegates verifies criteria.IsTerminal matches the underlying
// events.IsTerminal for terminal and non-terminal payloads.
func TestIsTerminalDelegates(t *testing.T) {
	terminal := criteria.NewEnvelope("r", &criteria.RunCompleted{})
	if !criteria.IsTerminal(terminal) {
		t.Error("expected RunCompleted to be terminal")
	}
	failed := criteria.NewEnvelope("r", &criteria.RunFailed{})
	if !criteria.IsTerminal(failed) {
		t.Error("expected RunFailed to be terminal")
	}
	nonTerminal := criteria.NewEnvelope("r", &criteria.StepLog{})
	if criteria.IsTerminal(nonTerminal) {
		t.Error("expected StepLog to be non-terminal")
	}
	if criteria.IsTerminal(nil) {
		t.Error("expected nil to be non-terminal")
	}
}

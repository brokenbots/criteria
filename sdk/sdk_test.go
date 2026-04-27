package overseer_test

import (
	"testing"

	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
	"github.com/brokenbots/overseer/events"
	overseer "github.com/brokenbots/overseer/sdk"
)

// TestPayloadAliasIdentity verifies that the SDK type aliases are truly
// identical to the underlying pb types — no conversion needed.
func TestPayloadAliasIdentity(t *testing.T) {
	// Assign *pb.RunStarted to *overseer.RunStarted and back.
	orig := &pb.RunStarted{WorkflowName: "test-workflow"}
	var sdk *overseer.RunStarted = orig
	var roundtrip *pb.RunStarted = sdk
	if roundtrip != orig {
		t.Fatal("round-trip pointer identity broken for RunStarted")
	}

	// Envelope alias
	env := &pb.Envelope{RunId: "run-1"}
	var sdkEnv *overseer.Envelope = env
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
		{"RunStarted", &overseer.RunStarted{}},
		{"RunCompleted", &overseer.RunCompleted{}},
		{"RunFailed", &overseer.RunFailed{}},
		{"StepEntered", &overseer.StepEntered{}},
		{"StepOutcome", &overseer.StepOutcome{}},
		{"StepTransition", &overseer.StepTransition{}},
		{"StepLog", &overseer.StepLog{}},
		{"StepResumed", &overseer.StepResumed{}},
		{"StepOutputCaptured", &overseer.StepOutputCaptured{}},
		{"WaitEntered", &overseer.WaitEntered{}},
		{"WaitResumed", &overseer.WaitResumed{}},
		{"ApprovalRequested", &overseer.ApprovalRequested{}},
		{"ApprovalDecision", &overseer.ApprovalDecision{}},
		{"BranchEvaluated", &overseer.BranchEvaluated{}},
		{"ForEachEntered", &overseer.ForEachEntered{}},
		{"ForEachIteration", &overseer.ForEachIteration{}},
		{"ForEachOutcome", &overseer.ForEachOutcome{}},
		{"ScopeIterCursorSet", &overseer.ScopeIterCursorSet{}},
		{"VariableSet", &overseer.VariableSet{}},
		{"OverseerHeartbeat", &overseer.OverseerHeartbeat{}},
		{"OverseerDisconnected", &overseer.OverseerDisconnected{}},
		{"WatchReady", &overseer.WatchReady{}},
		{"AdapterEvent", &overseer.AdapterEvent{}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewEnvelope panicked for %s: %v", tc.name, r)
				}
			}()
			env := overseer.NewEnvelope(runID, tc.payload)
			if env == nil {
				t.Fatal("NewEnvelope returned nil")
			}
			if env.RunId != runID {
				t.Fatalf("expected RunId %q, got %q", runID, env.RunId)
			}
			if overseer.TypeString(env) == "" {
				t.Fatalf("TypeString empty for %s — payload likely not set", tc.name)
			}
		})
	}
}

// TestTypeStringMatchesEvents verifies that overseer.TypeString delegates
// faithfully to the underlying events.TypeString implementation.
func TestTypeStringMatchesEvents(t *testing.T) {
	cases := []struct {
		payload  any
		wantType string
	}{
		{&overseer.RunStarted{}, "run.started"},
		{&overseer.RunCompleted{}, "run.completed"},
		{&overseer.RunFailed{}, "run.failed"},
		{&overseer.StepLog{}, "step.log"},
		{&overseer.WaitEntered{}, "wait.entered"},
		{&overseer.BranchEvaluated{}, "branch.evaluated"},
		{&overseer.ForEachEntered{}, "for_each.entered"},
		{&overseer.AdapterEvent{}, "adapter.event"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.wantType, func(t *testing.T) {
			env := overseer.NewEnvelope("run-1", tc.payload)
			got := overseer.TypeString(env)
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
	if overseer.SchemaVersion != events.SchemaVersion {
		t.Errorf("overseer.SchemaVersion = %d, events.SchemaVersion = %d",
			overseer.SchemaVersion, events.SchemaVersion)
	}
	if overseer.SchemaVersion != 1 {
		t.Errorf("expected SchemaVersion = 1, got %d", overseer.SchemaVersion)
	}
}

// TestIsTerminalDelegates verifies overseer.IsTerminal matches the underlying
// events.IsTerminal for terminal and non-terminal payloads.
func TestIsTerminalDelegates(t *testing.T) {
	terminal := overseer.NewEnvelope("r", &overseer.RunCompleted{})
	if !overseer.IsTerminal(terminal) {
		t.Error("expected RunCompleted to be terminal")
	}
	failed := overseer.NewEnvelope("r", &overseer.RunFailed{})
	if !overseer.IsTerminal(failed) {
		t.Error("expected RunFailed to be terminal")
	}
	nonTerminal := overseer.NewEnvelope("r", &overseer.StepLog{})
	if overseer.IsTerminal(nonTerminal) {
		t.Error("expected StepLog to be non-terminal")
	}
	if overseer.IsTerminal(nil) {
		t.Error("expected nil to be non-terminal")
	}
}

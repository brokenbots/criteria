package criteria_test

import (
	"testing"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestWorkflowAssignmentContract verifies the operator-side ServerService
// request/response shapes and the assignment-state enum exposed by the
// generated SDK bindings.
func TestWorkflowAssignmentContract(t *testing.T) {
	states := map[string]criteriav1.WorkflowAssignmentState{
		"QUEUED":   criteriav1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		"LEASED":   criteriav1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED,
		"TERMINAL": criteriav1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL,
		"REJECTED": criteriav1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED,
	}

	for want, got := range states {
		if got.String() != "WORKFLOW_ASSIGNMENT_STATE_"+want {
			t.Errorf("state %s string = %q", want, got.String())
		}
	}

	req := &criteriav1.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "hello",
		WorkflowSource: "workflow {}",
		Labels:         map[string]string{"env": "test"},
		IdempotencyKey: "idem-1",
	}
	if req.GetWorkflowName() != "hello" {
		t.Errorf("WorkflowName = %q", req.GetWorkflowName())
	}
	if req.GetIdempotencyKey() != "idem-1" {
		t.Errorf("IdempotencyKey = %q", req.GetIdempotencyKey())
	}
	if len(req.GetLabels()) != 1 || req.GetLabels()["env"] != "test" {
		t.Errorf("Labels = %v", req.GetLabels())
	}

	resp := &criteriav1.SubmitWorkflowAssignmentResponse{
		RunId:            "run-123",
		State:            criteriav1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED,
		RejectionReason:  "bad",
		LeasedCriteriaId: "crit-1",
	}
	if resp.GetRunId() != "run-123" {
		t.Errorf("RunId = %q", resp.GetRunId())
	}
	if resp.GetState() != criteriav1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
		t.Errorf("State = %v", resp.GetState())
	}

	disp := &criteriav1.GetAssignmentDispositionResponse{
		RunId:            "run-123",
		State:            criteriav1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED,
		RejectionReason:  "",
		LeasedCriteriaId: "crit-1",
	}
	if disp.GetRunId() != "run-123" || disp.GetLeasedCriteriaId() != "crit-1" {
		t.Errorf("Disposition fields mismatched: run_id=%q leased=%q", disp.GetRunId(), disp.GetLeasedCriteriaId())
	}
}

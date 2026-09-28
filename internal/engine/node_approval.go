package engine

import (
	"context"
	"fmt"

	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/workflow"
)

type approvalNode struct {
	node *workflow.ApprovalNode
}

func (n *approvalNode) Name() string { return n.node.Name }

// Evaluate implements Node for an approval node.
//
// Three entry conditions:
//   - First entry (PendingSignal == "" and ResumePayload == nil):
//     emits ApprovalRequested, sets PendingSignal = node.Name, returns ErrPaused.
//   - Crash-reattach (PendingSignal == node.Name, ResumePayload == nil):
//     re-emits ApprovalRequested, returns ErrPaused so the run stays blocked.
//   - Resume (ResumePayload != nil):
//     validates payload["decision"] against the node's outcomes before
//     consuming the resume context, then clears ResumePayload/PendingSignal,
//     emits ApprovalDecision, and returns the matched outcome. Unknown
//     decision values return an error and leave ResumePayload/PendingSignal
//     intact so a retry or reattach sees the same context (CRI-56).
func (n *approvalNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	if st.ResumePayload != nil {
		// Resumed: orchestrator delivered a decision. Validate it before
		// consuming the resume context (CRI-56): the payload fields are
		// cleared only once the decision is known-valid and the node commits
		// to its outcome.
		payload := st.ResumePayload
		decision := payload["decision"]
		target, ok := n.node.Outcomes[decision]
		if !ok {
			return "", fmt.Errorf("approval %q: unknown decision %q (expected \"approved\" or \"rejected\")", n.node.Name, decision)
		}

		st.ResumePayload = nil
		st.PendingSignal = ""
		deps.Sink.OnApprovalDecision(n.node.Name, decision, payload["actor"], payload)
		return target, nil
	}

	// First entry or crash-reattach: pause and wait for a decision.
	st.PendingSignal = n.node.Name
	deps.Sink.OnApprovalRequested(n.node.Name, n.node.Approvers, n.node.Reason)
	return "", engineruntime.ErrPaused
}

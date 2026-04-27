package engine

import (
	"context"
	"fmt"

	engineruntime "github.com/brokenbots/overlord/overseer/internal/engine/runtime"
	"github.com/brokenbots/overlord/workflow"
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
//     reads payload["decision"], emits ApprovalDecision, returns matched outcome.
//     Unknown decision values return an error.
func (n *approvalNode) Evaluate(ctx context.Context, st *RunState, deps Deps) (string, error) {
	if st.ResumePayload != nil {
		// Resumed: orchestrator delivered a decision.
		payload := st.ResumePayload
		st.ResumePayload = nil
		st.PendingSignal = ""

		decision := payload["decision"]
		actor := payload["actor"]
		deps.Sink.OnApprovalDecision(n.node.Name, decision, actor, payload)

		target, ok := n.node.Outcomes[decision]
		if !ok {
			return "", fmt.Errorf("approval %q: unknown decision %q (expected \"approved\" or \"rejected\")", n.node.Name, decision)
		}
		return target, nil
	}

	// First entry or crash-reattach: pause and wait for a decision.
	st.PendingSignal = n.node.Name
	deps.Sink.OnApprovalRequested(n.node.Name, n.node.Approvers, n.node.Reason)
	return "", engineruntime.ErrPaused
}

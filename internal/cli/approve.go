package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// NewApproveCmd delivers an approval decision or a signal outcome to a
// paused local run over its control listener (CRI-255): the primary
// replacement for the CRITERIA_LOCAL_APPROVAL file protocol.
func NewApproveCmd() *cobra.Command {
	var (
		runID    string
		signal   string
		decision string
		outcome  string
		actor    string
		payload  []string
	)
	cmd := &cobra.Command{
		Use:   "approve",
		Short: "Resolve a paused approval or signal wait on a local run",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if runID == "" {
				return fmt.Errorf("--run-id is required")
			}
			addr, err := LocalControlEndpoint(runID)
			if err != nil {
				return err
			}
			if addr == "" {
				return fmt.Errorf("run %s has no local control listener; approve resolves pauses issued by this machine's criteria apply (or use a castle server)", runID)
			}
			epayload, err := parseApprovalPayload(decision, outcome, actor, payload)
			if err != nil {
				return err
			}
			resp, err := localControlServiceClientFor(addr).ResolveResume(cmd.Context(), connect.NewRequest(&pb.ResumeRequest{
				RunId:   runID,
				Signal:  signal,
				Payload: epayload,
			}))
			if err != nil {
				return fmt.Errorf("approve: %w", err)
			}
			if !resp.Msg.Accepted {
				return fmt.Errorf("approve rejected: %s", resp.Msg.Reason)
			}
			fmt.Printf("decision delivered for run %s\n", runID)
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "Run ID to resolve")
	cmd.Flags().StringVar(&signal, "signal", "", "Pending signal to satisfy: the approval node name, or the wait node's declared signal")
	cmd.Flags().StringVar(&decision, "decision", "", "Decision for an approval node: approved | rejected")
	cmd.Flags().StringVar(&outcome, "outcome", "", "Outcome for a signal wait (must be one of the wait's outcomes)")
	cmd.Flags().StringVar(&actor, "actor", "", "Identity of the approver (audit metadata)")
	cmd.Flags().StringArrayVar(&payload, "payload", nil, "Additional payload metadata: key=value (repeatable)")
	return cmd
}

// parseApprovalPayload builds the ResolveResume payload from the flag
// surface: --decision/--outcome are sugar for payload["decision"] /
// payload["outcome"], --actor for payload["actor"], and --payload entries
// merge arbitrary metadata.
func parseApprovalPayload(decision, outcome, actor string, payload []string) (map[string]string, error) {
	out := map[string]string{}
	if decision != "" {
		out["decision"] = decision
	}
	if outcome != "" {
		out["outcome"] = outcome
	}
	if actor != "" {
		out["actor"] = actor
	}
	for _, kv := range payload {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --payload %q: expected key=value", kv)
		}
		out[k] = v
	}
	return out, nil
}

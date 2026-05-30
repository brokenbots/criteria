package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func NewInspectCmd() *cobra.Command {
	var (
		flags     serverClientFlags
		runID     string
		sessionID string
	)
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Inspect the state of a run or session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if runID == "" {
				return fmt.Errorf("--run-id is required")
			}
			client, err := flags.client()
			if err != nil {
				return err
			}
			resp, err := client.InspectRun(cmd.Context(), connect.NewRequest(&pb.InspectRunRequest{
				RunId:     runID,
				SessionId: sessionID,
			}))
			if err != nil {
				return fmt.Errorf("inspect: %w", err)
			}
			renderInspect(resp.Msg)
			return nil
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&runID, "run-id", "", "Run ID to inspect")
	cmd.Flags().StringVar(&sessionID, "session", "", "Optional session ID to inspect")
	return cmd
}

func renderInspect(msg *pb.InspectRunResponse) {
	fmt.Printf("session %s (%s)\n", msg.SessionId, msg.Adapter)
	fmt.Printf("  current_step:           %s\n", msg.CurrentStep)
	fmt.Printf("  pending_permissions:    %d\n", msg.PendingPermissions)

	if msg.LastActivityAt != nil && msg.LastActivityAt.IsValid() {
		age := time.Since(msg.LastActivityAt.AsTime())
		fmt.Printf("  last_activity:          %s (%s ago)\n", msg.LastActivityAt.AsTime().Format(time.RFC3339), roundDuration(age))
	}

	if msg.StateJson != "" {
		fmt.Printf("  state summary:\n")
		renderStateJSON(msg.StateJson)
	}
}

func renderStateJSON(raw string) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		fmt.Printf("    (raw): %s\n", strings.TrimSpace(raw))
		return
	}
	for k, v := range obj {
		switch val := v.(type) {
		case string:
			fmt.Printf("    %s: %q\n", k, val)
		case []any:
			fmt.Printf("    %s: %v\n", k, val)
		default:
			fmt.Printf("    %s: %v\n", k, val)
		}
	}
}

func roundDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

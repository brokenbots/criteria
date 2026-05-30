package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
			renderInspect(os.Stdout, resp.Msg)
			return nil
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&runID, "run-id", "", "Run ID to inspect")
	cmd.Flags().StringVar(&sessionID, "session", "", "Optional session ID to inspect")
	return cmd
}

func renderInspect(w io.Writer, msg *pb.InspectRunResponse) {
	if msg.Adapter != "" {
		fmt.Fprintf(w, "session %s (%s)\n", msg.SessionId, msg.Adapter)
	} else {
		fmt.Fprintf(w, "session %s\n", msg.SessionId)
	}
	fmt.Fprintf(w, "  current_step:           %s\n", msg.CurrentStep)
	fmt.Fprintf(w, "  pending_permissions:    %d\n", msg.PendingPermissions)

	if msg.LastActivityAt != nil && msg.LastActivityAt.IsValid() {
		age := time.Since(msg.LastActivityAt.AsTime())
		fmt.Fprintf(w, "  last_activity:          %s (%s ago)\n", msg.LastActivityAt.AsTime().Format(time.RFC3339), roundDuration(age))
	}

	if msg.StateJson != "" {
		fmt.Fprintf(w, "  state summary:\n")
		renderStateJSON(w, msg.StateJson)
	}
}

func renderStateJSON(w io.Writer, raw string) {
	if raw == "" {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		fmt.Fprintf(w, "    (raw): %s\n", strings.TrimSpace(raw))
		return
	}
	for k, v := range obj {
		switch val := v.(type) {
		case string:
			fmt.Fprintf(w, "    %s: %q\n", k, val)
		case []any:
			fmt.Fprintf(w, "    %s: %v\n", k, val)
		default:
			fmt.Fprintf(w, "    %s: %v\n", k, val)
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

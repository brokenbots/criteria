package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

func NewStatusCmd() *cobra.Command {
	var castleURL string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show registered Overseers and local in-flight run details",
		RunE: func(cmd *cobra.Command, args []string) error {
			if st, err := readLocalRunState(); err == nil {
				fmt.Printf("local run: %-36s  %-20s  pid=%d\n", st.RunID, st.Workflow, st.PID)
			}
			resp, err := http.Get(castleURL + "/api/v0/overseers")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			var list []map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
				return err
			}
			for _, o := range list {
				fmt.Printf("%-36s  %-20s  %s\n", o["id"], o["name"], o["status"])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&castleURL, "castle", envOrDefault("OVERSEER_CASTLE_URL", "http://localhost:8080"), "Castle base URL (or OVERSEER_CASTLE_URL)")
	return cmd
}

func NewStopCmd() *cobra.Command {
	var (
		castleURL string
		runID     string
	)
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Request run cancellation via Castle -> Overseer WS control message",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runID == "" {
				return fmt.Errorf("--run-id is required")
			}
			req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v0/runs/%s/stop", castleURL, runID), nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("stop: %s: %s", resp.Status, string(b))
			}
			fmt.Printf("stop requested for run %s\n", runID)
			return nil
		},
	}
	cmd.Flags().StringVar(&castleURL, "castle", envOrDefault("OVERSEER_CASTLE_URL", "http://localhost:8080"), "Castle base URL (or OVERSEER_CASTLE_URL)")
	cmd.Flags().StringVar(&runID, "run-id", "", "Run ID to cancel")
	return cmd
}

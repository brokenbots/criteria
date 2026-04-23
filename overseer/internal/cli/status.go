package cli

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

func NewStatusCmd() *cobra.Command {
	var castleURL string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show registered Overseers known to a Castle",
		RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringVar(&castleURL, "castle", "http://localhost:8080", "Castle base URL")
	return cmd
}

func NewStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the Overseer (Phase 0: send SIGTERM to the running process)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("stop: send SIGINT/SIGTERM to the overseer process directly in Phase 0")
		},
	}
}

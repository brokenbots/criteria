package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func NewPauseCmd() *cobra.Command {
	var (
		flags serverClientFlags
		runID string
	)
	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Pause an active run without losing state",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if runID == "" {
				return fmt.Errorf("--run-id is required")
			}
			// CRI-255: a run executed by a local apply is controlled through
			// its loopback control listener (discovered from the run's state
			// dir); without the record (or with an explicit server target)
			// the verb rides the castle server surface as before.
			if !controlSurfaceOverridden(cmd) {
				addr, err := LocalControlEndpoint(runID)
				if err != nil {
					return err
				}
				if addr != "" {
					if _, err := localControlServiceClientFor(addr).PauseRun(cmd.Context(), &pb.PauseRunRequest{RunId: runID}); err != nil {
						return fmt.Errorf("pause: %w", err)
					}
					fmt.Printf("pause requested for run %s\n", runID)
					return nil
				}
			}
			client, err := flags.client()
			if err != nil {
				return err
			}
			if _, err := client.PauseRun(cmd.Context(), &pb.PauseRunRequest{RunId: runID}); err != nil {
				return fmt.Errorf("pause: %w", err)
			}
			fmt.Printf("pause requested for run %s\n", runID)
			return nil
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&runID, "run-id", "", "Run ID to pause")
	return cmd
}

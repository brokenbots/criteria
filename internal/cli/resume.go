package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func NewResumeCmd() *cobra.Command {
	var (
		flags serverClientFlags
		runID string
	)
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume a previously paused run",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if runID == "" {
				return fmt.Errorf("--run-id is required")
			}
			client, err := flags.client()
			if err != nil {
				return err
			}
			if _, err := client.ResumeRun(cmd.Context(), connect.NewRequest(&pb.ResumeRunRequest{RunId: runID})); err != nil {
				return fmt.Errorf("resume: %w", err)
			}
			fmt.Printf("resume requested for run %s\n", runID)
			return nil
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&runID, "run-id", "", "Run ID to resume")
	return cmd
}

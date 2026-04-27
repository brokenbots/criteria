package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
)

// castleClientFlags groups the transport flags shared by every CLI command
// that talks to Castle. Bind to a *cobra.Command with bind().
type castleClientFlags struct {
	URL      string
	CAFile   string
	CertFile string
	KeyFile  string
}

func (f *castleClientFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.URL, "castle", envOrDefault("OVERSEER_CASTLE_URL", "http://localhost:8080"), "Castle base URL (or OVERSEER_CASTLE_URL)")
	cmd.Flags().StringVar(&f.CAFile, "tls-ca", envOrDefault("OVERSEER_TLS_CA", ""), "PEM CA bundle for Castle TLS verification (or OVERSEER_TLS_CA)")
	cmd.Flags().StringVar(&f.CertFile, "tls-cert", envOrDefault("OVERSEER_TLS_CERT", ""), "PEM client certificate for mTLS (or OVERSEER_TLS_CERT)")
	cmd.Flags().StringVar(&f.KeyFile, "tls-key", envOrDefault("OVERSEER_TLS_KEY", ""), "PEM client key for mTLS (or OVERSEER_TLS_KEY)")
}

func (f *castleClientFlags) client() (overlordv1connect.CastleServiceClient, error) {
	hc, err := castleHTTPClient(f.URL, f.CAFile, f.CertFile, f.KeyFile)
	if err != nil {
		return nil, err
	}
	return overlordv1connect.NewCastleServiceClient(hc, f.URL), nil
}

func NewStatusCmd() *cobra.Command {
	var flags castleClientFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show registered Overseers and local in-flight run details",
		RunE: func(cmd *cobra.Command, args []string) error {
			if st, err := readLocalRunState(); err == nil {
				fmt.Printf("local run: %-36s  %-20s  pid=%d\n", st.RunID, st.Workflow, st.PID)
			}
			client, err := flags.client()
			if err != nil {
				return err
			}
			resp, err := client.ListOverseers(cmd.Context(), connect.NewRequest(&pb.ListOverseersRequest{}))
			if err != nil {
				return err
			}
			for _, o := range resp.Msg.Overseers {
				fmt.Printf("%-36s  %-20s  %s\n", o.OverseerId, o.Name, o.Status)
			}
			return nil
		},
	}
	flags.bind(cmd)
	return cmd
}

func NewStopCmd() *cobra.Command {
	var (
		flags  castleClientFlags
		runID  string
		reason string
	)
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Request run cancellation via Castle -> Overseer control stream",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runID == "" {
				return fmt.Errorf("--run-id is required")
			}
			client, err := flags.client()
			if err != nil {
				return err
			}
			if _, err := client.StopRun(cmd.Context(), connect.NewRequest(&pb.StopRunRequest{RunId: runID, Reason: reason})); err != nil {
				return fmt.Errorf("stop: %w", err)
			}
			fmt.Printf("stop requested for run %s\n", runID)
			return nil
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&runID, "run-id", "", "Run ID to cancel")
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason reported on the Overseer control stream")
	return cmd
}

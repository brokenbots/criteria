package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// serverClientFlags groups the transport flags shared by every CLI command
// that talks to the server. Bind to a *cobra.Command with bind().
type serverClientFlags struct {
	URL      string
	CAFile   string
	CertFile string
	KeyFile  string
}

func (f *serverClientFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.URL, "server", envOrDefault("CRITERIA_SERVER_URL", "http://localhost:8080"), "Server base URL (or CRITERIA_SERVER_URL)")
	cmd.Flags().StringVar(&f.CAFile, "tls-ca", envOrDefault("CRITERIA_TLS_CA", ""), "PEM CA bundle for server TLS verification (or CRITERIA_TLS_CA)")
	cmd.Flags().StringVar(&f.CertFile, "tls-cert", envOrDefault("CRITERIA_TLS_CERT", ""), "PEM client certificate for mTLS (or CRITERIA_TLS_CERT)")
	cmd.Flags().StringVar(&f.KeyFile, "tls-key", envOrDefault("CRITERIA_TLS_KEY", ""), "PEM client key for mTLS (or CRITERIA_TLS_KEY)")
}

func (f *serverClientFlags) client() (criteriav1connect.ServerServiceClient, error) {
	hc, err := serverHTTPClient(f.URL, f.CAFile, f.CertFile, f.KeyFile)
	if err != nil {
		return nil, err
	}
	return criteriav1connect.NewServerServiceClient(hc, f.URL), nil
}

func NewStatusCmd() *cobra.Command {
	var flags serverClientFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show registered agents and local in-flight run details",
		RunE: func(cmd *cobra.Command, args []string) error {
			if st, err := readLocalRunState(); err == nil {
				fmt.Printf("local run: %-36s  %-20s  pid=%d\n", st.RunID, st.Workflow, st.PID)
			}
			client, err := flags.client()
			if err != nil {
				return err
			}
			resp, err := client.ListAgents(cmd.Context(), connect.NewRequest(&pb.ListAgentsRequest{}))
			if err != nil {
				return err
			}
			for _, o := range resp.Msg.Agents {
				fmt.Printf("%-36s  %-20s  %s\n", o.CriteriaId, o.Name, o.Status)
			}
			return nil
		},
	}
	flags.bind(cmd)
	return cmd
}

func NewStopCmd() *cobra.Command {
	var (
		flags  serverClientFlags
		runID  string
		reason string
	)
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Request run cancellation via server -> agent control stream",
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
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason reported on the agent control stream")
	return cmd
}

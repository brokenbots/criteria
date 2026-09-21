package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/runstate"
)

// NewServeUICmd starts the loopback run-state server standalone against a
// criteria state dir: it serves the whole run list (unscoped store) plus the
// embedded run-viewer bundle at the root. One command opens the local UI.
// Control verbs answer UNIMPLEMENTED locally (CRI-255 adds them).
func NewServeUICmd() *cobra.Command {
	var (
		host string
		port int
		home string
	)

	cmd := &cobra.Command{
		Use:   "serve-ui",
		Short: "Serve the local run viewer on loopback (read-only run-state API + embedded UI)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			store := runstate.NewStore()
			if home != "" {
				store = runstate.NewStoreAt(home)
			}
			root, err := store.RunsRoot()
			if err != nil {
				return err
			}
			if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
				return fmt.Errorf("no run state directory at %s yet (no runs recorded)", root)
			}

			srv := runstate.NewServer(store)
			if viewer, err := runstate.NewViewer(); err == nil {
				srv = srv.WithViewer(viewer)
			}
			ln, err := srv.Listen(host, port)
			if err != nil {
				return err
			}
			url := fmt.Sprintf("http://%s/", ln.Addr())
			fmt.Fprintf(os.Stderr, "criteria run viewer: %s (ctrl-c to stop)\n", url)

			serveErr := make(chan error, 1)
			go func() { serveErr <- srv.Serve(ln) }()

			select {
			case err := <-serveErr:
				return err
			case <-ctx.Done():
				srv.Stop()
				<-serveErr // drain the graceful close
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Loopback bind address (non-loopback binds are refused)")
	cmd.Flags().IntVar(&port, "port", 0, "Port to bind (0 = auto-select)")
	cmd.Flags().StringVar(&home, "home", "", "Criteria home directory (default: CRITERIA_HOME or ~/.local/criteria)")
	return cmd
}

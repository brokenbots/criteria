package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/runstate"
)

// runServeUI is the serve-ui body: standalone loopback server over the
// whole state dir (unscoped store) plus the embedded run-viewer bundle,
// drained on signal. castleAddr, when set, mounts the KB-328 console
// surface: Login/Logout/ListSessions proxy to that castle control plane,
// while ResolveResume has no local run here (standalone serve-ui owns no
// run) and answers UNIMPLEMENTED, mirroring the control verbs' standalone
// behavior.
func runServeUI(ctx context.Context, store *runstate.Store, host string, port int, castleAddr string) error {
	root, err := store.RunsRoot()
	if err != nil {
		return err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("no run state directory at %s yet (no runs recorded)", root)
	}

	console, err := runstate.NewCastleConsole(strings.TrimSpace(castleAddr), nil, nil)
	if err != nil {
		return err
	}
	srv := runstate.NewServer(store).WithCastleConsole(console.Handler())
	if viewer, err := runstate.NewViewer(); err == nil {
		srv = srv.WithViewer(viewer)
	}
	ln, err := srv.Listen(host, port)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "criteria run viewer: http://%s/runview/ (ctrl-c to stop)\n", ln.Addr())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		srv.Stop(ctx)
		<-serveErr // drain the graceful close
		return nil
	}
}

// NewServeUICmd starts the loopback run-state server standalone against a
// criteria state dir: it serves the whole run list plus the embedded
// run-viewer under /runview/. One command opens the local UI. Control verbs
// answer UNIMPLEMENTED locally (CRI-255 adds them).
func NewServeUICmd() *cobra.Command {
	var (
		host       string
		port       int
		home       string
		castleAddr string
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
			return runServeUI(ctx, store, host, port, castleAddr)
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Loopback bind address (non-loopback binds are refused)")
	cmd.Flags().IntVar(&port, "port", 0, "Port to bind (0 = auto-select)")
	cmd.Flags().StringVar(&castleAddr, "castle-addr", envOrDefault("CRITERIA_CASTLE_ADDR", ""), "Castle control plane base URL the console proxies Login/Logout/ListSessions to (KB-328; login-only: ResolveResume has no local run here)")
	cmd.Flags().StringVar(&home, "home", "", "Criteria home directory (default: CRITERIA_HOME or ~/.local/criteria)")
	return cmd
}

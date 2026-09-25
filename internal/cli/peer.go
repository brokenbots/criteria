// Package cli holds the cobra subcommands for the criteria binary.
package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/peer"
)

const peerEnvDocs = `Peer-side configuration is environment-first so k8s manifests keep working:

Required:
  CRITERIA_REMOTE_HOST            host:port or unix path of the orchestrator
                                  peer stream. Required; the peer exits
                                  non-zero without it.

Transport / identity:
  CRITERIA_REMOTE_TOKEN           bearer token for the peer stream.
  CRITERIA_REMOTE_SCOPE           scope reported on supervision events.
  CRITERIA_REMOTE_DIGEST          pinned OCI digest; if a digest-addressed
                                  artifact exists in the local adapter cache
                                  it is preferred over PATH resolution.
  CRITERIA_REMOTE_TLS_CERT        client TLS certificate PEM path.
  CRITERIA_REMOTE_TLS_KEY         client TLS key PEM path.
  CRITERIA_REMOTE_CA              CA bundle PEM path for server verification.
                                  TLS_CERT/KEY/CA are all-or-nothing and use
                                  TLS 1.2+; partial settings are rejected.

Adapter identity (same precedence as the remote runner):
  CRITERIA_ADAPTER_NAME           adapter name; a manifest wins if also set.
  CRITERIA_ADAPTER_VERSION        adapter version (default "0.0.0" unless the
                                  manifest or the child reports one).
  CRITERIA_ADAPTER_BINARY         explicit child binary path (beats PATH).
  CRITERIA_ADAPTER_MANIFEST       manifest file; fills name/version and the
                                  conventional binary path when set.

Peer behavior:
  CRITERIA_PEER_CHILD_KEEPALIVE   "true" (default): the child survives host
                                  disconnects and is killed only by peer
                                  shutdown or a Control RPC. "false" is
                                  reserved for host-driven teardown.
  CRITERIA_PEER_JOURNAL_LIMIT     bounded supervision journal size
                                  (default 4096 events; replay is bounded).
  CRITERIA_PEER_BACKOFF_MIN       reconnect backoff floor (default 1s).
  CRITERIA_PEER_BACKOFF_MAX       reconnect backoff ceiling (default 30s).

  CRITERIA_LOG_LEVEL              log level: debug, info (default), warn,
                                  or error. Unknown values log a warning
                                  and fall back to info.

Every CRITERIA_REMOTE_* variable is scrubbed from the adapter child's
environment (prefix-based, including CRITERIA_REMOTE itself), so the child
cannot sniff into phone-home mode.`

// peerLogLevel maps CRITERIA_LOG_LEVEL to a slog level, warning (at the
// fallback level) on unknown values instead of failing the boot.
func peerLogLevel(log *slog.Logger, raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		log.Warn("unknown CRITERIA_LOG_LEVEL; falling back to info", "value", raw)
		return slog.LevelInfo
	}
}

func NewPeerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "peer",
		Short: "Run the engine-side peer supervisor for one adapter child",
		Long:  peerEnvDocs,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPeer(cmd.Context())
		},
	}
}

func runPeer(parent context.Context) error {
	// Structured slog JSON logging, repo convention. The level comes from
	// CRITERIA_LOG_LEVEL; unknown values warn and fall back to info.
	level := peerLogLevel(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})), os.Getenv(peer.EnvLogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("component", "peer")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := peer.LoadConfigFromEnv()
	if err != nil {
		return err
	}
	if err := cfg.Resolve(); err != nil {
		return err
	}
	log.Info("peer config resolved",
		"adapter", cfg.AdapterName,
		"binary", cfg.Binary(),
		"digest", cfg.Digest,
		"scope", cfg.Scope,
		"host", cfg.Host,
		"child_keepalive", cfg.ChildKeepAlive,
		"journal_limit", cfg.JournalLimit,
		"backoff_min", cfg.BackoffMin.String(),
		"backoff_max", cfg.BackoffMax.String(),
	)

	rt := peer.NewRuntime(cfg, log)
	if err := rt.Boot(ctx); err != nil {
		return err
	}
	log.Info("peer ready", "adapter", cfg.AdapterName, "pid", os.Getpid())

	<-ctx.Done()
	// Bounded shutdown: give the child a grace period before the loader tears
	// it down. Child keepalive semantics only matter for host disconnects, not
	// for SIGINT/SIGTERM on the peer itself.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Shutdown(shutdownCtx); err != nil {
		log.Error("peer shutdown failed", "error", err)
		return err
	}
	return nil
}

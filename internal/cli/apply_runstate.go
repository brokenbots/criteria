package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/brokenbots/criteria/internal/runstate"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// openRunEventsFile opens (append-only, CRI-125) the run's ND-JSON events
// file under <home>/runs/<runID>/events.ndjson. The run-state server reads
// this file plus run-state.json; events remain on their primary sink (stdout
// or --events-file) and are only teed here.
func openRunEventsFile(runID string) (io.Writer, func(), error) {
	d, err := runDataDir(runID)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(d, "events.ndjson"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open run events file: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

func newRunStateControlHandler(runID string, ctrl *localRunControl, cancelRun context.CancelFunc) func(id, verb string) error {
	return func(id, verb string) error {
		if id != runID {
			return runstate.ErrNotFound
		}
		switch verb {
		case "stop":
			cancelRun()
			return nil
		case "pause", "resume":
			if ctrl == nil {
				return runstate.ErrUnsupportedVerb
			}
			if verb == "pause" {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), pauseAckTimeout)
				defer cancel()
				return ctrl.pause(ctx)
			}
			return ctrl.resume()
		default:
			return runstate.ErrUnsupportedVerb
		}
	}
}

func startLocalRunStateServer(log *slog.Logger, runID, controlListenAddr string, ctrl *localRunControl, withViewer bool, cancelRun context.CancelFunc) (viewerURL string, stop func(), err error) {
	store := runstate.NewStore().Scoped(runID)
	srv := runstate.NewServer(store).WithControl(newRunStateControlHandler(runID, ctrl, cancelRun))
	if ctrl != nil {
		svc := &localControlService{ctrl: ctrl, runID: runID}
		pattern, h := criteriav1connect.NewLocalControlServiceHandler(svc)
		srv.WithLocalService(pattern, h)
	}
	if withViewer {
		viewer, viewerErr := runstate.NewViewer()
		if viewerErr == nil {
			srv = srv.WithViewer(viewer)
		}
	}
	host, port, err := resolveControlAddr(controlListenAddr)
	if err != nil {
		return "", nil, err
	}
	ln, err := srv.Listen(host, port)
	if err != nil {
		return "", nil, err
	}
	addr := ln.Addr().String()
	url := fmt.Sprintf("http://%s/runview/", ln.Addr())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	var stopOnce sync.Once

	if err := writeControlEndpoint(runID, addr); err != nil {
		log.Warn("could not publish control endpoint record; control verbs must address the listener directly", "run_id", runID, "error", err)
	}
	log.Info("local control listener available", "addr", addr, "run_id", runID)

	stop = func() {
		// Idempotent: double teardown (explicit stop plus deferred stop)
		// must not double-drain the serve error channel.
		stopOnce.Do(func() {
			removeControlEndpoint(runID)
			srv.Stop()
			<-serveErr // drain (Serve returns nil on Stop)
		})
	}
	return url, stop, nil
}

// attachLocalRunStateServer starts the loopback run-state server for runID
// and returns the context the engine should run under (cancellable by the
// server's stop verb) plus the listener address. On bind failure it logs a
// warning and returns the parent context with a nil address and a no-op
// stop: the viewer is a lifeline, not a gate. A run that pauses while no
// listener is attached waits for its state to be resolved out-of-band
// (CRITERIA_LOCAL_APPROVAL) or for an invocation restart.
func attachLocalRunStateServer(ctx context.Context, log *slog.Logger, runID, controlListenAddr string, ctrl *localRunControl, withViewer bool) (runCtx context.Context, stop func()) {
	runCtx, cancelRun := context.WithCancel(ctx)
	// srvStop is deliberately a fresh variable: binding it to the named
	// return "stop" would make the returned closure call itself (the return
	// statement assigns the closure to "stop"), recursing to a stack
	// overflow when apply tears the server down.
	url, srvStop, err := startLocalRunStateServer(log, runID, controlListenAddr, ctrl, withViewer, cancelRun)
	if err != nil {
		log.Warn("local control listener unavailable; continuing without it", "run_id", runID, "error", err)
		return ctx, cancelRun
	}
	if url != "" {
		log.Info("run viewer available", "url", url, "run_id", runID)
	}
	return runCtx, func() {
		srvStop()
		cancelRun()
	}
}

// resolveRunListenAddr merges --control-addr and the legacy --ui-port into
// the single loopback listen address of the run-state/control listener: since
// CRI-255 both surfaces ride the same socket, so an explicit --ui-port still
// pins the port when --control-addr is unset.
func resolveRunListenAddr(opts *applyOptions) string {
	if strings.TrimSpace(opts.controlAddr) != "" {
		return opts.controlAddr
	}
	if opts.uiPort != 0 {
		return fmt.Sprintf("127.0.0.1:%d", opts.uiPort)
	}
	return ""
}

// workflowSourceHash returns the sha256 hex digest of the compiled workflow
// source, recorded as the run's workflow_hash.
func workflowSourceHash(src []byte) string {
	sum := sha256.Sum256(src)
	return hex.EncodeToString(sum[:])
}

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

	"github.com/brokenbots/criteria/internal/runstate"
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

// startLocalRunStateServer binds the loopback run-state server scoped to
// runID, serves the embedded run-viewer at the root, prints the viewer URL
// to stderr, and serves until the returned stop func is called. The stop
// verb of the control handler cancels the given engine context; pause and
// resume are UNIMPLEMENTED (CRI-255 adds checkpoint-gated controls).
func startLocalRunStateServer(log *slog.Logger, runID string, port int, cancelRun context.CancelFunc) (func(), error) {
	store := runstate.NewStore().Scoped(runID)
	srv := runstate.NewServer(store).WithControl(func(id, verb string) error {
		if id != runID {
			return runstate.ErrNotFound
		}
		switch verb {
		case "stop":
			cancelRun()
			return nil
		default:
			return runstate.ErrUnsupportedVerb
		}
	})
	viewer, err := runstate.NewViewer()
	if err == nil {
		srv = srv.WithViewer(viewer)
	}
	ln, err := srv.Listen("", port) // loopback default host; port 0 = auto
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://%s/", ln.Addr())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	log.Info("run viewer available", "url", url, "run_id", runID)

	return func() {
		_ = ln.Close()
		<-serveErr // drain (Serve returns http.ErrServerClosed or the close error)
	}, nil
}

// workflowSourceHash returns the sha256 hex digest of the compiled workflow
// source, recorded as the run's workflow_hash.
func workflowSourceHash(src []byte) string {
	sum := sha256.Sum256(src)
	return hex.EncodeToString(sum[:])
}

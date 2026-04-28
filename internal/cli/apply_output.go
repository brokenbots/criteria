package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
)

type outputMode int

const (
	outputModeJSON outputMode = iota
	outputModeConcise
)

// resolveOutputMode maps the --output flag to a concrete mode. "auto" picks
// concise when stdout is a TTY, json otherwise. The resolution is independent
// of --events-file: that flag controls where ND-JSON goes, not whether
// stdout shows concise output.
func resolveOutputMode(flag string, stdout *os.File) (outputMode, error) {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "", "auto":
		if run.IsTerminal(stdout) {
			return outputModeConcise, nil
		}
		return outputModeJSON, nil
	case "concise":
		return outputModeConcise, nil
	case "json":
		return outputModeJSON, nil
	default:
		return 0, fmt.Errorf("invalid --output %q (want auto|concise|json)", flag)
	}
}

// openNDJSONWriter returns the writer for the ND-JSON LocalSink. Precedence:
// (1) --events-file path, if set; (2) stdout when mode==json; (3) io.Discard
// when mode==concise (no events file → ND-JSON is suppressed entirely).
//
// In concise mode the LocalSink still runs because it owns the canonical
// payload encoding; the bytes simply have no consumer.
func openNDJSONWriter(eventsPath string, mode outputMode) (io.Writer, func(), error) {
	if strings.TrimSpace(eventsPath) != "" {
		f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, nil, err
		}
		return f, func() { _ = f.Close() }, nil
	}
	if mode == outputModeJSON {
		return os.Stdout, func() {}, nil
	}
	return io.Discard, func() {}, nil
}

// buildLocalSink composes the engine sink for standalone mode. LocalSink
// always runs (drives the ND-JSON record and the checkpoint hook). When mode
// is concise, a ConsoleSink is added in front of stdout and the two are
// fanned out via MultiSink.
func buildLocalSink(runID string, jsonOut io.Writer, mode outputMode, steps []string, checkpointFn func(step string, attempt int)) engine.Sink {
	local := &run.LocalSink{
		RunID:        runID,
		Out:          jsonOut,
		CheckpointFn: checkpointFn,
	}
	if mode != outputModeConcise {
		return local
	}
	console := run.NewConsoleSink(os.Stdout, steps, run.ColorEnabled(os.Stdout))
	return run.NewMultiSink(local, console)
}


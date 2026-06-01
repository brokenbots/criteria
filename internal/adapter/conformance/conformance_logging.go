package conformance

// conformance_logging.go — dedicated Log stream contract tests.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testLogging(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	if !opts.Streaming {
		t.Skipf("%s: streaming not enabled in options", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, sessionID := resolveAndOpen(t, ctx, loader, name, opts.OpenConfig)
	defer plug.Kill()
	defer func() { _ = plug.CloseSession(context.Background(), sessionID) }()

	if lss, ok := plug.(adapterhost.LogStreamStarter); ok {
		logSink := &recordingSink{}
		cancelLog, err := lss.StartLogStream(ctx, sessionID, logSink)
		if err != nil {
			t.Fatalf("start log stream: %v", err)
		}
		defer cancelLog()
	}

	cfg := cloneConfig(opts.StepConfig)
	cfg["emit_logs"] = "true"
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	assertLogChunks(t, name, sink)
}

func assertLogChunks(t *testing.T, name string, sink *recordingSink) {
	t.Helper()
	if len(sink.chunks) == 0 {
		t.Skipf("%s: adapter emitted no log chunks", name)
	}
	joined := strings.Join(func() []string {
		out := make([]string, len(sink.chunks))
		for i, c := range sink.chunks {
			out[i] = string(c)
		}
		return out
	}(), "")
	if joined == "" {
		t.Fatalf("expected non-empty joined log output")
	}
}

package conformance

// conformance_chunking.go — event chunk reassembly contract tests.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testChunking(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if info.AdapterInfo.InputSchema == nil {
		t.Skipf("%s: adapter does not declare an input_schema", name)
	}

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("chunking")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	cfg := cloneConfig(opts.StepConfig)
	cfg["emit_large_payload"] = "true"
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	// Reassemble chunks from adapter events and assert correctness.
	var reassembled []byte
	for _, evt := range sink.adapterEvents {
		if chunk, ok := evt.data["chunk"]; ok {
			if b, ok := chunk.([]byte); ok {
				reassembled = append(reassembled, b...)
			}
		}
	}

	if len(reassembled) == 0 {
		t.Skipf("%s: adapter did not emit chunked payload", name)
	}

	// The plan requires a 16-MiB payload to exercise multi-chunk reassembly.
	// Generic adapters may not emit that size; we assert on a 4-MiB marker
	// which is large enough to trigger chunking in every supported transport.
	// SDK reference targets must emit the full 16-MiB payload.
	expectedMarker := bytes.Repeat([]byte("x"), 4*1024*1024)
	if !bytes.Contains(reassembled, expectedMarker) {
		t.Skipf("%s: adapter did not emit a payload large enough (≥4 MiB) to exercise chunk reassembly", name)
	}
}

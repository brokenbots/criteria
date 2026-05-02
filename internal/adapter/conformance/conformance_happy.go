package conformance

// conformance_happy.go — happy-path and streaming contract tests.

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func testHappyPath(t *testing.T, name string, factory targetFactory, opts Options) { //nolint:gocritic // W15: Options passes by value for API clarity
	t.Helper()
	target := factory(t)
	step := baseStep(name, target.Name(), opts.StepConfig)
	sink := &recordingSink{}
	res, err := executeNoPanic(t, target, context.Background(), step, sink)

	if opts.ExpectError != nil {
		if err == nil {
			t.Fatalf("expected error, got nil (outcome=%q)", res.Outcome)
		}
		if !opts.ExpectError(err) {
			t.Fatalf("expected matching error, got: %v", err)
		}
		return
	}

	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	_ = sink.totalEvents()
}

func testNilSink(t *testing.T, name string, factory targetFactory, opts Options) { //nolint:gocritic // W15: Options passes by value for API clarity
	t.Helper()
	target := factory(t)
	step := baseStep(name, target.Name(), opts.StepConfig)
	_, err := executeNoPanic(t, target, context.Background(), step, noopSink{})
	if opts.ExpectError != nil {
		if err == nil {
			t.Fatal("expected error with noop sink, got nil")
		}
		if !opts.ExpectError(err) {
			t.Fatalf("expected matching error, got: %v", err)
		}
	}
}

func testChunkedIO(t *testing.T, name string, factory targetFactory, opts Options) { //nolint:gocritic // W15: Options passes by value for API clarity
	t.Helper()
	cfg, expected, ok := chunkedIOConfig(opts.StepConfig)
	if !ok {
		t.Skip("chunked-io test skipped: no stream-producing config available")
	}
	target := factory(t)
	sink := &recordingSink{}
	step := baseStep(name, target.Name(), cfg)

	_, err := executeNoPanic(t, target, context.Background(), step, sink)
	if err != nil {
		t.Fatalf("Execute returned error for chunked IO: %v", err)
	}

	logs := sink.logChunks()
	if len(logs) <= 1 {
		t.Fatalf("expected multiple Log calls, got %d", len(logs))
	}
	joined := bytes.Join(logs, nil)
	if len(joined) <= 64*1024 {
		t.Fatalf("expected >64KiB of log output, got %d bytes", len(joined))
	}
	if !bytes.Equal(joined, expected) {
		t.Fatalf("chunked output mismatch: got %d bytes, expected %d bytes", len(joined), len(expected))
	}
}

func testNameStability(t *testing.T, name string, factory targetFactory) {
	t.Helper()
	target := factory(t)
	n1 := target.Name()
	n2 := target.Name()
	if strings.TrimSpace(n1) == "" {
		t.Fatal("adapter Name() returned empty string")
	}
	if n1 != n2 {
		t.Fatalf("adapter Name() unstable across calls: %q != %q", n1, n2)
	}
	if n1 != name {
		t.Fatalf("adapter Name() mismatch: got %q, expected %q", n1, name)
	}
}

func chunkedIOConfig(base map[string]string) (config map[string]string, expected []byte, ok bool) {
	cfg := cloneConfig(base)
	if _, hasCmd := cfg["command"]; !hasCmd {
		return nil, nil, false
	}
	if runtime.GOOS == "windows" {
		return nil, nil, false
	}

	line := strings.Repeat("x", 512)
	lineCount := 256
	expectedLine := []byte(line + "\n")
	cfg["command"] = fmt.Sprintf("i=0; while [ $i -lt %d ]; do printf '%%s\\n' '%s'; i=$((i+1)); done", lineCount, line)
	return cfg, bytes.Repeat(expectedLine, lineCount), true
}

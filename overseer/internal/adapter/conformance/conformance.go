package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

// Options configures adapter-specific conformance expectations.
type Options struct {
	// StepConfig is the HCL-style config passed to the step node under test.
	StepConfig map[string]string
	// AllowedOutcomes is the set of valid Outcome strings for this adapter.
	AllowedOutcomes []string
	// Streaming indicates the adapter is expected to emit >0 Log events.
	Streaming bool
	// ExpectError, when non-nil, asserts the adapter returns a matching error
	// (used for expected-failure adapters like the non-copilot-build stub).
	ExpectError func(error) bool
}

// Run executes the shared adapter conformance contract.
func Run(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Fatal("conformance: name is required")
	}
	if factory == nil {
		t.Fatal("conformance: factory is required")
	}

	t.Run("name_stability", func(t *testing.T) { testNameStability(t, name, factory) })
	t.Run("nil_sink", func(t *testing.T) { testNilSink(t, name, factory, opts) })
	t.Run("happy_path", func(t *testing.T) { testHappyPath(t, name, factory, opts) })

	if opts.ExpectError == nil {
		t.Run("context_cancellation", func(t *testing.T) { testCancel(t, name, factory, opts) })
		t.Run("step_timeout", func(t *testing.T) { testTimeout(t, name, factory, opts) })
		t.Run("outcome_domain", func(t *testing.T) { testOutcomeDomain(t, name, factory, opts) })
		if opts.Streaming {
			t.Run("chunked_io", func(t *testing.T) { testChunkedIO(t, name, factory, opts) })
		}
	}
}

func testHappyPath(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	a := factory()
	step := baseStep(name, a.Name(), opts.StepConfig)
	sink := &recordingSink{}
	res, err := executeNoPanic(t, a, context.Background(), step, sink)

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
	if strings.TrimSpace(res.Outcome) == "" {
		t.Fatalf("Execute returned empty outcome")
	}
	_ = sink.totalEvents()
}

func testCancel(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	defer goleak.VerifyNone(t)

	cfg, ok := longRunningConfig(opts.StepConfig)
	if !ok {
		t.Skip("cancellation test skipped: no long-running config available")
	}
	a := factory()
	step := baseStep(name, a.Name(), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	time.AfterFunc(50*time.Millisecond, cancel)

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = executeNoPanic(t, a, ctx, step, &recordingSink{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return within 500ms after cancellation")
	}

	if execErr == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(execErr, context.Canceled) && !errors.Is(execErr, context.DeadlineExceeded) {
		t.Fatalf("expected cancellation/deadline error, got: %v", execErr)
	}
}

func testTimeout(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	defer goleak.VerifyNone(t)

	cfg, ok := longRunningConfig(opts.StepConfig)
	if !ok {
		t.Skip("timeout test skipped: no long-running config available")
	}
	a := factory()
	step := baseStep(name, a.Name(), cfg)
	step.Timeout = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = executeNoPanic(t, a, ctx, step, &recordingSink{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return within 500ms after timeout")
	}

	if execErr == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(execErr, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded error, got: %v", execErr)
	}
}

func testNilSink(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	a := factory()
	step := baseStep(name, a.Name(), opts.StepConfig)
	_, err := executeNoPanic(t, a, context.Background(), step, noopSink{})
	if opts.ExpectError != nil {
		if err == nil {
			t.Fatal("expected error with noop sink, got nil")
		}
		if !opts.ExpectError(err) {
			t.Fatalf("expected matching error, got: %v", err)
		}
	}
}

func testOutcomeDomain(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	if len(opts.AllowedOutcomes) == 0 {
		t.Skip("outcome-domain test skipped: no allowed outcomes configured")
	}
	allowed := make(map[string]struct{}, len(opts.AllowedOutcomes))
	for _, outcome := range opts.AllowedOutcomes {
		allowed[outcome] = struct{}{}
	}

	a := factory()
	step := baseStep(name, a.Name(), opts.StepConfig)
	res, err := executeNoPanic(t, a, context.Background(), step, &recordingSink{})
	if err != nil {
		return
	}
	if _, ok := allowed[res.Outcome]; !ok {
		t.Fatalf("outcome %q not in allowed set %v", res.Outcome, opts.AllowedOutcomes)
	}
}

func testChunkedIO(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	cfg, expected, ok := chunkedIOConfig(opts.StepConfig)
	if !ok {
		t.Skip("chunked-io test skipped: no stream-producing config available")
	}
	a := factory()
	sink := &recordingSink{}
	step := baseStep(name, a.Name(), cfg)

	_, err := executeNoPanic(t, a, context.Background(), step, sink)
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

func testNameStability(t *testing.T, name string, factory func() adapter.Adapter) {
	t.Helper()
	a := factory()
	n1 := a.Name()
	n2 := a.Name()
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

func executeNoPanic(t *testing.T, a adapter.Adapter, ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (res adapter.Result, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("adapter %q panicked: %v", a.Name(), r)
		}
	}()
	return a.Execute(ctx, step, sink)
}

func baseStep(name, adapterName string, config map[string]string) *workflow.StepNode {
	cfg := make(map[string]string, len(config))
	for k, v := range config {
		cfg[k] = v
	}
	return &workflow.StepNode{
		Name:    name,
		Adapter: adapterName,
		Config:  cfg,
		Outcomes: map[string]string{
			"success": "done",
			"failure": "done",
		},
	}
}

func longRunningConfig(base map[string]string) (map[string]string, bool) {
	cfg := cloneConfig(base)
	if _, ok := cfg["command"]; ok {
		cfg["command"] = longRunningCommand()
		return cfg, true
	}
	return nil, false
}

func chunkedIOConfig(base map[string]string) (map[string]string, []byte, bool) {
	cfg := cloneConfig(base)
	if _, ok := cfg["command"]; !ok {
		return nil, nil, false
	}
	if runtime.GOOS == "windows" {
		return nil, nil, false
	}

	line := strings.Repeat("x", 512)
	lineCount := 256
	expectedLine := []byte(line + "\n")
	expected := bytes.Repeat(expectedLine, lineCount)

	cfg["command"] = fmt.Sprintf("i=0; while [ $i -lt %d ]; do printf '%%s\\n' '%s'; i=$((i+1)); done", lineCount, line)
	return cfg, expected, true
}

func longRunningCommand() string {
	if runtime.GOOS == "windows" {
		return "ping 127.0.0.1 -n 6 >NUL"
	}
	return "sleep 5"
}

func cloneConfig(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type noopSink struct{}

func (noopSink) Log(string, []byte)  {}
func (noopSink) Adapter(string, any) {}

type recordingSink struct {
	mu          sync.Mutex
	logEvents   int
	adapterEvts int
	chunks      [][]byte
}

func (s *recordingSink) Log(_ string, chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logEvents++
	s.chunks = append(s.chunks, append([]byte(nil), chunk...))
}

func (s *recordingSink) Adapter(string, any) {
	s.mu.Lock()
	s.adapterEvts++
	s.mu.Unlock()
}

func (s *recordingSink) totalEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logEvents + s.adapterEvts
}

func (s *recordingSink) logChunks() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.chunks))
	for i := range s.chunks {
		out[i] = append([]byte(nil), s.chunks[i]...)
	}
	return out
}

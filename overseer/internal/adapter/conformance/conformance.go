package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/workflow"
)

// Options configures adapter-specific conformance expectations.
type Options struct {
	// StepConfig is the HCL-style config passed to the step node under test.
	StepConfig map[string]string
	// PermissionConfig optionally overrides StepConfig for permission_request_shape.
	PermissionConfig map[string]string
	// AllowedOutcomes is the set of valid Outcome strings for this adapter.
	AllowedOutcomes []string
	// Streaming indicates the adapter is expected to emit >0 Log events.
	Streaming bool
	// ExpectError, when non-nil, asserts the adapter returns a matching error
	// (used for expected-failure adapters like the non-copilot-build stub).
	ExpectError func(error) bool
}

type executeTarget interface {
	Name() string
	Execute(context.Context, *workflow.StepNode, adapter.EventSink) (adapter.Result, error)
}

type targetFactory func(*testing.T) executeTarget

// Run executes the shared adapter conformance contract.
func Run(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Fatal("conformance: name is required")
	}
	if factory == nil {
		t.Fatal("conformance: factory is required")
	}

	runContractTests(t, name, opts, func(_ *testing.T) executeTarget {
		return adapterTarget{impl: factory()}
	})
}

// RunPlugin executes the shared adapter contract against a plugin binary.
func RunPlugin(t *testing.T, name, binaryPath string, opts Options) {
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Fatal("conformance: name is required")
	}
	if strings.TrimSpace(binaryPath) == "" {
		t.Fatal("conformance: binaryPath is required")
	}

	loader := plugin.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != name {
			return "", fmt.Errorf("unexpected plugin request %q (expected %q)", requested, name)
		}
		return binaryPath, nil
	})
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}
	info, err := probe.Info(ctx)
	if err != nil {
		probe.Kill()
		t.Fatalf("plugin info: %v", err)
	}
	probe.Kill()

	runContractTests(t, name, opts, newPluginTargetFactory(name, loader))

	t.Run("session_lifecycle", func(t *testing.T) {
		testSessionLifecycle(t, name, loader, opts, info)
	})
	t.Run("concurrent_sessions", func(t *testing.T) {
		testConcurrentSessions(t, name, loader, opts, info)
	})
	t.Run("session_crash_detection", func(t *testing.T) {
		testSessionCrashDetection(t, name, loader, opts, info)
	})
	t.Run("permission_request_shape", func(t *testing.T) {
		testPermissionRequestShape(t, name, loader, opts, info)
	})
}

func runContractTests(t *testing.T, name string, opts Options, factory targetFactory) {
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

func testHappyPath(t *testing.T, name string, factory targetFactory, opts Options) {
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

func testCancel(t *testing.T, name string, factory targetFactory, opts Options) {
	t.Helper()

	cfg, ok := longRunningConfig(opts.StepConfig)
	if !ok {
		t.Skip("cancellation test skipped: no long-running config available")
	}
	target := factory(t)
	if !isPluginTarget(target) {
		defer goleak.VerifyNone(t)
	}
	step := baseStep(name, target.Name(), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	time.AfterFunc(50*time.Millisecond, cancel)

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = executeNoPanic(t, target, ctx, step, &recordingSink{})
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
	if !isCancellationLikeError(execErr) {
		t.Fatalf("expected cancellation/deadline error, got: %v", execErr)
	}
}

func testTimeout(t *testing.T, name string, factory targetFactory, opts Options) {
	t.Helper()

	cfg, ok := longRunningConfig(opts.StepConfig)
	if !ok {
		t.Skip("timeout test skipped: no long-running config available")
	}
	target := factory(t)
	if !isPluginTarget(target) {
		defer goleak.VerifyNone(t)
	}
	step := baseStep(name, target.Name(), cfg)
	step.Timeout = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = executeNoPanic(t, target, ctx, step, &recordingSink{})
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
	if !isDeadlineLikeError(execErr) {
		t.Fatalf("expected deadline exceeded error, got: %v", execErr)
	}
}

func testNilSink(t *testing.T, name string, factory targetFactory, opts Options) {
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

func testOutcomeDomain(t *testing.T, name string, factory targetFactory, opts Options) {
	t.Helper()
	if len(opts.AllowedOutcomes) == 0 {
		t.Skip("outcome-domain test skipped: no allowed outcomes configured")
	}
	allowed := make(map[string]struct{}, len(opts.AllowedOutcomes))
	for _, outcome := range opts.AllowedOutcomes {
		allowed[outcome] = struct{}{}
	}

	target := factory(t)
	step := baseStep(name, target.Name(), opts.StepConfig)
	res, err := executeNoPanic(t, target, context.Background(), step, &recordingSink{})
	if err != nil {
		return
	}
	if _, ok := allowed[res.Outcome]; !ok {
		t.Fatalf("outcome %q not in allowed set %v", res.Outcome, opts.AllowedOutcomes)
	}
}

func testChunkedIO(t *testing.T, name string, factory targetFactory, opts Options) {
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

func executeNoPanic(t *testing.T, target executeTarget, ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (res adapter.Result, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("adapter %q panicked: %v", target.Name(), r)
		}
	}()
	return target.Execute(ctx, step, sink)
}

func newPluginTargetFactory(name string, loader plugin.Loader) targetFactory {
	return func(t *testing.T) executeTarget {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		plug, err := loader.Resolve(ctx, name)
		if err != nil {
			t.Fatalf("resolve plugin: %v", err)
		}
		info, err := plug.Info(ctx)
		if err != nil {
			plug.Kill()
			t.Fatalf("plugin info: %v", err)
		}

		sessionID := newSessionID("conformance")
		if err := plug.OpenSession(ctx, sessionID, nil); err != nil {
			plug.Kill()
			t.Fatalf("open session %q: %v", sessionID, err)
		}

		t.Cleanup(func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = plug.CloseSession(closeCtx, sessionID)
			closeCancel()
			plug.Kill()
		})

		return pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}
	}
}

func testSessionLifecycle(t *testing.T, name string, loader plugin.Loader, opts Options, info plugin.Info) {
	t.Helper()
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("lifecycle")
	if err := plug.OpenSession(ctx, sessionID, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}

	step := baseStep(name, info.Name, opts.StepConfig)
	res1, err := executeNoPanic(t, pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, &recordingSink{})
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	assertValidOutcome(t, res1.Outcome, opts)

	res2, err := executeNoPanic(t, pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, &recordingSink{})
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	assertValidOutcome(t, res2.Outcome, opts)

	if err := plug.CloseSession(ctx, sessionID); err != nil {
		t.Fatalf("close session: %v", err)
	}

	_, err = executeNoPanic(t, pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, &recordingSink{})
	if err == nil {
		t.Fatal("expected execute on closed session to fail")
	}
}

func testConcurrentSessions(t *testing.T, name string, loader plugin.Loader, opts Options, info plugin.Info) {
	t.Helper()
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plugA, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin A: %v", err)
	}
	defer plugA.Kill()

	plugB, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin B: %v", err)
	}
	defer plugB.Kill()

	if pidA, okA := plugin.ProcessPID(plugA); okA {
		if pidB, okB := plugin.ProcessPID(plugB); okB && pidA == pidB {
			t.Fatalf("expected distinct plugin PIDs per session, got %d", pidA)
		}
	}

	sessionA := newSessionID("concurrent-a")
	sessionB := newSessionID("concurrent-b")
	if err := plugA.OpenSession(ctx, sessionA, nil); err != nil {
		t.Fatalf("open session A: %v", err)
	}
	if err := plugB.OpenSession(ctx, sessionB, nil); err != nil {
		t.Fatalf("open session B: %v", err)
	}
	defer func() {
		_ = plugA.CloseSession(context.Background(), sessionA)
		_ = plugB.CloseSession(context.Background(), sessionB)
	}()

	targetA := pluginSessionTarget{plugin: plugA, sessionID: sessionA, name: info.Name}
	targetB := pluginSessionTarget{plugin: plugB, sessionID: sessionB, name: info.Name}

	stepConfigA := cloneConfig(opts.StepConfig)
	stepConfigA["conformance_session_marker"] = sessionA
	stepConfigB := cloneConfig(opts.StepConfig)
	stepConfigB["conformance_session_marker"] = sessionB
	stepA := baseStep(name+"-a", info.Name, stepConfigA)
	stepB := baseStep(name+"-b", info.Name, stepConfigB)

	var wg sync.WaitGroup
	var resA, resB adapter.Result
	var errA, errB error
	sinkA := &recordingSink{}
	sinkB := &recordingSink{}

	wg.Add(2)
	go func() {
		defer wg.Done()
		resA, errA = executeNoPanic(t, targetA, context.Background(), stepA, sinkA)
	}()
	go func() {
		defer wg.Done()
		resB, errB = executeNoPanic(t, targetB, context.Background(), stepB, sinkB)
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("session A execute: %v", errA)
	}
	if errB != nil {
		t.Fatalf("session B execute: %v", errB)
	}
	assertValidOutcome(t, resA.Outcome, opts)
	assertValidOutcome(t, resB.Outcome, opts)

	if sinkA.containsText(sessionB) {
		t.Fatalf("session A sink unexpectedly contains session B marker %q", sessionB)
	}
	if sinkB.containsText(sessionA) {
		t.Fatalf("session B sink unexpectedly contains session A marker %q", sessionA)
	}
}

func testSessionCrashDetection(t *testing.T, name string, loader plugin.Loader, opts Options, info plugin.Info) {
	t.Helper()
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("crash")
	if err := plug.OpenSession(ctx, sessionID, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	target := pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}
	step := baseStep(name, info.Name, opts.StepConfig)
	if _, err := executeNoPanic(t, target, context.Background(), step, &recordingSink{}); err != nil {
		t.Fatalf("initial execute before crash: %v", err)
	}

	pid, ok := plugin.ProcessPID(plug)
	if !ok || pid <= 0 {
		t.Skip("session crash detection skipped: plugin PID unavailable")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find plugin process %d: %v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill plugin process %d: %v", pid, err)
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer timeoutCancel()
	result, err := executeNoPanic(t, target, timeoutCtx, step, &recordingSink{})
	if err == nil {
		t.Fatalf("expected execute after crash to fail (outcome=%q)", result.Outcome)
	}
}

func testPermissionRequestShape(t *testing.T, name string, loader plugin.Loader, opts Options, info plugin.Info) {
	t.Helper()
	if !hasCapability(info.Capabilities, "permission_gating") {
		t.Skip("permission_request_shape skipped: plugin does not advertise permission_gating")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("permission")
	if err := plug.OpenSession(ctx, sessionID, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	cfg := opts.PermissionConfig
	if len(cfg) == 0 {
		cfg = opts.StepConfig
	}
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	res, err := executeNoPanic(t, pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, sink)
	if err != nil {
		t.Fatalf("execute with permission request config: %v", err)
	}
	if res.Outcome != "needs_review" {
		t.Fatalf("permission denial must end with needs_review, got %q", res.Outcome)
	}

	permissionEvent, ok := sink.firstAdapterEvent("permission.request")
	if !ok {
		t.Fatal("expected permission.request adapter event")
	}
	id := strings.TrimSpace(fmt.Sprint(permissionEvent["id"]))
	tool := strings.TrimSpace(fmt.Sprint(permissionEvent["tool"]))
	if id == "" {
		t.Fatal("permission request id must be non-empty")
	}
	if tool == "" {
		t.Fatal("permission request tool must be non-empty")
	}
}

func assertValidOutcome(t *testing.T, outcome string, opts Options) {
	t.Helper()
	if strings.TrimSpace(outcome) == "" {
		t.Fatal("empty outcome")
	}
	if len(opts.AllowedOutcomes) == 0 {
		return
	}
	for _, allowed := range opts.AllowedOutcomes {
		if allowed == outcome {
			return
		}
	}
	t.Fatalf("outcome %q not in allowed set %v", outcome, opts.AllowedOutcomes)
}

func hasCapability(capabilities []string, capability string) bool {
	for _, c := range capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

func isPluginTarget(target executeTarget) bool {
	_, ok := target.(pluginSessionTarget)
	return ok
}

func isCancellationLikeError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context canceled") || strings.Contains(msg, "deadline exceeded")
}

func isDeadlineLikeError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "deadline exceeded")
}

func newSessionID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

type adapterTarget struct {
	impl adapter.Adapter
}

func (a adapterTarget) Name() string {
	return a.impl.Name()
}

func (a adapterTarget) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	return a.impl.Execute(ctx, step, sink)
}

type pluginSessionTarget struct {
	plugin    plugin.Plugin
	sessionID string
	name      string
}

func (p pluginSessionTarget) Name() string {
	return p.name
}

func (p pluginSessionTarget) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	return p.plugin.Execute(ctx, p.sessionID, step, sink)
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
	if _, ok := cfg["delay_ms"]; ok {
		cfg["delay_ms"] = "5000"
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
	mu            sync.Mutex
	logEvents     int
	adapterEvts   int
	chunks        [][]byte
	adapterData   []string
	adapterEvents []recordedAdapterEvent
}

func (s *recordingSink) Log(_ string, chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logEvents++
	s.chunks = append(s.chunks, append([]byte(nil), chunk...))
}

func (s *recordingSink) Adapter(kind string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapterEvts++
	s.adapterData = append(s.adapterData, fmt.Sprint(data))
	if eventMap, ok := data.(map[string]any); ok {
		copied := make(map[string]any, len(eventMap))
		for k, v := range eventMap {
			copied[k] = v
		}
		s.adapterEvents = append(s.adapterEvents, recordedAdapterEvent{kind: kind, data: copied})
	}
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

func (s *recordingSink) containsText(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, chunk := range s.chunks {
		if bytes.Contains(chunk, []byte(text)) {
			return true
		}
	}
	for _, payload := range s.adapterData {
		if strings.Contains(payload, text) {
			return true
		}
	}
	return false
}

func (s *recordingSink) firstAdapterEvent(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.adapterEvents {
		if evt.kind == kind {
			copied := make(map[string]any, len(evt.data))
			for k, v := range evt.data {
				copied[k] = v
			}
			return copied, true
		}
	}
	return nil, false
}

type recordedAdapterEvent struct {
	kind string
	data map[string]any
}

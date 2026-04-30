package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/testutil"
	"github.com/brokenbots/criteria/workflow"
)

type recordingLoader struct {
	inner Loader

	mu      sync.Mutex
	plugins []Plugin
}

func (l *recordingLoader) Resolve(ctx context.Context, name string) (Plugin, error) {
	p, err := l.inner.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.plugins = append(l.plugins, p)
	l.mu.Unlock()
	return p, nil
}

func (l *recordingLoader) Shutdown(ctx context.Context) error { return l.inner.Shutdown(ctx) }

func (l *recordingLoader) lastPlugin() Plugin {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.plugins) == 0 {
		return nil
	}
	return l.plugins[len(l.plugins)-1]
}

type adapterEventCollector struct {
	mu     sync.Mutex
	events []adapterEvent
}

type adapterEvent struct {
	kind string
	data map[string]any
}

func (c *adapterEventCollector) Log(string, []byte) {}

func (c *adapterEventCollector) Adapter(kind string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var payload map[string]any
	if m, ok := data.(map[string]any); ok {
		payload = m
	}
	c.events = append(c.events, adapterEvent{kind: kind, data: payload})
}

func (c *adapterEventCollector) saw(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt.kind == kind {
			return true
		}
	}
	return false
}

func (c *adapterEventCollector) first(kind string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

func TestSessionManagerOpenExecuteClose(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, map[string]string{"bootstrap": "1"}); err != nil {
		t.Fatalf("open: %v", err)
	}

	res, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "run"}, &adapterEventCollector{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("outcome=%q want success", res.Outcome)
	}

	if err := sm.Close(context.Background(), "agent"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := sm.Close(context.Background(), "agent"); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
}

func TestSessionManagerUnknownExecuteAndDoubleOpen(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	_, err := sm.Execute(context.Background(), "missing", &workflow.StepNode{Name: "run"}, &adapterEventCollector{})
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("execute unknown err=%v", err)
	}
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); !errors.Is(err, ErrSessionAlreadyOpen) {
		t.Fatalf("double open err=%v", err)
	}
}

func TestSessionManagerCrashPolicyFail(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastPlugin().Kill()

	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, sink)
	if err == nil {
		t.Fatal("expected crash error")
	}
	if result.Outcome != "failure" {
		t.Fatalf("outcome=%q want failure", result.Outcome)
	}
	if !sink.saw("session.crash") {
		t.Fatal("expected session.crash adapter event")
	}
}

func TestSessionManagerCrashPolicyRespawn(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashRespawn, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastPlugin().Kill()

	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, sink)
	if err != nil {
		t.Fatalf("execute with respawn: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("outcome=%q want success", result.Outcome)
	}
	if !sink.saw("session.respawned") {
		t.Fatal("expected session.respawned adapter event")
	}
}

func TestSessionManagerCrashPolicyAbortRun(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashAbortRun, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastPlugin().Kill()

	_, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, &adapterEventCollector{})
	var fatal *FatalRunError
	if !errors.As(err, &fatal) {
		t.Fatalf("error=%v want FatalRunError", err)
	}
}

// TestSessionManagerPermissionGrantAndDeny verifies that the session manager
// emits permission.granted and permission.denied events when executing a step
// that requests multiple tools against the permissive test plugin.
func TestSessionManagerPermissionGrantAndDeny(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "permissive", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	// allow_tools = ["read_file"]; plugin requests read_file + write_file
	step := &workflow.StepNode{
		Name:       "run",
		Input:      map[string]string{"perm_tools": "read_file,write_file"},
		AllowTools: []string{"read_file"},
	}
	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", step, sink)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// permissive plugin returns needs_review when any tool is denied
	if result.Outcome != "needs_review" {
		t.Fatalf("outcome=%q want needs_review", result.Outcome)
	}
	if !sink.saw("permission.granted") {
		t.Error("expected permission.granted event for read_file")
	}
	if !sink.saw("permission.denied") {
		t.Error("expected permission.denied event for write_file")
	}
	if sink.saw("permission.request") {
		t.Error("unexpected legacy permission.request event")
	}
	granted, ok := sink.first("permission.granted")
	if !ok {
		t.Fatal("expected permission.granted payload")
	}
	if granted["pattern"] != "read_file" {
		t.Fatalf("permission.granted pattern=%v want read_file", granted["pattern"])
	}
	if granted["request_id"] == "" {
		t.Fatal("permission.granted request_id must be non-empty")
	}
	denied, ok := sink.first("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied payload")
	}
	if denied["reason"] == "" {
		t.Fatal("permission.denied reason must be non-empty")
	}
	if denied["request_id"] == "" {
		t.Fatal("permission.denied request_id must be non-empty")
	}
	// allow_tools must be echoed back in the denial payload.
	allowTools, _ := denied["allow_tools"].([]string)
	if len(allowTools) != 1 || allowTools[0] != "read_file" {
		t.Fatalf("permission.denied allow_tools=%v want [read_file]", allowTools)
	}

	_ = sm.Close(context.Background(), "agent")
}

// TestSessionManagerDenialPayloadFullContract verifies the complete set of
// fields that the host must include in every permission.denied event:
// tool, reason, request_id, and allow_tools.
func TestSessionManagerDenialPayloadFullContract(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "permissive", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	step := &workflow.StepNode{
		Name:       "run",
		Input:      map[string]string{"perm_tools": "write_file"},
		AllowTools: []string{"read_file", "shell:git *"},
	}
	sink := &adapterEventCollector{}
	if _, err := sm.Execute(context.Background(), "agent", step, sink); err != nil {
		t.Fatalf("execute: %v", err)
	}

	denied, ok := sink.first("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied event")
	}
	if denied["tool"] == "" {
		t.Fatal("permission.denied: tool must be non-empty")
	}
	if denied["reason"] == "" {
		t.Fatal("permission.denied: reason must be non-empty")
	}
	if denied["request_id"] == "" {
		t.Fatal("permission.denied: request_id must be non-empty")
	}
	allowTools, _ := denied["allow_tools"].([]string)
	if len(allowTools) != 2 {
		t.Fatalf("permission.denied allow_tools=%v want 2 entries", allowTools)
	}

	_ = sm.Close(context.Background(), "agent")
}

// TestSessionManagerCopilotAliasGrantAtHostBoundary verifies end-to-end alias
// expansion at the host boundary: allow_tools=["read_file"] must grant a
// plugin permission request for the canonical kind "read", because the host
// expands "read_file" → "read" via the copilot adapter alias map.
// A denied request for "write" must carry allow_tools and a non-empty
// suggestion field in its payload.
func TestSessionManagerCopilotAliasGrantAtHostBoundary(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	// Register the permissive test binary under the "copilot" adapter name so
	// the host applies copilot alias expansion when evaluating permission kinds.
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "copilot", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	// Plugin requests canonical kinds: "read" (should be granted via alias)
	// and "write" (should be denied: no "write_file" in allow_tools).
	step := &workflow.StepNode{
		Name:       "run",
		Input:      map[string]string{"perm_tools": "read,write"},
		AllowTools: []string{"read_file"},
	}
	sink := &adapterEventCollector{}
	if _, err := sm.Execute(context.Background(), "agent", step, sink); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// "read_file" in allow_tools aliases to "read" → must be granted.
	if !sink.saw("permission.granted") {
		t.Fatal("expected permission.granted for canonical kind 'read' via read_file alias")
	}
	granted, _ := sink.first("permission.granted")
	if granted["tool"] != "read" {
		t.Fatalf("granted tool=%v want read", granted["tool"])
	}

	// "write" has no match → must be denied.
	if !sink.saw("permission.denied") {
		t.Fatal("expected permission.denied for canonical kind 'write'")
	}
	denied, _ := sink.first("permission.denied")
	if denied["tool"] != "write" {
		t.Fatalf("denied tool=%v want write", denied["tool"])
	}
	allowTools, _ := denied["allow_tools"].([]string)
	if len(allowTools) != 1 || allowTools[0] != "read_file" {
		t.Fatalf("denied allow_tools=%v want [read_file]", allowTools)
	}
	if denied["suggestion"] == "" {
		t.Fatal("permission.denied: suggestion must be non-empty for copilot adapter alias kinds")
	}

	_ = sm.Close(context.Background(), "agent")
}

// TestSessionManagerDefaultDenyAll verifies that a step without allow_tools
// denies every permission request.
// TestSessionManagerNilAllowToolsEmitsEmptyList verifies that when a step has
// nil AllowTools, the host normalizes it to []string{} before embedding it in
// the permission.denied payload so the field is always present and correctly
// typed (not JSON null, not absent).
func TestSessionManagerNilAllowToolsEmitsEmptyList(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "permissive", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	// AllowTools intentionally omitted (nil) — triggers the normalization path.
	step := &workflow.StepNode{
		Name:  "run",
		Input: map[string]string{"perm_tools": "read_file"},
	}
	sink := &adapterEventCollector{}
	if _, err := sm.Execute(context.Background(), "agent", step, sink); err != nil {
		t.Fatalf("execute: %v", err)
	}

	denied, ok := sink.first("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied event")
	}
	// allow_tools must be present and typed []string even when the step
	// declared no AllowTools — the host normalises nil → [].
	raw, exists := denied["allow_tools"]
	if !exists {
		t.Fatal("permission.denied: allow_tools field must be present when AllowTools is nil")
	}
	allowTools, ok := raw.([]string)
	if !ok {
		t.Fatalf("permission.denied allow_tools type=%T want []string", raw)
	}
	if len(allowTools) != 0 {
		t.Fatalf("permission.denied allow_tools=%v want empty slice", allowTools)
	}

	_ = sm.Close(context.Background(), "agent")
}

func TestSessionManagerDefaultDenyAll(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "permissive", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	// No AllowTools — default deny policy
	step := &workflow.StepNode{
		Name:  "run",
		Input: map[string]string{"perm_tools": "read_file"},
	}
	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", step, sink)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != "needs_review" {
		t.Fatalf("outcome=%q want needs_review", result.Outcome)
	}
	if sink.saw("permission.granted") {
		t.Error("expected no permission.granted events with empty allow_tools")
	}
	if !sink.saw("permission.denied") {
		t.Error("expected permission.denied event from default deny policy")
	}
	denied, ok := sink.first("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied payload")
	}
	if denied["reason"] == "" {
		t.Fatal("permission.denied reason must be non-empty")
	}

	_ = sm.Close(context.Background(), "agent")
}

func TestSessionManagerShellFingerprintAllowlist(t *testing.T) {
	pluginBin := testutil.BuildPermissivePlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return pluginBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "permissive", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	step := &workflow.StepNode{
		Name:       "run",
		Input:      map[string]string{"perm_tools": "shell|git status,shell|rm -rf /"},
		AllowTools: []string{"shell:git *"},
	}
	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", step, sink)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome != "needs_review" {
		t.Fatalf("outcome=%q want needs_review", result.Outcome)
	}

	granted, ok := sink.first("permission.granted")
	if !ok {
		t.Fatal("expected permission.granted event for shell|git status")
	}
	if granted["pattern"] != "shell:git *" {
		t.Fatalf("granted pattern=%v want shell:git *", granted["pattern"])
	}
	denied, ok := sink.first("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied event for shell|rm -rf /")
	}
	if denied["tool"] != "shell" {
		t.Fatalf("denied tool=%v want shell", denied["tool"])
	}

	_ = sm.Close(context.Background(), "agent")
}

var _ adapter.EventSink = (*adapterEventCollector)(nil)

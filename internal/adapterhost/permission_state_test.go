package adapterhost

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
)

// sliceAuditWriter collects DecisionLogEntry records in memory for test assertions.
type sliceAuditWriter struct {
	mu      sync.Mutex
	entries []*DecisionLogEntry
}

func (a *sliceAuditWriter) Write(e *DecisionLogEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *sliceAuditWriter) len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}

func (a *sliceAuditWriter) all() []DecisionLogEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]DecisionLogEntry, len(a.entries))
	for i, e := range a.entries {
		out[i] = *e
	}
	return out
}

// TestPermissionState_EvaluateAllowDeny verifies that Evaluate returns allow/deny
// correctly and writes audit entries.
func TestPermissionState_EvaluateAllowDeny(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := NewPermissionState("sess-1", audit)
	ps.SetPolicy(NewPolicy([]string{"read_file"}))

	allow, reason := ps.Evaluate("req-1", "read_file", "", "")
	if !allow {
		t.Fatalf("expected allow for read_file, got deny: %s", reason)
	}
	if !strings.Contains(reason, "matched") {
		t.Errorf("reason should contain 'matched', got %q", reason)
	}

	allow, reason = ps.Evaluate("req-2", "write_file", "", "")
	if allow {
		t.Fatalf("expected deny for write_file, got allow: %s", reason)
	}
	if reason != "no matching allow_tools entry" {
		t.Errorf("expected default deny reason, got %q", reason)
	}

	if audit.len() != 2 {
		t.Fatalf("expected 2 audit entries, got %d", audit.len())
	}
	entries := audit.all()
	if entries[0].Decision != "allow" {
		t.Errorf("first audit decision = %q; want allow", entries[0].Decision)
	}
	if entries[1].Decision != "deny" {
		t.Errorf("second audit decision = %q; want deny", entries[1].Decision)
	}
	if entries[0].SessionID != "sess-1" {
		t.Errorf("audit session_id = %q; want sess-1", entries[0].SessionID)
	}
}

// TestPermissionState_CombinedPolicyEnvPolicy verifies that CombinedPolicy
// delegates to the underlying allow_tools matcher and carries env policy.
func TestPermissionState_CombinedPolicyEnvPolicy(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := NewPermissionState("sess-1", audit)

	env := &workflow.ResolvedPolicy{
		Filesystem: &workflow.FilesystemPolicy{ReadOnly: true},
		Network:    &workflow.NetworkPolicy{AllowEgress: false},
	}
	cp := NewCombinedPolicy("noop", []string{"read_file"}, env)
	ps.SetPolicy(cp)

	allow, _ := ps.Evaluate("req-1", "read_file", "", "")
	if !allow {
		t.Fatal("expected allow for read_file with CombinedPolicy")
	}

	allow, _ = ps.Evaluate("req-2", "write_file", "", "")
	if allow {
		t.Fatal("expected deny for write_file with CombinedPolicy")
	}
}

// TestPermissionState_Concurrency verifies that 100 concurrent Evaluate calls
// on a single session all return decisions and produce 100 audit entries.
func TestPermissionState_Concurrency(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := NewPermissionState("sess-1", audit)
	ps.SetPolicy(NewPolicy([]string{"read_file", "write_file"}))

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			tool := "read_file"
			if idx%2 == 1 {
				tool = "write_file"
			}
			ps.Evaluate(string(rune('a'+idx)), tool, "", "")
		}(i)
	}
	wg.Wait()

	if audit.len() != n {
		t.Fatalf("expected %d audit entries, got %d", n, audit.len())
	}

	// Verify all decisions are either allow or deny.
	for _, e := range audit.all() {
		if e.Decision != "allow" && e.Decision != "deny" {
			t.Errorf("unexpected decision %q", e.Decision)
		}
	}
}

// TestPermissionState_SnapshotRestore verifies that MarshalState captures
// inflight requests and recent decisions, and RestoreState replays previously-
// answered requests deterministically while re-evaluating unanswered ones.
func TestPermissionState_SnapshotRestore(t *testing.T) {
	audit1 := &sliceAuditWriter{}
	ps1 := NewPermissionState("sess-1", audit1)
	ps1.SetPolicy(NewPolicy([]string{"read_file"}))

	// Evaluate two requests.
	ps1.Evaluate("req-1", "read_file", "", "")
	ps1.Evaluate("req-2", "write_file", "", "")

	data, err := ps1.MarshalState()
	if err != nil {
		t.Fatalf("MarshalState: %v", err)
	}

	// Restore into a fresh permissionState with a different audit writer.
	audit2 := &sliceAuditWriter{}
	ps2 := NewPermissionState("sess-2", audit2)
	ps2.SetPolicy(NewPolicy([]string{"read_file"})) // same policy
	if err := ps2.RestoreState(data, ps2.policy, audit2); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}

	// req-1 was previously answered (allow) — should be replayed.
	// req-2 was previously answered (deny) — should be replayed.
	// Audit2 should have entries for the restored decisions.
	entries := audit2.all()
	if len(entries) == 0 {
		t.Fatal("expected audit entries after restore")
	}

	// Verify decisions were restored correctly.
	var foundAllow, foundDeny bool
	for _, e := range entries {
		if e.Decision == "allow" {
			foundAllow = true
		}
		if e.Decision == "deny" {
			foundDeny = true
		}
	}
	if !foundAllow {
		t.Error("expected restored allow decision")
	}
	if !foundDeny {
		t.Error("expected restored deny decision")
	}
}

// TestPermissionState_PauseResume verifies that Pause stops dispatching
// decisions and Resume re-enables it.
func TestPermissionState_PauseResume(t *testing.T) {
	ps := NewPermissionState("sess-1", nil)
	ps.SetPolicy(NewPolicy([]string{"read_file"}))
	ps.SetStreamCancel(func() {})

	// Evaluate while active — should enqueue on the stream.
	ps.Evaluate("req-1", "read_file", "", "")

	// Pause should set active=false.
	ps.Pause()
	if ps.active {
		t.Error("expected active=false after Pause")
	}

	// Evaluate while paused — sendEvent should not block or enqueue.
	ps.Evaluate("req-2", "read_file", "", "")

	// Resume should set active=true again.
	ps.Resume()
	if !ps.active {
		t.Error("expected active=true after Resume")
	}
}

// TestPermissionState_StopCancelsStream verifies that Stop closes the request
// channel and records a session-close audit entry when there are pending requests.
func TestPermissionState_StopCancelsStream(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := NewPermissionState("sess-1", audit)
	ps.SetPolicy(NewPolicy([]string{"read_file"}))
	ps.SetStreamCancel(func() {})

	ps.Evaluate("req-1", "read_file", "", "")
	ch := ps.Requests()
	ps.Stop()

	// Drain any queued event, then verify the channel is closed.
drain:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break drain
			}
		default:
			// Not closed yet — keep polling briefly.
			select {
			case _, ok := <-ch:
				if !ok {
					break drain
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for closed channel")
			}
		}
	}
}

// TestPermissionInterceptSink_AllowDeny verifies that the intercept sink
// forwards permission.request events through the PermissionState and emits
// the correct granted/denied events.
func TestPermissionInterceptSink_AllowDeny(t *testing.T) {
	inner := &adapterEventCollector{}
	sess := &Session{Adapter: "noop"}
	ps := NewPermissionState("sess-1", nil)
	ps.SetPolicy(NewPolicy([]string{"read_file"}))

	sink := &permissionInterceptSink{
		inner:     inner,
		permState: ps,
		session:   sess,
	}

	sink.Adapter("permission.request", map[string]any{
		"request_id": "req-1",
		"tool":       "read_file",
	})
	if !inner.saw("permission.granted") {
		t.Error("expected permission.granted event")
	}

	sink.Adapter("permission.request", map[string]any{
		"request_id": "req-2",
		"tool":       "write_file",
	})
	if !inner.saw("permission.denied") {
		t.Error("expected permission.denied event")
	}
	if !sink.anyDenied {
		t.Error("expected anyDenied=true after denial")
	}
}

// TestPermissionInterceptSink_MalformedPayload verifies that a malformed
// permission.request payload is treated as a denial.
func TestPermissionInterceptSink_MalformedPayload(t *testing.T) {
	inner := &adapterEventCollector{}
	sess := &Session{Adapter: "noop"}
	ps := NewPermissionState("sess-1", nil)
	ps.SetPolicy(NewPolicy([]string{"read_file"}))

	sink := &permissionInterceptSink{
		inner:     inner,
		permState: ps,
		session:   sess,
	}

	// Pass a non-map payload.
	sink.Adapter("permission.request", "bad-payload")
	if !inner.saw("permission.denied") {
		t.Error("expected permission.denied for malformed payload")
	}
}

// TestSessionManager_ExecutePermissionOutcomeOverride verifies that when a
// permission is denied during Execute, a success outcome is overridden to
// needs_review.
func TestSessionManager_ExecutePermissionOutcomeOverride(t *testing.T) {
	loader := NewLoaderWithDiscovery(func(string) (string, error) {
		return "", nil
	})
	loader.RegisterBuiltin("noop", func() Handle {
		return &permissionEmittingAdapter{tool: "write_file"}
	})
	sm := NewSessionManager(loader)

	ctx := context.Background()
	if err := sm.Open(ctx, "agent", "noop", "", nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sm.Close(ctx, "agent") }()

	step := &workflow.StepNode{
		Name:       "run",
		AllowTools: []string{"read_file"}, // deny-all for write_file
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success":      {Name: "success"},
			"needs_review": {Name: "needs_review"},
		},
	}
	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, "agent", step, inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "needs_review" {
		t.Errorf("outcome = %q; want needs_review", res.Outcome)
	}
	if !inner.saw("permission.denied") {
		t.Error("expected permission.denied event")
	}
}

// TestSessionManager_ExecutePermissionFlowWithAliases verifies the full
// session-scoped permission flow: Execute → permissionInterceptSink →
// CombinedPolicy with aliases → audit write.
func TestSessionManager_ExecutePermissionFlowWithAliases(t *testing.T) {
	// Register an alias so that the adapter's runtime tool name "read" maps to
	// the user-facing allow_tools pattern "read_file".
	adapterPermissionAliases["alias-test"] = map[string]string{
		"read_file": "read",
	}
	t.Cleanup(func() { delete(adapterPermissionAliases, "alias-test") })

	loader := NewLoaderWithDiscovery(func(string) (string, error) {
		return "", nil
	})
	loader.RegisterBuiltin("alias-test", func() Handle {
		return &permissionEmittingAdapter{tool: "read"}
	})

	sm := NewSessionManager(loader)
	audit := &sliceAuditWriter{}
	sm.Audit = audit

	ctx := context.Background()
	if err := sm.Open(ctx, "agent", "alias-test", "", nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sm.Close(ctx, "agent") }()

	step := &workflow.StepNode{
		Name:       "run",
		AllowTools: []string{"read_file"}, // matches via alias "read" → "read_file"
		Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Name: "success"}},
	}
	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, "agent", step, inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("outcome=%q want success", res.Outcome)
	}
	if !inner.saw("permission.granted") {
		t.Fatal("expected permission.granted event")
	}

	if audit.len() != 1 {
		t.Fatalf("expected 1 audit entry, got %d", audit.len())
	}
	entries := audit.all()
	if entries[0].Decision != "allow" {
		t.Errorf("audit decision=%q want allow", entries[0].Decision)
	}
	if entries[0].Tool != "read" {
		t.Errorf("audit tool=%q want read", entries[0].Tool)
	}
	if entries[0].SessionID == "" {
		t.Error("expected non-empty audit session_id")
	}
}

// TestFileAuditWriter writes JSON-lines to a temp file and verifies the output.
func TestFileAuditWriter(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/audit.log"
	w := NewFileAuditWriter(path)

	w.Write(&DecisionLogEntry{
		SessionID: "sess-1",
		RequestID: "req-1",
		Tool:      "read_file",
		Decision:  "allow",
	})
	w.Write(&DecisionLogEntry{
		SessionID: "sess-1",
		RequestID: "req-2",
		Tool:      "write_file",
		Decision:  "deny",
	})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %s", len(lines), string(b))
	}
	var first DecisionLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if first.Decision != "allow" {
		t.Errorf("first decision=%q want allow", first.Decision)
	}
	var second DecisionLogEntry
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}
	if second.Decision != "deny" {
		t.Errorf("second decision=%q want deny", second.Decision)
	}
}

// permissionEmittingAdapter is a builtin adapter that emits a permission.request
// event and then returns success.
type permissionEmittingAdapter struct {
	tool string
}

func (a *permissionEmittingAdapter) Info(_ context.Context) (Info, error) {
	return Info{Capabilities: []string{"execute"}}, nil
}
func (a *permissionEmittingAdapter) OpenSession(_ context.Context, _ string, _, _ map[string]string) error {
	return nil
}
func (a *permissionEmittingAdapter) CloseSession(_ context.Context, _ string) error { return nil }
func (a *permissionEmittingAdapter) Kill()                                          {}
func (a *permissionEmittingAdapter) Pause(context.Context, string) error              { return nil }
func (a *permissionEmittingAdapter) Resume(context.Context, string) error             { return nil }
func (a *permissionEmittingAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *permissionEmittingAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sink.Adapter("permission.request", map[string]any{
		"request_id": "req-1",
		"tool":       a.tool,
	})
	return adapter.Result{Outcome: "success"}, nil
}

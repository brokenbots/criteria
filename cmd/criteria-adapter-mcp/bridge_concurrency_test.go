package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// kb155Bridge opens an echo-mcp-backed MCP session with a distinct id and
// wires a CloseSession teardown.
func kb155Bridge(t *testing.T, b *MCPBridge, sessionID string) {
	t.Helper()
	if testEchoBin == "" {
		t.Fatal("echo-mcp binary not available (TestMain not run)")
	}
	t.Cleanup(func() {
		_, _ = b.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: sessionID})
	})
}

// kb155RunExecute fires one Execute call on a shared session with the given
// message, on its own permitting sender.
func kb155RunExecute(b *MCPBridge, sessionID, message string, sender *permittingEventSender) func() error {
	return func() error {
		return b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: sessionID,
			Input: map[string]string{
				"tool":            "echo",
				"success_outcome": "success",
				"message":         message,
				"sleep_ms":        "400",
			},
		}, sender)
	}
}

// kb155ResultEvent verifies the last event of the stream is a success
// result, failing the test otherwise.
func kb155ResultEvent(t *testing.T, events []*v2.ExecuteEvent, label string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatalf("%s: no events", label)
	}
	last := events[len(events)-1]
	res := last.GetResult()
	if res == nil {
		t.Fatalf("%s: last event must be a Result; got %T", label, last.GetEvent())
	}
	if res.GetOutcome() != "success" {
		t.Fatalf("%s: outcome=%q want success", label, res.GetOutcome())
	}
}

// kb155ContentTexts joins the mcp.content text payloads of a stream.
func kb155ContentTexts(t *testing.T, events []*v2.ExecuteEvent) string {
	t.Helper()
	var texts []string
	for _, ev := range events {
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "mcp.content" {
			p := a.GetPayload()
			if p == nil {
				continue
			}
			if v, ok := p.GetFields()["text"]; ok {
				texts = append(texts, v.GetStringValue())
			}
		}
	}
	if len(texts) == 0 {
		t.Fatal("stream carries no mcp.content text")
	}
	return strings.Join(texts, "\n")
}

// kb155ProgressEvents returns the mcp.progress adapter events of a stream.
func kb155ProgressEvents(t *testing.T, events []*v2.ExecuteEvent) []*v2.ExecuteEvent {
	t.Helper()
	var out []*v2.ExecuteEvent
	for _, ev := range events {
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "mcp.progress" {
			out = append(out, ev)
		}
	}
	return out
}

// kb155PermRequests returns the permission.request events of a stream.
func kb155PermRequests(t *testing.T, events []*v2.ExecuteEvent) []*v2.ExecuteEvent {
	t.Helper()
	var out []*v2.ExecuteEvent
	for _, ev := range events {
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "permission.request" {
			out = append(out, ev)
		}
	}
	return out
}

// TestMCPBridge_Info_ConcurrentExecute verifies the bridge declares the
// KB-155 multiplexing capability alongside the established ones.
func TestMCPBridge_Info_ConcurrentExecute(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	info, err := b.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.GetName() != adapterName || info.GetVersion() != adapterVersion {
		t.Fatalf("identity = %q/%q want %q/%q", info.GetName(), info.GetVersion(), adapterName, adapterVersion)
	}
	found := false
	for _, c := range info.GetCapabilities() {
		if c == "concurrent_execute" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("capabilities %v lack concurrent_execute", info.GetCapabilities())
	}
}

// TestMCPBridge_ConcurrentExecuteCorrelatesPerStream drives two concurrent
// Execute RPCs over ONE session: each stream must get its own reply carrying
// its own message — never the sibling's — plus exactly one permission
// request that the host (auto-)answered for that specific call.
func TestMCPBridge_ConcurrentExecuteCorrelatesPerStream(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb155Bridge(t, b, "sess-mux")
	ctx := context.Background()
	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-mux",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	senderA := &permittingEventSender{bridge: b}
	senderB := &permittingEventSender{bridge: b}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = kb155RunExecute(b, "sess-mux", "msg-a", senderA)()
	}()
	go func() {
		defer wg.Done()
		_ = kb155RunExecute(b, "sess-mux", "msg-b", senderB)()
	}()
	wg.Wait()

	textA := kb155ContentTexts(t, senderA.all())
	textB := kb155ContentTexts(t, senderB.all())
	if !strings.Contains(textA, "msg-a") || strings.Contains(textA, "msg-b") {
		t.Fatalf("stream A text %q must carry msg-a only", textA)
	}
	if !strings.Contains(textB, "msg-b") || strings.Contains(textB, "msg-a") {
		t.Fatalf("stream B text %q must carry msg-b only", textB)
	}
	kb155ResultEvent(t, senderA.all(), "stream A")
	kb155ResultEvent(t, senderB.all(), "stream B")

	// One permission.request per stream, auto-allowed, so the bridge must
	// have proceeded for both calls.
	if got := len(kb155PermRequests(t, senderA.all())); got != 1 {
		t.Fatalf("stream A permissions = %d want 1", got)
	}
	if got := len(kb155PermRequests(t, senderB.all())); got != 1 {
		t.Fatalf("stream B permissions = %d want 1", got)
	}
}

// TestMCPBridge_ProgressAttributionUnderConcurrency verifies the bridge's
// session-level routing during an overlap: exactly one progress notification
// per in-flight call lands on the stream that issued it (token keying), and
// the fixture's overlap marker proves the calls ran concurrently rather
// than serialized.
func TestMCPBridge_ProgressAttributionUnderConcurrency(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb155Bridge(t, b, "sess-prog")
	ctx := context.Background()
	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-prog",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	senderA := &permittingEventSender{bridge: b}
	senderB := &permittingEventSender{bridge: b}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = kb155RunExecute(b, "sess-prog", "prog-a", senderA)()
	}()
	go func() {
		defer wg.Done()
		_ = kb155RunExecute(b, "sess-prog", "prog-b", senderB)()
	}()
	wg.Wait()

	progressA := kb155ProgressEvents(t, senderA.all())
	progressB := kb155ProgressEvents(t, senderB.all())
	if len(progressA) != 1 {
		t.Fatalf("stream A progress events = %d want 1: %v", len(progressA),
			describeEvents(progressA))
	}
	if len(progressB) != 1 {
		t.Fatalf("stream B progress events = %d want 1: %v", len(progressB),
			describeEvents(progressB))
	}

	tokA, tokB := progressToken(progressA[0]), progressToken(progressB[0])
	if tokA == "" || tokB == "" {
		t.Fatalf("progress notifications must carry progressToken, got %q and %q", tokA, tokB)
	}
	if tokA == tokB {
		t.Fatalf("progress tokens must be distinct per call, got %q twice", tokA)
	}

	// criteria_overlap reflects the fixture's peak concurrent calls: with a
	// 400ms hold on both calls it must reach 2. When it does, each progress
	// event is stamped while both calls are live (or near it); require at
	// least one overlap>=2 marker across the two streams.
	overlap := func(evs []*v2.ExecuteEvent) float64 {
		for _, ev := range evs {
			p := ev.GetAdapter().GetPayload()
			if p == nil {
				continue
			}
			if v, ok := p.GetFields()["criteria_overlap"]; ok {
				return v.GetNumberValue()
			}
		}
		return 0
	}
	maxOverlap := max(overlap(progressA), overlap(progressB))
	if maxOverlap < 2 {
		t.Fatalf("calls did not overlap: max criteria_overlap=%.0f want >= 2", maxOverlap)
	}
}

// TestSessionState_RouteProgress unit-tests the session-level attribution
// rules: tokened traffic goes to the token's owner, stale tokens are dropped,
// and untokened traffic is delivered only when exactly one execute is
// in flight (never misattributed across a concurrent overlap).
func TestSessionState_RouteProgress(t *testing.T) {
	s := &sessionState{}
	e1 := s.registerExec(&fakeEventSender{}, nil)
	defer s.unregisterExec(e1)
	e2 := s.registerExec(&fakeEventSender{}, nil)

	untok := mcpclient.Notification{Method: "notifications/progress", Params: map[string]any{"progress": 1}}

	// Two in flight: untokened is ambiguous and must be dropped on both.
	s.routeProgress(untok)
	for _, e := range []*sessionExec{e1, e2} {
		f := e.sink.(*fakeEventSender)
		if got := len(f.all()); got != 0 {
			t.Fatalf("untokened with 2 in flight delivered %d events on %q", got, e.token)
		}
	}

	// One left: untokened goes to the sole execute.
	s.unregisterExec(e2)
	s.routeProgress(untok)
	if got := len(e1.sink.(*fakeEventSender).all()); got != 1 {
		t.Fatalf("untokened with 1 in flight delivered %d events want 1", got)
	}

	// Tokened goes to its owner only.
	tokd := mcpclient.Notification{Method: "notifications/progress",
		Params: map[string]any{"progress": 1, "progressToken": e1.token}}
	s.routeProgress(tokd)
	if got := len(e1.sink.(*fakeEventSender).all()); got != 2 {
		t.Fatalf("tokened delivered %d events on owner want 2", got)
	}

	// Stale token: dropped.
	stale := mcpclient.Notification{Method: "notifications/progress",
		Params: map[string]any{"progress": 1, "progressToken": "criteria-999"}}
	s.routeProgress(stale)
	if got := len(e1.sink.(*fakeEventSender).all()); got != 2 {
		t.Fatalf("stale token leaked %d event(s) want none", got-2)
	}
}

// TestMCPBridge_AwaitPermissionContextCancel verifies a cancelled caller does
// not leave a pending permission request behind: the request is cleaned up
// and the Execute returns the context error.
func TestMCPBridge_AwaitPermissionContextCancel(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb155Bridge(t, b, "sess-cancel")
	ctx := context.Background()
	if _, err := b.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "sess-cancel",
		Config:    map[string]string{"command": testEchoBin},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	// No auto-answer on this stream: the permission.request stays pending
	// until the caller cancels.
	sender := &fakeEventSender{}
	cctx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- b.Execute(cctx, &v2.ExecuteRequest{
			SessionId: "sess-cancel",
			Input:     map[string]string{"tool": "echo", "message": "cancel-me"},
		}, sender)
	}()

	// Wait until the request is registered, then cancel before any decision.
	waitFor := func() bool {
		b.pendingPermsMu.Lock()
		defer b.pendingPermsMu.Unlock()
		return len(b.pendingPerms) == 1
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if waitFor() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Execute must fail on cancelled caller")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not settle after cancel")
	}

	// Cleanup contract: no pending permission request may remain.
	b.pendingPermsMu.Lock()
	left := len(b.pendingPerms)
	b.pendingPermsMu.Unlock()
	if left != 0 {
		t.Fatalf("pending permission requests left after cancel: %d", left)
	}
}

// describeEvents renders event kinds of a stream for failure messages.
func describeEvents(events []*v2.ExecuteEvent) string {
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		if a := ev.GetAdapter(); a != nil {
			kinds = append(kinds, a.GetEventKind())
		} else if ev.GetResult() != nil {
			kinds = append(kinds, "result:"+ev.GetResult().GetOutcome())
		}
	}
	return strings.Join(kinds, ",")
}

func progressToken(ev *v2.ExecuteEvent) string {
	p := ev.GetAdapter().GetPayload()
	if p == nil {
		return ""
	}
	if v, ok := p.GetFields()["progressToken"]; ok {
		return v.GetStringValue()
	}
	return ""
}

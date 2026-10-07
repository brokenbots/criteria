package main

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// kb157Open opens a session with the given config against a fresh bridge and
// wires a CloseSession teardown. Session IDs must be unique per test.
func kb157Open(t *testing.T, b *MCPBridge, sessionID string, cfg map[string]string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = b.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: sessionID})
	})
	if _, err := b.OpenSession(context.Background(), &v2.OpenSessionRequest{
		SessionId: sessionID,
		Config:    cfg,
	}); err != nil {
		t.Fatalf("OpenSession(%s): %v", sessionID, err)
	}
}

// kb157InfoNames returns the sorted tool names advertised by Info.
func kb157InfoNames(t *testing.T, b *MCPBridge) []string {
	t.Helper()
	info, err := b.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	names := make([]string, 0, len(info.GetTools()))
	for _, tool := range info.GetTools() {
		names = append(names, tool.GetName())
	}
	sort.Strings(names)
	return names
}

// kb157ExecuteLists calls the shift-mcp `lists` tool (the tools/list-call
// counter observable) and returns the count as text.
func kb157ExecuteLists(t *testing.T, b *MCPBridge, sessionID string) int {
	t.Helper()
	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: sessionID,
		Input:     map[string]string{"tool": "lists", "success_outcome": "success"},
	}, sender); err != nil {
		t.Fatalf("Execute(lists): %v", err)
	}
	text := kb155ContentTexts(t, sender.all())
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("lists tool text %q is not a count: %v", text, err)
	}
	return n
}

// kb157ValidateUnknownTool asserts the typed failure signature on a collected
// stream (main-goroutine use): a failure result carrying the reserved
// call_error "unknown_tool" output (CRI-172), sent before any permission
// gating.
func kb157ValidateUnknownTool(t *testing.T, events []*v2.ExecuteEvent, toolName string) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("unknown tool %s: events = %v, want exactly the typed failure", toolName, describeEvents(events))
	}
	res := events[0].GetResult()
	if res == nil {
		t.Fatalf("unknown tool %s: event must be a Result; got %T", toolName, events[0].GetEvent())
	}
	if res.GetOutcome() != "failure" {
		t.Fatalf("unknown tool %s: outcome = %q want failure", toolName, res.GetOutcome())
	}
	var outputs map[string]any
	if len(res.GetOutputsJson()) > 0 {
		if err := json.Unmarshal(res.GetOutputsJson(), &outputs); err != nil {
			t.Fatalf("unknown tool %s: outputs_json: %v", toolName, err)
		}
	}
	if got, _ := outputs["call_error"].(string); got != "unknown_tool" {
		t.Fatalf("unknown tool %s: call_error = %q want unknown_tool", toolName, got)
	}
}

// kb157AssertUnknownTool is kb157ValidateUnknownTool for a call issued on the
// test goroutine.
func kb157AssertUnknownTool(t *testing.T, b *MCPBridge, sessionID, toolName string) {
	t.Helper()
	sender := &fakeEventSender{}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: sessionID,
		Input:     map[string]string{"tool": toolName},
	}, sender); err != nil {
		t.Fatalf("Execute(%s): %v", toolName, err)
	}
	kb157ValidateUnknownTool(t, sender.all(), toolName)
}

// kb157SameNames reports whether the two name sets are equal.
func kb157SameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// kb157NamesMessage renders a name set for failure output.
func kb157NamesMessage(names []string) string {
	return strings.Join(names, ",")
}

// TestMCPBridge_Info_DiscoveryKeyedPerEnvironment proves the CRI-171 fix
// (KB-157): sessions against different MCP servers each hold their own
// discovered surface, so the advertised Info.tools is their deterministic
// union instead of the wholesale replacement of the prior single map; the
// surface also stays deduplicated across same-environment re-opens.
func TestMCPBridge_Info_DiscoveryKeyedPerEnvironment(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	// Both fixtures serve distinct tool surfaces; distinct configs (command)
	// fingerprint to distinct environment keys.
	kb157Open(t, b, "sess-echo", map[string]string{"command": testEchoBin})
	kb157Open(t, b, "sess-shift", map[string]string{"command": testShiftBin})

	want := []string{"early", "echo", "lists", "structured"}
	if got := kb157InfoNames(t, b); !kb157SameNames(got, want) {
		t.Fatalf("union of both environments = [%s], want [%s]", kb157NamesMessage(got), kb157NamesMessage(want))
	}
	// The bridge really keys per environment: two entries, one per config.
	b.mu.Lock()
	envs := len(b.discovered)
	b.mu.Unlock()
	if envs != 2 {
		t.Fatalf("discovered entries = %d want 2 (one per environment)", envs)
	}

	// A second session on the SAME environment must dedupe, not duplicate.
	kb157Open(t, b, "sess-echo-2", map[string]string{"command": testEchoBin})
	if got := kb157InfoNames(t, b); !kb157SameNames(got, want) {
		t.Fatalf("same-env re-open changed/duplicated union = [%s], want [%s]",
			kb157NamesMessage(got), kb157NamesMessage(want))
	}

	// Deterministic union outlives all sessions (CRI-171/CRI-172 contract).
	for _, id := range []string{"sess-echo", "sess-shift", "sess-echo-2"} {
		if _, err := b.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: id}); err != nil {
			t.Fatalf("CloseSession(%s): %v", id, err)
		}
	}
	if got := kb157InfoNames(t, b); !kb157SameNames(got, want) {
		t.Fatalf("union after all closes = [%s], want [%s]", kb157NamesMessage(got), kb157NamesMessage(want))
	}

	// Re-opening the same environments keeps the same advertised set.
	kb157Open(t, b, "sess-shift-2", map[string]string{"command": testShiftBin})
	if got := kb157InfoNames(t, b); !kb157SameNames(got, want) {
		t.Fatalf("union after re-open = [%s], want [%s]", kb157NamesMessage(got), kb157NamesMessage(want))
	}
}

// TestMCPBridge_Info_DiscoveryConcurrentEnvironments proves the aggregation
// is race-safe and deterministic (KB-157): concurrently opened sessions for
// two environments yield the stable union, and repeated Info calls made
// concurrently render identical tool sets.
func TestMCPBridge_Info_DiscoveryConcurrentEnvironments(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	want := []string{"early", "echo", "lists", "structured"}

	var wg sync.WaitGroup
	openErrs := make([]error, 2)
	wg.Add(2)
	for i, id := range []string{"sess-echo-c", "sess-shift-c"} {
		cfg := map[string]string{"command": testEchoBin}
		if i == 1 {
			cfg = map[string]string{"command": testShiftBin}
		}
		go func(i int, id string, cfg map[string]string) {
			defer wg.Done()
			t.Cleanup(func() {
				_, _ = b.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: id})
			})
			_, openErrs[i] = b.OpenSession(context.Background(), &v2.OpenSessionRequest{
				SessionId: id,
				Config:    cfg,
			})
		}(i, id, cfg)
	}
	wg.Wait()
	for i, err := range openErrs {
		if err != nil {
			t.Fatalf("concurrent OpenSession(%d): %v", i, err)
		}
	}

	if got := kb157InfoNames(t, b); !kb157SameNames(got, want) {
		t.Fatalf("concurrent union = [%s], want [%s]", kb157NamesMessage(got), kb157NamesMessage(want))
	}

	// Concurrent Info renders must be identical, read for read.
	const readers = 8
	results := make([][]string, readers)
	var rWg sync.WaitGroup
	rWg.Add(readers)
	for i := 0; i < readers; i++ {
		go func(i int) {
			defer rWg.Done()
			results[i] = kb157InfoNames(t, b)
		}(i)
	}
	rWg.Wait()
	for i := 1; i < readers; i++ {
		if !kb157SameNames(results[i], results[0]) {
			t.Fatalf("concurrent Info rendered [%s] vs [%s]", kb157NamesMessage(results[i]), kb157NamesMessage(results[0]))
		}
	}
}

// TestMCPBridge_Execute_RefreshesDiscoveryOnLateTool proves the tools/list
// refresh path (KB-157 item 2): the "late" tool appears on the server's
// SECOND listing, so an Execute naming it must re-consult the server once the
// cache window has elapsed and succeed — and the bridge's advertised surface
// must pick the refreshed state up. A still-unknown name keeps the typed
// unknown_tool signature (CRI-172).
func TestMCPBridge_Execute_RefreshesDiscoveryOnLateTool(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb157Open(t, b, "sess-late", map[string]string{"command": testShiftBin})
	if got := kb157InfoNames(t, b); strings.Contains(kb157NamesMessage(got), "late") {
		t.Fatalf("surface after first listing = [%s], must not contain late yet", kb157NamesMessage(got))
	}

	// Zero the TTL so the single Execute miss below deterministically refreshes.
	b.mu.Lock()
	state := b.sessions["sess-late"]
	b.mu.Unlock()
	if state == nil {
		t.Fatal("session state missing")
	}
	state.discoveryTTL = 0

	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "sess-late",
		Input:     map[string]string{"tool": "late", "message": "late-hello", "success_outcome": "success"},
	}, sender); err != nil {
		t.Fatalf("Execute(late): %v", err)
	}
	res := sender.all()
	kb155ResultEvent(t, res, "late call")
	if text := kb155ContentTexts(t, res); !strings.Contains(text, "late-hello") {
		t.Fatalf("late call text = %q, want the echoed message", text)
	}

	// Refreshed surface is visible in Info under the same environment key.
	infoNames := kb157InfoNames(t, b)
	found := false
	for _, name := range infoNames {
		if name == "late" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Info after refresh = [%s], want late advertised", kb157NamesMessage(infoNames))
	}

	// A stale-but-still-unknown name keeps the typed signature.
	kb157AssertUnknownTool(t, b, "sess-late", "still-unknown")
}

// TestMCPBridge_Info_SurfaceOutlivesAndReopens is the item-4 re-verification
// (KB-157): with sessions keyed per environment, the advertised Info surface
// is the last discovery for that environment between and after runs — never
// revoked by CloseSession, and stable across a re-open of the same
// environment.
func TestMCPBridge_Info_SurfaceOutlivesAndReopens(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb157Open(t, b, "sess-item4", map[string]string{"command": testShiftBin})

	// Refresh to the second listing so the surface includes late.
	b.mu.Lock()
	state := b.sessions["sess-item4"]
	b.mu.Unlock()
	state.discoveryTTL = 0
	kb157AssertUnknownTool(t, b, "sess-item4", "ghost")
	if got := kb157InfoNames(t, b); !kb157SameNames(got, []string{"early", "late", "lists"}) {
		t.Fatalf("after refresh = [%s], want [early,late,lists]", kb157NamesMessage(got))
	}

	// Between runs: close and verify the surface persists.
	if _, err := b.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: "sess-item4"}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if got := kb157InfoNames(t, b); !kb157SameNames(got, []string{"early", "late", "lists"}) {
		t.Fatalf("after close = [%s], want surface retained]", kb157NamesMessage(got))
	}

	// After re-opening the same environment: the fresh discovery replaces
	// that environment's surface in place. The reopened server process is
	// fresh, so its first listing again lacks late — and the bridge reflects
	// exactly that ([early,lists]), proving same-env replacement is
	// deterministic, not accidental accretion.
	kb157Open(t, b, "sess-item4-2", map[string]string{"command": testShiftBin})
	if got := kb157InfoNames(t, b); !kb157SameNames(got, []string{"early", "lists"}) {
		t.Fatalf("after re-open = [%s], want [early,lists]", kb157NamesMessage(got))
	}
}

// TestMCPBridge_Execute_DiscoveryCacheBoundedByTTL proves the per-request
// cache semantics (KB-157 item 2): within the default window a miss does not
// re-consult tools/list; with the window zeroed, one miss triggers exactly
// one refresh.
func TestMCPBridge_Execute_DiscoveryCacheBoundedByTTL(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb157Open(t, b, "sess-ttl", map[string]string{"command": testShiftBin})

	// Default TTL: miss is absorbed by the cache, no extra tools/list.
	kb157AssertUnknownTool(t, b, "sess-ttl", "ghost")
	if got := kb157ExecuteLists(t, b, "sess-ttl"); got != 1 {
		t.Fatalf("tools/list calls after cached miss = %d want 1 (no refresh yet)", got)
	}

	// Zero TTL: the next miss triggers exactly one refresh.
	b.mu.Lock()
	b.sessions["sess-ttl"].discoveryTTL = 0
	b.mu.Unlock()
	kb157AssertUnknownTool(t, b, "sess-ttl", "ghost2")
	if got := kb157ExecuteLists(t, b, "sess-ttl"); got != 2 {
		t.Fatalf("tools/list calls after zero-TTL miss = %d want 2 (one refresh)", got)
	}
}

// TestMCPBridge_Execute_RefreshSingleflights proves concurrent misses refresh
// tools/list exactly once (KB-157): the refreshMu single-flight collapses the
// stampede; the losers reuse the cached result inside the same window.
func TestMCPBridge_Execute_RefreshSingleflights(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb157Open(t, b, "sess-1f", map[string]string{
		"command": testShiftBin,
		"env":     "MCP_LIST_DELAY_MS=100",
	})

	// Backdate the discovery stamp so the window has elapsed when the misses
	// fire; the fixture's 100ms listing delay then guarantees any
	// non-single-flighted design would stack many refreshes. Backdating (vs a
	// tiny TTL) keeps every arrival inside the default 30s window regardless
	// of scheduler or -race timing, so exactly one refresh is always observed.
	const misses = 5
	b.mu.Lock()
	b.sessions["sess-1f"].discoveredAt = time.Now().Add(-time.Hour)
	b.mu.Unlock()
	var wg sync.WaitGroup
	senders := make([]*fakeEventSender, misses)
	exeErrs := make([]error, misses)
	wg.Add(misses)
	for i := 0; i < misses; i++ {
		senders[i] = &fakeEventSender{}
		go func(i int) {
			defer wg.Done()
			exeErrs[i] = b.Execute(context.Background(), &v2.ExecuteRequest{
				SessionId: "sess-1f",
				Input:     map[string]string{"tool": "ghost"},
			}, senders[i])
		}(i)
	}
	wg.Wait()
	for i := range misses {
		if exeErrs[i] != nil {
			t.Fatalf("concurrent miss %d: %v", i, exeErrs[i])
		}
		kb157ValidateUnknownTool(t, senders[i].all(), "ghost")
	}

	if got := kb157ExecuteLists(t, b, "sess-1f"); got != 2 {
		t.Fatalf("tools/list calls after %d concurrent misses = %d want 2 (one refresh, single-flighted)", misses, got)
	}
}

// TestMCPBridge_Execute_RefreshFailureKeepsTypedUnknownTool proves a failed
// refresh is non-fatal and signature-preserving (KB-157): with the server
// erroring on every listing after the first, a zero-TTL miss falls back to
// the typed unknown_tool result, the session and Info surface stay intact,
// and known tools keep working.
func TestMCPBridge_Execute_RefreshFailureKeepsTypedUnknownTool(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}}
	kb157Open(t, b, "sess-err", map[string]string{
		"command": testShiftBin,
		"env":     "MCP_LIST_ERROR_AFTER=1",
	})
	b.mu.Lock()
	b.sessions["sess-err"].discoveryTTL = 0
	b.mu.Unlock()

	// The miss triggers a refresh; the listing fails; Execute still reports
	// the typed unknown_tool.
	kb157AssertUnknownTool(t, b, "sess-err", "ghost")

	// Failed refresh must not have poisoned the surfaces.
	if got := kb157InfoNames(t, b); !kb157SameNames(got, []string{"early", "lists"}) {
		t.Fatalf("Info after failed refresh = [%s], want [early,lists] intact", kb157NamesMessage(got))
	}

	// And known tools keep working.
	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "sess-err",
		Input:     map[string]string{"tool": "early", "message": "still-fine", "success_outcome": "success"},
	}, sender); err != nil {
		t.Fatalf("Execute(early): %v", err)
	}
	kb155ResultEvent(t, sender.all(), "early call after failed refresh")
}

// TestMCPBridge_EnvironmentKey_DistinctConfigs proves the fingerprint differs
// whenever any config entry differs, is stable across permutations of the map
// (sorted-key serialization), and stays injective for values containing the
// JSON-escaped separators (no glued-pair collisions).
func TestMCPBridge_EnvironmentKey_DistinctConfigs(t *testing.T) {
	base := map[string]string{"command": "server-a", "env": "A=1"}
	same := map[string]string{"env": "A=1", "command": "server-a"}
	if environmentKey(base) != environmentKey(same) {
		t.Fatal("same config in different map order must fingerprint identically")
	}
	cases := map[string]map[string]string{
		"other command": {"command": "server-b", "env": "A=1"},
		"other env":     {"command": "server-a", "env": "A=2"},
		"extra entry":   {"command": "server-a", "env": "A=1", "cwd": "/tmp"},
	}
	for label, cfg := range cases {
		if environmentKey(base) == environmentKey(cfg) {
			t.Fatalf("%s must fingerprint differently from base", label)
		}
	}
	// Injectivity: a value embedding a separator must not re-assemble into a
	// different config's fingerprint.
	glued := environmentKey(map[string]string{"a": "b,c", "d": "e"})
	paired := environmentKey(map[string]string{"a": "b", "d": "c,e"})
	if glued == paired {
		t.Fatalf("glued values collided: %q vs %q", glued, paired)
	}
}

// TestMCPBridge_RefreshDiscovery_AwaitsWindow proves the TTL cache actually
// holds the line: refreshDiscovery called immediately after discovery is a
// no-op, and once the window elapses it refreshes and re-stamps.
func TestMCPBridge_RefreshDiscovery_AwaitsWindow(t *testing.T) {
	b := &MCPBridge{sessions: map[string]*sessionState{}, discovered: map[string]map[string]mcpclient.Tool{}}
	kb157Open(t, b, "sess-window", map[string]string{"command": testShiftBin})
	b.mu.Lock()
	state := b.sessions["sess-window"]
	envKey := state.envKey
	b.mu.Unlock()

	// Still inside the default window: refresh is a cached no-op — no extra
	// listing, stamp untouched.
	firstStamp := state.discoveredAt
	listsBefore := kb157ExecuteLists(t, b, "sess-window")
	if listsBefore != 1 {
		t.Fatalf("lists calls observed = %d want 1 (open-session listing only)", listsBefore)
	}
	state.refreshDiscovery(context.Background(), b)
	if got := kb157ExecuteLists(t, b, "sess-window"); got != listsBefore {
		t.Fatalf("lists calls after cached refresh = %d want %d (refresh must be cached)", got, listsBefore)
	}
	if !state.discoveredAt.Equal(firstStamp) {
		t.Fatal("cached refresh must not re-stamp discoveredAt")
	}

	// Elapsed window (zero TTL): refresh re-consults and re-stamps.
	state.discoveryTTL = 0
	state.refreshDiscovery(context.Background(), b)
	if state.discoveredAt.Equal(firstStamp) {
		t.Fatal("refresh after elapsed window must re-stamp discoveredAt")
	}
	b.mu.Lock()
	surface := b.discovered[envKey]
	b.mu.Unlock()
	if _, ok := surface["late"]; !ok {
		t.Fatal("refresh must have replaced the environment surface with the second listing")
	}
}

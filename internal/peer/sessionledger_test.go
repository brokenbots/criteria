package peer

import (
	"bytes"
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brokenbots/criteria/internal/adapterhost"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestServer_ControlSessionArmsRequireSessionV1 is the negative acceptance
// check for the ADR-0014 M6 arms: a peer whose negotiated identity-frame
// capabilities lack session.v1 (simulating an older peer build) must answer
// Control with open_session / close_session / respawn_child / idle_reap
// typed unimplemented errors — not silence and not an accepted=false
// detail — so the caller can tell a protocol-level gap from a runtime
// rejection. Session arms share the kind oneof with kill_child, which
// stays ungated.
func TestServer_ControlSessionArmsRequireSessionV1(t *testing.T) {
	f := newPeerServeFixture(t)
	f.server.capabilities = []string{peerAdapterV2FullCapability, peerSupervisionV1Capability, peerWorkflowV1Capability}

	for i, arm := range []struct {
		name string
		req  *criteriav1.ControlRequest
	}{
		{"open_session", &criteriav1.ControlRequest{Kind: &criteriav1.ControlRequest_OpenSession{OpenSession: &criteriav1.OpenSession{SessionId: "sx"}}}},
		{"close_session", &criteriav1.ControlRequest{Kind: &criteriav1.ControlRequest_CloseSession{CloseSession: &criteriav1.CloseSession{SessionId: "sx"}}}},
		{"respawn_child", &criteriav1.ControlRequest{Kind: &criteriav1.ControlRequest_RespawnChild{RespawnChild: &criteriav1.RespawnChild{PolicyGraceMs: 100}}}},
		{"idle_reap", &criteriav1.ControlRequest{Kind: &criteriav1.ControlRequest_IdleReap{IdleReap: &criteriav1.IdleReap{AfterMs: 100}}}},
	} {
		if _, err := f.server.Control(f.ctx, arm.req); err == nil {
			t.Errorf("arm[%d] %s accepted without session.v1", i, arm.name)
			continue
		} else if got := status.Code(err); got != codes.Unimplemented {
			t.Errorf("arm[%d] %s status code = %v, want Unimplemented (typed error): %v", i, arm.name, got, err)
		}
	}
	// The shared-kind kill_child arm stays ungated on the same narrowed
	// capability set: the typed error above is specifically the session.v1
	// gate on the M6 arms.
	if _, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	}); err != nil {
		t.Fatalf("kill_child must stay ungated, got error: %v", err)
	}
}

// TestServer_ControlSessionLedgerLifecycle drives the ledger verbs
// through the negotiated Control surface against a planted hosted child:
// open journals opened, re-entry re-adopts, detach keeps the engagement
// tracked, release drops it, a post-release close is rejected honestly,
// and every fact carries the journaled envelope (adapter/scope/session)
// with gapless sequence numbers on replay.
func TestServer_ControlSessionLedgerLifecycle(t *testing.T) {
	f := newPeerServeFixture(t)
	if !f.server.negotiated(peerSessionV1Capability) {
		t.Fatal("default server does not advertise session.v1")
	}
	f.rt.mu.Lock()
	f.rt.children["fakex"] = &childState{name: "fakex"}
	f.rt.order = append(f.rt.order, "fakex")
	f.rt.mu.Unlock()

	control := func(kind *criteriav1.ControlRequest) *criteriav1.ControlResponse {
		t.Helper()
		resp, err := f.server.Control(f.ctx, kind)
		if err != nil {
			t.Fatalf("control: %v", err)
		}
		return resp
	}
	open := func(id string) *criteriav1.ControlResponse {
		return control(&criteriav1.ControlRequest{AdapterType: "", Scope: "sc",
			Kind: &criteriav1.ControlRequest_OpenSession{OpenSession: &criteriav1.OpenSession{SessionId: id}}})
	}
	closeS := func(id, reason string, detach bool) *criteriav1.ControlResponse {
		return control(&criteriav1.ControlRequest{AdapterType: "", Scope: "sc",
			Kind: &criteriav1.ControlRequest_CloseSession{CloseSession: &criteriav1.CloseSession{
				SessionId: id, Reason: reason, Detach: detach,
			}}})
	}

	if resp := open("sx"); !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "opened") {
		t.Fatalf("first open: want accepted opened, got %+v", resp)
	}
	if resp := open("sx"); !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "adopted") {
		t.Fatalf("re-open: want accepted adopted, got %+v", resp)
	}
	if resp := closeS("sx", "handoff", true); !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "detached") {
		t.Fatalf("detach close: want accepted detached, got %+v", resp)
	}
	// A detached engagement stays tracked: re-entry re-adopts instead of
	// opening fresh.
	if resp := open("sx"); !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "adopted") {
		t.Fatalf("open after detach: want accepted adopted, got %+v", resp)
	}
	if resp := closeS("sx", "done", false); !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "closed") {
		t.Fatalf("release close: want accepted closed, got %+v", resp)
	}
	// The engagement is gone: releasing again is an honest rejection.
	if resp := closeS("sx", "", false); resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "no ledger session") {
		t.Fatalf("close of released session: want rejected, got %+v", resp)
	}
	// Required-field rejections.
	if resp := open(""); resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "session id is required") {
		t.Fatalf("open with empty id: want rejected, got %+v", resp)
	}
	if resp := closeS("", "", false); resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "session id is required") {
		t.Fatalf("close with empty id: want rejected, got %+v", resp)
	}
	// An adapter outside the runtime's child table is rejected with its
	// name (an unknown adapter is a runtime rejection, not a protocol one).
	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "ghost",
		Kind:        &criteriav1.ControlRequest_OpenSession{OpenSession: &criteriav1.OpenSession{SessionId: "sx"}},
	})
	if err != nil {
		t.Fatalf("unknown adapter open: %v", err)
	}
	if resp.GetAccepted() || !strings.Contains(resp.GetDetail(), `no adapter child "ghost"`) {
		t.Fatalf("unknown adapter open: want rejected with child identity, got %+v", resp)
	}

	// Journal truth: opened, adopted, detached, adopted, closed in order,
	// each on the resolved child's envelope, gapless across the run.
	var wantEvents = []string{SessionLedgerOpened, SessionLedgerAdopted, SessionLedgerDetached, SessionLedgerAdopted, SessionLedgerClosed}
	replay := f.rt.Journal().Replay(0)
	var gotEvents []string
	var seqs []uint64
	for _, ev := range replay {
		if led := ev.GetSessionLedger(); led != nil {
			gotEvents = append(gotEvents, led.GetEvent())
			seqs = append(seqs, ev.GetEventSeq())
		}
	}
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("journaled ledger events = %v, want %v", gotEvents, wantEvents)
	}
	for i, want := range wantEvents {
		if gotEvents[i] != want {
			t.Errorf("journal ledger event [%d] = %q, want %q", i, gotEvents[i], want)
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Errorf("ledger event seqs not gapless: %d then %d", seqs[i-1], seqs[i])
		}
	}
	// Envelope attribution rides the journaled event, not the payload.
	for _, ev := range replay {
		if ev.GetSessionLedger() != nil && ev.GetEventSeq() == seqs[0] {
			if ev.GetAdapterType() != "fakex" || ev.GetScope() != "sc" || ev.GetSessionId() != "sx" {
				t.Errorf("ledger envelope = adapter %q scope %q session %q", ev.GetAdapterType(), ev.GetScope(), ev.GetSessionId())
			}
		}
	}
	// The close reason rides the payload on the detached + closed facts.
	for _, ev := range replay {
		if led := ev.GetSessionLedger(); led != nil && led.GetEvent() == SessionLedgerDetached && led.GetReason() != "handoff" {
			t.Errorf("detached fact lost its reason: %+v", led)
		}
	}
}

// TestServer_ControlSessionLedgerServeAdapterRole checks the ledger in the
// serve-adapter role (rt == nil): the served workflow adapter is the only
// attribution subject, the journal is the caller-supplied one (nil
// defaulting exercised), and both verbs answer through the production
// NewServeAdapterServer constructor path.
func TestServer_ControlSessionLedgerServeAdapterRole(t *testing.T) {
	f := newPeerServeFixture(t)
	server := NewServeAdapterServer(&f.cfg, f.child, nil, captureLogger(&bytes.Buffer{}))

	resp, err := server.Control(f.ctx, &criteriav1.ControlRequest{
		Scope: "sc",
		Kind:  &criteriav1.ControlRequest_OpenSession{OpenSession: &criteriav1.OpenSession{SessionId: "sx"}},
	})
	if err != nil {
		t.Fatalf("open in serve-adapter role: %v", err)
	} else if !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "opened") {
		t.Fatalf("open in serve-adapter role: want accepted opened, got %+v", resp)
	}
	resp, err = server.Control(f.ctx, &criteriav1.ControlRequest{
		Scope: "sc",
		Kind:  &criteriav1.ControlRequest_CloseSession{CloseSession: &criteriav1.CloseSession{SessionId: "sx"}},
	})
	if err != nil {
		t.Fatalf("close in serve-adapter role: %v", err)
	}
	if !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "closed") {
		t.Fatalf("close in serve-adapter role: want accepted closed, got %+v", resp)
	}
	// The serve-adapter role hosts only its served adapter.
	resp, err = server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "other",
		Scope:       "sc",
		Kind:        &criteriav1.ControlRequest_CloseSession{CloseSession: &criteriav1.CloseSession{SessionId: "sx"}},
	})
	if err != nil {
		t.Fatalf("mismatched adapter close: %v", err)
	}
	if resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "serve-adapter hosts only") {
		t.Fatalf("mismatched adapter close: want rejected, got %+v", resp)
	}
	// The ledger facts were journaled on the serve-adapter journal with the
	// served adapter as attribution.
	var opened, closed int
	for _, ev := range server.serveAdapterJournal.Replay(0) {
		if led := ev.GetSessionLedger(); led != nil {
			if ev.GetAdapterType() != "fakex" {
				t.Errorf("serve-adapter ledger attribution = %q, want the served adapter", ev.GetAdapterType())
			}
			switch led.GetEvent() {
			case SessionLedgerOpened:
				opened++
			case SessionLedgerClosed:
				closed++
			}
		}
	}
	if opened != 1 || closed != 1 {
		t.Fatalf("serve-adapter journal opened/closed facts = %d/%d, want 1/1", opened, closed)
	}
}

// TestEventJournal_SessionLedgerRoundTrip covers the journal side of the
// ADR-0014 M6 arm: the four ledger verbs append like every other known
// supervision payload (capability-agnostic durable truth) and survive
// Replay in order, and out-of-vocabulary event strings are rejected instead
// of journaled.
func TestEventJournal_SessionLedgerRoundTrip(t *testing.T) {
	j := NewEventJournal(8)

	for i, verb := range []string{SessionLedgerOpened, SessionLedgerAdopted, SessionLedgerDetached, SessionLedgerClosed} {
		if _, err := j.Append(&criteriav1.SupervisionEvent_SessionLedger{
			SessionLedger: &criteriav1.SessionLedger{Event: verb, Reason: "why"},
		}, "fakex", "sc", "sx"); err != nil {
			t.Fatalf("append %s: %v", verb, err)
		}
		replay := j.Replay(0)
		ev := replay[len(replay)-1]
		led := ev.GetSessionLedger()
		if led == nil || led.GetEvent() != verb || led.GetReason() != "why" {
			t.Fatalf("replay[%d] = %+v, want ledger %s", i, led, verb)
		}
		if ev.GetAdapterType() != "fakex" || ev.GetScope() != "sc" || ev.GetSessionId() != "sx" {
			t.Errorf("replay[%d] envelope = %q/%q/%q", i, ev.GetAdapterType(), ev.GetScope(), ev.GetSessionId())
		}
	}

	if _, err := j.Append(&criteriav1.SupervisionEvent_SessionLedger{
		SessionLedger: &criteriav1.SessionLedger{Event: "evaporated"},
	}, "fakex", "sc", "sx"); err == nil {
		t.Fatal("unknown ledger event accepted into the journal")
	} else if !strings.Contains(err.Error(), "vocabulary") {
		t.Errorf("unknown ledger event error = %v, want vocabulary mention", err)
	}
}

// TestServer_ControlRespawnChildDeadGeneration drives Control RespawnChild
// end to end over a real adapter subprocess: after a crash the host's
// respawn decree overrides the ensure-path crash no-respawn gate and
// produces a fresh generation synchronously — the accepted response speaks
// for a journaled spawn of a new pid.
func TestServer_ControlRespawnChildDeadGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("respawn test spawns a real adapter subprocess")
	}
	f := newRespawnControlFixture(t)
	defer f.shutdown()

	oldPid := f.killChildWithSignal(t)

	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_RespawnChild{RespawnChild: &criteriav1.RespawnChild{PolicyGraceMs: 0}},
	})
	if err != nil {
		t.Fatalf("respawn dead generation: %v", err)
	}
	if !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "respawned") {
		t.Fatalf("respawn dead generation: want accepted respawned, got %+v", resp)
	}
	newPid := f.liveChildPID(t)
	if newPid == oldPid || newPid <= 0 {
		t.Fatalf("child pid after respawn = %d (was %d): no fresh generation", newPid, oldPid)
	}
	spawnFacts := 0
	for _, ev := range f.runtime.Journal().Replay(0) {
		if ev.GetSpawned() != nil {
			spawnFacts++
		}
	}
	if spawnFacts < 2 {
		t.Fatalf("journal spawn facts after respawn = %d, want >= 2", spawnFacts)
	}
}

// TestServer_ControlRespawnChildLiveGrace drives the live-generation path:
// the decree is accepted with the declared policy grace answered verbatim,
// the displacement lands after the grace window detached from the answering
// RPC (it must survive the caller moving on), the stale generation's exit
// is journaled as graceful BEFORE the successor's spawn fact, and no crash
// fact appears for a peer-displaced death.
func TestServer_ControlRespawnChildLiveGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("respawn test spawns a real adapter subprocess")
	}
	f := newRespawnControlFixture(t)
	defer f.shutdown()

	oldPid := f.liveChildPID(t)
	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_RespawnChild{RespawnChild: &criteriav1.RespawnChild{PolicyGraceMs: 150}},
	})
	if err != nil {
		t.Fatalf("respawn live generation: %v", err)
	}
	if !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "policy grace") || !strings.Contains(resp.GetDetail(), "150ms") {
		t.Fatalf("respawn live generation: want accepted with declared grace, got %+v", resp)
	}
	// The displacement is asynchronous under the declared grace: wait for a
	// second spawn fact of the same definition.
	secondSpawn := func() bool {
		for i := 0; i < 300; i++ {
			spawnFacts := 0
			for _, ev := range f.runtime.Journal().Replay(0) {
				if ev.GetSpawned() != nil {
					spawnFacts++
				}
			}
			if spawnFacts >= 2 {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	if !secondSpawn() {
		t.Fatal("displacement never spawned a second generation")
	}
	if newPid := f.liveChildPID(t); newPid == oldPid || newPid <= 0 {
		t.Fatalf("child pid after displacement = %d (was %d): no displacement", newPid, oldPid)
	}
	// Ordering + graceful accounting, positional: exactly two spawned facts
	// (boot + displacement) and exactly one exited fact (the stale
	// generation), with the exit journaled graceful and strictly between
	// the two spawn facts.
	var spawnedSeqs, exitedSeqs []uint64
	var crashFacts int
	for _, ev := range f.runtime.Journal().Replay(0) {
		if ev.GetSpawned() != nil {
			spawnedSeqs = append(spawnedSeqs, ev.GetEventSeq())
		}
		if ev.GetExited() != nil {
			exitedSeqs = append(exitedSeqs, ev.GetEventSeq())
			if !ev.GetExited().GetGraceful() {
				t.Errorf("displaced generation exit not graceful: %+v", ev.GetExited())
			}
		}
		if ev.GetCrash() != nil {
			crashFacts++
		}
	}
	if len(spawnedSeqs) != 2 {
		t.Fatalf("spawned fact count = %d (seqs %v), want exactly boot + displacement", len(spawnedSeqs), spawnedSeqs)
	}
	if len(exitedSeqs) != 1 {
		t.Fatalf("exited fact count = %d (seqs %v), want exactly the displaced generation", len(exitedSeqs), exitedSeqs)
	}
	if !(spawnedSeqs[0] < exitedSeqs[0] && exitedSeqs[0] < spawnedSeqs[1]) {
		t.Errorf("stale exit (seq %d) not journaled between boot spawn (%d) and successor spawn (%d)",
			exitedSeqs[0], spawnedSeqs[0], spawnedSeqs[1])
	}
	if crashFacts != 0 {
		t.Errorf("peer-displaced death journal produced %d crash fact(s)", crashFacts)
	}
}

// TestServer_ControlRespawnChildGuards pins the fail-closed rejections: a
// kill_child-commanded generation stays dead (the kill decree outranks the
// respawn decree) and addressing a definition outside the boot spec is
// rejected with the child name in the detail.
func TestServer_ControlRespawnChildGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("respawn test spawns a real adapter subprocess")
	}
	f := newRespawnControlFixture(t)
	defer f.shutdown()

	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "ghost",
		Kind:        &criteriav1.ControlRequest_RespawnChild{RespawnChild: &criteriav1.RespawnChild{PolicyGraceMs: 0}},
	})
	if err != nil {
		t.Fatalf("respawn unknown adapter: %v", err)
	}
	if resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "ghost") {
		t.Fatalf("respawn unknown adapter: want rejected with the child name, got %+v", resp)
	}

	f.runtime.mu.Lock()
	f.runtime.children["noop"].killRequested = true
	f.runtime.mu.Unlock()
	resp, err = f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_RespawnChild{RespawnChild: &criteriav1.RespawnChild{PolicyGraceMs: 0}},
	})
	if err != nil {
		t.Fatalf("respawn kill-guarded child: %v", err)
	}
	if resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "no respawn") {
		t.Fatalf("respawn kill-guarded child: want rejected, got %+v", resp)
	}
}

// TestServer_ControlIdleReapDeclaredWindow drives Control IdleReap through
// the negotiated surface: the window is recorded policy (readable on the
// runtime, no journal fact), the response echoes the effective window,
// re-declaring overwrites, and a zero window withdraws.
func TestServer_ControlIdleReapDeclaredWindow(t *testing.T) {
	f := newPeerServeFixture(t)
	f.rt.mu.Lock()
	f.rt.children["fakex"] = &childState{name: "fakex"}
	f.rt.mu.Unlock()

	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "fakex",
		Kind:        &criteriav1.ControlRequest_IdleReap{IdleReap: &criteriav1.IdleReap{AfterMs: 45000}},
	})
	if err != nil {
		t.Fatalf("idle_reap declare: %v", err)
	}
	if !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "declared") {
		t.Fatalf("idle_reap declare: want accepted declared, got %+v", resp)
	}
	if window, ok := f.rt.idleReapWindow("fakex"); !ok || window != 45*time.Second {
		t.Fatalf("declared window = %v (ok=%v), want 45s", window, ok)
	}

	// Re-declare overwrites the window.
	if _, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "fakex",
		Kind:        &criteriav1.ControlRequest_IdleReap{IdleReap: &criteriav1.IdleReap{AfterMs: 15000}},
	}); err != nil {
		t.Fatalf("idle_reap re-declare: %v", err)
	}
	if window, ok := f.rt.idleReapWindow("fakex"); !ok || window != 15*time.Second {
		t.Fatalf("re-declared window = %v (ok=%v), want 15s", window, ok)
	}

	// A zero window withdraws the policy.
	resp, err = f.server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "fakex",
		Kind:        &criteriav1.ControlRequest_IdleReap{IdleReap: &criteriav1.IdleReap{AfterMs: 0}},
	})
	if err != nil {
		t.Fatalf("idle_reap withdraw: %v", err)
	}
	if !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "withdrawn") {
		t.Fatalf("idle_reap withdraw: want accepted withdrawn, got %+v", resp)
	}
	if window, ok := f.rt.idleReapWindow("fakex"); ok {
		t.Fatalf("withdrawn policy still readable: %v", window)
	}

	// Unknown adapter is rejected with the child name.
	resp, err = f.server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "ghost",
		Kind:        &criteriav1.ControlRequest_IdleReap{IdleReap: &criteriav1.IdleReap{AfterMs: 1000}},
	})
	if err != nil {
		t.Fatalf("idle_reap unknown adapter: %v", err)
	}
	if resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "no adapter child") {
		t.Fatalf("idle_reap unknown adapter: want rejected, got %+v", resp)
	}

	// Declarations are policy, not lifecycle truth: no journal fact.
	for _, ev := range f.rt.Journal().Replay(0) {
		if ev.GetSessionLedger() != nil {
			t.Errorf("idle_reap declaration journal produced a ledger fact: %+v", ev)
		}
	}
}

// TestServer_ControlIdleReapAfterMsOverflow pins the declare-side bound:
// an after_ms beyond the representable time.Duration range is rejected
// instead of wrapping into a negative window and masquerading as a
// withdrawal.
func TestServer_ControlIdleReapAfterMsOverflow(t *testing.T) {
	f := newPeerServeFixture(t)
	f.rt.mu.Lock()
	f.rt.children["fakex"] = &childState{name: "fakex"}
	f.rt.mu.Unlock()

	for _, tc := range []struct {
		name    string
		afterMs uint64
	}{
		{"just past the bound", uint64(maxIdleReapAfterMs) + 1},
		{"wraps int64", 1 << 63},
	} {
		resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
			AdapterType: "fakex",
			Kind:        &criteriav1.ControlRequest_IdleReap{IdleReap: &criteriav1.IdleReap{AfterMs: tc.afterMs}},
		})
		if err != nil {
			t.Fatalf("idle_reap %s: %v", tc.name, err)
		}
		if resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "exceeds") {
			t.Fatalf("idle_reap %s: want rejected with the bound, got %+v", tc.name, resp)
		}
		if _, ok := f.rt.idleReapWindow("fakex"); ok {
			t.Fatalf("idle_reap %s: rejected declare left a policy behind", tc.name)
		}
	}

	// The boundary value itself is representable and declares cleanly.
	resp, err := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		AdapterType: "fakex",
		Kind:        &criteriav1.ControlRequest_IdleReap{IdleReap: &criteriav1.IdleReap{AfterMs: uint64(maxIdleReapAfterMs)}},
	})
	if err != nil {
		t.Fatalf("idle_reap boundary declare: %v", err)
	}
	if !resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "declared") {
		t.Fatalf("idle_reap boundary declare: want accepted declared, got %+v", resp)
	}
	if window, ok := f.rt.idleReapWindow("fakex"); !ok || window <= 0 {
		t.Fatalf("boundary window = %v (ok=%v), want positive", window, ok)
	}
}

// respawnControlFixture couples a real-subprocess peer runtime with its
// phone-home server so the M6 control arms round-trip through the entire
// negotiated Control path (child_respawn_test.go fixture shape).
type respawnControlFixture struct {
	t        *testing.T
	server   *Server
	runtime  *peerRuntime
	ctx      context.Context
	shutdown func()
}

// newRespawnControlFixture boots the noop adapter as a real child process
// and its phone-home server. All capabilities are default (session.v1
// negotiated) so the arms exercise the runtime, not the gate.
func newRespawnControlFixture(t *testing.T) *respawnControlFixture {
	t.Helper()
	bin := buildNoopAdapter(t)

	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:7999"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "noop"
	cfg.AdapterBinary = bin
	cfg.JournalLimit = 32

	var logs bytes.Buffer
	rt := NewRuntime(&cfg, captureLogger(&logs))
	rt.exitPoll = 20 * time.Millisecond
	rt.shutdownGrace = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	if err := rt.Boot(ctx); err != nil {
		cancel()
		t.Fatalf("boot: %v", err)
	}
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	return &respawnControlFixture{
		t:       t,
		server:  server,
		runtime: rt,
		ctx:     ctx,
		shutdown: func() {
			cancel()
			if err := rt.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		},
	}
}

// killChildWithSignal kills the live child out from under the runtime with
// an ungraceful signal (crash semantics, no killRequested flag) and blocks
// until the exit watcher observes it. Returns the killed pid.
func (f *respawnControlFixture) killChildWithSignal(t *testing.T) int {
	t.Helper()
	oldPid := f.liveChildPID(t)
	if err := syscall.Kill(int(oldPid), syscall.SIGKILL); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	for i := 0; i < 300; i++ {
		f.runtime.mu.Lock()
		exited := f.runtime.children["noop"] != nil && adapterhost.ProcessExited(f.runtime.children["noop"].handle)
		f.runtime.mu.Unlock()
		if exited {
			return oldPid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child exit never observed")
	return 0
}

// liveChildPID returns the live child's pid (0 when none).
func (f *respawnControlFixture) liveChildPID(t *testing.T) int {
	t.Helper()
	f.runtime.mu.Lock()
	defer f.runtime.mu.Unlock()
	c := f.runtime.children["noop"]
	if c == nil {
		return 0
	}
	pid, _ := adapterhost.ProcessPID(c.handle)
	return int(pid)
}

package remote

// peer_crash_test.go — T-07 peer-path crash semantics end to end over the
// wire: the supervision journal's CrashClassified.reason is the wire fact
// the host consumes verbatim (SupervisedHandle seam), the CRI-287 teardown
// window routes a peer child's death that arrives with the engine's
// canceled turn as timeout, and pre-crash adapter Log lines reach the host
// sink even when the Execute stream died from the crash (evidence survives).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/tunables"
	"github.com/brokenbots/criteria/workflow"
)

// peerLoader is a fake adapterhost.Loader handing out a pre-resolved peer
// handle, so the SessionManager binds a real peerHandle through the normal
// Open path and drives it through Execute (capability caching, log stream
// start, crash classification all real).
type peerLoader struct {
	handle adapterhost.Handle
}

func (l *peerLoader) Resolve(context.Context, string) (adapterhost.Handle, error) {
	return l.handle, nil
}

func (l *peerLoader) Shutdown(context.Context) error { return nil }

// startPeerSessionManager builds a SessionManager whose session is bound to
// the live peerHandle, mirroring how a real remote adapter session is opened.
func startPeerSessionManager(t *testing.T, provider *peerSessionProvider) (*adapterhost.SessionManager, string, *peerHandle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ph, ok := handle.(*peerHandle)
	if !ok {
		t.Fatalf("handle type %T, want *peerHandle", handle)
	}
	sm := adapterhost.NewSessionManager(&peerLoader{handle: handle})
	if err := sm.Open(ctx, "noop.develop", "noop", "", nil, nil); err != nil {
		t.Fatalf("Open on peer handle: %v", err)
	}
	return sm, "noop.develop", ph
}

// journalCrash appends the ungraceful-exit journal pair the peer child
// records (Exited first, then CrashClassified — internal/peer/child.go).
func journalCrash(fp *fakePeer, exitCode int32, reason, detail string) {
	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: exitCode}},
	})
	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Crash{Crash: &criteriav1.CrashClassified{Reason: reason, Detail: detail}},
	})
}

// TestPeerSupervisionCrashReasonWireFact pins the SupervisedHandle seam on
// the real peerHandle: the journal's classification arrives verbatim, and a
// journal with only the plain Exited record reports no classification (the
// host reads the terminal process fact from the journal and treats it as no
// crash for the exited child — journal-only classification, ADR-0014 M5).
func TestPeerSupervisionCrashReasonWireFact(t *testing.T) {
	t.Run("journal CrashClassified is the wire fact", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		journalCrash(fp, 143, adapterhost.CrashReasonProcessTerminated, "adapter child exited while supervised")
		fp.connect(t, addr)

		_, _, ph := startPeerSessionManager(t, provider)
		waitFor(t, "terminal journal fact", func() bool {
			return ph.ps.processExited(ph.name)
		})

		// The Exited and Crash records are separate journal events, so the
		// classification can still be in flight when the terminal fact
		// lands: wait for the classification itself before pinning the
		// wire fact.
		waitFor(t, "supervision classification from journal", func() bool {
			_, ok := adapterhost.SupervisionCrashReason(ph)
			return ok
		})
		reason, ok := adapterhost.SupervisionCrashReason(ph)
		if !ok {
			t.Fatal("SupervisionCrashReason = ok=false, want the journal classification")
		}
		if reason != adapterhost.CrashReasonProcessTerminated {
			t.Fatalf("reason = %q, want the wire fact verbatim %q", reason, adapterhost.CrashReasonProcessTerminated)
		}
	})
	t.Run("exited only reports no classification", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		fp.appendEvent(&criteriav1.SupervisionEvent{
			Kind: &criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: 1}},
		})
		fp.connect(t, addr)

		_, _, ph := startPeerSessionManager(t, provider)
		waitFor(t, "terminal journal fact", func() bool {
			return ph.ps.processExited(ph.name)
		})

		if reason, ok := adapterhost.SupervisionCrashReason(ph); ok {
			t.Fatalf("SupervisionCrashReason = (%q, true), want ok=false for the plain Exited placeholder", reason)
		}
	})
	t.Run("live child reports no classification", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		fp.connect(t, addr)

		_, _, ph := startPeerSessionManager(t, provider)
		if _, ok := adapterhost.SupervisionCrashReason(ph); ok {
			t.Fatal("SupervisionCrashReason must report ok=false for a live child")
		}
	})
}

// TestPeerSettledSupervisionCrashReason pins the remote settle arm
// (JournalCrashSettler): the peer's exit watcher emits its terminal journal
// facts up to one poll tick after the transport already failed the host's
// call, so a classification racing a fresh child death reads an empty
// journal. The settled query bridges that in-flight window on the live
// Supervise stream and classifies exactly like the plain query once the
// facts land; a placeholder-only journal and a budget that expires
// without facts keep the fail-closed no-classification shape.
func TestPeerSettledSupervisionCrashReason(t *testing.T) {
	t.Run("facts arriving during the settle window classify", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		fp.connect(t, addr)

		_, _, ph := startPeerSessionManager(t, provider)

		// The crash facts land mid-flight, after the settled query started
		// polling — the exact shape classification races on a real peer
		// (the host's Execute fails before the peer's exit watcher journals
		// the death).
		go func() {
			time.Sleep(100 * time.Millisecond)
			journalCrash(fp, -1, adapterhost.CrashReasonProcessTerminated, "adapter child exited while supervised (exit code -1, signal 9)")
		}()

		settleCtx, cancel := context.WithTimeout(context.Background(), tunables.DefaultCrashCloseSettle)
		defer cancel()
		reason, ok := adapterhost.SettledSupervisionCrashReason(settleCtx, ph, tunables.DefaultCrashCloseSettle)
		if !ok {
			t.Fatal("settled query = ok=false, want the journal classification once the facts arrive mid-window")
		}
		if reason != adapterhost.CrashReasonProcessTerminated {
			t.Fatalf("reason = %q, want %q", reason, adapterhost.CrashReasonProcessTerminated)
		}
	})

	t.Run("exited placeholder settles with no classification", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		fp.appendEvent(&criteriav1.SupervisionEvent{
			Kind: &criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: 1}},
		})
		fp.connect(t, addr)

		_, _, ph := startPeerSessionManager(t, provider)
		waitFor(t, "terminal journal fact", func() bool {
			return ph.ps.processExited(ph.name)
		})

		if reason, ok := adapterhost.SettledSupervisionCrashReason(context.Background(), ph, 100*time.Millisecond); ok {
			t.Fatalf("settled query = (%q, true), want ok=false for the plain Exited placeholder", reason)
		}
	})
}

// TestPeerLostConnClassifiesWithoutJournalFact pins the connection-level
// structural fact the peer synthesizes when the supervision journal holds
// no terminal (ADR-0014 M5): with peerHandle.ProcessExited gone, a conn
// drop is the only evidence the host gets, so the lost-conn arm must fire —
// otherwise a peer session with a dead conn and no journal fact would never
// classify as a crash and silently miss the KB-246 recreate window. The
// journal is left empty on purpose: a delivered CrashClassified would mask
// the deletion of the lost-conn arm.
func TestPeerLostConnClassifiesWithoutJournalFact(t *testing.T) {
	t.Run("lost conn with no child run: TransportClosed, end to end", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		fp.connect(t, addr)

		sm, name, ph := startPeerSessionManager(t, provider)
		fp.drop()
		waitFor(t, "peer session marked lost", func() bool {
			return ph.ps.lost.Load()
		})

		reason, ok := adapterhost.SupervisionCrashReason(ph)
		if !ok {
			t.Fatal("SupervisionCrashReason = ok=false on a lost conn with no journal terminal, want (TransportClosed, true)")
		}
		if reason != adapterhost.CrashReasonTransportClosed {
			t.Fatalf("reason = %q, want %q (the lost-conn crash fact)", reason, adapterhost.CrashReasonTransportClosed)
		}

		// No teardown mark: the dead conn is the crash. The host classifies
		// the failure from the synthesized lost-conn fact alone — no error
		// text, no journal terminal.
		coll := &peerEventCollector{}
		_, err := sm.Execute(context.Background(), name, &workflow.StepNode{Name: "develop"}, coll, nil)
		var crashErr *adapterhost.SessionCrashError
		if !errors.As(err, &crashErr) || crashErr.Session != name {
			t.Fatalf("Execute err = %v, want SessionCrashError for %s (lost conn with no journal fact must still crash-classify)", err, name)
		}
		event, ok := coll.first("session.crash")
		if !ok {
			t.Fatal("expected the session.crash event")
		}
		if got := event["crash_reason"]; got != adapterhost.CrashReasonTransportClosed {
			t.Errorf("crash_reason = %q, want %q", got, adapterhost.CrashReasonTransportClosed)
		}
	})

	t.Run("lost conn with in-flight child run: ChildRunLost", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		// The child run started and never reached a terminal arm: the run
		// arm outranks the plain transport fact (ADR-0008 child-run loss).
		fp.appendEvent(childRunStartedArm("run-lost-conn"))
		fp.connect(t, addr)

		_, _, ph := startPeerSessionManager(t, provider)
		waitForInFlightRun(t, ph.ps, "run-lost-conn")
		fp.drop()
		waitFor(t, "peer session marked lost", func() bool {
			return ph.ps.lost.Load()
		})

		reason, ok := adapterhost.SupervisionCrashReason(ph)
		if !ok {
			t.Fatal("SupervisionCrashReason = ok=false on a lost conn with an in-flight child run, want (ChildRunLost, true)")
		}
		if reason != adapterhost.CrashReasonChildRunLost {
			t.Fatalf("reason = %q, want %q (the in-flight run outranks the transport fact)", reason, adapterhost.CrashReasonChildRunLost)
		}
	})
}

// TestPeerCRI287TeardownWindowPeerPath pins requirement 2 end to end on the
// peer path: with the engine-initiated step-timeout teardown window open, a
// supervision-delivered ProcessExited arriving with the engine's canceled
// turn is a teardown consequence — the failed Execute routes as timeout, not
// a crash. Once the window expires, the same wire fact classifies the crash
// verbatim through the SessionManager.
func TestPeerCRI287TeardownWindowPeerPath(t *testing.T) {
	t.Run("inside the window: routed as timeout, not a crash", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		journalCrash(fp, 143, adapterhost.CrashReasonProcessTerminated, "adapter child exited while supervised")
		fp.connect(t, addr)

		sm, name, _ := startPeerSessionManager(t, provider)
		sm.StepTimeoutTeardownWindow = time.Minute
		sm.MarkEngineStepTimeoutTeardown()

		// The crash tears the shared phone-home conn; the in-flight step's
		// Execute stream dies with the turn.
		fp.drop()
		coll := &peerEventCollector{}
		_, err := sm.Execute(context.Background(), name, &workflow.StepNode{Name: "develop"}, coll, nil)
		if err == nil {
			t.Fatal("expected the Execute to fail on the dead conn")
		}
		var crashErr *adapterhost.SessionCrashError
		if errors.As(err, &crashErr) {
			t.Fatalf("Execute err = %v, want the raw transport error (timeout routing inside the window)", err)
		}
		if _, ok := coll.first("session.crash"); ok {
			t.Error("session.crash event must not be emitted inside the teardown window")
		}
	})

	t.Run("outside the window: wire fact classifies verbatim", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		journalCrash(fp, 143, adapterhost.CrashReasonProcessTerminated, "adapter child exited while supervised")
		fp.connect(t, addr)

		sm, name, ph := startPeerSessionManager(t, provider)
		// The journal's Exited + CrashClassified replay onto the peerHandle
		// asynchronously; wait for the wire facts to land before dropping
		// the conn, so the classification is delivered before the transport
		// dies (otherwise CI races a bare codes.Canceled past handleCrash).
		waitFor(t, "terminal journal fact", func() bool {
			return ph.ps.processExited(ph.name)
		})
		waitFor(t, "CrashClassified from journal", func() bool {
			r, ok := adapterhost.SupervisionCrashReason(ph)
			return ok && r == adapterhost.CrashReasonProcessTerminated
		})
		// No teardown mark: the death is a genuine crash.
		fp.drop()
		coll := &peerEventCollector{}
		_, err := sm.Execute(context.Background(), name, &workflow.StepNode{Name: "develop"}, coll, nil)
		var crashErr *adapterhost.SessionCrashError
		if !errors.As(err, &crashErr) || crashErr.Session != name {
			t.Fatalf("Execute err = %v, want SessionCrashError for %s", err, name)
		}
		event, ok := coll.first("session.crash")
		if !ok {
			t.Fatal("expected the session.crash event")
		}
		if got := event["crash_reason"]; got != adapterhost.CrashReasonProcessTerminated {
			t.Errorf("crash_reason = %q, want the journal wire fact verbatim %q", got, adapterhost.CrashReasonProcessTerminated)
		}
	})
}

// TestPeerLogEvidenceSurvivesCrash pins the evidence-survives property
// (review §8.1): pre-crash adapter Log lines reach the host sink through the
// peer Log stream, and the StreamFlushed drain marker is set — both survive
// a crash that kills the Execute stream, so the host keeps the pre-crash log
// evidence for postmortems.
func TestPeerLogEvidenceSurvivesCrash(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ph := handle.(*peerHandle)
	ps := mustPeerSession(t, provider)

	// The peer drained its log backlog into the journal (StreamFlushed on
	// the log channel): the host-side drain marker the evidence path reads.
	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Flushed{Flushed: &criteriav1.StreamFlushed{Channel: "log", UpToSeq: 4}},
	})
	waitFor(t, "log drain marker", func() bool {
		return ps.logDrained(ps.dial.AdapterType)
	})

	coll := &logEventCollector{}
	starter, ok := handle.(adapterhost.LogStreamStarter)
	if !ok {
		t.Fatalf("peer handle %T does not implement LogStreamStarter", handle)
	}
	cancelLog, _, err := starter.StartLogStream(ctx, "s1", coll)
	if err != nil {
		t.Fatalf("StartLogStream: %v", err)
	}
	defer cancelLog()
	waitFor(t, "pre-crash log line at the host sink", func() bool {
		return coll.count("hello from peer") > 0
	})

	// Snapshot the evidence accumulated before the crash: it must survive
	// verbatim, so a regression that drops the sink or overwrites the
	// journal backlog fails even if some lines still trickle through after.
	coll.mu.Lock()
	beforeCrash := append([]string(nil), coll.lines...)
	coll.mu.Unlock()

	// The crash kills the Execute stream (and with it the shared conn), but
	// the evidence already at the host sink must survive.
	fp.drop()
	if _, execErr := ph.Execute(ctx, "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{}, nil); execErr == nil {
		t.Fatal("expected the Execute stream to die with the crash")
	}
	// The crash is terminal on the peer path: a follow-up Execute on the
	// dead handle keeps failing, which is what re-arms wire-fact
	// classification once the CRI-287 teardown window has expired.
	if _, execErr := ph.Execute(ctx, "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{}, nil); execErr == nil {
		t.Fatal("expected the follow-up Execute on the dead peer handle to fail as well")
	}
	coll.mu.Lock()
	after := append([]string(nil), coll.lines...)
	coll.mu.Unlock()
	if len(after) < len(beforeCrash) {
		t.Fatalf("host sink lost pre-crash log lines after the crash (before=%d, after=%d)", len(beforeCrash), len(after))
	}
	for i, line := range beforeCrash {
		if after[i] != line {
			t.Fatalf("host sink log line %d changed across the crash: before %q, after %q", i, line, after[i])
		}
	}
	if !ps.logDrained(ps.dial.AdapterType) {
		t.Fatal("StreamFlushed drain marker must survive the crash")
	}
}

// peerEventCollector is an adapter.EventSink collecting the SessionManager's
// adapter events for a peer-path Execute (the local adapterEventCollector
// lives in the adapterhost package).
type peerEventCollector struct {
	mu     sync.Mutex
	events []peerEventRecord
}

type peerEventRecord struct {
	kind string
	data map[string]any
}

func (c *peerEventCollector) Log(string, []byte) {}

func (c *peerEventCollector) Adapter(kind string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var payload map[string]any
	if m, ok := data.(map[string]any); ok {
		payload = m
	}
	c.events = append(c.events, peerEventRecord{kind: kind, data: payload})
}

func (c *peerEventCollector) first(kind string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

// logEventCollector is an adapterhost.LogEventSink collecting raw lines.
type logEventCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *logEventCollector) Emit(ev *v2.LogEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, string(ev.GetLine()))
	return nil
}

func (c *logEventCollector) count(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

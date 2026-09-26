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
// host then falls through to the ProcessExited evidence path).
func TestPeerSupervisionCrashReasonWireFact(t *testing.T) {
	t.Run("journal CrashClassified is the wire fact", func(t *testing.T) {
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp := newFakePeer("")
		journalCrash(fp, 143, adapterhost.CrashReasonProcessTerminated, "adapter child exited while supervised")
		fp.connect(t, addr)

		_, _, ph := startPeerSessionManager(t, provider)
		reporter := adapterhost.ProcessExitReporter(ph)
		waitFor(t, "ProcessExited from journal", reporter.ProcessExited)

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
		reporter := adapterhost.ProcessExitReporter(ph)
		waitFor(t, "ProcessExited from journal", reporter.ProcessExited)

		if reason, ok := adapterhost.SupervisionCrashReason(ph); ok {
			t.Fatalf("SupervisionCrashReason = (%q, true), want ok=false for the plain Exited placeholder", reason)
		}
		if !adapterhost.ProcessExited(ph) {
			t.Fatal("ProcessExited must stay true so classification falls through to the exit evidence")
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
		_, err := sm.Execute(context.Background(), name, &workflow.StepNode{Name: "develop"}, coll)
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

		sm, name, _ := startPeerSessionManager(t, provider)
		// No teardown mark: the death is a genuine crash.
		fp.drop()
		coll := &peerEventCollector{}
		_, err := sm.Execute(context.Background(), name, &workflow.StepNode{Name: "develop"}, coll)
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
	ps := mustPeerSession(t, provider, "noop", "")

	// The peer drained its log backlog into the journal (StreamFlushed on
	// the log channel): the host-side drain marker the evidence path reads.
	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Flushed{Flushed: &criteriav1.StreamFlushed{Channel: "log", UpToSeq: 4}},
	})
	waitFor(t, "log drain marker", ps.logDrained)

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

	// The crash kills the Execute stream (and with it the shared conn), but
	// the evidence already at the host sink must survive.
	fp.drop()
	if _, execErr := ph.Execute(ctx, "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{}); execErr == nil {
		t.Fatal("expected the Execute stream to die with the crash")
	}
	if got := coll.count("hello from peer"); got < 1 {
		t.Fatalf("host sink lost pre-crash log lines after the crash (count=%d)", got)
	}
	if !ps.logDrained() {
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

// KB-70 regression tests. The per-scope remote-adapter session wait used to
// fail init with the CRI-137 dead-Job verdict while the adapter pod was still
// Pending — a burst of per-scope pod creations pushes pod start past the
// fixed handshake budget, and a pod that never scheduled cannot handshake.
// These tests pin the two-budget semantics: the handshake budget runs only
// once the adapter has started (pod-state probe Running, or an identity frame
// presented for the session key), while a never-started pod is bounded by the
// separate scheduling budget and fails citing the pod's observed phase
// instead of claiming the Job is complete or dead.
package remote

import (
	"context"
	"strings"
	"testing"
	"time"
)

// podPhaseStep moves a timedPodProbe to a phase once the probe has existed
// for at least after.
type podPhaseStep struct {
	phase string
	after time.Duration
}

// timedPodProbe is a PodStateProbe reporting a scripted phase timeline: the
// phase of the last step whose after has passed, with the first step's phase
// reported until then and the final step repeating forever. Steps must be
// ordered by ascending after. Like the wait's poll loop, only the wait
// goroutine touches it in these tests, so no locking is needed.
type timedPodProbe struct {
	steps []podPhaseStep
	start time.Time
}

func newTimedPodProbe(t *testing.T, steps ...podPhaseStep) *timedPodProbe {
	t.Helper()
	return &timedPodProbe{steps: steps, start: time.Now()}
}

func (p *timedPodProbe) PodState(string, string) (PodState, bool) {
	if len(p.steps) == 0 {
		return PodState{}, false
	}
	idx := 0
	elapsed := time.Since(p.start)
	for i, step := range p.steps {
		if elapsed >= step.after {
			idx = i
		}
	}
	return PodState{Phase: p.steps[idx].phase}, true
}

// newKB70TestShim wraps newCri137TestShim with the KB-70 knobs: scheduling
// budget, fast pod-state polling, and an optional pod-state probe.
func newKB70TestShim(t *testing.T, scope string, handshakeBudget, schedulingBudget time.Duration, probe PodStateProbe) (*Shim, string) {
	t.Helper()
	shim, addr := newCri137TestShim(t, scope, handshakeBudget)
	shim.schedulingBudget = schedulingBudget
	shim.podStatePollInterval = 20 * time.Millisecond
	if probe != nil {
		shim.SetPodStateProbe(probe)
	}
	return shim, addr
}

func TestWaitForFreshHandle_PodPendingPastHandshakeBudget_StillCompletesOnHandshake(t *testing.T) {
	// The refire scenario: the adapter pod is Pending well past the
	// handshake budget (burst scheduling latency) before it goes Running and
	// the adapter phone-homes. The wait must NOT fail while Pending — the
	// handshake budget is frozen until the pod starts — and must complete
	// once the dial lands.
	scope := "root/kb70-pending-then-running"
	probe := newTimedPodProbe(t, podPhaseStep{phase: "Pending"}, podPhaseStep{phase: "Running", after: 250 * time.Millisecond})
	shim, addr := newKB70TestShim(t, scope, 150*time.Millisecond, 30*time.Second, probe)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := shim.WaitForHandle(ctx, "noop", scope)
		done <- err
	}()

	// The pod stays Pending past the 150ms handshake budget — under the old
	// fixed timer the wait would already have failed with the dead-Job
	// verdict before the adapter could possibly dial. At 250ms the pod goes
	// Running (freezing the handshake budget paid off) and the adapter
	// phone-homes promptly after, completing the wait.
	time.Sleep(260 * time.Millisecond)
	if err := dialFakeAdapter(addr, &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Token: "current-token", Scope: scope}, nil); err != nil {
		t.Fatalf("adapter dial: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait failed although the pod went Running and dialed: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("wait never completed after the pod went Running and dialed")
	}
}

func TestWaitForFreshHandle_PodRunningWithoutHandshake_FailsAtHandshakeBudget(t *testing.T) {
	// A pod observed Running that presents no identity frame is genuinely a
	// dead or completed Job: it must fail at the existing handshake budget
	// with the CRI-137 verdict — and must NOT be rescued by the scheduling
	// budget, which stays far away.
	scope := "root/kb70-running-no-handshake"
	probe := newTimedPodProbe(t, podPhaseStep{phase: "Running"})
	shim, _ := newKB70TestShim(t, scope, 150*time.Millisecond, 30*time.Second, probe)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	_, err := shim.WaitForHandle(ctx, "noop", scope)
	if err == nil {
		t.Fatal("wait returned a handle although the Running pod never dialed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "without a successful identity handshake") {
		t.Errorf("handshake-budget error must state the bounded wait, got: %v", err)
	}
	if !strings.Contains(msg, "adapter Job may be complete or dead") {
		t.Errorf("dead-Job diagnosis must be kept for a started pod, got: %v", err)
	}
	if !strings.Contains(msg, "observed Running but never dialed") {
		t.Errorf("error must cite the Running pod state, got: %v", err)
	}
	if strings.Contains(msg, "waiting for the adapter pod to start") {
		t.Errorf("handshake-budget failure must not be reported as scheduling, got: %v", err)
	}
}

func TestWaitForFreshHandle_PodPendingAtSchedulingExpiry_FailsWithPodPhase(t *testing.T) {
	// A pod still Pending when the scheduling budget expires must fail with
	// its actual phase — the failure message must not claim the adapter Job
	// is complete or dead, since a pod that never ran had no Job to die.
	scope := "root/kb70-pending-at-expiry"
	probe := newTimedPodProbe(t, podPhaseStep{phase: "Pending"})
	shim, _ := newKB70TestShim(t, scope, 5*time.Minute, 400*time.Millisecond, probe)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	start := time.Now()
	_, err := shim.WaitForHandle(ctx, "noop", scope)
	if err == nil {
		t.Fatal("wait returned a handle although the pod never started")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("wait failed after %s, want at least the scheduling budget's shape", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "exceeded 400ms waiting for the adapter pod to start") {
		t.Errorf("error must cite the scheduling budget, got: %v", err)
	}
	if !strings.Contains(msg, "no identity handshake observed at all") {
		t.Errorf("error must keep the no-handshake diagnosis anchor, got: %v", err)
	}
	if !strings.Contains(msg, "was never observed Running") || !strings.Contains(msg, `last observed pod phase: "Pending"`) {
		t.Errorf("error must cite the pending pod phase, got: %v", err)
	}
	if strings.Contains(msg, "adapter Job may be complete or dead") {
		t.Errorf("dead-Job verdict is wrong for a pod that never started, got: %v", err)
	}
}

func TestWaitForFreshHandle_PodFailedBeforeDialing_FailsPromptly(t *testing.T) {
	// A terminal probe phase (Job crashed or completed) can never handshake:
	// the wait must fail promptly instead of waiting out either budget.
	scope := "root/kb70-pod-failed"
	probe := newTimedPodProbe(t, podPhaseStep{phase: "Failed"})
	shim, _ := newKB70TestShim(t, scope, 5*time.Minute, 5*time.Minute, probe)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	start := time.Now()
	_, err := shim.WaitForHandle(ctx, "noop", scope)
	if err == nil {
		t.Fatal("wait returned a handle although the pod phase is terminal")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("terminal pod phase failed after %s, want a prompt verdict", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "failed without a successful identity handshake") {
		t.Errorf("error must distinguish the terminal-phase verdict, got: %v", err)
	}
	if !strings.Contains(msg, `phase is "Failed"`) {
		t.Errorf("error must cite the observed phase, got: %v", err)
	}
}

func TestWaitForFreshHandle_NoProbeNoDials_UsesSchedulingGrace(t *testing.T) {
	// Production incident shape: no pod-state probe is wired and the pod
	// never dialed, so nothing at all was observed. The wait must NOT fail
	// at the handshake budget (the pod never scheduled) — it is bounded by
	// the scheduling budget and fails citing the unknown phase and the
	// missing probe, not the dead-Job verdict.
	scope := "root/kb70-no-probe-no-dials"
	shim, _ := newKB70TestShim(t, scope, 150*time.Millisecond, 500*time.Millisecond, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	start := time.Now()
	_, err := shim.WaitForHandle(ctx, "noop", scope)
	if err == nil {
		t.Fatal("wait returned a handle although nothing was ever observed")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("wait failed after %s, want the scheduling grace to hold (KB-70)", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "exceeded 500ms waiting for the adapter pod to start") {
		t.Errorf("error must come from the scheduling budget, got: %v", err)
	}
	if !strings.Contains(msg, "no pod-state probe is wired") {
		t.Errorf("error must disclose the missing pod-state observation, got: %v", err)
	}
	if strings.Contains(msg, "adapter Job may be complete or dead") {
		t.Errorf("dead-Job verdict is wrong for an unobserved pod, got: %v", err)
	}
}

func TestWaitForFreshHandle_NoProbeLateDial_HandshakeBudgetResumes(t *testing.T) {
	// Without a probe, a late identity frame is the only pod-started signal:
	// once it is presented (even if it is then rejected), the handshake
	// budget resumes from that moment instead of the scheduling grace.
	scope := "root/kb70-late-dial"
	shim, addr := newKB70TestShim(t, scope, 150*time.Millisecond, 30*time.Second, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := shim.WaitForHandle(ctx, "noop", scope)
		done <- err
	}()
	waitForWaiterRegistration(t, shim, scope)

	// The dial lands well past the handshake budget: the scheduling grace
	// must absorb the latency, the rejection then resumes the handshake
	// budget, and the wait fails with the rejection diagnosis.
	time.Sleep(400 * time.Millisecond)
	conn := dialRawHandshake(t, addr, &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Token: "stale-token", Scope: scope})
	_ = conn.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wait returned a handle although the dial was rejected")
		}
		msg := err.Error()
		if !strings.Contains(msg, "without a successful identity handshake") || !strings.Contains(msg, "accept_token verification failed for scope") {
			t.Errorf("wait must fail with the rejection diagnosis once a dial was observed, got: %v", err)
		}
		if strings.Contains(msg, "waiting for the adapter pod to start") {
			t.Errorf("an observed dial must stop the scheduling grace, got: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("wait never failed after the late rejected dial")
	}
}

func TestPeerWaitForFreshHandle_PodPendingPastHandshakeBudget_StillCompletesOnHandshake(t *testing.T) {
	// The peer wait path shares awaitWaiter with the legacy path: it must get
	// the same scheduling grace while the pod is Pending past the handshake
	// budget and complete once the pod goes Running and a legacy handshake
	// lands.
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	provider.shim.verifyFailureBudget = 150 * time.Millisecond
	provider.shim.schedulingBudget = 30 * time.Second
	provider.shim.podStatePollInterval = 20 * time.Millisecond
	provider.shim.SetPodStateProbe(newTimedPodProbe(t,
		podPhaseStep{phase: "Pending"},
		podPhaseStep{phase: "Running", after: 250 * time.Millisecond},
	))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := provider.WaitForFreshHandle(ctx, "noop", "", nil)
		if err == nil {
			// Drain the handle so the wake-up path is fully exercised; the
			// session stays bound until the shim stops in fixture cleanup.
		}
		done <- err
	}()

	// The legacy handshake lands after the handshake budget expired
	// (pod still Pending at that point) and shortly after the pod went
	// Running — the same refire shape as the legacy-path test above.
	time.Sleep(260 * time.Millisecond)
	if err := dialFakeAdapter(addr, &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234"}, nil); err != nil {
		t.Fatalf("legacy dial: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("peer wait failed although the legacy dial landed after the handshake budget: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("peer wait never completed")
	}
}
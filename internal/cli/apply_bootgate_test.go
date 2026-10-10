package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/cli/applytest"
)

// Boot-gate environment names (KB-234, mirrored from internal/bootgate).
const (
	bootGateViewURLEnv = "CRITERIA_OPERATOR_VIEW_URL"
	bootGateTicketEnv  = "CRITERIA_RUN_TICKET"
	bootGateJobEnv     = "CRITERIA_RUN_JOB"
)

// operatorViewCall records one operator-view probe.
type operatorViewCall struct {
	ticket string
	job    string
}

// fakeOperatorView serves an httptest operator CriteriaRun view. The server
// closes via t.Cleanup; phase/terminal/status mutate safely between boots
// (the KB-234 integration scenario flips the view as a CR changes state).
type fakeOperatorView struct {
	srv      *httptest.Server
	calls    atomic.Value // []operatorViewCall
	status   atomic.Int64
	phase    atomic.Value // string
	terminal atomic.Bool
}

func newFakeOperatorView(t *testing.T) *fakeOperatorView {
	t.Helper()
	v := &fakeOperatorView{}
	v.status.Store(http.StatusOK)
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call operatorViewCall
		call.ticket = r.URL.Query().Get("ticket")
		call.job = r.URL.Query().Get("job")
		calls := v.loadCalls()
		calls = append(calls, call)
		v.calls.Store(calls)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(v.status.Load()))
		phase, _ := v.phase.Load().(string)
		if phase == "" {
			phase = "Provisioning"
		}
		_, _ = w.Write([]byte(`{"phase":"` + phase + `","terminal":` + boolText(v.terminal.Load()) + `}`))
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fakeOperatorView) loadCalls() []operatorViewCall {
	c, _ := v.calls.Load().([]operatorViewCall)
	return c
}

func (v *fakeOperatorView) setTerminal(phase string) {
	v.phase.Store(phase)
	v.terminal.Store(true)
}

func (v *fakeOperatorView) setNotFound() {
	v.status.Store(http.StatusNotFound)
	v.terminal.Store(false)
	v.phase.Store("deleted")
}

func (v *fakeOperatorView) setLive() {
	v.status.Store(http.StatusOK)
	v.terminal.Store(false)
	v.phase.Store("Provisioning")
}

// bootGateCalls returns the probes the view endpoint received.
func (v *fakeOperatorView) callCount() int { return len(v.loadCalls()) }

// enable sets the operator-injected gate envs for the test.
func (v *fakeOperatorView) enable(t *testing.T) {
	t.Helper()
	t.Setenv(bootGateViewURLEnv, v.srv.URL)
	t.Setenv(bootGateTicketEnv, "kb-234")
	t.Setenv(bootGateJobEnv, "criteria-runner-job")
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestRunApplyServer_BootGateBlocksTerminalCR covers the KB-234 acceptance
// scenario in its terminal flavor: a runner Job pod restarts while its
// operator reports the CriteriaRun already terminal. The boot must exit 0
// without any castle interaction — no registration and no run creation, so
// no pending-forever orphan can be minted the way KB-225's boot-1 did.
func TestRunApplyServer_BootGateBlocksTerminalCR(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	view := newFakeOperatorView(t)
	view.setTerminal("Succeeded")
	view.enable(t)

	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	if err := runApplyServer(context.Background(), applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	}); err != nil {
		t.Fatalf("blocked boot must exit 0, got error: %v", err)
	}
	assertBootGateSilent(t, fake)
	if view.callCount() != 1 {
		t.Errorf("operator view probes = %d, want 1", view.callCount())
	}
}

// TestRunApplyServer_BootGateBlocksDeletedCRWhilePodAlive covers the KB-234
// acceptance scenario in its delete flavor: the operator killed the CR while
// the runner Job pod survived and restarted. The gate must report deleted
// (a 404 for the runner's identity — deletion while the pod is still alive)
// and exit 0 with zero castle interaction.
func TestRunApplyServer_BootGateBlocksDeletedCRWhilePodAlive(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	view := newFakeOperatorView(t)
	view.setNotFound()
	view.enable(t)

	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	if err := runApplyServer(context.Background(), applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	}); err != nil {
		t.Fatalf("blocked boot must exit 0, got error: %v", err)
	}
	assertBootGateSilent(t, fake)
	if view.callCount() != 1 {
		t.Errorf("operator view probes = %d, want 1", view.callCount())
	}
}

// assertBootGateSilent asserts the castle fake received nothing: no Register,
// no CreateRun, no events. A blocked boot must not touch castle at all.
func assertBootGateSilent(t *testing.T, fake *applytest.Fake) {
	t.Helper()
	if fake.RegistrationCount() != 0 {
		t.Errorf("RegistrationCount() = %d, want 0 on a blocked boot", fake.RegistrationCount())
	}
	if fake.CreatedRunCount() != 0 {
		t.Errorf("CreatedRunCount() = %d, want 0 on a blocked boot", fake.CreatedRunCount())
	}
	if evts := fake.Events(); len(evts) != 0 {
		t.Errorf("blocked boot emitted %d castle events, want none", len(evts))
	}
}

// TestRunApplyServer_BootGateProceedsWhenCRLive is the control case: the
// operator reports a live, non-terminal CR, so the boot must behave exactly
// as before the gate existed — one register, one created run, workflow
// completes.
func TestRunApplyServer_BootGateProceedsWhenCRLive(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	view := newFakeOperatorView(t)
	view.setLive()
	view.enable(t)

	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	if err := runApplyServer(context.Background(), applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	}); err != nil {
		t.Fatalf("runApplyServer with a live CR: %v", err)
	}
	if fake.RegistrationCount() != 1 {
		t.Errorf("RegistrationCount() = %d, want 1", fake.RegistrationCount())
	}
	if fake.CreatedRunCount() != 1 {
		t.Errorf("CreatedRunCount() = %d, want 1", fake.CreatedRunCount())
	}
	fake.WaitForCond(t, 15*time.Second, func() bool { return fake.HasEventOfType("RunCompleted") })
}

// TestRunApplyServer_BootGateFailsOpenWhenViewUnavailable pins the unknown
// posture: a view that cannot answer (HTTP 500) must NOT kill the boot (the
// gate is a liveness oracle, not a trust oracle); the boot proceeds fail-open
// with the KB-233 backstops bounding any residual orphan.
func TestRunApplyServer_BootGateFailsOpenWhenViewUnavailable(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	view := newFakeOperatorView(t)
	view.status.Store(http.StatusInternalServerError)
	view.enable(t)

	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	if err := runApplyServer(context.Background(), applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	}); err != nil {
		t.Fatalf("fail-open boot must proceed, got error: %v", err)
	}
	if fake.RegistrationCount() != 1 {
		t.Errorf("RegistrationCount() = %d, want 1 on a fail-open boot", fake.RegistrationCount())
	}
	if fake.CreatedRunCount() != 1 {
		t.Errorf("CreatedRunCount() = %d, want 1 on a fail-open boot", fake.CreatedRunCount())
	}
}

// TestRunApplyServer_BootGateDisabledWithoutEnvs pins the disabled posture:
// without any operator-injected gate env the boot behaves exactly as before
// (standalone orchestrators and local mode are unaffected).
func TestRunApplyServer_BootGateDisabledWithoutEnvs(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)

	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	if err := runApplyServer(context.Background(), applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	}); err != nil {
		t.Fatalf("runApplyServer without gate envs: %v", err)
	}
	if fake.CreatedRunCount() != 1 {
		t.Errorf("CreatedRunCount() = %d, want 1", fake.CreatedRunCount())
	}
}

// TestRunApplyServer_BootGatePartialConfigFailsLoudly pins the loud-error
// posture for a half-configured gate: identity without a view URL must fail
// the boot visibly instead of silently skipping the check.
func TestRunApplyServer_BootGatePartialConfigFailsLoudly(t *testing.T) {
	requireNoGoroutineLeak(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fake := applytest.New(t)
	t.Setenv(bootGateTicketEnv, "kb-234")
	t.Setenv(bootGateJobEnv, "criteria-runner-job")
	// CRITERIA_OPERATOR_VIEW_URL deliberately unset.

	wfPath := writeWorkflowFile(t, twoStepWorkflow)
	err := runApplyServer(context.Background(), applyOptions{
		workflowPath: wfPath,
		serverURL:    fake.URL(),
		name:         "test-agent",
	})
	if err == nil {
		t.Fatal("partial boot-gate configuration must fail the boot loudly")
	}
	if !strings.Contains(err.Error(), "boot gate") {
		t.Errorf("error %q must attribute the failure to the boot gate", err)
	}
	assertBootGateSilent(t, fake)
}

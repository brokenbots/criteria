package remote

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestPeerIntegrationLateScopeRegistrationConverges is the KB-232 dial-side
// contract proof: an adapter pod that dials into a per-scope environment
// BEFORE the runner registers its scope is rejected with "not registered" on
// every attempt, but the pod's phone-home loop keeps retrying with backoff
// across the scope's whole session window (never dropped after N tries), so
// a scope that turns reachable converges: the next re-dial is accepted and
// the runner's pending session wait is served.
//
// This is the pod-side half of the KB-232 evidence: the runner rejected
// every dial with `scope "..." not registered` and the step burned its
// deadline. The engine-side fix (register before the token file is visible
// and before any blocking handshake) closes the race window; this test pins
// that whenever a pod still lands in such a window it re-dials until the
// scope registers instead of dying after a fixed attempt budget.
func TestPeerIntegrationLateScopeRegistrationConverges(t *testing.T) {
	t.Setenv("CRITERIA_HEARTBEAT_INTERVAL", "200ms")
	scopesDir := t.TempDir()
	scope := writeScopesTokens(t, scopesDir, "kb232_late", scopesInstA, "tok-late", "noop")

	// The shim starts with an EMPTY scope registry: the pod dials before
	// the runner registers the scope, reproducing the evidence window.
	ma := startMultiAdapterFixture(t, integrationOpts{
		perScope:     true,
		scope:        scope,
		token:        "tok-late",
		noPeer:       true,
		skipRegister: true,
	})

	// Host-side capture: the shim's rejection warn must carry the scope
	// name, mirroring the `remote shim accept failed error="scope ... not
	// registered"` evidence line.
	shimLogs := captureLogs(t)

	// Pod-side capture on the peer's own logger: the re-dial backoff path.
	podLogs := &capturedLogs{}
	podLogger := slog.New(slog.NewTextHandler(podLogs, nil))
	ma.fx.startPeer(t, &peerOpts{
		scopesDir: scopesDir,
		scope:     scope,
		token:     "tok-late",
		manifest:  ma.manifestSpecs(),
		logger:    podLogger,
	})

	// The shim rejects the first dials with the registered-scope error and
	// the pod starts its reconnect loop. The evidence line names the exact
	// scope the pod presented.
	if !waitForLog(t, shimLogs, "not registered", 10*time.Second) {
		t.Fatalf("expected shim rejection logs for the unregistered scope:\n%s", shimLogs.String())
	}
	if !shimLogs.contains(scopesInstA) {
		t.Errorf("shim rejection logs must name the presented scope instance %s:\n%s", scopesInstA, shimLogs.String())
	}

	// The pod keeps retrying: several rejected attempts accumulate over a
	// window (backoff 50-200ms per attempt in the fixture), and nothing
	// drops the loop after a fixed number of failures.
	deads := 0
	deadline := time.Now().Add(5 * time.Second)
	for deads < 3 && time.Now().Before(deadline) {
		deads = strings.Count(podLogs.String(), "peer phone-home connection lost; reconnecting")
		time.Sleep(50 * time.Millisecond)
	}
	if deads < 3 {
		t.Fatalf("expected at least 3 reconnect retries while the scope stays unregistered, got %d:\n%s", deads, podLogs.String())
	}
	if !strings.Contains(podLogs.String(), "backoff=") {
		t.Errorf("expected the retry logs to show the backoff delay:\n%s", podLogs.String())
	}

	// The runner registers the scope late (the KB-232 contract: registration
	// completes while the pod is still waiting). The pod's NEXT re-dial —
	// not a fresh pod — is accepted and the runner's session wait resolves.
	ma.fx.shim.RegisterScope(scope, "tok-late")

	ph := ma.fx.waitForAdapterHandle(t, "noop", scope)
	if err := ph.OpenSession(context.Background(), "s-late-scope", nil, nil); err != nil {
		t.Fatalf("OpenSession after late registration: %v", err)
	}
}

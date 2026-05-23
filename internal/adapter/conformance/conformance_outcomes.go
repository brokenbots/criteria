package conformance

// conformance_outcomes.go — outcome domain and permission-request-shape tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testOutcomeDomain(t *testing.T, name string, factory targetFactory, opts *Options) {
	t.Helper()
	if len(opts.AllowedOutcomes) == 0 {
		t.Skip("outcome-domain test skipped: no allowed outcomes configured")
	}
	allowed := make(map[string]struct{}, len(opts.AllowedOutcomes))
	for _, outcome := range opts.AllowedOutcomes {
		allowed[outcome] = struct{}{}
	}

	target := factory(t)
	step := baseStep(name, target.Name(), opts.StepConfig)
	res, err := executeNoPanic(t, target, context.Background(), step, &recordingSink{})
	if err != nil {
		return
	}
	if _, ok := allowed[res.Outcome]; !ok {
		t.Fatalf("outcome %q not in allowed set %v", res.Outcome, opts.AllowedOutcomes)
	}
}

func testPermissionRequestShape(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	if !hasCapability(info.Capabilities, "permission_gating") {
		t.Skip("permission_request_shape skipped: adapter does not advertise permission_gating")
	}

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("permission")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig)); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	cfg := opts.PermissionConfig
	if len(cfg) == 0 {
		cfg = opts.StepConfig
	}
	// No allow_tools on the step; the adapter emits permission.request events
	// which the host forwards upstream. The adapter's own handling determines
	// the outcome (WS16 adds full grant/deny policy).
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	_, err = executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, sink)
	if err != nil {
		t.Fatalf("execute with permission request config: %v", err)
	}
	assertPermissionDeniedEvent(t, sink)
}

// assertPermissionRequestEvent verifies that the recording sink contains a
// permission.request adapter event forwarded from the adapter, confirming that
// the host passes permission events through to the upstream sink.
func assertPermissionDeniedEvent(t *testing.T, sink *recordingSink) {
	t.Helper()
	// WS03: the adapter emits permission.request; the host forwards it to the
	// upstream sink without policy evaluation. WS16 adds grant/deny semantics.
	_, ok := sink.firstAdapterEvent("permission.request")
	if !ok {
		t.Fatal("expected permission.request adapter event forwarded to upstream sink")
	}
}

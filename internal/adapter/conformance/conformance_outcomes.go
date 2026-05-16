package conformance

// conformance_outcomes.go — outcome domain and permission-request-shape tests.

import (
	"context"
	"strings"
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
	// No allow_tools on the step → default deny-all policy applies.
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	res, err := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, sink)
	if err != nil {
		t.Fatalf("execute with permission request config: %v", err)
	}
	wantOutcome := opts.PermissionDenialOutcome
	if wantOutcome == "" {
		wantOutcome = "needs_review"
	}
	if res.Outcome != wantOutcome {
		t.Fatalf("permission denial must end with %q, got %q", wantOutcome, res.Outcome)
	}
	assertPermissionDeniedEvent(t, sink)
}

// assertPermissionDeniedEvent verifies that the recording sink contains a
// well-formed permission.denied adapter event (non-empty request_id and tool).
func assertPermissionDeniedEvent(t *testing.T, sink *recordingSink) {
	t.Helper()
	// The host policy emits permission.denied (not the legacy permission.request)
	// for every denied request. Verify the event carries the request_id so the
	// adapter's original request can be correlated.
	deniedEvent, ok := sink.firstAdapterEvent("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied adapter event from host deny policy")
	}
	// Use type assertion so a missing or nil field (which fmt.Sprint renders as
	// "<nil>") is correctly treated as an absent value.
	requestID, _ := deniedEvent["request_id"].(string)
	tool, _ := deniedEvent["tool"].(string)
	if strings.TrimSpace(requestID) == "" {
		t.Fatal("permission.denied event must include non-empty request_id")
	}
	if strings.TrimSpace(tool) == "" {
		t.Fatal("permission.denied event must include non-empty tool")
	}
}

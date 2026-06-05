package conformance

// conformance_ordering.go — lifecycle event ordering contract tests.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testOrdering(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	if len(opts.LifecycleOrder) == 0 {
		t.Skipf("%s: lifecycle_order not configured in options", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, sessionID := resolveAndOpen(t, ctx, loader, name, opts.OpenConfig, opts.Secrets)
	defer plug.Kill()
	defer func() { _ = plug.CloseSession(context.Background(), sessionID) }()

	step := baseStep(name, info.Name, opts.StepConfig)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	assertLifecycleOrder(t, name, sink, opts.LifecycleOrder)
}

func assertLifecycleOrder(t *testing.T, name string, sink *recordingSink, expected []string) {
	t.Helper()
	canonical := []string{"session_opened", "execute_started", "execute_finished", "session_closed"}
	var observed []string
	for _, evt := range sink.adapterEvents {
		if slices.Contains(canonical, evt.kind) {
			observed = append(observed, evt.kind)
		}
	}
	if len(observed) == 0 {
		t.Skipf("%s: no lifecycle events observed in sink", name)
	}

	// Exact equality: the adapter's declared LifecycleOrder must match
	// the observed canonical events precisely. Adapters that intentionally
	// omit events must declare a shorter LifecycleOrder.
	if !slices.Equal(observed, expected) {
		t.Fatalf("lifecycle order mismatch: observed %v, expected %v", observed, expected)
	}
}

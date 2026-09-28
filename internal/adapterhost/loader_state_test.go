package adapterhost

// loader_state_test.go — CRI-201 end-to-end: a real adapter binary's
// InfoResponse.state descriptor must survive the production path
// loader.Resolve → plug.Info(ctx) → AdapterInfoFromProto into the host-side
// AdapterInfo.State. Adapters that declare no state (noop) translate to nil,
// exactly as before this surface existed.

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// TestLoader_Info_PropagatesStateDeclarationViaProto verifies the full
// production path carries the state declaration declared by the real
// stateful adapter binary: mode blob, schema stateful.v1, 64KiB cap,
// per-turn granularity.
func TestLoader_Info_PropagatesStateDeclarationViaProto(t *testing.T) {
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return testStatefulAdapterBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	plug, err := loader.Resolve(context.Background(), "stateful")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer plug.Kill()

	info, err := plug.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}

	state := info.AdapterInfo.State
	if state == nil {
		t.Fatal("AdapterInfo.State = nil; want the stateful binary's blob declaration")
	}
	want := workflow.StateDeclaration{
		Mode:        "blob",
		Schema:      "stateful.v1",
		MaxBytes:    64 * 1024,
		Granularity: "per-turn",
	}
	if *state != want {
		t.Fatalf("AdapterInfo.State = %+v; want %+v", *state, want)
	}
	if state.EffectiveMaxBytes() != 64*1024 {
		t.Fatalf("EffectiveMaxBytes() = %d; want the adapter-declared 64KiB cap", state.EffectiveMaxBytes())
	}
}

// TestLoader_Info_StateDeclarationAbsent verifies the regression-free half of
// the contract: an adapter binary with no state descriptor (noop) translates
// to a nil declaration, so adapters without a declaration behave exactly as
// today.
func TestLoader_Info_StateDeclarationAbsent(t *testing.T) {
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return testNoopAdapterBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	plug, err := loader.Resolve(context.Background(), "noop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer plug.Kill()

	info, err := plug.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}

	if state := info.AdapterInfo.State; state != nil {
		t.Fatalf("AdapterInfo.State = %+v; want nil for an adapter with no descriptor", state)
	}
}

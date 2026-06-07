package adapterhost_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// TestPublicSDKFixtureConformance proves that an adapter built exclusively
// against sdk/adapterhost (no internal/ reach-through) passes the full adapter
// conformance harness. This is the golden signal that the public package
// surface is sufficient for external adapter authors.
func TestPublicSDKFixtureConformance(t *testing.T) {
	bin := buildPublicSDKFixture(t)
	conformance.RunAdapter(
		t,
		"public-sdk-fixture",
		bin,
		conformance.Options{
			// StepConfig with delay_ms enables context_cancellation and step_timeout
			// sub-tests, proving context propagation works across the adapter subprocess
			// boundary when using only the public sdk/adapterhost surface.
			StepConfig:      map[string]string{"delay_ms": "0"},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

// TestPublicSDKFixture_NativeTypedOutputs is the WS-C end-to-end proof: a real
// out-of-process adapter binary emits structured + scalar outputs on the native
// outputs_json channel, and the host decodes them — through the full gRPC wire —
// to native cty types against the step's OutputSchema. No legacy string-map
// field and no jsondecode() are involved anywhere in the chain.
func TestPublicSDKFixture_NativeTypedOutputs(t *testing.T) {
	bin := buildPublicSDKFixture(t)
	ctx := context.Background()

	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) { return bin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(ctx) })

	handle, err := loader.Resolve(ctx, "public-sdk-fixture")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := handle.OpenSession(ctx, "sess", nil, nil); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = handle.CloseSession(ctx, "sess") })

	step := &workflow.StepNode{
		Name:       "produce",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "public-sdk-fixture",
		Input:      map[string]string{"emit_typed": "true"},
		OutputSchema: map[string]workflow.ConfigField{
			"meta":  {CtyType: cty.DynamicPseudoType},
			"count": {CtyType: cty.Number},
			// "ok" is undeclared — decoded by inference.
		},
		Outcomes: map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
	}

	result, err := handle.Execute(ctx, "sess", step, noopExecSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", result.Outcome)
	}

	if got := result.Outputs["count"]; !got.RawEquals(cty.NumberIntVal(42)) {
		t.Errorf("count = %#v, want number 42", got)
	}
	if got := result.Outputs["ok"]; !got.RawEquals(cty.True) {
		t.Errorf("ok = %#v, want bool true (inferred)", got)
	}
	meta := result.Outputs["meta"]
	if !meta.Type().IsObjectType() || !meta.GetAttr("id").RawEquals(cty.NumberIntVal(7)) {
		t.Errorf("meta = %#v, want object with id=7", meta)
	}
	if name := meta.GetAttr("name"); name.AsString() != "widget" {
		t.Errorf("meta.name = %#v, want \"widget\"", name)
	}
}

// noopExecSink is a no-op EventSink for the typed-outputs e2e test; the result
// is read from Execute's return value, not the event stream.
type noopExecSink struct{}

func (noopExecSink) Log(string, []byte)  {}
func (noopExecSink) Adapter(string, any) {}

var (
	buildPublicSDKOnce sync.Once
	publicSDKBinPath   string
)

// buildPublicSDKFixture returns the path to the public-sdk fixture binary,
// building it only once for the test-binary lifetime so that -count=N runs
// don't trigger N concurrent `go build` invocations.
func buildPublicSDKFixture(t *testing.T) string {
	t.Helper()
	buildPublicSDKOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			panic("plugin_test: resolve caller path")
		}
		moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		dir, err := os.MkdirTemp("", "criteria-publicsdk-test-")
		if err != nil {
			panic("plugin_test: create temp dir: " + err.Error())
		}
		bin := filepath.Join(dir, "criteria-adapter-public-sdk-fixture")
		cmd := exec.Command("go", "build", "-o", bin,
			"./internal/adapterhost/testfixtures/publicsdk")
		cmd.Dir = moduleRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			panic("plugin_test: build public-sdk fixture: " + err.Error() + "\n" + string(output))
		}
		publicSDKBinPath = bin
	})
	if publicSDKBinPath == "" {
		t.Fatal("buildPublicSDKFixture: binary path not set after sync.Once")
	}
	return publicSDKBinPath
}

package conformance

// conformance.go — Run/RunPlugin entry points and contract test orchestration.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// Options configures adapter-specific conformance expectations.
type Options struct {
	// OpenConfig is optional plugin OpenSession config for RunPlugin tests.
	OpenConfig map[string]string
	// StepConfig is the HCL-style config passed to the step node under test.
	StepConfig map[string]string
	// PermissionConfig optionally overrides StepConfig for permission_request_shape.
	PermissionConfig map[string]string
	// AllowedOutcomes is the set of valid Outcome strings for this adapter.
	AllowedOutcomes []string
	// Streaming indicates the adapter is expected to emit >0 Log events.
	Streaming bool
	// ExpectError, when non-nil, asserts the adapter returns a matching error
	// (used for expected-failure adapters like the non-copilot-build stub).
	ExpectError func(error) bool
	// PermissionDenialOutcome is the outcome expected when a permission request
	// is denied by the host. Defaults to "needs_review" when empty. Adapters
	// that explicitly return "failure" on denial (e.g. the copilot adapter
	// post-W15) should set this to "failure".
	PermissionDenialOutcome string
}

type executeTarget interface {
	Name() string
	Execute(context.Context, *workflow.StepNode, adapter.EventSink) (adapter.Result, error)
}

type targetFactory func(*testing.T) executeTarget

// Run executes the shared adapter conformance contract.
func Run(t *testing.T, name string, factory func() adapter.Adapter, opts Options) { //nolint:gocritic // W15: Options passes by value for API clarity
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Fatal("conformance: name is required")
	}
	if factory == nil {
		t.Fatal("conformance: factory is required")
	}

	runContractTests(t, name, opts, func(_ *testing.T) executeTarget {
		return adapterTarget{impl: factory()}
	})
}

// RunPlugin executes the shared adapter contract against a plugin binary.
func RunPlugin(t *testing.T, name, binaryPath string, opts Options) { //nolint:gocritic // W15: Options passes by value for API clarity
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Fatal("conformance: name is required")
	}
	if strings.TrimSpace(binaryPath) == "" {
		t.Fatal("conformance: binaryPath is required")
	}

	loader := plugin.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != name {
			return "", fmt.Errorf("unexpected plugin request %q (expected %q)", requested, name)
		}
		return binaryPath, nil
	})
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}
	info, err := probe.Info(ctx)
	if err != nil {
		probe.Kill()
		t.Fatalf("plugin info: %v", err)
	}
	probe.Kill()

	runContractTests(t, name, opts, newPluginTargetFactory(name, loader, opts))

	t.Run("session_lifecycle", func(t *testing.T) {
		testSessionLifecycle(t, name, loader, opts, info)
	})
	t.Run("concurrent_sessions", func(t *testing.T) {
		testConcurrentSessions(t, name, loader, opts, info)
	})
	t.Run("session_crash_detection", func(t *testing.T) {
		testSessionCrashDetection(t, name, loader, opts, info)
	})
	t.Run("permission_request_shape", func(t *testing.T) {
		testPermissionRequestShape(t, name, loader, opts, info)
	})
}

func runContractTests(t *testing.T, name string, opts Options, factory targetFactory) { //nolint:gocritic // W15: Options passes by value for API clarity
	t.Run("name_stability", func(t *testing.T) { testNameStability(t, name, factory) })
	t.Run("nil_sink", func(t *testing.T) { testNilSink(t, name, factory, opts) })
	t.Run("happy_path", func(t *testing.T) { testHappyPath(t, name, factory, opts) })

	if opts.ExpectError == nil {
		t.Run("context_cancellation", func(t *testing.T) { testCancel(t, name, factory, opts) })
		t.Run("step_timeout", func(t *testing.T) { testTimeout(t, name, factory, opts) })
		t.Run("outcome_domain", func(t *testing.T) { testOutcomeDomain(t, name, factory, opts) })
		if opts.Streaming {
			t.Run("chunked_io", func(t *testing.T) { testChunkedIO(t, name, factory, opts) })
		}
	}
}

func newPluginTargetFactory(name string, loader plugin.Loader, opts Options) targetFactory { //nolint:gocritic // W15: Options passes by value for API clarity
	return func(t *testing.T) executeTarget {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		plug, err := loader.Resolve(ctx, name)
		if err != nil {
			t.Fatalf("resolve plugin: %v", err)
		}
		info, err := plug.Info(ctx)
		if err != nil {
			plug.Kill()
			t.Fatalf("plugin info: %v", err)
		}

		sessionID := newSessionID("conformance")
		if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig)); err != nil {
			plug.Kill()
			t.Fatalf("open session %q: %v", sessionID, err)
		}

		t.Cleanup(func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = plug.CloseSession(closeCtx, sessionID)
			closeCancel()
			plug.Kill()
		})

		return pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}
	}
}

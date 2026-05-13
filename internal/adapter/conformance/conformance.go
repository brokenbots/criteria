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
	// ExpectedLifecycleOrder is the ordered slice of adapter event kinds that
	// must arrive in this exact relative order during a happy-path execution.
	// Events whose kinds are not in this slice are ignored.
	ExpectedLifecycleOrder []string
	// ConcurrentSessionStressN is the number of concurrent sessions to open
	// during testConcurrentSessionStress. Defaults to defaultConcurrentStressN
	// when zero. Set to 1 or below to skip the stress test.
	ConcurrentSessionStressN int
	// ErrorInjectionConfig, when non-nil, enables testErrorInjectionHandshake.
	// Its contents are reserved for future per-adapter injection knobs; setting
	// it to an empty (non-nil) map is sufficient to opt in.
	ErrorInjectionConfig map[string]string
	// SupportsPartialFailure, when true, enables testPartialFailureRecovery.
	// The adapter must return an error implementing adapter.FailureWithContext
	// and deliver at least one event before the failure point when
	// test_only=partial_failure is set in the step config.
	SupportsPartialFailure bool
	// PermissionDenyWithErrorConfig, when non-nil, enables the
	// permission-deny-with-error edge-case tests. Its value is used as the
	// step config for those tests; set to a config that triggers a permission
	// request in the adapter under test.
	PermissionDenyWithErrorConfig map[string]string
}

type executeTarget interface {
	Name() string
	Execute(context.Context, *workflow.StepNode, adapter.EventSink) (adapter.Result, error)
}

type targetFactory func(*testing.T) executeTarget

// Run executes the shared adapter conformance contract.
func Run(t *testing.T, name string, factory func() adapter.Adapter, opts Options) {
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Fatal("conformance: name is required")
	}
	if factory == nil {
		t.Fatal("conformance: factory is required")
	}

	runContractTests(t, name, &opts, func(_ *testing.T) executeTarget {
		return adapterTarget{impl: factory()}
	})
}

// RunPlugin executes the shared adapter contract against a plugin binary.
func RunPlugin(t *testing.T, name, binaryPath string, opts Options) {
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

	// 30 s matches the StartTimeout in the loader so the context does not
	// expire before the plugin process finishes advertising its socket.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	runContractTests(t, name, &opts, newPluginTargetFactory(name, loader, &opts))

	t.Run("session_lifecycle", func(t *testing.T) {
		testSessionLifecycle(t, name, loader, &opts, &info)
	})
	t.Run("concurrent_sessions", func(t *testing.T) {
		testConcurrentSessions(t, name, loader, &opts, &info)
	})
	t.Run("session_crash_detection", func(t *testing.T) {
		testSessionCrashDetection(t, name, loader, &opts, &info)
	})
	t.Run("permission_request_shape", func(t *testing.T) {
		testPermissionRequestShape(t, name, loader, &opts, &info)
	})
}

func runContractTests(t *testing.T, name string, opts *Options, factory targetFactory) {
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

func newPluginTargetFactory(name string, loader plugin.Loader, opts *Options) targetFactory {
	return func(t *testing.T) executeTarget {
		t.Helper()
		// 30 s matches the StartTimeout in the loader.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

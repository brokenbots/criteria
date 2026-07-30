package conformance

// conformance.go — Run/RunAdapter entry points and contract test orchestration.

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// Options configures adapter-specific conformance expectations.
type Options struct {
	// OpenConfig is optional adapter OpenSession config for RunAdapter tests.
	OpenConfig map[string]string
	// Secrets is optional resolved secrets delivered on every OpenSession the
	// harness opens. Adapters that require a secret to open a session (e.g.
	// copilot's GitHub token, D69) must set this, or every suite fails closed.
	Secrets map[string]string
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

	// HeartbeatStallThreshold is the duration the host uses as its stall
	// threshold in the heartbeat conformance tests. It lets the suite run with
	// a short threshold instead of waiting the production 90s. If zero a
	// default short threshold is used.
	HeartbeatStallThreshold time.Duration

	// ErrorInjection, when true, enables the error_injection suite.
	ErrorInjection bool
	// PermissionDenyPaths, when true, enables the three deny-path sub-tests.
	PermissionDenyPaths bool
	// ConcurrentStressN is the number of concurrent sessions for the stress
	// test. Zero disables the suite.
	ConcurrentStressN int
	// LifecycleOrder is the canonical lifecycle event-type sequence this
	// adapter emits. Empty disables the ordering suite.
	LifecycleOrder []string
	// SupportedFeatures is the adapter's supported_features list (from v2
	// InfoResponse). The harness uses this to gate optional suites.
	SupportedFeatures []string
}

type executeTarget interface {
	Name() string
	Execute(context.Context, *workflow.StepNode, adapter.EventSink) (adapter.Result, error)
}

type targetFactory func(*testing.T) executeTarget

//go:embed matrix.yaml
var matrixYAML []byte

var (
	matrixSuites         []string
	matrixRequiredSuites []string
	matrixLoadOnce       sync.Once
)

func loadMatrix() (suites, required []string) {
	matrixLoadOnce.Do(func() {
		var m struct {
			Suites         []string `yaml:"suites"`
			RequiredSuites []string `yaml:"required_suites"`
		}
		if err := yaml.Unmarshal(matrixYAML, &m); err != nil {
			// matrix.yaml is bundled at build time; a parse error is a programming
			// error and should surface loudly rather than be swallowed.
			panic(fmt.Sprintf("parse embedded matrix.yaml: %v", err))
		}
		matrixSuites = m.Suites
		matrixRequiredSuites = m.RequiredSuites
	})
	return matrixSuites, matrixRequiredSuites
}

func requiredSuiteSet() map[string]struct{} {
	_, required := loadMatrix()
	set := make(map[string]struct{}, len(required))
	for _, r := range required {
		set[r] = struct{}{}
	}
	return set
}

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

// RunAdapter executes the shared adapter contract against an adapter binary.
func RunAdapter(t *testing.T, name, binaryPath string, opts Options) {
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Fatal("conformance: name is required")
	}
	if strings.TrimSpace(binaryPath) == "" {
		t.Fatal("conformance: binaryPath is required")
	}

	loader := adapterhost.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != name {
			return "", fmt.Errorf("unexpected adapter request %q (expected %q)", requested, name)
		}
		return binaryPath, nil
	})
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	// 30 s matches the StartTimeout in the loader so the context does not
	// expire before the adapter process finishes advertising its socket.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	info, err := probe.Info(ctx)
	if err != nil {
		probe.Kill()
		t.Fatalf("adapter info: %v", err)
	}
	probe.Kill()

	// Auto-populate supported features from the adapter so capability-gated
	// suites skip correctly even when the caller leaves opts empty.
	if len(opts.SupportedFeatures) == 0 {
		opts.SupportedFeatures = append([]string(nil), info.SupportedFeatures...)
	}

	runContractTests(t, name, &opts, newAdapterTargetFactory(name, loader, &opts))
	runV2Suites(t, name, loader, &opts, &info)
}

func runV2Suites(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	required := requiredSuiteSet()
	runSuite := func(t *testing.T, suite string, fn func(*testing.T)) {
		t.Helper()
		skipped := false
		t.Run(suite, func(t *testing.T) {
			defer func() { skipped = t.Skipped() }()
			fn(t)
		})
		if skipped {
			if _, ok := required[suite]; ok {
				t.Fatalf("required conformance suite %q was skipped", suite)
			}
		}
	}

	runSuite(t, "session_lifecycle", func(t *testing.T) { testSessionLifecycle(t, name, loader, opts, info) })
	runSuite(t, "concurrent_sessions", func(t *testing.T) { testConcurrentSessions(t, name, loader, opts, info) })
	runSuite(t, "session_crash_detection", func(t *testing.T) { testSessionCrashDetection(t, name, loader, opts, info) })
	runSuite(t, "permission_request_shape", func(t *testing.T) { testPermissionRequestShape(t, name, loader, opts, info) })

	runSuite(t, "permissions", func(t *testing.T) { testPermissions(t, name, loader, opts, info) })
	runSuite(t, "logging", func(t *testing.T) { testLogging(t, name, loader, opts, info) })
	runSuite(t, "pause_resume", func(t *testing.T) { testPauseResume(t, name, loader, opts, info) })
	runSuite(t, "snapshot_restore", func(t *testing.T) { testSnapshotRestore(t, name, loader, opts, info) })
	runSuite(t, "inspect", func(t *testing.T) { testInspect(t, name, loader, opts, info) })
	runSuite(t, "secrets", func(t *testing.T) { testSecrets(t, name, loader, opts, info) })
	runSuite(t, "sensitive_output", func(t *testing.T) { testSensitiveOutput(t, name, loader, opts, info) })
	runSuite(t, "heartbeats", func(t *testing.T) { testHeartbeats(t, name, loader, opts, info) })
	runSuite(t, "chunking", func(t *testing.T) { testChunking(t, name, loader, opts, info) })
	runSuite(t, "error_injection", func(t *testing.T) { testErrorInjection(t, name, loader, opts, info) })
	runSuite(t, "ordering", func(t *testing.T) { testOrdering(t, name, loader, opts, info) })
	runSuite(t, "concurrent_stress", func(t *testing.T) { testConcurrentStress(t, name, loader, opts, info) })
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

func newAdapterTargetFactory(name string, loader adapterhost.Loader, opts *Options) targetFactory {
	return func(t *testing.T) executeTarget {
		t.Helper()
		// 30 s matches the StartTimeout in the loader.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		plug, err := loader.Resolve(ctx, name)
		if err != nil {
			t.Fatalf("resolve adapter: %v", err)
		}
		info, err := plug.Info(ctx)
		if err != nil {
			plug.Kill()
			t.Fatalf("adapter info: %v", err)
		}

		sessionID := newSessionID("conformance")
		if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
			plug.Kill()
			t.Fatalf("open session %q: %v", sessionID, err)
		}

		t.Cleanup(func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = plug.CloseSession(closeCtx, sessionID)
			closeCancel()
			plug.Kill()
		})

		return adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}
	}
}

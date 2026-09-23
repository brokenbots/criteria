package conformance_test

// promptable_adapter_test.go — conformance coverage for the prompt-capable
// testdata fixture (ADR-0006 D3, workstream CRI-259 exit criteria 3/4/6).
//
// The promptable fixture is the only in-repo adapter that declares
// supports_prompt, so it is the positive-path target for prompt delivery;
// the noop fixture stays the negative-path target (no Prompt RPC on the
// wire, UNSUPPORTED_ADAPTER at the host).

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

var (
	promptableBinOnce sync.Once
	promptableBinPath string
)

func buildPromptableAdapter(t *testing.T) string {
	t.Helper()
	promptableBinOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("resolve caller path")
		}
		moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
		dir, err := os.MkdirTemp("", "criteria-promptable-test-")
		if err != nil {
			t.Fatalf("create temp dir: %v", err)
		}
		// No cleanup: the binary is shared by all promptable tests in the
		// process via promptableBinOnce, so removing it on the first test's
		// cleanup would break later tests. The OS temp dir is reaped on
		// reboot; this mirrors buildPublicSDKFixture.
		bin := filepath.Join(dir, "criteria-adapter-promptable-conformance")
		cmd := exec.Command("go", "build", "-o", bin, "./internal/adapter/conformance/testdata/promptable")
		cmd.Dir = moduleRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build promptable adapter: %v\n%s", err, string(output))
		}
		promptableBinPath = bin
	})
	if promptableBinPath == "" {
		t.Fatal("buildPromptableAdapter: binary path not set after sync.Once")
	}
	return promptableBinPath
}

// TestPromptableAdapterConformance proves the fixture builds and passes the
// full conformance harness (exit criterion 6). Only heartbeats is required
// by the matrix; the capability-gated suites skip exactly as they do for
// noop.
func TestPromptableAdapterConformance(t *testing.T) {
	conformance.RunAdapter(
		t,
		"promptable",
		buildPromptableAdapter(t),
		conformance.Options{
			StepConfig:      map[string]string{"delay_ms": "0"},
			AllowedOutcomes: []string{"success"},
		},
	)
}

// TestPromptableAdapterPromptAccepted drives the production host delivery
// path (SessionManager.Prompt) against the real fixture binary: the
// capability gate passes (supports_prompt advertised), the Prompt RPC is
// issued, and the adapter accepts it.
func TestPromptableAdapterPromptAccepted(t *testing.T) {
	bin := buildPromptableAdapter(t)
	loader := adapterhost.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != "promptable" {
			return "", errors.New("unexpected adapter request " + requested)
		}
		return bin, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	probe, err := loader.Resolve(ctx, "promptable")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := probe.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !hasCapability(info.Capabilities, "supports_prompt") {
		t.Fatalf("Info capabilities lack supports_prompt: %v", info.Capabilities)
	}

	sm := adapterhost.NewSessionManager(loader)
	defer func() { _ = sm.Shutdown(ctx) }()
	if err := sm.Open(ctx, "sess-1", "promptable", "", nil, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}

	sessionID, err := sm.Prompt(ctx, "sess-1", promptableStep(), "", "recheck the failing tool")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if sessionID != "sess-1" {
		t.Fatalf("Prompt session = %q, want sess-1", sessionID)
	}
}

// TestNoopAdapterPromptUnsupported is the negative-path fixture test (exit
// criterion 4, live half): the noop adapter does not declare
// supports_prompt, so the host short-circuits with UNSUPPORTED_ADAPTER and
// never issues a Prompt RPC. The zero-wire-calls half of the criterion is
// asserted by the in-memory fake-handle tests in adapterhost; here the
// gate is proven against a real adapter process whose Info carries no
// supports_prompt.
func TestNoopAdapterPromptUnsupported(t *testing.T) {
	bin := buildNoopAdapter(t)
	loader := adapterhost.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != "noop" {
			return "", errors.New("unexpected adapter request " + requested)
		}
		return bin, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sm := adapterhost.NewSessionManager(loader)
	defer func() { _ = sm.Shutdown(ctx) }()
	if err := sm.Open(ctx, "sess-1", "noop", "", nil, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	if sm.HasCapability("sess-1", "supports_prompt") {
		t.Fatal("noop adapter unexpectedly declares supports_prompt")
	}
	if _, err := sm.Prompt(ctx, "sess-1", promptableStep(), "", "hello"); !errors.Is(err, adapterhost.ErrPromptUnsupportedAdapter) {
		t.Fatalf("Prompt error = %v, want ErrPromptUnsupportedAdapter", err)
	}
}

func promptableStep() *workflow.StepNode {
	return &workflow.StepNode{
		Name:       "work",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "promptable",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success", Next: "done"},
		},
	}
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
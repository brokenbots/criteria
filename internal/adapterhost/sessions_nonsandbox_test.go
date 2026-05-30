package adapterhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

func TestBuildSandboxCustomizer_NonSandboxEnv(t *testing.T) {
	sm := NewSessionManager(nil)
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "docker.default"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"docker.default": {Type: "docker", Name: "default"},
		},
	}
	customizer, cleanup, err := sm.buildSandboxCustomizer("noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer != nil {
		t.Fatal("expected nil customizer for non-sandbox env")
	}
	if cleanup != nil {
		t.Fatal("expected nil cleanup for non-sandbox env")
	}
}

func TestBuildSandboxCustomizer_NoGraph(t *testing.T) {
	sm := NewSessionManager(nil)
	customizer, cleanup, err := sm.buildSandboxCustomizer("noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer != nil || cleanup != nil {
		t.Fatal("expected nil customizer and cleanup when graph is nil")
	}
}

func TestBuildSandboxCustomizer_NoAdapter(t *testing.T) {
	sm := NewSessionManager(nil)
	sm.graph = &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"sandbox.default": {Type: "sandbox", Name: "default"},
		},
	}
	customizer, cleanup, err := sm.buildSandboxCustomizer("noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer != nil || cleanup != nil {
		t.Fatal("expected nil customizer and cleanup when adapter is missing")
	}
}

func TestBuildSandboxCustomizer_NoEnvKey(t *testing.T) {
	sm := NewSessionManager(nil)
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default"},
		},
	}
	customizer, cleanup, err := sm.buildSandboxCustomizer("noop.default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer != nil || cleanup != nil {
		t.Fatal("expected nil customizer and cleanup when no env key")
	}
}

func TestSessionManagerOpenExecuteClose(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, map[string]string{"bootstrap": "1"}); err != nil {
		t.Fatalf("open: %v", err)
	}

	res, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "run"}, &adapterEventCollector{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("outcome=%q want success", res.Outcome)
	}

	if err := sm.Close(context.Background(), "agent"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := sm.Close(context.Background(), "agent"); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
}

func TestSessionManagerUnknownExecuteAndDoubleOpen(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	_, err := sm.Execute(context.Background(), "missing", &workflow.StepNode{Name: "run"}, &adapterEventCollector{})
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("execute unknown err=%v", err)
	}
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); !errors.Is(err, ErrSessionAlreadyOpen) {
		t.Fatalf("double open err=%v", err)
	}
}

func TestSessionManagerCrashPolicyFail(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastHandle().Kill()

	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, sink)
	if err == nil {
		t.Fatal("expected crash error")
	}
	if result.Outcome != "failure" {
		t.Fatalf("outcome=%q want failure", result.Outcome)
	}
	if !sink.saw("session.crash") {
		t.Fatal("expected session.crash adapter event")
	}
}

func TestSessionManagerCrashPolicyRespawn(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashRespawn, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastHandle().Kill()

	sink := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, sink)
	if err != nil {
		t.Fatalf("execute with respawn: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("outcome=%q want success", result.Outcome)
	}
	if !sink.saw("session.respawned") {
		t.Fatal("expected session.respawned adapter event")
	}
}

func TestSessionManagerCrashPolicyAbortRun(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashAbortRun, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "first"}, &adapterEventCollector{}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	loader.lastHandle().Kill()

	_, err := sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "second"}, &adapterEventCollector{})
	var fatal *FatalRunError
	if !errors.As(err, &fatal) {
		t.Fatalf("error=%v want FatalRunError", err)
	}
}

// TestSession_ClosingFlagSuppressesCrashHeuristic verifies that setting the
// closing flag causes isLikelySessionCrash to return false even for
// EOF/connection-close errors (W12).
func TestSession_ClosingFlagSuppressesCrashHeuristic(t *testing.T) {
	sess := &Session{}
	sess.closing.Store(true)
	for _, errMsg := range []string{
		"eof",
		"eof: connection terminated",
		"transport is closing",
		"broken pipe",
		"connection refused",
		"terminated",
	} {
		if isLikelySessionCrash(sess, errors.New(errMsg)) {
			t.Errorf("expected isLikelySessionCrash to return false with closing flag set, err=%q", errMsg)
		}
	}
}

// TestSession_UnexpectedExitTriggersHeuristic verifies that without the
// closing flag, crash-like errors trigger the heuristic (W12).
func TestSession_UnexpectedExitTriggersHeuristic(t *testing.T) {
	sess := &Session{}
	for _, errMsg := range []string{
		"eof",
		"eof: connection terminated",
		"transport is closing",
		"broken pipe",
		"connection refused",
		"terminated",
	} {
		if !isLikelySessionCrash(sess, errors.New(errMsg)) {
			t.Errorf("expected isLikelySessionCrash to return true without closing flag, err=%q", errMsg)
		}
	}
}

// TestSession_ExecuteEOFWithoutCloseIsCrash verifies that an Execute call
// returning an EOF-like error without a preceding Close still triggers the
// crash heuristic (W12 risk mitigation).
func TestSession_ExecuteEOFWithoutCloseIsCrash(t *testing.T) {
	sess := &Session{}
	// closing flag is NOT set — this simulates an unsolicited adapter exit
	if !isLikelySessionCrash(sess, errors.New("read: eof")) {
		t.Error("expected crash heuristic to fire for EOF without closing flag")
	}
}

// TestSessionManager_HasCapability_AfterOpen verifies that after opening a
// session with the noop adapter (which declares "parallel_safe"), HasCapability
// returns true for "parallel_safe" and false for an undeclared capability.
func TestSessionManager_HasCapability_AfterOpen(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	base := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
	loader := &recordingLoader{inner: base}
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	if err := sm.Open(context.Background(), "agent", "noop", OnCrashFail, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sm.Close(context.Background(), "agent")

	if !sm.HasCapability("agent", "parallel_safe") {
		t.Error("HasCapability(\"agent\", \"parallel_safe\") = false; want true (noop declares parallel_safe)")
	}
	if sm.HasCapability("agent", "nonexistent_cap") {
		t.Error("HasCapability(\"agent\", \"nonexistent_cap\") = true; want false")
	}
}

// TestSessionManager_HasCapability_UnknownSession verifies that HasCapability
// returns false for a session name that has not been opened.
func TestSessionManager_HasCapability_UnknownSession(t *testing.T) {
	sm := NewSessionManager(nil)
	if sm.HasCapability("ghost", "parallel_safe") {
		t.Error("HasCapability on unknown session = true; want false")
	}
}

// TestLoader_Info_PropagatesCapabilitiesViaProto verifies the full production
// path: loader.Resolve → plug.Info(ctx) → info.AdapterInfo.Capabilities carries
// the capabilities declared by the real noop adapter binary. This asserts that
// the AdapterInfoFromProto translation and the RPC Info() call chain work
// together end-to-end, so collectSchemas (which stores info.AdapterInfo into the
// schemas map used by workflow.Compile) carries parallel_safe correctly.
func TestLoader_Info_PropagatesCapabilitiesViaProto(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
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

	// AdapterInfo.Capabilities must carry "parallel_safe" from the noop binary.
	found := false
	for _, c := range info.AdapterInfo.Capabilities {
		if c == "parallel_safe" {
			found = true
		}
	}
	if !found {
		t.Errorf("AdapterInfo.Capabilities = %v; want to contain \"parallel_safe\"", info.AdapterInfo.Capabilities)
	}
}

// TestCompile_ParallelGate_ViaRealAdapterInfo verifies the full schema-collection
// → compile contract for the parallel_safe capability gate:
//  1. With schemas built from the real noop loader (noop declares parallel_safe),
//     compiling a parallel step targeting noop succeeds.
//  2. With schemas that carry an adapter entry WITHOUT parallel_safe (simulating
//     a resolvable but non-safe adapter), compiling the same step produces a
//     DiagError containing "parallel_safe".
//
// This test covers the path: loader.Resolve → Info() → AdapterInfoFromProto
// → schemas map → workflow.Compile → adapterHasCapability gate.
func TestCompile_ParallelGate_ViaRealAdapterInfo(t *testing.T) {
	const parallelWorkflowSrc = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target   = adapter.noop.default
  parallel = ["a", "b"]
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}
state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`

	spec, diags := workflow.Parse("test.hcl", []byte(parallelWorkflowSrc))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags.Error())
	}

	// Case 1: schemas built from real noop Info() (declares parallel_safe).
	adapterBin := buildNoopAdapter(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return adapterBin, nil })
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	plug, err := loader.Resolve(context.Background(), "noop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	info, err := plug.Info(context.Background())
	plug.Kill()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	schemas := map[string]workflow.AdapterInfo{"noop": info.AdapterInfo}

	_, diags = workflow.Compile(spec, schemas)
	if diags.HasErrors() {
		t.Errorf("Case 1 (noop with parallel_safe): unexpected compile error: %v", diags.Error())
	}

	// Case 2: schemas with adapter present but no parallel_safe.
	schemasNotSafe := map[string]workflow.AdapterInfo{"noop": {}}
	_, diags = workflow.Compile(spec, schemasNotSafe)
	if !diags.HasErrors() {
		t.Error("Case 2 (no parallel_safe): expected DiagError; got none")
	} else if !strings.Contains(diags.Error(), "parallel_safe") {
		t.Errorf("Case 2 error = %q; want mention of \"parallel_safe\"", diags.Error())
	}
}

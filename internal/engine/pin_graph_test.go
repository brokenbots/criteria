package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

const (
	pinDigest      = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherPinDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

var (
	noopBinOnce sync.Once
	noopBinPath string
	noopBinErr  error
)

// getNoopAdapterBin returns a built criteria-adapter-noop binary. It is built
// once and reused across tests to keep engine-level regression tests fast.
func getNoopAdapterBin(t *testing.T) string {
	t.Helper()
	noopBinOnce.Do(func() {
		noopBinPath, noopBinErr = buildNoopAdapterBin()
	})
	if noopBinErr != nil {
		t.Fatalf("build noop adapter: %v", noopBinErr)
	}
	return noopBinPath
}

func buildNoopAdapterBin() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("could not resolve module root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dir, err := os.MkdirTemp("", "criteria-noop-adapter-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "criteria-adapter-noop")
	out, err := execAt(root, "go", "build", "-o", bin, "./internal/adapter/conformance/testdata/noop")
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("%w: %s", err, out)
	}
	return bin, nil
}

func execAt(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func writeLockfile(t *testing.T, dir string, lf *lockfile.Lockfile) {
	t.Helper()
	if err := lockfile.Write(filepath.Join(dir, lockfile.LockfileName), lf); err != nil {
		t.Fatalf("write lockfile in %q: %v", dir, err)
	}
}

func compileWorkflowDir(t *testing.T, dir string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("parse %q: %v", dir, diags)
	}
	g, diags := workflow.CompileWithContext(context.Background(), spec, nil, workflow.CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("compile %q: %v", dir, diags)
	}
	return g
}

func encodedDigest(digest string) string {
	return strings.ReplaceAll(digest, ":", "-")
}

func installNoopAtDigest(t *testing.T, adaptersRoot string) {
	t.Helper()
	src := getNoopAdapterBin(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read noop binary: %v", err)
	}
	dst := filepath.Join(adaptersRoot, encodedDigest(pinDigest), "criteria-adapter-noop")
	writeFile(t, dst, string(data))
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatalf("chmod %q: %v", dst, err)
	}
}

func installNoopByName(t *testing.T, adaptersRoot string) {
	t.Helper()
	src := getNoopAdapterBin(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read noop binary: %v", err)
	}
	dst := filepath.Join(adaptersRoot, "criteria-adapter-noop")
	writeFile(t, dst, string(data))
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatalf("chmod %q: %v", dst, err)
	}
}

func newDefaultLoader(t *testing.T, adaptersRoot string) *adapterhost.DefaultLoader {
	t.Helper()
	t.Setenv("CRITERIA_ADAPTERS", adaptersRoot)
	return adapterhost.NewLoaderWithDiscovery(nil)
}

func runEngine(t *testing.T, g *workflow.FSMGraph, loader adapterhost.Loader, opts ...Option) (*fakeSink, error) {
	t.Helper()
	sink := &fakeSink{}
	allOpts := append([]Option{WithWorkflowDir(g.WorkflowDir)}, opts...)
	eng := NewTestEngine(g, loader, sink, allOpts...)
	err := eng.Run(context.Background())
	return sink, err
}

// recordingFakeAdapter counts OpenSession/CloseSession calls and records the
// config passed to OpenSession so tests can observe runtime config resolution.
type recordingFakeAdapter struct {
	fakeAdapter
	openConfig map[string]string
	opened     int
	closed     int
}

func (r *recordingFakeAdapter) OpenSession(_ context.Context, _ string, config, _ map[string]string) error {
	r.opened++
	r.openConfig = config
	return nil
}

func (r *recordingFakeAdapter) CloseSession(_ context.Context, _ string) error {
	r.closed++
	return nil
}

func rootWorkflowWithSubworkflow() string {
	return `workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

subworkflow "child" {
  source = "./child"
}

step "start" {
  target = subworkflow.child
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`
}

func childWorkflow() string {
	return `workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "noop" "bot" {
  source = "ghcr.io/brokenbots/criteria-adapter-noop"
}

step "run" {
  target = adapter.noop.bot
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`
}

func childLockfile(digest string) *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type:               "noop",
			Name:               "bot",
			Reference:          "ghcr.io/brokenbots/criteria-adapter-noop",
			ResolvedDigest:     digest,
			SourceURL:          "https://github.com/brokenbots/criteria",
			SDKProtocolVersion: 2,
			Platforms:          []string{"linux/amd64", "linux/arm64", "darwin/arm64"},
		}},
	}
}

func rootLockfile() *lockfile.Lockfile {
	return childLockfile(pinDigest)
}

// TestPinGraph_DeletedSubworkflowLockfileMidRun deletes a subworkflow lockfile
// after compilation and asserts the run completes using the pin captured at
// compile time.
func TestPinGraph_DeletedSubworkflowLockfileMidRun(t *testing.T) {
	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "child")

	writeFile(t, filepath.Join(rootDir, "main.chcl"), rootWorkflowWithSubworkflow())
	writeFile(t, filepath.Join(childDir, "main.chcl"), childWorkflow())
	writeLockfile(t, childDir, childLockfile(pinDigest))

	adaptersRoot := t.TempDir()
	installNoopAtDigest(t, adaptersRoot)

	g := compileWorkflowDir(t, rootDir)
	// Simulate a mid-run deletion of the subworkflow lockfile.
	if err := os.Remove(filepath.Join(childDir, lockfile.LockfileName)); err != nil {
		t.Fatalf("remove child lockfile: %v", err)
	}

	sink, err := runEngine(t, g, newDefaultLoader(t, adaptersRoot))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.terminalOK || sink.terminal != "done" {
		t.Fatalf("expected terminal done, got terminal=%q ok=%v", sink.terminal, sink.terminalOK)
	}
}

// TestPinGraph_ReplacedSubworkflowLockfileMidRun swaps the subworkflow lockfile
// for a different digest after compilation and asserts the original compiled
// digest is still used. The new digest's binary is deliberately absent.
func TestPinGraph_ReplacedSubworkflowLockfileMidRun(t *testing.T) {
	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "child")

	writeFile(t, filepath.Join(rootDir, "main.chcl"), rootWorkflowWithSubworkflow())
	writeFile(t, filepath.Join(childDir, "main.chcl"), childWorkflow())
	writeLockfile(t, childDir, childLockfile(pinDigest))

	adaptersRoot := t.TempDir()
	installNoopAtDigest(t, adaptersRoot)

	g := compileWorkflowDir(t, rootDir)
	// Replace the child lockfile with a different digest but do not install that
	// digest's binary. Success proves the compiled pin set is unchanged.
	writeLockfile(t, childDir, childLockfile(otherPinDigest))

	sink, err := runEngine(t, g, newDefaultLoader(t, adaptersRoot))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.terminalOK || sink.terminal != "done" {
		t.Fatalf("expected terminal done, got terminal=%q ok=%v", sink.terminal, sink.terminalOK)
	}
}

// TestPinGraph_FilePromptMidRunImmutable rewrites a file()-referenced adapter
// config asset after compilation and asserts the adapter receives the
// compile-time content.
func TestPinGraph_FilePromptMidRunImmutable(t *testing.T) {
	rootDir := t.TempDir()

	prompt1 := "you are the original prompt"
	prompt2 := "you are the rewritten prompt"
	writeFile(t, filepath.Join(rootDir, "prompt.md"), prompt1)

	configBlock := `
  config {
    system_prompt = file("./prompt.md")
  }
`
	writeFile(t, filepath.Join(rootDir, "main.chcl"), fmt.Sprintf(`workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "bot" {
  source = "ghcr.io/brokenbots/criteria-adapter-noop"
%s}

step "start" {
  target = adapter.noop.bot
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`, configBlock))

	adaptersRoot := t.TempDir()
	installNoopAtDigest(t, adaptersRoot)
	writeLockfile(t, rootDir, rootLockfile())

	g := compileWorkflowDir(t, rootDir)
	// Rewrite the referenced file after compilation.
	writeFile(t, filepath.Join(rootDir, "prompt.md"), prompt2)

	rec := &recordingFakeAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": rec}}

	if _, err := runEngine(t, g, loader); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := rec.openConfig["system_prompt"]; got != prompt1 {
		t.Fatalf("expected system_prompt %q (compile-time), got %q", prompt1, got)
	}
}

// TestPinGraph_UnresolvableSubworkflowAdapterFailsAtStartup asserts that a
// missing adapter in a subworkflow fails before any root step executes.
func TestPinGraph_UnresolvableSubworkflowAdapterFailsAtStartup(t *testing.T) {
	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "child")

	writeFile(t, filepath.Join(rootDir, "main.chcl"), rootWorkflowWithSubworkflow())
	writeFile(t, filepath.Join(childDir, "main.chcl"), childWorkflow())
	// Child adapter is pinned but its binary is missing.
	writeLockfile(t, childDir, childLockfile(pinDigest))

	g := compileWorkflowDir(t, rootDir)
	sink, err := runEngine(t, g, newDefaultLoader(t, t.TempDir()))
	if err == nil {
		t.Fatal("expected startup failure for missing subworkflow adapter, got nil")
	}
	if len(sink.stepsRun) != 0 {
		t.Fatalf("expected no root steps to run, got %v", sink.stepsRun)
	}
	msg := err.Error()
	if !strings.Contains(msg, "noop.bot") {
		t.Errorf("error should name adapter instance noop.bot: %v", err)
	}
	if !strings.Contains(msg, childDir) {
		t.Errorf("error should name workflow directory %q: %v", childDir, err)
	}
	if !strings.Contains(msg, "criteria adapter lock") {
		t.Errorf("error should mention remediation command: %v", err)
	}
}

// TestPinGraph_LockfilePinnedNoByNameFallback asserts that a workflow with an
// OCI adapter reference never falls back to by-name discovery when the lockfile
// entry is missing or its binary is absent.
func TestPinGraph_LockfilePinnedNoByNameFallback(t *testing.T) {
	rootDir := t.TempDir()

	writeFile(t, filepath.Join(rootDir, "main.chcl"), `workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "bot" {
  source = "ghcr.io/brokenbots/criteria-adapter-noop"
}

step "start" {
  target = adapter.noop.bot
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
	// Lockfile is present but the digest-addressed binary is missing.
	writeLockfile(t, rootDir, rootLockfile())

	adaptersRoot := t.TempDir()
	// A by-name binary is available and must NOT be used as a fallback.
	installNoopByName(t, adaptersRoot)

	g := compileWorkflowDir(t, rootDir)
	_, err := runEngine(t, g, newDefaultLoader(t, adaptersRoot))
	if err == nil {
		t.Fatal("expected failure when digest-addressed binary is missing, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "noop.bot") {
		t.Errorf("error should name adapter instance noop.bot: %v", err)
	}
	if !strings.Contains(msg, rootDir) {
		t.Errorf("error should name workflow directory %q: %v", rootDir, err)
	}
	if !strings.Contains(msg, "criteria adapter lock") {
		t.Errorf("error should mention remediation command: %v", err)
	}
}

// TestPinGraph_DevBindingResolvesByName asserts that adapters registered with
// `criteria adapter dev` continue to resolve by name even when a lockfile pin
// is present.
func TestPinGraph_DevBindingResolvesByName(t *testing.T) {
	rootDir := t.TempDir()

	writeFile(t, filepath.Join(rootDir, "main.chcl"), `workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "bot" {
  source = "ghcr.io/brokenbots/criteria-adapter-noop"
}

step "start" {
  target = adapter.noop.bot
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
	writeLockfile(t, rootDir, rootLockfile())
	// Do not install the pinned binary; rely on the dev binding.

	loader := adapterhost.NewLoaderWithDiscovery(nil)
	loader.SetDevBindings(map[string]string{"noop.bot": getNoopAdapterBin(t)})

	g := compileWorkflowDir(t, rootDir)
	sink, err := runEngine(t, g, loader)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.terminalOK || sink.terminal != "done" {
		t.Fatalf("expected terminal done, got terminal=%q ok=%v", sink.terminal, sink.terminalOK)
	}
}

// TestPinGraph_VarAndDynamicWorkingDir asserts that --var overrides still reach
// adapter config at runtime and that a working_directory supplied by a runtime
// variable is passed to the adapter at scope entry.
func TestPinGraph_VarAndDynamicWorkingDir(t *testing.T) {
	rootDir := t.TempDir()
	wd := t.TempDir()

	writeFile(t, filepath.Join(rootDir, "main.chcl"), `workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

variable "working_directory" { default = "/tmp/default" }

adapter "noop" "bot" {
  source = "ghcr.io/brokenbots/criteria-adapter-noop"
  config {
    working_directory = var.working_directory
  }
}

step "start" {
  target = adapter.noop.bot
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
	writeLockfile(t, rootDir, rootLockfile())
	adaptersRoot := t.TempDir()
	installNoopAtDigest(t, adaptersRoot)

	g := compileWorkflowDir(t, rootDir)
	rec := &recordingFakeAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": rec}}

	vars := map[string]cty.Value{"working_directory": cty.StringVal(wd)}
	if _, err := runEngine(t, g, loader, WithVarOverrides(vars)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := rec.openConfig["working_directory"]; got != wd {
		t.Fatalf("expected working_directory %q, got %q", wd, got)
	}
}

// TestPinGraph_SubworkflowSessionBoundLazily asserts that subworkflow adapter
// sessions are opened at scope entry and torn down at scope exit, not held
// open for the whole run.
func TestPinGraph_SubworkflowSessionBoundLazily(t *testing.T) {
	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "child")

	writeFile(t, filepath.Join(rootDir, "main.chcl"), rootWorkflowWithSubworkflow())
	writeFile(t, filepath.Join(childDir, "main.chcl"), childWorkflow())

	g := compileWorkflowDir(t, rootDir)

	rec := &recordingFakeAdapter{fakeAdapter: fakeAdapter{name: "noop", outcome: "success"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"noop": rec}}

	if _, err := runEngine(t, g, loader); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.opened != 1 {
		t.Fatalf("expected session opened once at scope entry, got %d", rec.opened)
	}
	if rec.closed != 1 {
		t.Fatalf("expected session closed once at scope exit, got %d", rec.closed)
	}
}

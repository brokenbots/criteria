// Package smoke contains end-to-end smoke tests gated by environment variables
// so they do not run on every `go test` invocation.
package smoke

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// testSink captures the terminal state of a workflow run.
type testSink struct {
	mu       sync.Mutex
	terminal string
	success  bool
}

func (s *testSink) OnRunStarted(string, string) {}
func (s *testSink) OnRunCompleted(state string, ok bool) {
	s.mu.Lock()
	s.terminal = state
	s.success = ok
	s.mu.Unlock()
}
func (s *testSink) OnRunFailed(string, string)                                   {}
func (s *testSink) OnStepEntered(string, string, int)                            {}
func (s *testSink) OnStepOutcome(string, string, time.Duration, error)           {}
func (s *testSink) OnStepTransition(string, string, string)                      {}
func (s *testSink) OnStepResumed(string, int, string)                            {}
func (s *testSink) OnVariableSet(string, string, string)                         {}
func (s *testSink) OnStepOutputCaptured(string, map[string]string)               {}
func (s *testSink) OnRunPaused(string, string, string)                           {}
func (s *testSink) OnWaitEntered(string, string, string, string)                 {}
func (s *testSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *testSink) OnApprovalRequested(string, []string, string)                 {}
func (s *testSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *testSink) OnBranchEvaluated(string, string, string, string)             {}
func (s *testSink) OnForEachEntered(string, int)                                 {}
func (s *testSink) OnStepIterationStarted(string, int, string, bool)             {}
func (s *testSink) OnStepIterationCompleted(string, string, string)              {}
func (s *testSink) OnStepIterationItem(string, int, string)                      {}
func (s *testSink) OnScopeIterCursorSet(string)                                  {}
func (s *testSink) OnAdapterLifecycle(string, string, string, string)            {}
func (s *testSink) OnRunOutputs([]map[string]string)                             {}
func (s *testSink) OnStepOutcomeDefaulted(string, string, string)                {}
func (s *testSink) OnStepOutcomeUnknown(string, string)                          {}
func (s *testSink) StepEventSink(string) adapter.EventSink                       { return noopEventSink{} }

type noopEventSink struct{}

func (noopEventSink) Log(string, []byte)  {}
func (noopEventSink) Adapter(string, any) {}

// TestRemoteAdapter_HappyPath exercises the full phone-home flow: criteria
// starts a remote shim, the fixture adapter dials in, and a workflow step
// executes successfully.
//
// Gated by CRITERIA_REMOTE_E2E=1.
func TestRemoteAdapter_HappyPath(t *testing.T) {
	if os.Getenv("CRITERIA_REMOTE_E2E") != "1" {
		t.Skip("set CRITERIA_REMOTE_E2E=1 to run remote adapter smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	adapterBin := buildFixtureAdapter(t, moduleRoot)
	digest := sha256OfFile(t, adapterBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "remote-smoke-happy"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-token"
}

adapter "remote-smoke" "demo" {
  environment = remote.test
}

step "run" {
  target = adapter.remote-smoke.demo
  input {
    greeting = "hello"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("remote-smoke", "demo", digest)

	adapterCmd, adapterCancel := startAdapter(ctx, t, adapterBin, shimAddr, "smoke-token", digest)
	defer adapterCancel()

	sink := &testSink{}
	eng := engine.New(graph, adapterhost.NewLoader(), sink,
		engine.WithWorkflowDir(workflowDir),
		engine.WithLockfile(lf),
	)

	if err := eng.Run(ctx); err != nil {
		t.Fatalf("engine run: %v", err)
	}

	adapterCancel()
	_ = adapterCmd.Wait()

	if !sink.success {
		t.Fatalf("workflow did not complete successfully: terminal=%q", sink.terminal)
	}
}

// TestRemoteAdapter_CrashRecovery kills the adapter mid-execution, asserts the
// crash policy kicks in, then restarts the adapter and verifies the workflow
// completes after the respawn retry.
//
// Gated by CRITERIA_REMOTE_E2E=1.
func TestRemoteAdapter_CrashRecovery(t *testing.T) {
	if os.Getenv("CRITERIA_REMOTE_E2E") != "1" {
		t.Skip("set CRITERIA_REMOTE_E2E=1 to run remote adapter smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	adapterBin := buildFixtureAdapter(t, moduleRoot)
	digest := sha256OfFile(t, adapterBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "remote-smoke-crash"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-token"
}

adapter "remote-smoke" "demo" {
  environment = remote.test
  on_crash = "respawn"
}

step "run" {
  target = adapter.remote-smoke.demo
  input {
    greeting = "hello"
    delay_ms = "5000"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("remote-smoke", "demo", digest)

	stepStartedFile := filepath.Join(t.TempDir(), "step-started")

	// Start criteria apply in the background.
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	type engineResult struct {
		err      error
		success  bool
		terminal string
	}
	engDone := make(chan engineResult, 1)
	go func() {
		sink := &testSink{}
		eng := engine.New(graph, adapterhost.NewLoader(), sink,
			engine.WithWorkflowDir(workflowDir),
			engine.WithLockfile(lf),
		)
		r := engineResult{err: eng.Run(engCtx), success: sink.success, terminal: sink.terminal}
		engDone <- r
	}()

	// Give the engine a moment to start the shim.
	time.Sleep(500 * time.Millisecond)

	// Start the adapter with a marker file so we know when Execute begins.
	adapterCmd1, adapterCancel1 := startAdapterWithMarker(ctx, t, adapterBin, shimAddr, "smoke-token", digest, stepStartedFile)

	// Wait until the adapter signals that the step has started.
	waitForFile(t, stepStartedFile, 10*time.Second)

	// Give the step a moment to enter the delay so we kill it mid-execution.
	time.Sleep(500 * time.Millisecond)

	// Start the replacement adapter BEFORE killing the old one so the shim
	// has a fresh session ready when the engine calls respawn.
	_, adapterCancel2 := startAdapter(ctx, t, adapterBin, shimAddr, "smoke-token", digest)
	defer adapterCancel2()

	// Now kill the old adapter — the shim already has the new connection.
	adapterCancel1()
	_ = adapterCmd1.Wait()

	// Wait for criteria to finish.
	select {
	case r := <-engDone:
		if r.err != nil {
			t.Fatalf("engine run error: %v", r.err)
		}
		if !r.success {
			t.Fatalf("workflow did not complete successfully: terminal=%q", r.terminal)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for engine run to finish")
	}
}

// --- helpers ---

func findModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func buildFixtureAdapter(t *testing.T, moduleRoot string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "criteria-adapter-remote-smoke")
	srcDir := filepath.Join(moduleRoot, "internal", "ci", "smoke", "testdata", "criteria-adapter-remote-smoke")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture adapter: %v\n%s", err, string(out))
	}
	return binary
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open file for sha256: %v", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash file: %v", err)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func pickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

func parseWorkflow(t *testing.T, contents string) *workflow.Spec {
	t.Helper()
	spec, diags := workflow.Parse("workflow.hcl", []byte(strings.TrimSpace(contents)+"\n"))
	if diags.HasErrors() {
		t.Fatalf("parse workflow: %v", diags)
	}
	return spec
}

func compileWorkflow(t *testing.T, spec *workflow.Spec) *workflow.FSMGraph {
	t.Helper()
	graph, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile workflow: %v", diags)
	}
	return graph
}

func buildLockfile(adapterType, adapterName, digest string) *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               adapterType,
				Name:               adapterName,
				Reference:          "local",
				ResolvedDigest:     digest,
				SourceURL:          "local",
				SDKProtocolVersion: 2,
			},
		},
	}
}

func startAdapter(ctx context.Context, t *testing.T, bin, addr, token, digest string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	return startAdapterWithMarker(ctx, t, bin, addr, token, digest, "")
}

func startAdapterWithMarker(ctx context.Context, t *testing.T, bin, addr, token, digest, stepStartedFile string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, bin)
	cmd.Env = append(os.Environ(),
		"CRITERIA_REMOTE_HOST="+addr,
		"CRITERIA_REMOTE_TOKEN="+token,
		"CRITERIA_REMOTE_DIGEST="+digest,
	)
	if stepStartedFile != "" {
		cmd.Env = append(cmd.Env, "CRITERIA_REMOTE_STEP_STARTED_FILE="+stepStartedFile)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start adapter: %v", err)
	}
	return cmd, cancel
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for file %s", path)
}

// requireTools skips the test if any of the named binaries are not in PATH.
func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("required tool %q not found in PATH", tool)
		}
	}
}

// pickFreeHostAddr returns a free TCP address bound to 0.0.0.0 so that it is
// reachable from containers running on the same Docker network (e.g. kind).
func pickFreeHostAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("bind free address: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// hostIPReachableFromKind tries to determine the Docker host IP that is
// routable from a kind cluster. It first inspects the "kind" Docker network
// and falls back to the default route source address.
func hostIPReachableFromKind(t *testing.T) string {
	t.Helper()
	// Try the gateway of the kind network first.
	out, err := exec.Command("docker", "network", "inspect", "kind", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err == nil {
		ip := strings.TrimSpace(string(out))
		if ip != "" && ip != "<no value>" {
			return ip
		}
	}
	// Fallback: source IP used to reach an external address.
	out, err = exec.Command("ip", "route", "get", "1.1.1.1").Output()
	if err == nil {
		fields := strings.Fields(string(out))
		for i, f := range fields {
			if f == "src" && i+1 < len(fields) {
				return strings.TrimSpace(fields[i+1])
			}
		}
	}
	t.Fatal("could not determine host IP reachable from kind cluster")
	return ""
}

// buildFixtureAdapterImage compiles the fixture adapter for linux/amd64 and
// packages it as a minimal scratch Docker image with the given tag.
func buildFixtureAdapterImage(t *testing.T, moduleRoot, tag string) {
	t.Helper()
	buildDir := t.TempDir()
	srcDir := filepath.Join(moduleRoot, "internal", "ci", "smoke", "testdata", "criteria-adapter-remote-smoke")
	// Cross-compile for the Linux container.
	cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, "adapter"), ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture adapter binary: %v\n%s", err, out)
	}
	// Write a minimal Dockerfile.
	dockerfile := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nCOPY adapter /adapter\nENTRYPOINT [\"/adapter\"]\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	// Build the image from the build directory.
	buildCmd := exec.Command("docker", "build", "-t", tag, ".")
	buildCmd.Dir = buildDir
	out, err = buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture adapter image: %v\n%s", err, out)
	}
}

func kindCreate(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("kind", "create", "cluster", "--name", name, "--wait", "60s").CombinedOutput()
	if err != nil {
		t.Fatalf("kind create cluster: %v\n%s", err, out)
	}
}

func kindDelete(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("kind", "delete", "cluster", "--name", name).CombinedOutput()
	if err != nil {
		t.Fatalf("kind delete cluster: %v\n%s", err, out)
	}
}

func kindLoadImage(t *testing.T, name, tag string) {
	t.Helper()
	out, err := exec.Command("kind", "load", "docker-image", tag, "--name", name).CombinedOutput()
	if err != nil {
		t.Fatalf("kind load image: %v\n%s", err, out)
	}
}

// kubectlApply applies the given manifest YAML to the cluster.
func kubectlApply(t *testing.T, namespace, manifests string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, out)
	}
}

func kubectlWaitForDeployment(t *testing.T, name, namespace string, timeout time.Duration) {
	t.Helper()
	out, err := exec.Command("kubectl", "wait", "--for=condition=available",
		"--timeout="+timeout.String(), "-n", namespace, "deployment/"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl wait for deployment: %v\n%s", err, out)
	}
}

func kubectlDeletePod(t *testing.T, labelSelector, namespace string) {
	t.Helper()
	out, err := exec.Command("kubectl", "delete", "pods", "-n", namespace,
		"-l", labelSelector, "--grace-period=0", "--force").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl delete pod: %v\n%s", err, out)
	}
}

// waitForPodLog polls kubectl logs until the given substring appears or the
// timeout expires.
func waitForPodLog(t *testing.T, namespace, labelSelector, substring string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("kubectl", "logs", "-n", namespace, "-l", labelSelector, "--tail=50").CombinedOutput()
		if strings.Contains(string(out), substring) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for pod log containing %q", substring)
}

func TestRemoteAdapter_K8sHappyPath(t *testing.T) {
	if os.Getenv("CRITERIA_REMOTE_E2E") != "1" {
		t.Skip("set CRITERIA_REMOTE_E2E=1 to run remote adapter smoke tests")
	}
	requireTools(t, "kind", "kubectl", "docker", "go")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)

	clusterName := "criteria-smoke-" + strconv.Itoa(int(time.Now().UnixNano()))
	imageTag := "criteria-adapter-remote-smoke:e2e"
	defer kindDelete(t, clusterName)

	buildFixtureAdapterImage(t, moduleRoot, imageTag)
	kindCreate(t, clusterName)
	kindLoadImage(t, clusterName, imageTag)

	shimAddr := pickFreeHostAddr(t)
	hostIP := hostIPReachableFromKind(t)
	_, portStr, _ := net.SplitHostPort(shimAddr)
	adapterHostAddr := net.JoinHostPort(hostIP, portStr)

	workflowDir := t.TempDir()
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("k8s-fixture")))

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "remote-smoke-k8s-happy"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-k8s-token"
}

adapter "fixture" "greeter" {
  environment = remote.test
}

step "run" {
  target = adapter.fixture.greeter
  input {
    name = "k8s"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("fixture", "greeter", digest)

	// Deploy the adapter into kind.
	const namespace = "criteria-smoke"
	kubectlApply(t, namespace, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: greeter-config
  namespace: %s
data:
  CRITERIA_REMOTE_HOST: %s
---
apiVersion: v1
kind: Secret
metadata:
  name: greeter-token
  namespace: %s
stringData:
  CRITERIA_REMOTE_TOKEN: smoke-k8s-token
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: greeter
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: greeter
  template:
    metadata:
      labels:
        app: greeter
    spec:
      containers:
      - name: greeter
        image: %s
        imagePullPolicy: Never
        envFrom:
        - configMapRef:
            name: greeter-config
        - secretRef:
            name: greeter-token
        env:
        - name: CRITERIA_ADAPTER_NAME
          value: fixture
        - name: CRITERIA_REMOTE_DIGEST
          value: %s
`, namespace, namespace, adapterHostAddr, namespace, namespace, imageTag, digest))

	kubectlWaitForDeployment(t, "greeter", namespace, 60*time.Second)

	// Wait for the adapter pod to phone home.
	waitForPodLog(t, namespace, "app=greeter", "serving gRPC", 30*time.Second)

	sink := &testSink{}
	eng := engine.New(graph, adapterhost.NewLoader(), sink,
		engine.WithWorkflowDir(workflowDir),
		engine.WithLockfile(lf),
	)

	if err := eng.Run(ctx); err != nil {
		t.Fatalf("engine run: %v", err)
	}

	if !sink.success {
		t.Fatalf("workflow did not complete successfully: terminal=%q", sink.terminal)
	}
}

func TestRemoteAdapter_K8sCrashRecovery(t *testing.T) {
	if os.Getenv("CRITERIA_REMOTE_E2E") != "1" {
		t.Skip("set CRITERIA_REMOTE_E2E=1 to run remote adapter smoke tests")
	}
	requireTools(t, "kind", "kubectl", "docker", "go")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)

	clusterName := "criteria-smoke-" + strconv.Itoa(int(time.Now().UnixNano()))
	imageTag := "criteria-adapter-remote-smoke:e2e"
	defer kindDelete(t, clusterName)

	buildFixtureAdapterImage(t, moduleRoot, imageTag)
	kindCreate(t, clusterName)
	kindLoadImage(t, clusterName, imageTag)

	shimAddr := pickFreeHostAddr(t)
	hostIP := hostIPReachableFromKind(t)
	_, portStr, _ := net.SplitHostPort(shimAddr)
	adapterHostAddr := net.JoinHostPort(hostIP, portStr)

	workflowDir := t.TempDir()
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("k8s-fixture")))

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "remote-smoke-k8s-crash"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-k8s-token"
}

adapter "fixture" "greeter" {
  environment = remote.test
}

step "run" {
  target = adapter.fixture.greeter
  input {
    name     = "k8s"
    delay_ms = "15000"
  }
  on_crash = "respawn"
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("fixture", "greeter", digest)

	const namespace = "criteria-smoke"
	kubectlApply(t, namespace, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: greeter-config
  namespace: %s
data:
  CRITERIA_REMOTE_HOST: %s
---
apiVersion: v1
kind: Secret
metadata:
  name: greeter-token
  namespace: %s
stringData:
  CRITERIA_REMOTE_TOKEN: smoke-k8s-token
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: greeter
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: greeter
  template:
    metadata:
      labels:
        app: greeter
    spec:
      containers:
      - name: greeter
        image: %s
        imagePullPolicy: Never
        envFrom:
        - configMapRef:
            name: greeter-config
        - secretRef:
            name: greeter-token
        env:
        - name: CRITERIA_ADAPTER_NAME
          value: fixture
        - name: CRITERIA_REMOTE_DIGEST
          value: %s
`, namespace, namespace, adapterHostAddr, namespace, namespace, imageTag, digest))

	kubectlWaitForDeployment(t, "greeter", namespace, 60*time.Second)

	// Wait for the adapter pod to phone home.
	waitForPodLog(t, namespace, "app=greeter", "serving gRPC", 30*time.Second)

	sink := &testSink{}
	eng := engine.New(graph, adapterhost.NewLoader(), sink,
		engine.WithWorkflowDir(workflowDir),
		engine.WithLockfile(lf),
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Run(ctx)
	}()

	// Wait for the step to start executing (delay_ms = 15s).
	waitForPodLog(t, namespace, "app=greeter", "step execution started", 30*time.Second)

	// Delete the adapter pod mid-execution.
	kubectlDeletePod(t, "app=greeter", namespace)

	// Wait for the deployment to recreate the pod.
	kubectlWaitForDeployment(t, "greeter", namespace, 60*time.Second)

	// Wait for the engine to finish.
	if err := <-errCh; err != nil {
		t.Fatalf("engine run: %v", err)
	}

	if !sink.success {
		t.Fatalf("workflow did not complete successfully after crash recovery: terminal=%q", sink.terminal)
	}
}

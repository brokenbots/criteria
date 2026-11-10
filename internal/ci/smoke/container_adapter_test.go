// container_adapter_test.go — the container-lane gate: drives the real CLI
// flows (publish → pull → lock) against a throwaway loopback OCI registry and
// then runs the workflow through the engine's container lane so the adapter
// child launches in-image (docker run with the recorded container image) and
// the go-plugin identity handshake reaches the in-process env host. A green
// run here is the runner-pod proof the container image resolution, launch,
// and handshake all work end-to-end (ADR-0014 M8).
//
// Gated by CRITERIA_CONTAINER_E2E=1 and a Docker-capable host, mirroring the
// CRITERIA_REMOTE_E2E pattern in remote_adapter_test.go.
package smoke

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/envreg"
	"github.com/brokenbots/criteria/workflow/lockfile"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

// newE2ERegistry starts a throwaway OCI registry container on a free
// loopback port and returns its <localhost>:<port> address; localhost
// registries are reached over plain HTTP by both the CLI (oci IsLocalhost)
// and the docker daemon's default insecure fallback.
func newE2ERegistry(t *testing.T) string {
	t.Helper()

	port := freeTCPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	name := fmt.Sprintf("criteria-e2e-registry-%d", port)

	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-p", addr+":5000", "--name", name, "registry:2").CombinedOutput()
	if err != nil {
		t.Fatalf("start registry container: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	registry := fmt.Sprintf("localhost:%d", port)
	if !waitRegistryReady(registry, 30*time.Second) {
		t.Fatalf("registry %s did not become ready", registry)
	}
	return registry
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitRegistryReady(registry string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + registry + "/v2/")
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// buildNoopAdapterImage compiles the in-tree noop fixture for the container
// platform and packages it as a scratch Docker image tagged into the
// throwaway registry. Returns the <registry>/<repo>:v1.0.0 image reference.
func buildNoopAdapterImage(t *testing.T, moduleRoot, moduleBinDir, registry, repo string) string {
	t.Helper()

	srcDir := filepath.Join(moduleRoot, "internal", "adapter", "conformance", "testdata", "noop")
	noopBin := filepath.Join(moduleBinDir, "criteria-adapter-noop")
	cmd := exec.Command("go", "build", "-o", noopBin, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build noop fixture adapter: %v\n%s", err, out)
	}

	buildDir := t.TempDir()
	dockerfile := "FROM scratch\nCOPY criteria-adapter-noop /criteria-adapter-noop\n" +
		"ENTRYPOINT [\"/criteria-adapter-noop\"]\n"
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	binData, err := os.ReadFile(filepath.Join(moduleBinDir, "criteria-adapter-noop"))
	if err != nil {
		t.Fatalf("read noop binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "criteria-adapter-noop"), binData, 0o755); err != nil {
		t.Fatalf("stage noop binary: %v", err)
	}

	ref := registry + "/" + repo + ":v1.0.0"
	build := exec.Command("docker", "build", "-t", ref, ".")
	build.Dir = buildDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build adapter container image: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "push", ref).CombinedOutput(); err != nil {
		t.Fatalf("push adapter container image: %v\n%s", err, out)
	}
	return ref
}

// buildCriteriaCLI compiles the criteria CLI so the probe drives the real
// publish/pull/lock operator flows instead of package internals.
func buildCriteriaCLI(t *testing.T, moduleRoot, moduleBinDir string) string {
	t.Helper()
	cli := filepath.Join(moduleBinDir, "criteria")
	cmd := exec.Command("go", "build", "-o", cli, "./cmd/criteria")
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build criteria CLI: %v\n%s", err, out)
	}
	return cli
}

func runCLI(t *testing.T, cli string, args ...string) string {
	t.Helper()
	cmd := exec.Command(cli, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("criteria %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestContainerAdapter_HappyPath publishes the noop fixture as a
// container-capable adapter artifact into a throwaway loopback registry,
// locks a container-bound workflow against it, and runs the workflow through
// the engine. The engine resolves the locked container image, launches the
// adapter child in-image, and completes the identity handshake plus a real
// step execution.
//
// Gated by CRITERIA_CONTAINER_E2E=1; requires Docker.
func TestContainerAdapter_HappyPath(t *testing.T) {
	if envreg.CRITERIAContainerE2E.String() != "1" {
		t.Skip("set CRITERIA_CONTAINER_E2E=1 to run container adapter smoke tests")
	}
	requireTools(t, "docker")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	moduleBinDir := t.TempDir()

	registry := newE2ERegistry(t)
	imageRef := buildNoopAdapterImage(t, moduleRoot, moduleBinDir, registry, "e2e-noop-image")

	cli := buildCriteriaCLI(t, moduleRoot, moduleBinDir)
	noopBin := filepath.Join(moduleBinDir, "criteria-adapter-noop")

	// publish records imageRef (with its registry-resolved digest) into the
	// artifact manifest, so the container_image block reaches the lockfile.
	runCLI(t, cli, "adapter", "publish", noopBin,
		"--registry", registry+"/e2e-noop:v1.0.0",
		"--image", imageRef)
	runCLI(t, cli, "adapter", "pull", registry+"/e2e-noop:v1.0.0", "--allow-unsigned")

	workflowDir := t.TempDir()
	wf := workflowtest.New().
		WithHeader("container-e2e", "0.1", "warm", "done").
		WithEnvironment("container", "e2e", "").
		WithAdapter("noop", "default",
			workflowtest.AdapterEnvironment(workflowtest.EnvironmentRef("container", "e2e")),
			workflowtest.AdapterSource(registry+"/e2e-noop"),
			workflowtest.AdapterVersion("v1.0.0")).
		WithState("done", true, boolPtr(true)).
		WithState("failed", true, boolPtr(false)).
		WithStep("warm",
			workflowtest.StepRaw("input {\n  delay_ms = 0\n}"),
			workflowtest.Outcome("success", "state.done"),
			workflowtest.Outcome("failure", "state.failed"),
		)
	if err := os.WriteFile(filepath.Join(workflowDir, "workflow.hcl"), []byte(wf.Build()), 0o644); err != nil {
		t.Fatalf("write workflow HCL: %v", err)
	}

	runCLI(t, cli, "adapter", "lock", workflowDir, "--allow-unsigned")
	lf, err := lockfile.Read(filepath.Join(workflowDir, ".criteria.lock.hcl"))
	if err != nil {
		t.Fatalf("read CLI-generated lockfile: %v", err)
	}
	entry := findLockEntry(t, lf, "noop", "default")
	if entry.ContainerImage == nil {
		t.Fatalf("lock entry noop.default has no container_image block; published image did not reach the lock")
	}

	spec := parseWorkflow(t, wf.Build())
	graph := compileWorkflow(t, spec)

	sink := &testSink{}
	eng := engine.New(graph, adapterhost.NewLoader(), sink,
		engine.WithWorkflowDir(workflowDir),
		engine.WithLockfile(lf),
	)
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("engine run through the container lane: %v", err)
	}
	if !sink.success || sink.terminal != "done" {
		t.Fatalf("container-lane workflow did not complete successfully: terminal=%q success=%v",
			sink.terminal, sink.success)
	}
}

func findLockEntry(t *testing.T, lf *lockfile.Lockfile, envType, name string) *lockfile.LockedAdapter {
	t.Helper()
	for i := range lf.Adapters {
		if lf.Adapters[i].Type == envType && lf.Adapters[i].Name == name {
			return &lf.Adapters[i]
		}
	}
	t.Fatalf("lockfile has no lock entry for adapter %s.%s", envType, name)
	return nil
}

func boolPtr(v bool) *bool { return &v }

// TestContainerAdapter_WorkflowShape pins that the builder-emitted container-e2e
// workflow is valid HCL even when the smoke gate is off, so builder drift
// fails the default test run rather than only the gated run.
func TestContainerAdapter_WorkflowShape(t *testing.T) {
	wf := workflowtest.New().
		WithHeader("container-e2e", "0.1", "warm", "done").
		WithEnvironment("container", "e2e", "").
		WithAdapter("noop", "default",
			workflowtest.AdapterEnvironment(workflowtest.EnvironmentRef("container", "e2e")),
			workflowtest.AdapterSource("localhost:5000/e2e-noop"),
			workflowtest.AdapterVersion("v1.0.0")).
		WithState("done", true, boolPtr(true)).
		WithStep("warm",
			workflowtest.StepRaw("input {\n  delay_ms = 0\n}"),
			workflowtest.Next("state.done"),
		)
	spec := parseWorkflow(t, wf.Build())
	compileWorkflow(t, spec)
}

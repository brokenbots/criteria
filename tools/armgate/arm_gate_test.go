// Package armgate holds the regression suite for tools/armgate/arm-gate.sh,
// the arm-contract compile gate against the runner-pod engine stamp.
package armgate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const stubValidateOK = `#!/bin/sh
case "$1" in
  version)  echo "__VERSION__" ;;
  validate) echo "$2: ok" ;;
  *)        exit 64 ;;
esac
exit 0
`

var scriptPath = filepath.Join(moduleRoot(), "tools", "armgate", "arm-gate.sh")

// TestMain compiles the real criteria CLI once so the gate runs against the
// actual validate surface, not a fake: the stubs below cover the fail-closed
// decision paths around binary discovery, while the real-binary cases cover
// the compile-exposure regression itself.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "armgate-criteria-bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "armgate: create temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)

	bin := filepath.Join(dir, "criteria")
	build := exec.Command("go", "build", "-o", bin, "./cmd/criteria")
	build.Dir = moduleRoot()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "armgate: build criteria binary: %v\n%s", err, out)
		return 1
	}
	criteriaBin = bin
	return m.Run()
}

// criteriaBin is set by TestMain.
var criteriaBin string

func moduleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("armgate: resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// runGate invokes the script with a hermetic environment: a minimal PATH so
// the host's own criteria binary can never leak in as a candidate, and temp
// HOME/CRITERIA_HOME/TMPDIR so validate touches nothing user-owned.
func runGate(t *testing.T, pathExtra, environ string, args ...string) (string, int) {
	t.Helper()

	dir := t.TempDir()
	env := []string{
		"PATH=" + pathExtra + "/usr/bin:/bin",
		"HOME=" + dir,
		"CRITERIA_HOME=" + dir,
		"TMPDIR=" + dir,
	}
	if environ != "" {
		env = append(env, environ)
	}

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running arm-gate: %v\noutput:\n%s", err, out)
	}
	return string(out), code
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(moduleRoot(), "tools", "armgate", "testdata", name)
}

// writePathStub installs a criteria-shaped stub on PATH. It answers
// `version` with the given output and pretends every `validate` passes.
func writePathStub(t *testing.T, versionOutput string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "criteria")
	body := strings.ReplaceAll(stubValidateOK, "__VERSION__", versionOutput)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return dir
}

// TestArmGate_FailsClosedOnNewerBaseEngine is the workstream regression: a
// tree whose only-compiled-on-release shape violates the newer engine's
// outcome-completeness rule MUST make the gate refuse to arm, even though
// trees on disk look "validated" from a stale pin's point of view.
func TestArmGate_FailsClosedOnNewerBaseEngine(t *testing.T) {
	out, code := runGate(t, "",
		"CRITERIA_OVERRIDE_VERSION=0.5.57",
		"--base-image", "registry.internal/criteria-base:v0.5.57",
		"--criteria-bin", criteriaBin,
		"--tree", fixture(t, "incomplete"),
	)
	if code != 1 {
		t.Fatalf("expected exit 1 (fail closed) for incomplete tree against v0.5.57 engine, got %d; output:\n%s", code, out)
	}
	for _, want := range []string{
		"outcome completeness requires handling adapter failure/error paths",
		"GATE FAILED",
		"do NOT arm",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected gate output to contain %q; output:\n%s", want, out)
		}
	}
}

// TestArmGate_CompositeBaseTagResolvesToLastEngineVersion exercises the
// observed production tag shape criteria-base:v0.1-v0.5.56, whose operative
// engine stamp is v0.5.56, and a tree that compiles clean under a newer
// binary (v0.5.57 >= v0.5.56) passes.
func TestArmGate_CompositeBaseTagResolvesToLastEngineVersion(t *testing.T) {
	out, code := runGate(t, "",
		"CRITERIA_OVERRIDE_VERSION=0.5.57",
		"--base-image", "registry.internal/criteria-base:v0.1-v0.5.56",
		"--criteria-bin", criteriaBin,
		"--tree", fixture(t, "valid"),
	)
	if code != 0 {
		t.Fatalf("expected exit 0 for clean tree under >= base engine, got %d; output:\n%s", code, out)
	}
	for _, want := range []string{
		"engine v0.5.56",
		"GATE PASSED",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected gate output to contain %q; output:\n%s", want, out)
		}
	}
}

// TestArmGate_ExactBaseEnginePasses: a binary reporting exactly the base
// stamp is acceptable, and the gate reports the version it gated under.
func TestArmGate_ExactBaseEnginePasses(t *testing.T) {
	out, code := runGate(t, "",
		"CRITERIA_OVERRIDE_VERSION=0.5.57",
		"--base-image", "criteria-base:v0.5.57",
		"--criteria-bin", criteriaBin,
		"--tree", fixture(t, "valid"),
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "GATE PASSED") {
		t.Fatalf("expected GATE PASSED; output:\n%s", out)
	}
}

// TestArmGate_OlderPathCandidateFailsClosed: a criteria on PATH older than
// the base stamp must not silently gate the arming, and without an explicit
// --criteria-repo the gate has no replacement and fails closed.
func TestArmGate_OlderPathCandidateFailsClosed(t *testing.T) {
	stubDir := writePathStub(t, "v0.5.56")
	out, code := runGate(t, stubDir+":",
		"",
		"--base-image", "criteria-base:v0.5.57",
		"--tree", fixture(t, "valid"),
	)
	if code != 1 {
		t.Fatalf("expected exit 1 (fail closed), got %d; output:\n%s", code, out)
	}
	for _, want := range []string{
		"reports v0.5.56, older than base engine v0.5.57",
		"no usable criteria binary was found",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected gate output to contain %q; output:\n%s", want, out)
		}
	}
}

// TestArmGate_UnparsableCandidateFailsClosed: a binary that reports no clean
// engine version (e.g. a dev build) must be rejected as an unknown stamp.
func TestArmGate_UnparsableCandidateFailsClosed(t *testing.T) {
	stubDir := writePathStub(t, "dev")
	out, code := runGate(t, stubDir+":",
		"",
		"--base-image", "criteria-base:v0.5.57",
		"--criteria-bin", filepath.Join(stubDir, "criteria"),
		"--tree", fixture(t, "valid"),
	)
	if code != 1 {
		t.Fatalf("expected exit 1 (fail closed), got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "reports no parseable engine version") {
		t.Fatalf("expected unparsable rejection; output:\n%s", out)
	}
}

// TestArmGate_PathCandidateFallsThroughToBuildFallback: an unusable PATH
// candidate is reported and replaced by the next candidate when one exists.
// With an empty checkout the fallback fails closed at the missing tag.
func TestArmGate_BuildFallbackTagMissingFailsClosed(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if out, err := exec.Command("/bin/sh", "-c", "/usr/bin/git init -q "+shq(repo)).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	out, code := runGate(t, "",
		"",
		"--base-image", "criteria-base:v0.5.57",
		"--criteria-repo", repo,
		"--tree", fixture(t, "valid"),
	)
	if code != 1 {
		t.Fatalf("expected exit 1 (fail closed), got %d; output:\n%s", code, out)
	}
	for _, want := range []string{
		"tag v0.5.57 not found",
		"no criteria binary at engine v0.5.57 or newer is available",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected gate output to contain %q; output:\n%s", want, out)
		}
	}
}

// TestArmGate_BuildFallbackMissingRepoFailsClosed validates the guard on the
// fallback argument itself.
func TestArmGate_BuildFallbackMissingRepoFailsClosed(t *testing.T) {
	out, code := runGate(t, "",
		"",
		"--base-image", "criteria-base:v0.5.57",
		"--criteria-repo", "/nonexistent/repo",
		"--tree", fixture(t, "valid"),
	)
	if code != 1 {
		t.Fatalf("expected exit 1 (fail closed), got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "is not a git checkout") {
		t.Fatalf("expected not-a-checkout rejection; output:\n%s", out)
	}
}

// TestArmGate_UnparsableBaseImageFailsClosed: a floating tag carries no
// engine stamp, so there is nothing the trees could be gated against.
func TestArmGate_UnparsableBaseImageFailsClosed(t *testing.T) {
	stubDir := writePathStub(t, "v0.6.0")
	out, code := runGate(t, stubDir+":",
		"",
		"--base-image", "criteria-base:latest",
		"--tree", fixture(t, "valid"),
	)
	if code != 1 {
		t.Fatalf("expected exit 1 (fail closed), got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "cannot determine an engine version") {
		t.Fatalf("expected engine-version failure; output:\n%s", out)
	}
}

// TestArmGate_MissingTreeFailsGateValidation: a tree argument that does not
// exist is a gate failure, not a silent pass.
func TestArmGate_MissingTreeFailsGateValidation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	out, code := runGate(t, "",
		"CRITERIA_OVERRIDE_VERSION=0.5.57",
		"--base-image", "criteria-base:v0.5.57",
		"--criteria-bin", criteriaBin,
		"--tree", missing,
	)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "GATE FAILED") {
		t.Fatalf("expected GATE FAILED; output:\n%s", out)
	}
}

// TestArmGate_MultipleTreesGateTogether: several route-pinned trees are all
// gated in one invocation.
func TestArmGate_MultipleTreesGateTogether(t *testing.T) {
	stubDir := writePathStub(t, "v0.5.57")
	out, code := runGate(t, stubDir+":",
		"",
		"--base-image", "criteria-base:v0.5.57",
		"--tree", fixture(t, "valid"),
		"--tree", fixture(t, "valid"),
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	for _, want := range []string{
		"reports v0.5.57",
		"GATE PASSED: 2 tree(s)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected gate output to contain %q; output:\n%s", want, out)
		}
	}
}

func TestArmGate_UsageFailsWithExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--base-image"},
		{"--nope"},
	} {
		out, code := runGate(t, "", "", args...)
		if code != 2 {
			t.Fatalf("expected exit 2 (usage) for args %v, got %d; output:\n%s", args, code, out)
		}
	}
}

// shq single-quotes s for /bin/sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

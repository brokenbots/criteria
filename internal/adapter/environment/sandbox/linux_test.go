//go:build linux

package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	"golang.org/x/sys/unix"

	"github.com/brokenbots/criteria/workflow"
)

func TestBuildSysProcAttr(t *testing.T) {
	attr := buildSysProcAttr()
	if attr == nil {
		t.Fatal("expected non-nil SysProcAttr")
	}
	want := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID |
		syscall.CLONE_NEWNET | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS)
	if attr.Cloneflags != want {
		t.Fatalf("Cloneflags = %#x, want %#x", attr.Cloneflags, want)
	}
	if len(attr.UidMappings) != 1 {
		t.Fatalf("expected 1 uid mapping, got %d", len(attr.UidMappings))
	}
	if attr.UidMappings[0].ContainerID != 0 {
		t.Fatalf("expected ContainerID 0, got %d", attr.UidMappings[0].ContainerID)
	}
}

func TestBuildRlimits(t *testing.T) {
	rs := buildRlimits(1024*1024, 5*time.Second)
	if len(rs) == 0 {
		t.Fatal("expected rlimits")
	}
	foundAS := false
	foundCPU := false
	for _, r := range rs {
		if r.Resource == unix.RLIMIT_AS {
			foundAS = true
			if r.Rlimit.Cur != 1024*1024 {
				t.Fatalf("RLIMIT_AS cur = %d, want %d", r.Rlimit.Cur, 1024*1024)
			}
		}
		if r.Resource == unix.RLIMIT_CPU {
			foundCPU = true
			if r.Rlimit.Cur != 5 {
				t.Fatalf("RLIMIT_CPU cur = %d, want 5", r.Rlimit.Cur)
			}
		}
	}
	if !foundAS {
		t.Fatal("expected RLIMIT_AS")
	}
	if !foundCPU {
		t.Fatal("expected RLIMIT_CPU")
	}
}

func TestLinuxPrepared_ApplyToCmd(t *testing.T) {
	prep := LinuxPrepared{
		SysProcAttr: &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER},
		Mode:        "strict",
		TargetPath:  "/usr/bin/true",
		ReadPaths:   []string{"/tmp"},
	}
	cmd := exec.Command("/bin/sh", "-c", "echo hello")
	if err := (&prep).ApplyToCmd(cmd, ""); err != nil {
		t.Fatalf("ApplyToCmd: %v", err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Cloneflags != syscall.CLONE_NEWUSER {
		t.Fatal("expected SysProcAttr to be set")
	}
	if cmd.Path == "" {
		t.Fatal("expected cmd.Path to be replaced with shim")
	}
	found := false
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "CRITERIA_SANDBOX_CONFIG_PATH=") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected CRITERIA_SANDBOX_CONFIG_PATH env var")
	}
}

func TestHandlerPrepare(t *testing.T) {
	caps := Probe()
	ctx := PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
		},
		Caps: caps,
	}
	prep, err := Handler{}.Prepare(ctx)
	if err != nil {
		if ctx.Policy.PolicyMode == "strict" {
			t.Fatalf("strict mode Prepare failed: %v", err)
		}
	}
	if prep.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr in prepared config")
	}
}

// TestShimIntegration compiles a tiny helper binary that imports this
// package, calls RunIfEnv, and then tries prohibited operations. The
// helper runs inside the shim so we can verify restrictions from the
// parent process.
func TestShimIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}

	dir := t.TempDir()
	helper := filepath.Join(dir, "sandbox_helper")

	fixtureDir := filepath.Join(os.Getenv("PWD"), "testfixture")
	cmd := exec.Command("go", "build", "-o", helper, fixtureDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, out)
	}

	cfg := ShimConfig{
		TargetPath:   helper,
		Mode:         "strict",
		ReadPaths:    []string{"/tmp"},
		AllowNetwork: false,
		Seccomp:      true,
	}
	tmpFile, err := os.CreateTemp("", "criteria-sandbox-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	if err := json.NewEncoder(tmpFile).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	runCmd := exec.Command(helper)
	runCmd.Env = append(os.Environ(), "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	outStr := string(out)
	t.Logf("helper output:\n%s", outStr)

	if strings.Contains(outStr, "NOT_SHIM") {
		t.Fatal("helper did not enter shim mode")
	}
	if strings.Contains(outStr, "OPEN_OK") {
		t.Fatal("expected /etc/passwd open to fail under sandbox")
	}
	if strings.Contains(outStr, "TMP_FAIL") {
		t.Fatal("expected /tmp to remain readable")
	}
	if strings.Contains(outStr, "SETUID_OK") {
		t.Fatal("expected setuid to fail under sandbox")
	}
	if strings.Contains(outStr, "CONNECT_OK") {
		t.Fatal("expected connect to 8.8.8.8:53 to fail under sandbox")
	}
}

func TestPrepareDegradation(t *testing.T) {
	// Simulate a host that lacks landlock.
	h := &Handler{}
	ctx := PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			PolicyMode: "permissive",
			TypeSpecific: map[string]cty.Value{
				"filesystem": cty.ObjectVal(map[string]cty.Value{
					"read": cty.TupleVal([]cty.Value{cty.StringVal("/etc")}),
				}),
			},
		},
		Caps: Capabilities{UserNamespaces: true, Landlock: false, Seccomp: true, Cgroupv2: true},
	}

	// Permissive mode: missing landlock is logged but we still get a
	// LinuxPrepared with namespaces.
	lp, err := h.Prepare(ctx)
	if err != nil {
		t.Fatalf("unexpected error in permissive mode: %v", err)
	}
	if lp.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr in permissive mode")
	}
	// Landlock paths are not populated because the capability is absent.
	if len(lp.ReadPaths) != 0 {
		t.Fatalf("expected no read paths when landlock is missing, got %v", lp.ReadPaths)
	}

	// Strict mode: missing landlock aborts.
	ctx.Policy.PolicyMode = "strict"
	_, err = h.Prepare(ctx)
	if err == nil {
		t.Fatal("expected error in strict mode when landlock is missing")
	}
	if !strings.Contains(err.Error(), "landlock") {
		t.Fatalf("expected error to mention landlock, got: %v", err)
	}
}

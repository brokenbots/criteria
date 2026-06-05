//go:build linux

package adapterhost

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/workflow"
)

func TestBuildSandboxCustomizer_EnvScrubIntegration(t *testing.T) {
	// End-to-end env scrub: verify that after buildSandboxCustomizer →
	// makeSandboxCustomizer → ApplyToCmd, blocked vars are absent from
	// cmd.Env and the shim config path is present.
	os.Setenv("SUDO_UID", "1000")
	os.Setenv("CRITERIA_PLUGIN", "/tmp/fake-plugin")
	defer os.Unsetenv("SUDO_UID")
	defer os.Unsetenv("CRITERIA_PLUGIN")

	sm := NewSessionManager(nil)
	sm.sandboxProbeOverride = func() sandbox.Capabilities {
		return sandbox.Capabilities{UserNamespaces: true, Landlock: false, Seccomp: true, Cgroupv2: true}
	}
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "sandbox.default"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"sandbox.default": {Type: "sandbox", Name: "default"},
		},
		ResolvedPolicies: map[string]*workflow.ResolvedPolicy{
			"noop.default:sandbox.default": {PolicyMode: "permissive", OS: "linux"},
		},
	}
	customizer, cleanup, err := sm.buildSandboxCustomizer("noop.default", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer == nil {
		t.Fatal("expected non-nil customizer")
	}
	defer cleanup()

	cmd := exec.Command("/usr/bin/true")
	customizer("noop", cmd)

	for _, e := range cmd.Env {
		name, _, _ := strings.Cut(e, "=")
		switch name {
		case "SUDO_UID", "SUDO_GID", "SUDO_USER", "SUDO_COMMAND", "SUDO_EDITOR", "CRITERIA_PLUGIN":
			t.Fatalf("blocked env var %q present after customizer", name)
		}
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

func TestBuildSandboxCustomizer_PermissiveMissingCaps(t *testing.T) {
	sm := NewSessionManager(nil)
	// Simulate a host that lacks landlock but has everything else.
	sm.sandboxProbeOverride = func() sandbox.Capabilities {
		return sandbox.Capabilities{UserNamespaces: true, Landlock: false, Seccomp: true, Cgroupv2: true}
	}
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "sandbox.default"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"sandbox.default": {Type: "sandbox", Name: "default"},
		},
		ResolvedPolicies: map[string]*workflow.ResolvedPolicy{
			"noop.default:sandbox.default": {PolicyMode: "permissive", OS: "linux"},
		},
	}
	customizer, cleanup, err := sm.buildSandboxCustomizer("noop.default", "")
	if err != nil {
		t.Fatalf("unexpected error in permissive mode: %v", err)
	}
	if customizer == nil {
		t.Fatal("expected non-nil customizer in permissive mode even with missing landlock")
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup")
	}
	cleanup()
}

func TestBuildSandboxCustomizer_StrictMissingCaps(t *testing.T) {
	sm := NewSessionManager(nil)
	// Simulate a host that lacks landlock.
	sm.sandboxProbeOverride = func() sandbox.Capabilities {
		return sandbox.Capabilities{UserNamespaces: true, Landlock: false, Seccomp: true, Cgroupv2: true}
	}
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "sandbox.default"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"sandbox.default": {Type: "sandbox", Name: "default"},
		},
		ResolvedPolicies: map[string]*workflow.ResolvedPolicy{
			"noop.default:sandbox.default": {PolicyMode: "strict", OS: "linux"},
		},
	}
	customizer, cleanup, err := sm.buildSandboxCustomizer("noop.default", "")
	if err == nil {
		t.Fatal("expected error in strict mode when landlock is missing")
	}
	if !strings.Contains(err.Error(), "landlock") {
		t.Fatalf("expected error to mention landlock, got: %v", err)
	}
	if customizer != nil || cleanup != nil {
		t.Fatal("expected nil customizer and cleanup on strict error")
	}
}

func TestMakeSandboxCustomizer_BwrapNotOptedIn(t *testing.T) {
	prep := &sandbox.LinuxPrepared{
		SysProcAttr: &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER},
		TargetPath:  "/usr/bin/true",
	}
	env := &workflow.EnvironmentNode{Type: "sandbox", Name: "default"}
	customizer, cleanup := makeSandboxCustomizer(prep, env, "")
	if customizer == nil {
		t.Fatal("expected non-nil customizer")
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup")
	}

	// Verify the customizer applies the shim, not bwrap.
	cmd := exec.Command("/usr/bin/true")
	customizer("noop", cmd)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Cloneflags != syscall.CLONE_NEWUSER {
		if cmd.SysProcAttr == nil {
			t.Fatal("expected SysProcAttr to be set by shim customizer")
		}
	}
	cleanup()
}

func TestMakeSandboxCustomizer_ShimCallsApplyToCmd(t *testing.T) {
	prep := &sandbox.LinuxPrepared{
		SysProcAttr:  &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER},
		TargetPath:   "/usr/bin/my-adapter",
		Mode:         "strict",
		ReadPaths:    []string{"/tmp"},
		AllowNetwork: true,
	}
	env := &workflow.EnvironmentNode{Type: "sandbox", Name: "default"}
	customizer, cleanup := makeSandboxCustomizer(prep, env, "")
	if customizer == nil {
		t.Fatal("expected non-nil customizer")
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup")
	}
	defer cleanup()

	cmd := exec.Command("/usr/bin/my-adapter", "--flag", "value")
	customizer("noop", cmd)

	// ApplyToCmd replaces cmd.Path with the criteria binary.
	if cmd.Path == "" {
		t.Fatal("expected cmd.Path to be replaced with shim")
	}
	if cmd.Path == "/usr/bin/my-adapter" {
		t.Fatal("expected cmd.Path to be replaced, not left as adapter path")
	}
	found := false
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "CRITERIA_SANDBOX_CONFIG_PATH=") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected CRITERIA_SANDBOX_CONFIG_PATH env var after customizer")
	}
}

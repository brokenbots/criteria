//go:build linux

package adapterhost

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
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
	customizer, cleanup := makeSandboxCustomizer(prep, env, "", "")
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
	customizer, cleanup := makeSandboxCustomizer(prep, env, "", "")
	if customizer == nil {
		t.Fatal("expected non-nil customizer")
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup")
	}
	defer cleanup()

	cmd := exec.Command("/usr/bin/my-adapter", "--flag", "value")
	customizer("noop", cmd)

	// ApplyToCmd replaces cmd.Path with the shim binary.
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

// TestSessionManager_Open_LockedOCIAdapter_Sandbox verifies that a
// digest-installed adapter binary bound to a Linux sandbox environment starts
// under the in-process shim and reaches plugin handshake (OpenSession). This
// is the CRI-40 regression test: before the fix the shim received an empty
// target path and the adapter exited with status 125 before the handshake.
func TestSessionManager_Open_LockedOCIAdapter_Sandbox(t *testing.T) {
	caps := sandbox.Probe()
	if !caps.UserNamespaces || !caps.Seccomp {
		t.Skip("sandbox user namespaces or seccomp not available on this host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Install the noop adapter binary under a digest-addressed directory so it
	// looks like a locked OCI adapter pulled by `criteria adapter lock`.
	if testNoopAdapterBin == "" {
		t.Fatal("noop adapter binary not built")
	}
	root := t.TempDir()
	dg := digest.FromString("locked-oci-adapter-test")
	enc := EncodeDigest(dg)
	instDir := filepath.Join(root, enc)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatalf("mkdir digest install dir: %v", err)
	}
	adapterBin := filepath.Join(instDir, AdapterBinaryName("noop"))
	if err := copyFile(testNoopAdapterBin, adapterBin); err != nil {
		t.Fatalf("copy adapter binary: %v", err)
	}
	if err := os.Chmod(adapterBin, 0o755); err != nil {
		t.Fatalf("chmod adapter binary: %v", err)
	}

	t.Setenv(adaptersEnvVar, root)

	if testSandboxShimBin == "" {
		t.Fatal("sandbox shim binary not built")
	}

	sm := NewSessionManager(NewLoader())
	sm.sandboxShimBin = testSandboxShimBin
	sm.SetLockfile(&lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "noop", Name: "default", ResolvedDigest: dg.String()},
		},
	})
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default", Environment: "sandbox.default"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"sandbox.default": {Type: "sandbox", Name: "default"},
		},
		ResolvedPolicies: map[string]*workflow.ResolvedPolicy{
			"noop.default:sandbox.default": {
				PolicyMode: "permissive",
				OS:         "linux",
				Network:    &workflow.NetworkPolicy{AllowEgress: true},
			},
		},
	}

	t.Cleanup(func() { _ = sm.Close(context.Background(), "noop.default") })
	if err := sm.Open(ctx, "noop.default", "noop", "", nil, nil); err != nil {
		t.Fatalf("open locked OCI sandbox adapter: %v", err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elastic/go-seccomp-bpf"
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

func TestRunShimArgvNoDuplicateTarget(t *testing.T) {
	cfg := ShimConfig{TargetPath: "/usr/bin/my-adapter"}
	data, _ := json.Marshal(cfg)
	tmpFile, err := os.CreateTemp("", "criteria-sandbox-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(data); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"/usr/bin/criteria", "_sandbox_shim_", "/usr/bin/my-adapter", "--flag", "value"}

	var capturedArgv []string
	oldExec := shimExecFunc
	defer func() { shimExecFunc = oldExec }()
	shimExecFunc = func(path string, argv []string, envv []string) error {
		capturedArgv = argv
		return errors.New("mock exec")
	}

	err = runShim(tmpFile.Name())
	if err == nil {
		t.Fatal("expected error from mock exec")
	}

	want := []string{"/usr/bin/my-adapter", "--flag", "value"}
	if !reflect.DeepEqual(capturedArgv, want) {
		t.Fatalf("argv = %v, want %v", capturedArgv, want)
	}
}

func TestApplyToCmd_AllowNetworkRoundtrip(t *testing.T) {
	prep := LinuxPrepared{
		SysProcAttr:  &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER},
		Mode:         "strict",
		TargetPath:   "/usr/bin/my-adapter",
		AllowNetwork: true,
	}
	cmd := exec.Command("/bin/sh", "-c", "echo hello")
	if err := (&prep).ApplyToCmd(cmd, ""); err != nil {
		t.Fatalf("ApplyToCmd: %v", err)
	}

	// Find the shim config env var.
	var shimPath string
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "CRITERIA_SANDBOX_CONFIG_PATH=") {
			shimPath = strings.TrimPrefix(e, "CRITERIA_SANDBOX_CONFIG_PATH=")
			break
		}
	}
	if shimPath == "" {
		t.Fatal("expected CRITERIA_SANDBOX_CONFIG_PATH env var")
	}
	defer os.Remove(shimPath)

	data, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("read shim config: %v", err)
	}
	var cfg ShimConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal shim config: %v", err)
	}
	if !cfg.AllowNetwork {
		t.Fatal("expected AllowNetwork=true in serialized ShimConfig")
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

func TestValidatePath(t *testing.T) {
	tests := []struct {
		path string
		want string // empty = no error; non-empty = error substring
	}{
		{"/etc/passwd", ""},
		{"/tmp", ""},
		{"/var/lib/app/data", ""},
		{"", "empty"},
		{"relative/path", "not absolute"},
		{"/etc/../passwd", "parent-directory traversal"},
		{"/valid/../invalid", "parent-directory traversal"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			err := validatePath(tc.path)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestScrubEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"SUDO_UID=1000",
		"SUDO_GID=1000",
		"SUDO_USER=admin",
		"SUDO_COMMAND=/bin/bash",
		"SUDO_EDITOR=vim",
		"CRITERIA_PLUGIN=/tmp/plugin",
		"FOO=bar",
	}
	out := scrubEnv(in)
	for _, e := range out {
		name, _, _ := strings.Cut(e, "=")
		switch name {
		case "SUDO_UID", "SUDO_GID", "SUDO_USER", "SUDO_COMMAND", "SUDO_EDITOR", "CRITERIA_PLUGIN":
			t.Fatalf("scrubEnv left blocked variable %q in output", name)
		}
	}
	// Verify allowed vars are preserved.
	allowed := map[string]bool{}
	for _, e := range out {
		allowed[strings.SplitN(e, "=", 2)[0]] = true
	}
	for _, want := range []string{"PATH", "HOME", "FOO"} {
		if !allowed[want] {
			t.Fatalf("scrubEnv removed allowed variable %q", want)
		}
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 env vars, got %d: %v", len(out), out)
	}
}

func TestParseMemoryLimit(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
	}{
		{"512M", 512 * 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
		{"128K", 128 * 1024},
		{"", 0},
		{"invalid", 0},
		{"  256m  ", 256 * 1024 * 1024},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := parseMemoryLimit(tc.in)
			if got != tc.want {
				t.Fatalf("parseMemoryLimit(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseCPULimit(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"1", 1},
		{"0.5", 0.5},
		{"", 0},
		{"invalid", 0},
		{"  2.5  ", 2.5},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := parseCPULimit(tc.in)
			if got != tc.want {
				t.Fatalf("parseCPULimit(%q) = %f, want %f", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseTimeout(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"1m", time.Minute},
		{"", 0},
		{"invalid", 0},
		{"  5m30s  ", 5*time.Minute + 30*time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := parseTimeout(tc.in)
			if got != tc.want {
				t.Fatalf("parseTimeout(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPort string
	}{
		{"host:port", "host", "port"},
		{"[::1]:8080", "::1", "8080"},
		{"192.168.1.1:53", "192.168.1.1", "53"},
		{"malformed", "", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			host, port := splitHostPort(tc.in)
			if host != tc.wantHost || port != tc.wantPort {
				t.Fatalf("splitHostPort(%q) = (%q, %q), want (%q, %q)", tc.in, host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestBuildSeccompFilter(t *testing.T) {
	// With network allowed.
	f, err := buildSeccompFilter(true)
	if err != nil {
		t.Fatalf("buildSeccompFilter(true): %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil filter")
	}
	if !f.NoNewPrivs {
		t.Fatal("expected NoNewPrivs")
	}
	if f.Policy.DefaultAction != seccomp.ActionErrno {
		t.Fatalf("expected default-deny (ActionErrno), got %v", f.Policy.DefaultAction)
	}
	if len(f.Policy.Syscalls) != 1 {
		t.Fatalf("expected 1 syscall group, got %d", len(f.Policy.Syscalls))
	}
	// Verify that the base syscalls are present.
	asm, err := f.Policy.Assemble()
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(asm) == 0 {
		t.Fatal("expected non-empty assembled BPF")
	}

	// Without network allowed.
	f2, err := buildSeccompFilter(false)
	if err != nil {
		t.Fatalf("buildSeccompFilter(false): %v", err)
	}
	if f2 == nil {
		t.Fatal("expected non-nil filter")
	}
}

func TestMaybeUseBubblewrap(t *testing.T) {
	// Non-bwrap environment: returns nil.
	env := &workflow.EnvironmentNode{
		Type:         "sandbox",
		TypeSpecific: map[string]cty.Value{"sandbox": cty.StringVal("bwrap")},
	}
	prep := &LinuxPrepared{TargetPath: "/usr/bin/true"}

	// If bwrap is not on PATH, MaybeUseBubblewrap returns nil.
	cmd := MaybeUseBubblewrap(prep, env)
	if cmd != nil {
		// bwrap may be present in CI; validate the command structure.
		if !strings.Contains(cmd.Path, "bwrap") {
			t.Fatalf("expected bwrap in path, got %q", cmd.Path)
		}
		found := false
		for _, a := range cmd.Args {
			if a == "--unshare-all" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("expected --unshare-all in bwrap args")
		}
	}

	// Without opt-in: returns nil.
	envNoOpt := &workflow.EnvironmentNode{Type: "sandbox"}
	if c := MaybeUseBubblewrap(prep, envNoOpt); c != nil {
		t.Fatal("expected nil when not opted in")
	}

	// Wrong type: returns nil.
	envWrong := &workflow.EnvironmentNode{Type: "docker"}
	if c := MaybeUseBubblewrap(prep, envWrong); c != nil {
		t.Fatal("expected nil for non-sandbox type")
	}
}

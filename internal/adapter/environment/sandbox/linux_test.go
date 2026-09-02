//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elastic/go-seccomp-bpf"
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/sys/unix"

	"github.com/brokenbots/criteria/workflow"
)

func TestBuildSysProcAttr_DenyNetwork(t *testing.T) {
	attr := buildSysProcAttr(false)
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

func TestBuildSysProcAttr_AllowNetwork(t *testing.T) {
	attr := buildSysProcAttr(true)
	if attr == nil {
		t.Fatal("expected non-nil SysProcAttr")
	}
	if attr.Cloneflags&syscall.CLONE_NEWNET != 0 {
		t.Fatalf("CLONE_NEWNET set when AllowNetwork=true: Cloneflags = %#x", attr.Cloneflags)
	}
	want := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID |
		syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS)
	if attr.Cloneflags != want {
		t.Fatalf("Cloneflags = %#x, want %#x", attr.Cloneflags, want)
	}
}

func TestBuildRlimits(t *testing.T) {
	rs := buildRlimits(1024*1024, 5*time.Second, 0)
	if len(rs) == 0 {
		t.Fatal("expected rlimits")
	}
	foundAS := false
	foundCPU := false
	foundNPROC := false
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
		if r.Resource == unix.RLIMIT_NPROC {
			foundNPROC = true
			if r.Rlimit.Cur != defaultMaxThreads {
				t.Fatalf("RLIMIT_NPROC cur = %d, want default %d", r.Rlimit.Cur, defaultMaxThreads)
			}
		}
	}
	if !foundAS {
		t.Fatal("expected RLIMIT_AS")
	}
	if !foundCPU {
		t.Fatal("expected RLIMIT_CPU")
	}
	if !foundNPROC {
		t.Fatal("expected RLIMIT_NPROC")
	}
}

func TestBuildRlimits_ExplicitMaxThreads(t *testing.T) {
	rs := buildRlimits(0, 0, 64)
	foundNOFILE := false
	foundNPROC := false
	for _, r := range rs {
		if r.Resource == unix.RLIMIT_NOFILE {
			foundNOFILE = true
			if r.Rlimit.Cur != 1024 {
				t.Fatalf("RLIMIT_NOFILE cur = %d, want 1024", r.Rlimit.Cur)
			}
		}
		if r.Resource == unix.RLIMIT_NPROC {
			foundNPROC = true
			if r.Rlimit.Cur != 64 || r.Rlimit.Max != 64 {
				t.Fatalf("RLIMIT_NPROC = %d/%d, want 64/64", r.Rlimit.Cur, r.Rlimit.Max)
			}
		}
	}
	if !foundNOFILE {
		t.Fatal("expected RLIMIT_NOFILE")
	}
	if !foundNPROC {
		t.Fatal("expected RLIMIT_NPROC")
	}
}

func TestParseMaxThreads(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"whitespace", "   ", 0, false},
		{"zero", "0", 0, false},
		{"positive", "64", 64, false},
		{"large", "2048", 2048, false},
		{"negative", "-1", 0, true},
		{"non-numeric", "abc", 0, true},
		{"unit-suffix", "64K", 0, true},
		{"hex", "0x40", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMaxThreads(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMaxThreads(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseMaxThreads(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
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

func TestHandlerPrepare_MaxThreads_Default(t *testing.T) {
	caps := Probe()
	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()
	assertRlimitNproc(t, prep.Rlimits, defaultMaxThreads)
}

func TestHandlerPrepare_MaxThreads_Explicit(t *testing.T) {
	caps := Probe()
	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			TypeSpecific: map[string]cty.Value{
				"resources": cty.ObjectVal(map[string]cty.Value{
					"max_threads": cty.StringVal("1024"),
				}),
			},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()
	assertRlimitNproc(t, prep.Rlimits, 1024)
}

func TestHandlerPrepare_MaxThreads_Invalid_Strict(t *testing.T) {
	caps := Probe()
	_, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "strict",
			TypeSpecific: map[string]cty.Value{
				"resources": cty.ObjectVal(map[string]cty.Value{
					"max_threads": cty.StringVal("not-a-number"),
				}),
			},
		},
		Caps: caps,
	})
	if err == nil {
		t.Fatal("expected Prepare error for invalid max_threads in strict mode")
	}
}

func TestHandlerPrepare_MaxThreads_Invalid_Permissive(t *testing.T) {
	caps := Probe()
	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			TypeSpecific: map[string]cty.Value{
				"resources": cty.ObjectVal(map[string]cty.Value{
					"max_threads": cty.StringVal("not-a-number"),
				}),
			},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()
	assertRlimitNproc(t, prep.Rlimits, defaultMaxThreads)
}

func assertRlimitNproc(t *testing.T, rlimits []RlimitConfig, want uint64) {
	t.Helper()
	for _, r := range rlimits {
		if r.Resource == unix.RLIMIT_NPROC {
			if r.Rlimit.Cur != want || r.Rlimit.Max != want {
				t.Fatalf("RLIMIT_NPROC = %d/%d, want %d/%d", r.Rlimit.Cur, r.Rlimit.Max, want, want)
			}
			return
		}
	}
	t.Fatalf("RLIMIT_NPROC not found in rlimits")
}

func TestHandlerPrepare_NetworkAllowEgress_NoNewNet(t *testing.T) {
	caps := Probe()
	ctx := PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Network:    &workflow.NetworkPolicy{AllowEgress: true},
		},
		Caps: caps,
	}
	prep, err := Handler{}.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if prep.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr in prepared config")
	}
	if prep.SysProcAttr.Cloneflags&syscall.CLONE_NEWNET != 0 {
		t.Fatalf("CLONE_NEWNET set when AllowEgress=true: Cloneflags = %#x", prep.SysProcAttr.Cloneflags)
	}
	if !prep.AllowNetwork {
		t.Fatal("expected AllowNetwork=true")
	}
}

func TestHandlerPrepare_NetworkDenyEgress_HasNewNet(t *testing.T) {
	caps := Probe()
	ctx := PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Network:    &workflow.NetworkPolicy{AllowEgress: false},
		},
		Caps: caps,
	}
	prep, err := Handler{}.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if prep.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr in prepared config")
	}
	if prep.SysProcAttr.Cloneflags&syscall.CLONE_NEWNET == 0 {
		t.Fatalf("CLONE_NEWNET not set when AllowEgress=false: Cloneflags = %#x", prep.SysProcAttr.Cloneflags)
	}
	if prep.AllowNetwork {
		t.Fatal("expected AllowNetwork=false")
	}
}

func TestHandlerPrepare_NetworkTypeSpecific_Wildcard_NoNewNet(t *testing.T) {
	caps := Probe()
	ctx := PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			TypeSpecific: map[string]cty.Value{
				"network": cty.ObjectVal(map[string]cty.Value{
					"allow": cty.ListVal([]cty.Value{cty.StringVal("*")}),
				}),
			},
		},
		Caps: caps,
	}
	prep, err := Handler{}.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if prep.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr in prepared config")
	}
	if prep.SysProcAttr.Cloneflags&syscall.CLONE_NEWNET != 0 {
		t.Fatalf("CLONE_NEWNET set when network.allow=[\"*\"]: Cloneflags = %#x", prep.SysProcAttr.Cloneflags)
	}
	if !prep.AllowNetwork {
		t.Fatal("expected AllowNetwork=true")
	}
	if len(prep.NetPorts) != 0 {
		t.Fatalf("expected no NetPorts for wildcard, got %v", prep.NetPorts)
	}
}

func TestHandlerPrepare_NetworkTypeSpecific_Exact_PopulatesNetPorts(t *testing.T) {
	caps := Probe()
	ctx := PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			TypeSpecific: map[string]cty.Value{
				"network": cty.ObjectVal(map[string]cty.Value{
					"allow": cty.ListVal([]cty.Value{
						cty.StringVal("127.0.0.1:443"),
						cty.StringVal("[::1]:80"),
					}),
				}),
			},
		},
		Caps: caps,
	}
	prep, err := Handler{}.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if !prep.AllowNetwork {
		t.Fatal("expected AllowNetwork=true")
	}
	wantPorts := map[uint16]bool{443: true, 80: true}
	gotPorts := make(map[uint16]bool, len(prep.NetPorts))
	for _, p := range prep.NetPorts {
		gotPorts[p] = true
	}
	if len(gotPorts) != len(wantPorts) {
		t.Fatalf("expected NetPorts %v, got %v", wantPorts, prep.NetPorts)
	}
	for p := range wantPorts {
		if !gotPorts[p] {
			t.Fatalf("missing port %d in NetPorts %v", p, prep.NetPorts)
		}
	}
}

func TestHandlerPrepare_NetworkTypeSpecific_Deny_HasNewNet(t *testing.T) {
	caps := Probe()
	ctx := PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			TypeSpecific: map[string]cty.Value{
				"network": cty.ObjectVal(map[string]cty.Value{
					"allow": cty.ListValEmpty(cty.String),
				}),
			},
		},
		Caps: caps,
	}
	prep, err := Handler{}.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if prep.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr in prepared config")
	}
	if prep.SysProcAttr.Cloneflags&syscall.CLONE_NEWNET == 0 {
		t.Fatalf("CLONE_NEWNET not set when network.allow=[]: Cloneflags = %#x", prep.SysProcAttr.Cloneflags)
	}
	if prep.AllowNetwork {
		t.Fatal("expected AllowNetwork=false")
	}
}

func TestHandlerPrepare_ProcessExec_Strict_Rejects(t *testing.T) {
	caps := Probe()
	_, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "strict",
			Process:    &workflow.ProcessPolicy{Exec: []string{"/bin/zsh"}},
		},
		Caps: caps,
	})
	if err == nil {
		t.Fatal("expected Prepare error for process.exec in strict mode on Linux")
	}
	if !strings.Contains(err.Error(), "cannot be enforced on Linux") {
		t.Errorf("expected Linux enforcement error, got: %v", err)
	}
}

func TestHandlerPrepare_ProcessExec_Permissive_RejectsExactList(t *testing.T) {
	caps := Probe()
	_, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Process:    &workflow.ProcessPolicy{Exec: []string{"/bin/zsh", "/usr/bin/fish"}},
		},
		Caps: caps,
	})
	if err == nil {
		t.Fatal("expected Prepare error for exact process.exec allow-list in permissive mode on Linux")
	}
	if !strings.Contains(err.Error(), "cannot be enforced on Linux") {
		t.Errorf("expected Linux enforcement error, got: %v", err)
	}
}

func TestHandlerPrepare_ProcessExec_Wildcard_Strict(t *testing.T) {
	caps := Probe()
	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "strict",
			Process:    &workflow.ProcessPolicy{Exec: []string{"*"}},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()
	if !prep.ProcessExecWildcard {
		t.Fatal("expected ProcessExecWildcard=true for process.exec=[\"*\"]")
	}
}

func TestHandlerPrepare_ProcessExec_Wildcard_Permissive(t *testing.T) {
	caps := Probe()
	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Process:    &workflow.ProcessPolicy{Exec: []string{"*"}},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()
	if !prep.ProcessExecWildcard {
		t.Fatal("expected ProcessExecWildcard=true for process.exec=[\"*\"]")
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
	// Network egress denial is enforced by CLONE_NEWNET inside a user namespace;
	// the base seccomp allow-list now permits the local socket syscalls required
	// by go-plugin, so the connect must fail in the new network namespace instead.
	runCmd.SysProcAttr = buildSysProcAttr(false)
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

func TestShimIntegration_AllowNetwork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	caps := Probe()
	if !caps.UserNamespaces {
		t.Skip("user namespaces not available")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	fixtureDir := filepath.Join(filepath.Dir(testFile), "testfixture", "http")

	dir := t.TempDir()
	helper := filepath.Join(dir, "http_helper")
	buildCmd := exec.Command("go", "build", "-o", helper, fixtureDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, out)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Network:    &workflow.NetworkPolicy{AllowEgress: true},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()

	cfg := ShimConfig{
		TargetPath:   helper,
		Mode:         prep.Mode,
		ReadPaths:    prep.ReadPaths,
		WritePaths:   prep.WritePaths,
		NetPorts:     prep.NetPorts,
		AllowNetwork: prep.AllowNetwork,
		Seccomp:      false, // isolate namespace behavior; seccomp tested separately
		Rlimits:      prep.Rlimits,
	}
	tmpFile, err := os.CreateTemp(dir, "criteria-sandbox-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	if err := json.NewEncoder(tmpFile).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	runCmd := exec.Command(helper, server.URL+"/")
	runCmd.SysProcAttr = prep.SysProcAttr
	runCmd.Env = append(os.Environ(), "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	outStr := string(out)
	t.Logf("helper output:\n%s", outStr)
	if !strings.Contains(outStr, "HTTP_OK") {
		t.Fatalf("expected HTTP_OK with AllowNetwork=true, got: %s", outStr)
	}
}

func TestShimIntegration_DenyNetwork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	caps := Probe()
	if !caps.UserNamespaces {
		t.Skip("user namespaces not available")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	fixtureDir := filepath.Join(filepath.Dir(testFile), "testfixture", "http")

	dir := t.TempDir()
	helper := filepath.Join(dir, "http_helper")
	buildCmd := exec.Command("go", "build", "-o", helper, fixtureDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, out)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Network:    &workflow.NetworkPolicy{AllowEgress: false},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()

	cfg := ShimConfig{
		TargetPath:   helper,
		Mode:         prep.Mode,
		ReadPaths:    prep.ReadPaths,
		WritePaths:   prep.WritePaths,
		NetPorts:     prep.NetPorts,
		AllowNetwork: prep.AllowNetwork,
		Seccomp:      false,
		Rlimits:      prep.Rlimits,
	}
	tmpFile, err := os.CreateTemp(dir, "criteria-sandbox-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	if err := json.NewEncoder(tmpFile).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	runCmd := exec.Command(helper, server.URL+"/")
	runCmd.SysProcAttr = prep.SysProcAttr
	runCmd.Env = append(os.Environ(), "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	outStr := string(out)
	t.Logf("helper output:\n%s", outStr)
	if !strings.Contains(outStr, "HTTP_FAIL") {
		t.Fatalf("expected HTTP_FAIL with AllowNetwork=false, got: %s", outStr)
	}
	if !strings.Contains(outStr, "network is unreachable") && !strings.Contains(outStr, "connection refused") {
		t.Fatalf("expected network-unreachable/connection-failed error, got: %s", outStr)
	}
}

// TestShimIntegration_CurlAllowNetwork verifies that a real libc resolver
// path (getent) and curl can both resolve a hostname and fetch HTTP inside
// a network-allowed Linux sandbox with seccomp enabled. This exercises the
// sendmmsg/recvmmsg batched socket syscalls used by glibc's threaded
// resolver, which were previously blocked by the default-deny filter.
func TestShimIntegration_CurlAllowNetwork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	caps := Probe()
	if !caps.UserNamespaces {
		t.Skip("user namespaces not available")
	}
	if !caps.Seccomp {
		t.Skip("seccomp not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	if _, err := exec.LookPath("getent"); err != nil {
		t.Skip("getent not on PATH")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	fixtureDir := filepath.Join(filepath.Dir(testFile), "testfixture", "curl")

	dir := t.TempDir()
	helper := filepath.Join(dir, "curl_helper")
	buildCmd := exec.Command("go", "build", "-o", helper, fixtureDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, out)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Network:    &workflow.NetworkPolicy{AllowEgress: true},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()

	cfg := ShimConfig{
		TargetPath: helper,
		Mode:       prep.Mode,
		// Broad system paths are required so the dynamically linked curl
		// binary, glibc resolver, dynamic loader and /dev devices can load.
		// No NetPorts are configured; egress is controlled by seccomp and by
		// sharing the host network namespace when AllowEgress is true.
		ReadPaths: []string{
			"/usr",
			"/lib",
			"/lib64",
			"/etc",
			"/proc",
			"/dev",
		},
		WritePaths:   []string{"/tmp", "/dev"},
		NetPorts:     []uint16{},
		AllowNetwork: prep.AllowNetwork,
		Seccomp:      true,
		Rlimits:      prep.Rlimits,
	}
	tmpFile, err := os.CreateTemp(dir, "criteria-sandbox-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	if err := json.NewEncoder(tmpFile).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	runCmd := exec.Command(helper, server.URL+"/")
	runCmd.SysProcAttr = prep.SysProcAttr
	runCmd.Env = append(os.Environ(), "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	outStr := string(out)
	t.Logf("helper output:\n%s", outStr)
	if !strings.Contains(outStr, "GETENT_OK") {
		t.Fatalf("expected GETENT_OK with AllowNetwork=true, got: %s", outStr)
	}
	if !strings.Contains(outStr, "CURL_OK") {
		t.Fatalf("expected CURL_OK with AllowNetwork=true, got: %s", outStr)
	}
}

// TestShimIntegration_CurlDenyNetwork verifies that a network-denied sandbox
// still blocks external egress. curl/getent are allowed to load but the
// new network namespace prevents any connect/DNS path from succeeding.
func TestShimIntegration_CurlDenyNetwork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	caps := Probe()
	if !caps.UserNamespaces {
		t.Skip("user namespaces not available")
	}
	if !caps.Seccomp {
		t.Skip("seccomp not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	if _, err := exec.LookPath("getent"); err != nil {
		t.Skip("getent not on PATH")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	fixtureDir := filepath.Join(filepath.Dir(testFile), "testfixture", "curl")

	dir := t.TempDir()
	helper := filepath.Join(dir, "curl_helper")
	buildCmd := exec.Command("go", "build", "-o", helper, fixtureDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, out)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
			Network:    &workflow.NetworkPolicy{AllowEgress: false},
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()

	cfg := ShimConfig{
		TargetPath: helper,
		Mode:       prep.Mode,
		ReadPaths: []string{
			"/usr",
			"/lib",
			"/lib64",
			"/etc",
			"/proc",
			"/dev",
		},
		WritePaths:   []string{"/tmp", "/dev"},
		NetPorts:     []uint16{},
		AllowNetwork: prep.AllowNetwork,
		Seccomp:      true,
		Rlimits:      prep.Rlimits,
	}
	tmpFile, err := os.CreateTemp(dir, "criteria-sandbox-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	if err := json.NewEncoder(tmpFile).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	runCmd := exec.Command(helper, server.URL+"/")
	runCmd.SysProcAttr = prep.SysProcAttr
	runCmd.Env = append(os.Environ(), "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	outStr := string(out)
	t.Logf("helper output:\n%s", outStr)
	if strings.Contains(outStr, "GETENT_OK") {
		t.Fatalf("expected GETENT to fail with AllowNetwork=false, got: %s", outStr)
	}
	if strings.Contains(outStr, "CURL_OK") {
		t.Fatalf("expected CURL to fail with AllowNetwork=false, got: %s", outStr)
	}
}

// TestMaxThreads_PthreadSurrogate verifies that RLIMIT_NPROC plumbing
// controls how many pthreads can be created inside the sandbox. The
// default policy and an explicit max_threads="2048" must both allow at
// least 100 threads, while max_threads="64" must cap the surrogate.
func TestMaxThreads_PthreadSurrogate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	caps := Probe()
	if !caps.UserNamespaces {
		t.Skip("user namespaces not available")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	fixtureDir := filepath.Join(filepath.Dir(testFile), "testfixture", "pthread")

	dir := t.TempDir()
	helper := filepath.Join(dir, "pthread_helper")
	buildCmd := exec.Command("go", "build", "-o", helper, fixtureDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, out)
	}

	cases := []struct {
		name       string
		maxThreads string
		minWant    int
		maxWant    int // inclusive upper bound; -1 means no upper bound
	}{
		{"default", "", 100, -1},
		{"explicit-2048", "2048", 100, -1},
		{"explicit-64", "64", 0, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := &workflow.ResolvedPolicy{
				OS:         "linux",
				PolicyMode: "permissive",
			}
			if tc.maxThreads != "" {
				policy.TypeSpecific = map[string]cty.Value{
					"resources": cty.ObjectVal(map[string]cty.Value{
						"max_threads": cty.StringVal(tc.maxThreads),
					}),
				}
			}

			prep, err := Handler{}.Prepare(PrepareContext{
				Policy: policy,
				Caps:   caps,
			})
			if err != nil {
				t.Fatalf("Prepare failed: %v", err)
			}
			defer prep.Cleanup()

			cfg := ShimConfig{
				TargetPath:   helper,
				Mode:         prep.Mode,
				ReadPaths:    prep.ReadPaths,
				WritePaths:   prep.WritePaths,
				NetPorts:     prep.NetPorts,
				AllowNetwork: prep.AllowNetwork,
				Seccomp:      false, // isolate rlimit behavior from seccomp
				Rlimits:      prep.Rlimits,
			}
			tmpFile, err := os.CreateTemp(dir, "criteria-sandbox-*.json")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(tmpFile.Name())
			if err := json.NewEncoder(tmpFile).Encode(cfg); err != nil {
				t.Fatal(err)
			}
			if err := tmpFile.Close(); err != nil {
				t.Fatal(err)
			}

			runCmd := exec.Command(helper, "100")
			runCmd.SysProcAttr = prep.SysProcAttr
			runCmd.Env = append(os.Environ(), "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())
			out, err := runCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper failed: %v\n%s", err, out)
			}
			outStr := string(out)
			t.Logf("helper output:\n%s", outStr)

			count, ok := parseThreadCount(outStr)
			if !ok {
				t.Fatalf("expected THREADS_OK count=N in output, got: %s", outStr)
			}
			if count < tc.minWant {
				t.Fatalf("thread count %d < min want %d", count, tc.minWant)
			}
			if tc.maxWant >= 0 && count > tc.maxWant {
				t.Fatalf("thread count %d > max want %d", count, tc.maxWant)
			}
		})
	}
}

func parseThreadCount(out string) (int, bool) {
	const prefix = "THREADS_OK count="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			n, err := strconv.Atoi(strings.TrimPrefix(line, prefix))
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// TestMaxThreads_CopilotRegression runs the real GitHub Copilot CLI inside
// the default sandbox and verifies that it does not abort with a thread-spawn
// panic / SIGABRT. The default max_threads policy (2048) must provide
// enough headroom for the Copilot native runtime. The test supplies only a
// dummy token, so an auth-related error is expected.
func TestMaxThreads_CopilotRegression(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	caps := Probe()
	if !caps.UserNamespaces {
		t.Skip("user namespaces not available")
	}
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skip("copilot not on PATH")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	fixtureDir := filepath.Join(filepath.Dir(testFile), "testfixture", "copilot")

	dir := t.TempDir()
	helper := filepath.Join(dir, "copilot_helper")
	buildCmd := exec.Command("go", "build", "-o", helper, fixtureDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, out)
	}

	prep, err := Handler{}.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			OS:         "linux",
			PolicyMode: "permissive",
		},
		Caps: caps,
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer prep.Cleanup()

	assertRlimitNproc(t, prep.Rlimits, defaultMaxThreads)

	cfg := ShimConfig{
		TargetPath:   helper,
		Mode:         prep.Mode,
		ReadPaths:    prep.ReadPaths,
		WritePaths:   prep.WritePaths,
		NetPorts:     prep.NetPorts,
		AllowNetwork: prep.AllowNetwork,
		Seccomp:      false, // isolate the max_threads behavior from seccomp
		Rlimits:      prep.Rlimits,
	}
	tmpFile, err := os.CreateTemp(dir, "criteria-sandbox-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	if err := json.NewEncoder(tmpFile).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	// Use a prompt that exercises the native runtime and reaches the auth
	// layer with the dummy token.
	runCmd := exec.Command(helper, "-p", "run echo hello")
	runCmd.SysProcAttr = prep.SysProcAttr
	runCmd.Env = append(os.Environ(),
		"CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name(),
		"GITHUB_TOKEN=dummy",
	)
	out, err := runCmd.CombinedOutput()
	outStr := string(out)
	t.Logf("copilot output:\n%s", outStr)

	// The process must not die from thread-exhaustion / SIGABRT.
	if isSignalExit(err, syscall.SIGABRT) {
		t.Fatalf("copilot exited via SIGABRT (thread exhaustion) under default sandbox: %v\n%s", err, outStr)
	}
	if strings.Contains(outStr, "failed to spawn thread") {
		t.Fatalf("copilot panicked with thread-spawn failure under default sandbox:\n%s", outStr)
	}
	if strings.Contains(outStr, "SIGABRT") {
		t.Fatalf("copilot was terminated by SIGABRT under default sandbox:\n%s", outStr)
	}

	// With only a dummy token we expect an auth-related exit, not success.
	if !strings.Contains(outStr, "Authentication") &&
		!strings.Contains(outStr, "No authentication information found") &&
		!strings.Contains(outStr, "token") &&
		!strings.Contains(outStr, "authenticate") {
		t.Fatalf("expected auth-related output from copilot, got:\n%s", outStr)
	}
}

// isSignalExit reports whether err is an *exec.ExitError caused by the
// given signal.
func isSignalExit(err error, sig syscall.Signal) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return ws.Signaled() && ws.Signal() == sig
		}
	}
	return false
}

func TestApplyToCmd_EnvScrubBlockedVarsAbsent(t *testing.T) {
	// Verify that when cmd.Env is nil (the default from exec.Command),
	// ApplyToCmd seeds it from os.Environ(), scrubs blocked vars, and
	// the result contains no SUDO_* or CRITERIA_PLUGIN entries.
	prep := LinuxPrepared{
		SysProcAttr: &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER},
		Mode:        "strict",
		TargetPath:  "/usr/bin/true",
	}
	cmd := exec.Command("/bin/sh", "-c", "echo hello")
	if cmd.Env != nil {
		t.Fatal("expected cmd.Env to be nil before ApplyToCmd")
	}
	if err := prep.ApplyToCmd(cmd, ""); err != nil {
		t.Fatalf("ApplyToCmd: %v", err)
	}
	defer prep.Cleanup()

	for _, e := range cmd.Env {
		name, _, _ := strings.Cut(e, "=")
		switch name {
		case "SUDO_UID", "SUDO_GID", "SUDO_USER", "SUDO_COMMAND", "SUDO_EDITOR", "CRITERIA_PLUGIN":
			t.Fatalf("blocked env var %q present after ApplyToCmd", name)
		}
	}
	// Verify the shim config path env var is present.
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

func TestCleanup_CgroupFDAndTempFile(t *testing.T) {
	prep := LinuxPrepared{
		SysProcAttr: &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER,
			UseCgroupFD: true,
			CgroupFD:    999, // synthetic fd; close will fail but we check it is attempted
		},
		ShimConfigPath: "/tmp/criteria-sandbox-fake.json",
	}
	// Create the fake temp file so removal succeeds.
	f, err := os.Create(prep.ShimConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := prep.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if prep.SysProcAttr.CgroupFD != 0 {
		t.Fatalf("expected CgroupFD zeroed, got %d", prep.SysProcAttr.CgroupFD)
	}
	if prep.SysProcAttr.UseCgroupFD {
		t.Fatal("expected UseCgroupFD false after cleanup")
	}
	if prep.ShimConfigPath != "" {
		t.Fatal("expected ShimConfigPath cleared after cleanup")
	}
	if _, err := os.Stat("/tmp/criteria-sandbox-fake.json"); !os.IsNotExist(err) {
		t.Fatal("expected temp config file to be removed")
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

func TestBuildSeccompFilter_NetworkSyscalls(t *testing.T) {
	allowed, err := buildSeccompFilter(true)
	if err != nil {
		t.Fatalf("buildSeccompFilter(true): %v", err)
	}
	for _, name := range []string{"sendmmsg", "recvmmsg"} {
		if !syscallsContains(allowed.Policy.Syscalls[0].Names, name) {
			t.Fatalf("expected %s in network-allowed seccomp allow-list", name)
		}
	}

	denied, err := buildSeccompFilter(false)
	if err != nil {
		t.Fatalf("buildSeccompFilter(false): %v", err)
	}
	for _, name := range []string{"sendmmsg", "recvmmsg"} {
		if syscallsContains(denied.Policy.Syscalls[0].Names, name) {
			t.Fatalf("expected %s to be absent from network-denied seccomp allow-list", name)
		}
	}
}

func syscallsContains(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func TestMaybeUseBubblewrap(t *testing.T) {
	// Non-bwrap environment: returns nil.
	env := &workflow.EnvironmentNode{
		Type:         "sandbox",
		TypeSpecific: map[string]cty.Value{"sandbox": cty.StringVal("bwrap")},
	}
	prep := &LinuxPrepared{TargetPath: "/usr/bin/true"}

	// If bwrap is not on PATH, MaybeUseBubblewrap returns nil.
	cmd := MaybeUseBubblewrap(prep, env, "")
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
	if c := MaybeUseBubblewrap(prep, envNoOpt, ""); c != nil {
		t.Fatal("expected nil when not opted in")
	}

	// Wrong type: returns nil.
	envWrong := &workflow.EnvironmentNode{Type: "docker"}
	if c := MaybeUseBubblewrap(prep, envWrong, ""); c != nil {
		t.Fatal("expected nil for non-sandbox type")
	}
}

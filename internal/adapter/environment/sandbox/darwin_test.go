//go:build darwin

package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

func TestProfile_Render(t *testing.T) {
	tests := []struct {
		name     string
		profile  Profile
		want     []string
		wantNot  []string
	}{
		{
			name: "basic default deny",
			profile: Profile{
				AllowExec:   []string{"/usr/local/bin/adapter"},
				DefaultDeny: true,
			},
			want: []string{
				"; criteria sandbox profile version 1",
				"(version 1)",
				"(deny default)",
				"(allow process-fork)",
				"(allow process-exec",
				"(literal \"/usr/local/bin/adapter\")",
			},
		},
		{
			name: "file read and write",
			profile: Profile{
				AllowFileReads:    []string{"/tmp/allowed"},
				AllowFileWrites:   []string{"/tmp/out"},
				AllowExec:         []string{"/usr/local/bin/adapter"},
				AllowNetworkHosts: []string{"127.0.0.1:443"},
				DefaultDeny:       true,
			},
			want: []string{
				"(allow file-read*",
				"(literal \"/tmp/allowed\")",
				"(allow file-write*",
				"(literal \"/tmp/out\")",
				"(allow network-outbound",
				"(remote ip \"127.0.0.1:443\")",
			},
		},
		{
			name: "block sysctl and mach",
			profile: Profile{
				BlockSysctl:     true,
				BlockMachLookup: true,
				DefaultDeny:     true,
			},
			want: []string{
				"(deny system-kext-load)",
				"(deny mach-lookup)",
			},
		},
		{
			name: "deny exec when empty",
			profile: Profile{
				DefaultDeny: true,
			},
			want:    []string{"(deny process-exec)"},
			wantNot: []string{"(allow process-exec"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.profile.Render()
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("rendered profile missing %q:\n%s", s, got)
				}
			}
			for _, s := range tc.wantNot {
				if strings.Contains(got, s) {
					t.Errorf("rendered profile should not contain %q:\n%s", s, got)
				}
			}
		})
	}
}

func TestFromPolicy(t *testing.T) {
	tests := []struct {
		name          string
		policy        workflow.ResolvedPolicy
		wantReads     []string
		wantWrites    []string
		wantNetwork   []string
		wantBlockSys  bool
		wantBlockMach bool
	}{
		{
			name: "filesystem read and write directories",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"filesystem": cty.ObjectVal(map[string]cty.Value{
						"read":  cty.TupleVal([]cty.Value{cty.StringVal("/tmp/rw"), cty.StringVal("/etc/readonly")}),
						"write": cty.TupleVal([]cty.Value{cty.StringVal("/tmp/rw")}),
					}),
				},
			},
			wantReads:  []string{"/tmp/rw", "/etc/readonly"},
			wantWrites: []string{"/tmp/rw"},
		},
		{
			name: "filesystem rules",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"filesystem": cty.ObjectVal(map[string]cty.Value{
						"read":       cty.TupleVal([]cty.Value{cty.StringVal("/etc/config.yaml")}),
						"write":      cty.TupleVal([]cty.Value{cty.StringVal("/tmp/log.txt")}),
						"read_write": cty.TupleVal([]cty.Value{cty.StringVal("/data/shared")}),
					}),
				},
			},
			wantReads:  []string{"/etc/config.yaml", "/data/shared"},
			wantWrites: []string{"/tmp/log.txt", "/data/shared"},
		},
		{
			name: "network allowed hosts",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"network": cty.ObjectVal(map[string]cty.Value{
						"allow": cty.TupleVal([]cty.Value{cty.StringVal("127.0.0.1:443"), cty.StringVal("::1:80")}),
					}),
				},
			},
			wantNetwork: []string{"127.0.0.1:443", "::1:80"},
		},
		{
			name: "block sysctl and mach",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"filesystem": cty.ObjectVal(map[string]cty.Value{
						"block_sysctl":      cty.BoolVal(true),
						"block_mach_lookup": cty.BoolVal(true),
					}),
				},
			},
			wantBlockSys:  true,
			wantBlockMach: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prof := FromPolicy(tc.policy, "")
			for _, p := range tc.wantReads {
				if !sliceContains(prof.AllowFileReads, p) {
					t.Errorf("AllowFileReads missing %q, got %v", p, prof.AllowFileReads)
				}
			}
			for _, p := range tc.wantWrites {
				if !sliceContains(prof.AllowFileWrites, p) {
					t.Errorf("AllowFileWrites missing %q, got %v", p, prof.AllowFileWrites)
				}
			}
			for _, h := range tc.wantNetwork {
				if !sliceContains(prof.AllowNetworkHosts, h) {
					t.Errorf("AllowNetworkHosts missing %q, got %v", h, prof.AllowNetworkHosts)
				}
			}
			if prof.BlockSysctl != tc.wantBlockSys {
				t.Errorf("BlockSysctl=%v, want %v", prof.BlockSysctl, tc.wantBlockSys)
			}
			if prof.BlockMachLookup != tc.wantBlockMach {
				t.Errorf("BlockMachLookup=%v, want %v", prof.BlockMachLookup, tc.wantBlockMach)
			}
		})
	}
}

func sliceContains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func TestProbe_SandboxExec(t *testing.T) {
	// By default Probe should return true on macOS (sandbox-exec is present
	// on stock macOS installs). We also verify the test-hook override path.
	ResetProbeCache()
	probeTestHook = func() Capabilities {
		return Capabilities{SandboxExec: false}
	}
	caps := Probe()
	if caps.SandboxExec {
		t.Error("expected SandboxExec=false via test hook")
	}
	if m := caps.Missing(); len(m) != 1 || m[0] != "sandbox_exec" {
		t.Errorf("expected Missing=[sandbox_exec], got %v", m)
	}
	if s := caps.String(); !strings.Contains(s, "sandbox_exec=false") {
		t.Errorf("expected String to contain sandbox_exec=false, got %s", s)
	}
	probeTestHook = nil
	ResetProbeCache()
}

func TestPrepare_Strict_MissingSandboxExec(t *testing.T) {
	probeTestHook = func() Capabilities { return Capabilities{SandboxExec: false} }
	defer func() { probeTestHook = nil; ResetProbeCache() }()

	h := Handler{}
	_, err := h.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{PolicyMode: "strict"},
		Caps:   Probe(),
	})
	if err == nil {
		t.Fatal("expected error in strict mode when sandbox-exec is missing")
	}
	if !strings.Contains(err.Error(), "sandbox-exec not available") {
		t.Errorf("expected 'sandbox-exec not available' in error, got: %v", err)
	}
}

func TestPrepare_Permissive_MissingSandboxExec(t *testing.T) {
	probeTestHook = func() Capabilities { return Capabilities{SandboxExec: false} }
	defer func() { probeTestHook = nil; ResetProbeCache() }()

	h := Handler{}
	prep, err := h.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{PolicyMode: "permissive"},
		Caps:   Probe(),
	})
	if err != nil {
		t.Fatalf("unexpected error in permissive mode: %v", err)
	}
	if !prep.fallback {
		t.Error("expected fallback=true when sandbox-exec is missing in permissive mode")
	}
}

func TestApplyToCmd_Fallback(t *testing.T) {
	cmd := exec.Command("/usr/local/bin/adapter", "--flag")
	cmd.Env = []string{"HOME=/Users/test", "PATH=/usr/local/bin::./relative", "SECRET=shh"}
	prep := LinuxPrepared{fallback: true, Mode: "permissive"}
	if err := prep.ApplyToCmd(cmd, "/usr/local/bin/criteria"); err != nil {
		t.Fatalf("ApplyToCmd fallback error: %v", err)
	}
	if cmd.Path != "/usr/local/bin/adapter" {
		// In fallback mode the adapter binary itself is unchanged.
		t.Errorf("cmd.Path=%q, want unchanged adapter path", cmd.Path)
	}
	if !sliceContains(cmd.Env, "HOME=/Users/test") {
		t.Error("expected HOME kept in env")
	}
	if sliceContains(cmd.Env, "SECRET=shh") {
		t.Error("expected SECRET scrubbed from env")
	}
	var pathEnv string
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "PATH=") {
			pathEnv = e
			break
		}
	}
	if strings.Contains(pathEnv, "./relative") || strings.Contains(pathEnv, "::") {
		t.Errorf("PATH should not contain relative/empty entries: %s", pathEnv)
	}
}

func TestSandboxProfile_Integration(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)

	allowedDir := t.TempDir()
	blockedDir := t.TempDir()

	allowedFile := filepath.Join(allowedDir, "allowed.txt")
	blockedFile := filepath.Join(blockedDir, "blocked.txt")
	os.WriteFile(allowedFile, []byte("ok"), 0o644)
	os.WriteFile(blockedFile, []byte("no"), 0o644)

	prof := Profile{
		DefaultDeny:     true,
		AllowExec:       []string{helper},
		AllowFileReads:  []string{allowedFile, helper, filepath.Dir(helper)},
		AllowNetworkHosts: []string{"127.0.0.1:55555"},
	}

	tmpPath, err := writeProfile(prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(tmpPath)

	// Allowed read
	out, err := runUnderSandbox(t, helper, tmpPath, "read", allowedFile)
	if err != nil {
		t.Fatalf("allowed read failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "READ_OK") {
		t.Errorf("allowed read: expected READ_OK, got:\n%s", out)
	}

	// Blocked read
	out, err = runUnderSandbox(t, helper, tmpPath, "read", blockedFile)
	if err == nil {
		// sandbox-exec may return non-zero when the child exits with a
		// non-zero code, or may succeed and let the child print the error.
		if !strings.Contains(out, "READ_FAIL") {
			t.Errorf("blocked read: expected READ_FAIL or sandbox-exec error, got:\n%s", out)
		}
	} else {
		if !strings.Contains(out, "READ_FAIL") && !strings.Contains(out, "Operation not permitted") {
			t.Errorf("blocked read: unexpected output:\n%s", out)
		}
	}

	// Blocked network (connect to disallowed port)
	out, err = runUnderSandbox(t, helper, tmpPath, "connect", "127.0.0.1", "80")
	if err == nil {
		if !strings.Contains(out, "CONNECT_FAIL") {
			t.Errorf("blocked network: expected CONNECT_FAIL, got:\n%s", out)
		}
	} else {
		if !strings.Contains(out, "CONNECT_FAIL") && !strings.Contains(out, "deny") {
			t.Errorf("blocked network: unexpected output:\n%s", out)
		}
	}
}

func runUnderSandbox(t *testing.T, helperPath, profilePath string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/usr/bin/sandbox-exec", append([]string{"-f", profilePath, helperPath}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func buildDarwinTestHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "helper")
	src := `package main
import (
	"fmt"
	"net"
	"os"
)
func main() {
	if len(os.Args) < 2 { os.Exit(1) }
	switch os.Args[1] {
	case "read":
		if len(os.Args) < 3 { os.Exit(1) }
		f, err := os.Open(os.Args[2])
		if err != nil { fmt.Println("READ_FAIL:", err); os.Exit(0) }
		f.Close(); fmt.Println("READ_OK")
	case "connect":
		if len(os.Args) < 4 { os.Exit(1) }
		conn, err := net.Dial("tcp", net.JoinHostPort(os.Args[2], os.Args[3]))
		if err != nil { fmt.Println("CONNECT_FAIL:", err); os.Exit(0) }
		conn.Close(); fmt.Println("CONNECT_OK")
	default:
		os.Exit(1)
	}
}
`
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write helper source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", bin, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	return bin
}

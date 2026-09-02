//go:build darwin

package sandbox

import (
	"bytes"
	"net"
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
		name    string
		profile Profile
		want    []string
		wantNot []string
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
			// Paths are emitted symlink-resolved (/tmp -> /private/tmp) and the
			// network host is mapped to a macOS-accepted form (literal IPs are
			// rejected by sandbox-exec; 127.0.0.1 is loopback -> localhost).
			want: []string{
				"(import \"system.sb\")",
				"(allow file-read*",
				"(literal \"/private/tmp/allowed\")",
				"(allow file-write*",
				"(literal \"/private/tmp/out\")",
				"(allow network-outbound",
				"(remote ip \"localhost:443\")",
			},
		},
		{
			name: "block kext and mach",
			profile: Profile{
				BlockKextLoad:   true,
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
		{
			name: "local adapter network-bind",
			profile: Profile{
				DefaultDeny:      true,
				AllowExec:        []string{"/usr/local/bin/adapter"},
				AllowFileWrites:  []string{"/tmp"},
				AllowNetworkBind: true,
			},
			want: []string{
				"(allow file-write*",
				"(subpath \"/private/tmp\")",
				"(allow network-bind)",
			},
		},
		{
			name: "no network-bind without local adapter",
			profile: Profile{
				DefaultDeny:      true,
				AllowFileWrites:  []string{"/tmp"},
				AllowNetworkBind: false,
			},
			want:    []string{"(allow file-write*"},
			wantNot: []string{"(allow network-bind)"},
		},
		{
			name: "wildcard network-outbound is unrestricted",
			profile: Profile{
				DefaultDeny:         true,
				AllowNetworkWildcard: true,
			},
			want: []string{
				"(allow network-outbound)",
			},
			wantNot: []string{
				"(remote ip",
			},
		},
		{
			name: "exact network-outbound keeps remote ip filter",
			profile: Profile{
				DefaultDeny:      true,
				AllowNetworkHosts: []string{"127.0.0.1:443"},
			},
			want: []string{
				"(allow network-outbound",
				"(remote ip \"localhost:443\")",
			},
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
		name            string
		policy          workflow.ResolvedPolicy
		adapterBinary   string
		wantReads       []string
		wantWrites      []string
		wantExec        []string
		wantNetwork          []string
		wantNetworkWildcard  bool
		wantNetworkBind      bool
		wantWarnings         int
		wantBlockKext        bool
		wantBlockMach        bool
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
			name: "adapter binary pre-populated",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"filesystem": cty.ObjectVal(map[string]cty.Value{
						"read": cty.TupleVal([]cty.Value{cty.StringVal("/etc/config.yaml")}),
					}),
				},
			},
			adapterBinary:   "/usr/local/bin/adapter",
			wantReads:       []string{"/etc/config.yaml", "/usr/local/bin/adapter", "/usr/local/bin"},
			wantExec:        []string{"/usr/local/bin/adapter"},
			wantNetworkBind: true,
		},
		{
			name: "process exec allow-list applied",
			policy: workflow.ResolvedPolicy{
				Process: &workflow.ProcessPolicy{
					Exec: []string{"/bin/zsh", "/usr/bin/git"},
				},
				TypeSpecific: map[string]cty.Value{
					"filesystem": cty.ObjectVal(map[string]cty.Value{
						"write": cty.TupleVal([]cty.Value{cty.StringVal("/tmp")}),
					}),
				},
			},
			adapterBinary:   "/usr/local/bin/criteria-adapter-shell",
			wantReads:       []string{"/usr/local/bin/criteria-adapter-shell", "/usr/local/bin", "/bin/zsh", "/usr/bin/git"},
			wantExec:        []string{"/usr/local/bin/criteria-adapter-shell", "/bin/zsh", "/usr/bin/git"},
			wantNetworkBind: true,
		},
		{
			name: "no process policy means no child executables",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"filesystem": cty.ObjectVal(map[string]cty.Value{
						"write": cty.TupleVal([]cty.Value{cty.StringVal("/tmp")}),
					}),
				},
			},
			adapterBinary:   "/usr/local/bin/criteria-adapter-noop",
			wantReads:       []string{"/usr/local/bin/criteria-adapter-noop", "/usr/local/bin"},
			wantExec:        []string{"/usr/local/bin/criteria-adapter-noop"},
			wantNetworkBind: true,
		},
		{
			name: "network allowed hosts",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"network": cty.ObjectVal(map[string]cty.Value{
						"allow": cty.TupleVal([]cty.Value{cty.StringVal("127.0.0.1:443"), cty.StringVal("[::1]:80")}),
					}),
				},
			},
			wantNetwork: []string{"127.0.0.1:443", "[::1]:80"},
		},
		{
			name: "network wildcard allows unrestricted egress",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"network": cty.ObjectVal(map[string]cty.Value{
						"allow": cty.TupleVal([]cty.Value{cty.StringVal("*")}),
					}),
				},
			},
			wantNetworkWildcard: true,
			wantWarnings:        0,
		},
		{
			name: "unresolvable network host",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"network": cty.ObjectVal(map[string]cty.Value{
						"allow": cty.TupleVal([]cty.Value{cty.StringVal("this-host-does-not-exist-12345.invalid:80")}),
					}),
				},
			},
			wantNetwork:  []string{},
			wantWarnings: 1,
		},
		{
			name: "block kext and mach",
			policy: workflow.ResolvedPolicy{
				TypeSpecific: map[string]cty.Value{
					"filesystem": cty.ObjectVal(map[string]cty.Value{
						"block_kext_load":   cty.BoolVal(true),
						"block_mach_lookup": cty.BoolVal(true),
					}),
				},
			},
			wantBlockKext: true,
			wantBlockMach: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prof := FromPolicy(tc.policy, tc.adapterBinary)
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
			for _, p := range tc.wantExec {
				if !sliceContains(prof.AllowExec, p) {
					t.Errorf("AllowExec missing %q, got %v", p, prof.AllowExec)
				}
			}
			for _, h := range tc.wantNetwork {
				if !sliceContains(prof.AllowNetworkHosts, h) {
					t.Errorf("AllowNetworkHosts missing %q, got %v", h, prof.AllowNetworkHosts)
				}
			}
			if prof.BlockKextLoad != tc.wantBlockKext {
				t.Errorf("BlockKextLoad=%v, want %v", prof.BlockKextLoad, tc.wantBlockKext)
			}
			if prof.BlockMachLookup != tc.wantBlockMach {
				t.Errorf("BlockMachLookup=%v, want %v", prof.BlockMachLookup, tc.wantBlockMach)
			}
			if prof.AllowNetworkBind != tc.wantNetworkBind {
				t.Errorf("AllowNetworkBind=%v, want %v", prof.AllowNetworkBind, tc.wantNetworkBind)
			}
			if prof.AllowNetworkWildcard != tc.wantNetworkWildcard {
				t.Errorf("AllowNetworkWildcard=%v, want %v", prof.AllowNetworkWildcard, tc.wantNetworkWildcard)
			}
			if len(prof.resolveWarnings) != tc.wantWarnings {
				t.Errorf("resolveWarnings=%d, want %d", len(prof.resolveWarnings), tc.wantWarnings)
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
		DefaultDeny:       true,
		AllowExec:         []string{helper},
		AllowFileReads:    []string{allowedFile, helper, filepath.Dir(helper)},
		AllowNetworkHosts: []string{"127.0.0.1:55555"},
	}

	tmpPath, err := writeProfile(&prof)
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
	"os/exec"
)
func main() {
	if len(os.Args) < 2 { os.Exit(1) }
	switch os.Args[1] {
	case "read":
		if len(os.Args) < 3 { os.Exit(1) }
		f, err := os.Open(os.Args[2])
		if err != nil { fmt.Println("READ_FAIL:", err); os.Exit(0) }
		f.Close(); fmt.Println("READ_OK")
	case "write":
		if len(os.Args) < 3 { os.Exit(1) }
		f, err := os.Create(os.Args[2])
		if err != nil { fmt.Println("WRITE_FAIL:", err); os.Exit(0) }
		_, werr := f.WriteString("WRITE_OK\n")
		f.Close()
		if werr != nil { fmt.Println("WRITE_FAIL:", werr); os.Exit(0) }
		fmt.Println("WRITE_OK")
	case "connect":
		if len(os.Args) < 4 { os.Exit(1) }
		conn, err := net.Dial("tcp", net.JoinHostPort(os.Args[2], os.Args[3]))
		if err != nil { fmt.Println("CONNECT_FAIL:", err); os.Exit(0) }
		conn.Close(); fmt.Println("CONNECT_OK")
	case "listenunix":
		if len(os.Args) < 3 { os.Exit(1) }
		ln, err := net.Listen("unix", os.Args[2])
		if err != nil { fmt.Println("LISTEN_FAIL:", err); os.Exit(0) }
		ln.Close()
		_ = os.Remove(os.Args[2])
		fmt.Println("LISTEN_OK")
	case "execsh":
		if len(os.Args) < 4 { os.Exit(1) }
		cmd := exec.Command("/bin/sh", "-c", os.Args[2])
		cmd.Dir = os.Args[3]
		out, err := cmd.CombinedOutput()
		if err != nil { fmt.Println("EXECSH_FAIL:", err, string(out)); os.Exit(0) }
		fmt.Print(string(out))
	case "exec":
		if len(os.Args) < 5 { os.Exit(1) }
		cmd := exec.Command(os.Args[2], os.Args[3])
		cmd.Dir = os.Args[4]
		out, err := cmd.CombinedOutput()
		if err != nil { fmt.Println("EXEC_FAIL:", err, string(out)); os.Exit(0) }
		fmt.Print(string(out))
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

func TestSanitizePathEnv(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "mixed relative and absolute PATH",
			env:  []string{"HOME=/Users/test", "PATH=/usr/local/bin::./relative:/bin", "SECRET=shh"},
			want: []string{"HOME=/Users/test", "PATH=/usr/local/bin:/bin", "SECRET=shh"},
		},
		{
			name: "all relative PATH entries",
			env:  []string{"PATH=./bin:../lib:rel"},
			want: []string{"PATH="},
		},
		{
			name: "empty PATH",
			env:  []string{"PATH="},
			want: []string{"PATH="},
		},
		{
			name: "no PATH variable",
			env:  []string{"HOME=/Users/test"},
			want: []string{"HOME=/Users/test"},
		},
		{
			name: "only absolute PATH",
			env:  []string{"PATH=/usr/bin:/bin"},
			want: []string{"PATH=/usr/bin:/bin"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizePathEnv(tc.env)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestWriteProfile(t *testing.T) {
	prof := Profile{
		DefaultDeny:    true,
		AllowExec:      []string{"/usr/local/bin/adapter"},
		AllowFileReads: []string{"/tmp/allowed"},
	}
	path, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(path)

	if !strings.HasPrefix(path, os.TempDir()) {
		t.Errorf("profile path %q not in temp dir", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if string(data) != prof.Render() {
		t.Errorf("profile content mismatch\ngot:\n%s\nwant:\n%s", data, prof.Render())
	}
}

func TestResolveHost_Caching(t *testing.T) {
	// localhost always resolves; exercise the cache path.
	resolved1 := resolveHost("localhost:443")
	if len(resolved1) == 0 {
		t.Fatal("expected localhost to resolve")
	}
	resolved2 := resolveHost("localhost:443")
	if len(resolved2) == 0 {
		t.Fatal("expected cached localhost to resolve")
	}
	if resolved1[0] != resolved2[0] {
		t.Fatalf("cache mismatch: %v vs %v", resolved1, resolved2)
	}

	// Verify port stripping works: with no input port the results must be bare
	// addresses. net.ParseIP distinguishes a bare IP (including IPv6 such as
	// "::1", which legitimately contains colons) from an "ip:port" form.
	resolvedNoPort := resolveHost("localhost")
	if len(resolvedNoPort) == 0 {
		t.Fatal("expected localhost without port to resolve")
	}
	for _, ip := range resolvedNoPort {
		if net.ParseIP(ip) == nil {
			t.Errorf("expected bare IP without port, got %q", ip)
		}
	}

	// Invalid hostname should return nil.
	invalid := resolveHost("this-host-does-not-exist-12345.invalid:80")
	if invalid != nil {
		t.Errorf("expected nil for invalid hostname, got %v", invalid)
	}
}

func TestFromPolicy_DNSResolution(t *testing.T) {
	prof := FromPolicy(workflow.ResolvedPolicy{
		TypeSpecific: map[string]cty.Value{
			"network": cty.ObjectVal(map[string]cty.Value{
				"allow": cty.TupleVal([]cty.Value{cty.StringVal("localhost:443")}),
			}),
		},
	}, "")
	if len(prof.AllowNetworkHosts) == 0 {
		t.Fatal("expected localhost:443 to resolve to at least one IP")
	}
	for _, h := range prof.AllowNetworkHosts {
		if !strings.HasSuffix(h, ":443") {
			t.Errorf("expected port 443 in %q", h)
		}
	}
}

func TestFromPolicy_DNSFailure_Warning(t *testing.T) {
	prof := FromPolicy(workflow.ResolvedPolicy{
		TypeSpecific: map[string]cty.Value{
			"network": cty.ObjectVal(map[string]cty.Value{
				"allow": cty.TupleVal([]cty.Value{cty.StringVal("this-host-does-not-exist-12345.invalid:80")}),
			}),
		},
	}, "")
	if len(prof.resolveWarnings) != 1 {
		t.Fatalf("expected 1 resolve warning, got %d", len(prof.resolveWarnings))
	}
	if prof.resolveWarnings[0].host != "this-host-does-not-exist-12345.invalid:80" {
		t.Errorf("unexpected warning host: %q", prof.resolveWarnings[0].host)
	}
}

func TestPrepare_Strict_ResolveWarnings(t *testing.T) {
	probeTestHook = func() Capabilities { return Capabilities{SandboxExec: true} }
	defer func() { probeTestHook = nil; ResetProbeCache() }()

	h := Handler{}
	_, err := h.Prepare(PrepareContext{
		Policy: &workflow.ResolvedPolicy{
			PolicyMode: "strict",
			TypeSpecific: map[string]cty.Value{
				"network": cty.ObjectVal(map[string]cty.Value{
					"allow": cty.TupleVal([]cty.Value{cty.StringVal("this-host-does-not-exist-12345.invalid:80")}),
				}),
			},
		},
		Caps: Probe(),
	})
	if err == nil {
		t.Fatal("expected error in strict mode with unresolvable host")
	}
	if !strings.Contains(err.Error(), "policy validation failed") {
		t.Errorf("expected 'policy validation failed' in error, got: %v", err)
	}
}

func TestCRI82_DarwinSandboxDirectorySubpath(t *testing.T) {
	prof := Profile{
		DefaultDeny:     true,
		AllowFileWrites: []string{"/tmp"},
	}
	rendered := prof.Render()
	if !strings.Contains(rendered, `(subpath "/private/tmp")`) {
		t.Errorf("expected (subpath \"/private/tmp\") for /tmp directory root, got:\n%s", rendered)
	}
	if strings.Contains(rendered, `(literal "/private/tmp")`) {
		t.Errorf("directory root /tmp should not render as (literal \"/private/tmp\"), got:\n%s", rendered)
	}
}

func TestCRI82_DarwinSandboxFileLiteral(t *testing.T) {
	prof := Profile{
		DefaultDeny:     true,
		AllowFileWrites: []string{"/tmp/adapter.log"},
	}
	rendered := prof.Render()
	if !strings.Contains(rendered, `(literal "/private/tmp/adapter.log")`) {
		t.Errorf("expected (literal \"/private/tmp/adapter.log\") for non-existent file entry, got:\n%s", rendered)
	}
	if strings.Contains(rendered, `(subpath "/private/tmp/adapter.log")`) {
		t.Errorf("file-level entry /tmp/adapter.log should not render as (subpath), got:\n%s", rendered)
	}
}

func TestCRI82_FromPolicyDirectorySubpath(t *testing.T) {
	prof := FromPolicy(workflow.ResolvedPolicy{
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"write": cty.TupleVal([]cty.Value{cty.StringVal("/tmp")}),
			}),
		},
	}, "")
	rendered := prof.Render()
	if !strings.Contains(rendered, `(subpath "/private/tmp")`) {
		t.Errorf("expected FromPolicy to render /tmp as (subpath \"/private/tmp\"), got:\n%s", rendered)
	}
}

func TestCRI82_DarwinSandboxAllowsDescendantWrite(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)
	allowedDir := t.TempDir()

	prof := Profile{
		DefaultDeny:     true,
		AllowExec:       []string{helper},
		AllowFileWrites: []string{allowedDir},
	}

	profilePath, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(profilePath)

	target := filepath.Join(allowedDir, "plugin-transport")
	out, err := runUnderSandbox(t, helper, profilePath, "write", target)
	if err != nil {
		t.Fatalf("allowed write failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WRITE_OK") {
		t.Errorf("allowed write: expected WRITE_OK, got:\n%s", out)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("allowed write: expected file %q to exist, got: %v", target, statErr)
	}
}

func TestCRI82_DarwinSandboxDeniesWriteOutsideAllowedRoot(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)
	allowedDir := t.TempDir()
	blockedDir := t.TempDir()

	prof := Profile{
		DefaultDeny:     true,
		AllowExec:       []string{helper},
		AllowFileWrites: []string{allowedDir},
	}

	profilePath, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(profilePath)

	target := filepath.Join(blockedDir, "plugin-transport")
	out, err := runUnderSandbox(t, helper, profilePath, "write", target)

	denied := err != nil ||
		strings.Contains(out, "WRITE_FAIL") ||
		strings.Contains(out, "Operation not permitted") ||
		strings.Contains(out, "EPERM")
	if !denied {
		t.Errorf("blocked write: expected denial, got err=%v, output:\n%s", err, out)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Errorf("blocked write: expected file %q to not exist, got stat err=%v", target, statErr)
	}
}

func TestCRI83_FromPolicySetsNetworkBindForLocalAdapter(t *testing.T) {
	prof := FromPolicy(workflow.ResolvedPolicy{
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"write": cty.TupleVal([]cty.Value{cty.StringVal("/tmp")}),
			}),
			"network": cty.ObjectVal(map[string]cty.Value{
				"allow_egress": cty.BoolVal(false),
			}),
		},
	}, "/usr/local/bin/adapter")

	if !prof.AllowNetworkBind {
		t.Error("expected AllowNetworkBind=true for local adapter")
	}
	rendered := prof.Render()
	if !strings.Contains(rendered, "(allow network-bind)") {
		t.Errorf("expected rendered profile to contain (allow network-bind), got:\n%s", rendered)
	}
	if !strings.Contains(rendered, `(subpath "/private/tmp")`) {
		t.Errorf("expected rendered profile to contain (subpath \"/private/tmp\"), got:\n%s", rendered)
	}
}

func TestCRI83_FromPolicyOmitsNetworkBindWithoutAdapter(t *testing.T) {
	prof := FromPolicy(workflow.ResolvedPolicy{
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"write": cty.TupleVal([]cty.Value{cty.StringVal("/tmp")}),
			}),
		},
	}, "")

	if prof.AllowNetworkBind {
		t.Error("expected AllowNetworkBind=false when no adapter binary is associated")
	}
	rendered := prof.Render()
	if strings.Contains(rendered, "(allow network-bind)") {
		t.Errorf("profile without adapter must not contain (allow network-bind), got:\n%s", rendered)
	}
}

func TestCRI83_DarwinSandboxAllowsUnixBindForLocalAdapter(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)
	allowedDir := t.TempDir()

	// Build the profile via FromPolicy, exactly as a local adapter run does,
	// so this integration test exercises the full production translation chain.
	prof := FromPolicy(workflow.ResolvedPolicy{
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"write": cty.TupleVal([]cty.Value{cty.StringVal(allowedDir)}),
			}),
			"network": cty.ObjectVal(map[string]cty.Value{
				"allow_egress": cty.BoolVal(false),
			}),
		},
	}, helper)

	if !prof.AllowNetworkBind {
		t.Fatal("FromPolicy with a local adapter must set AllowNetworkBind")
	}

	profilePath, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(profilePath)

	socketPath := filepath.Join(allowedDir, "plugin-transport.sock")
	out, err := runUnderSandbox(t, helper, profilePath, "listenunix", socketPath)
	if err != nil {
		t.Fatalf("allowed unix bind failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "LISTEN_OK") {
		t.Errorf("allowed unix bind: expected LISTEN_OK, got:\n%s", out)
	}
}

func TestCRI83_DarwinSandboxDeniesUnixBindWithoutNetworkBind(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)
	allowedDir := t.TempDir()

	// Same writable directory, but no network-bind rule. UDS bind must fail.
	prof := Profile{
		DefaultDeny:     true,
		AllowExec:       []string{helper},
		AllowFileWrites: []string{allowedDir},
	}

	profilePath, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(profilePath)

	socketPath := filepath.Join(allowedDir, "plugin-transport.sock")
	out, err := runUnderSandbox(t, helper, profilePath, "listenunix", socketPath)

	denied := err != nil ||
		strings.Contains(out, "LISTEN_FAIL") ||
		strings.Contains(out, "Operation not permitted") ||
		strings.Contains(out, "EPERM")
	if !denied {
		t.Errorf("expected unix bind denial without network-bind, got err=%v, output:\n%s", err, out)
	}
}

func TestCRI86_DarwinSandboxAllowsDeclaredExecutable(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)
	helperDir := filepath.Dir(helper)

	prof := FromPolicy(workflow.ResolvedPolicy{
		Process: &workflow.ProcessPolicy{
			Exec: []string{"/bin/bash"},
		},
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"read": cty.TupleVal([]cty.Value{cty.StringVal(helperDir)}),
			}),
			"network": cty.ObjectVal(map[string]cty.Value{
				"allow_egress": cty.BoolVal(false),
			}),
		},
	}, helper)

	if !sliceContains(prof.AllowExec, "/bin/bash") {
		t.Fatalf("declared /bin/bash must be allowed, got %v", prof.AllowExec)
	}

	profilePath, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(profilePath)

	out, err := runUnderSandbox(t, helper, profilePath, "exec", "/bin/bash", "-c echo DECLARED_OK", helperDir)
	if err != nil {
		t.Fatalf("exec under sandbox failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "DECLARED_OK") {
		t.Errorf("expected declared executable output DECLARED_OK, got:\n%s", out)
	}
}

func TestCRI87_Profile_Render_Wildcard(t *testing.T) {
	prof := Profile{
		DefaultDeny:       true,
		AllowExecWildcard: true,
	}
	rendered := prof.Render()
	if !strings.Contains(rendered, "(allow process-exec)\n") {
		t.Errorf("expected unrestricted (allow process-exec) for wildcard, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "(allow process-exec") && strings.Contains(rendered, "(literal") {
		t.Errorf("wildcard process-exec must not enumerate literal paths, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "(deny process-exec)") {
		t.Errorf("wildcard must not emit (deny process-exec), got:\n%s", rendered)
	}
}

func TestCRI87_FromPolicy_Wildcard_DoesNotWidenFilesystem(t *testing.T) {
	prof := FromPolicy(workflow.ResolvedPolicy{
		Process: &workflow.ProcessPolicy{Exec: []string{"*"}},
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"read": cty.TupleVal([]cty.Value{cty.StringVal("/tmp")}),
			}),
		},
	}, "/usr/local/bin/adapter")

	if !prof.AllowExecWildcard {
		t.Fatal("expected AllowExecWildcard=true for process.exec=[\"*\"]")
	}
	if sliceContains(prof.AllowExec, "*") {
		t.Errorf("wildcard must not be added to AllowExec literal list")
	}
	for _, p := range prof.AllowFileReads {
		if p == "*" {
			t.Errorf("wildcard must not widen file-read allow-list")
		}
	}
}

func TestCRI87_DarwinSandbox_AllowsWildcardExecutable(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)
	helperDir := filepath.Dir(helper)

	prof := FromPolicy(workflow.ResolvedPolicy{
		Process: &workflow.ProcessPolicy{Exec: []string{"*"}},
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"read": cty.TupleVal([]cty.Value{cty.StringVal(helperDir)}),
			}),
			"network": cty.ObjectVal(map[string]cty.Value{
				"allow_egress": cty.BoolVal(false),
			}),
		},
	}, helper)

	if !prof.AllowExecWildcard {
		t.Fatal("FromPolicy must set AllowExecWildcard for wildcard process policy")
	}

	profilePath, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(profilePath)

	out, err := runUnderSandbox(t, helper, profilePath, "exec", "/bin/bash", "-c echo WILDCARD_OK", helperDir)
	if err != nil {
		t.Fatalf("exec under sandbox failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WILDCARD_OK") {
		t.Errorf("expected wildcard executable output WILDCARD_OK, got:\n%s", out)
	}
}

func TestCRI86_DarwinSandboxDeniesUndeclaredExecutable(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available on this runner")
	}

	helper := buildDarwinTestHelper(t)
	helperDir := filepath.Dir(helper)

	prof := FromPolicy(workflow.ResolvedPolicy{
		TypeSpecific: map[string]cty.Value{
			"filesystem": cty.ObjectVal(map[string]cty.Value{
				"read": cty.TupleVal([]cty.Value{cty.StringVal(helperDir)}),
			}),
			"network": cty.ObjectVal(map[string]cty.Value{
				"allow_egress": cty.BoolVal(false),
			}),
		},
	}, helper)

	if sliceContains(prof.AllowExec, "/bin/bash") {
		t.Fatalf("undeclared /bin/bash must not be allowed, got %v", prof.AllowExec)
	}

	profilePath, err := writeProfile(&prof)
	if err != nil {
		t.Fatalf("writeProfile: %v", err)
	}
	defer os.Remove(profilePath)

	out, err := runUnderSandbox(t, helper, profilePath, "exec", "/bin/bash", "-c echo DECLARED_OK", helperDir)

	denied := err != nil ||
		strings.Contains(out, "EXEC_FAIL") ||
		strings.Contains(out, "Operation not permitted") ||
		strings.Contains(out, "EPERM")
	if !denied {
		t.Errorf("expected undeclared /bin/bash exec denial, got err=%v, output:\n%s", err, out)
	}
}

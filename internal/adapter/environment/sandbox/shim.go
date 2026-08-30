//go:build linux

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/elastic/go-seccomp-bpf"
	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

const shimEnvVar = "CRITERIA_SANDBOX_CONFIG_PATH"

// ShimConfig is the JSON-serialised data passed from the parent process
// to the shim child via a temp file pointed to by shimEnvVar.
type ShimConfig struct {
	TargetPath       string         `json:"target_path"`
	Mode             string         `json:"mode"`
	ReadPaths        []string       `json:"read_paths"`
	WritePaths       []string       `json:"write_paths"`
	NetPorts         []uint16       `json:"net_ports"`
	AllowNetwork     bool           `json:"allow_network"`
	Seccomp          bool           `json:"seccomp"`
	Rlimits          []RlimitConfig `json:"rlimits"`
	SkipRestrictions bool           `json:"skip_restrictions"`
}

// RunIfEnv detects shimEnvVar and, if present, performs the sandbox
// restrictions and re-execs the target binary. It returns true when the
// shim was executed so the caller can exit early.
func RunIfEnv() (ran bool, err error) {
	path := os.Getenv(shimEnvVar)
	if path == "" {
		return false, nil
	}
	defer os.Remove(path)
	return true, runShim(path)
}

// ApplyEnv detects shimEnvVar and, if present, applies the sandbox
// restrictions in the current process. The env var is cleared and the
// config file is removed before returning so the caller can continue
// safely. It returns nil when the env var is absent.
func ApplyEnv() error {
	path := os.Getenv(shimEnvVar)
	if path == "" {
		return nil
	}
	os.Unsetenv(shimEnvVar)
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read shim config: %w", err)
	}
	var cfg ShimConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse shim config: %w", err)
	}
	return applyShimRestrictions(&cfg)
}

var shimExecFunc = syscall.Exec

func runShim(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read shim config: %w", err)
	}
	var cfg ShimConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse shim config: %w", err)
	}

	// Test-only dedicated shim helpers are short-lived wrappers: the Go
	// runtime starts worker threads before main, so in-process installation of
	// a TSYNC seccomp filter (or tight rlimits) is fragile. Skip the
	// restriction primitives in that mode and just exec the real adapter.
	// The production criteria CLI path (criteria binary as its own shim)
	// leaves SkipRestrictions false, applying the profile in-process before
	// exec so the adapter inherits it.
	if !cfg.SkipRestrictions {
		if err := applyShimRestrictions(&cfg); err != nil {
			return err
		}
	}

	// Re-exec the real adapter binary, replacing this process image.
	// Unset the shim env var so the re-execed process does not try to
	// apply restrictions again (infinite recursion).
	os.Unsetenv(shimEnvVar)
	os.Remove(configPath)
	argv := []string{cfg.TargetPath}
	if len(os.Args) > 3 {
		argv = append(argv, os.Args[3:]...)
	}
	err = shimExecFunc(cfg.TargetPath, argv, os.Environ())
	return fmt.Errorf("exec %s: %w", cfg.TargetPath, err)
}

// applyShimRestrictions applies landlock, seccomp, rlimits, and
// PR_SET_NO_NEW_PRIVS based on cfg. It is used both by the shim
// (before exec) and by test fixtures (in-process).
func applyShimRestrictions(cfg *ShimConfig) error {
	// Prevent privilege escalation via setuid binaries inside the sandbox.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}

	// Pin the caller to a stable OS thread before installing any sandbox
	// primitives so each enforcement step targets the same M.
	runtime.LockOSThread()

	// Apply rlimits before anything else so later syscalls are bounded.
	for _, rl := range cfg.Rlimits {
		if err := syscall.Setrlimit(rl.Resource, &rl.Rlimit); err != nil {
			return fmt.Errorf("setrlimit %d: %w", rl.Resource, err)
		}
	}

	// Build and apply Landlock restrictions.
	if err := applyLandlock(cfg); err != nil {
		return err
	}

	// Build and apply seccomp filter.
	if cfg.Seccomp {
		if err := applySeccompFilter(cfg); err != nil {
			return err
		}
	}

	return nil
}

// readAccessFS grants read access to both files and directories plus
// execute/traverse access.  Execute on directory hierarchies is required
// for helpers such as exec.LookPath to stat binaries under read paths
// (e.g. /usr/bin/getent) on newer kernels.
var readAccessFS = landlock.AccessFSSet(ll.AccessFSExecute | ll.AccessFSReadFile | ll.AccessFSReadDir)

func applyLandlock(cfg *ShimConfig) error {
	if len(cfg.ReadPaths)+len(cfg.WritePaths)+len(cfg.NetPorts) == 0 {
		return nil
	}
	var llcfg landlock.Config
	if len(cfg.NetPorts) > 0 {
		llcfg = landlock.V4
	} else {
		llcfg = landlock.V1
	}
	if cfg.Mode == "permissive" {
		llcfg = llcfg.BestEffort()
	}
	ruleCap := 0
	if len(cfg.ReadPaths) > 0 {
		ruleCap++
	}
	if len(cfg.WritePaths) > 0 {
		ruleCap++
	}
	ruleCap += len(cfg.NetPorts)
	rules := make([]landlock.Rule, 0, ruleCap)
	if len(cfg.ReadPaths) > 0 {
		rules = append(rules, landlock.PathAccess(readAccessFS, cfg.ReadPaths...).IgnoreIfMissing())
	}
	// Network-enabled sandboxes may need to follow the host's resolv.conf
	// symlink into /run/systemd/resolve (common on Ubuntu/systemd-resolved
	// hosts). Grant read-only access to that directory when external egress
	// is allowed; the rule is ignored on systems where it does not exist.
	// Note: /run/dbus is intentionally NOT granted here. If nss-resolve cannot
	// contact systemd-resolved via D-Bus, glibc falls back to the "dns" module,
	// which exercises the sendmmsg/recvmmsg path this workstream enables.
	if cfg.AllowNetwork {
		rules = append(rules, landlock.PathAccess(readAccessFS, "/run/systemd/resolve").IgnoreIfMissing())
	}
	if len(cfg.WritePaths) > 0 {
		rules = append(rules, landlock.RWDirs(cfg.WritePaths...).IgnoreIfMissing())
	}
	for _, port := range cfg.NetPorts {
		rules = append(rules, landlock.ConnectTCP(port))
	}
	if err := llcfg.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("landlock restrict: %w", err)
	}
	return nil
}

func applySeccompFilter(cfg *ShimConfig) error {
	// The parent already built and validated the filter; here we
	// construct a minimal safe allow-list for file, network, IPC
	// and basic process syscalls that go-plugin needs.
	filter := seccompFilterForShim(cfg)
	if err := seccomp.LoadFilter(*filter); err != nil {
		return fmt.Errorf("seccomp load: %w", err)
	}
	return nil
}

// seccompFilterForShim builds the same default-deny filter that the
// parent constructs in buildSeccompFilter. Keeping the list identical
// avoids surprises where the parent validates one set and the child
// loads another.
func seccompFilterForShim(cfg *ShimConfig) *seccomp.Filter {
	syscalls := make([]string, len(baseSyscalls))
	copy(syscalls, baseSyscalls)
	if cfg.AllowNetwork {
		syscalls = append(syscalls, networkSyscalls...)
	}

	policy := seccomp.Policy{
		DefaultAction: seccomp.ActionErrno,
		Syscalls: []seccomp.SyscallGroup{
			{
				Names:  syscalls,
				Action: seccomp.ActionAllow,
			},
		},
	}

	return &seccomp.Filter{
		NoNewPrivs: true,
		Policy:     policy,
	}
}

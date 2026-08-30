//go:build linux

// Package sandbox implements the Linux sandbox primitives for criteria
// adapter execution.
//
// # Architecture
//
// The host process (criteria CLI) prepares a LinuxPrepared configuration
// from the workflow's sandbox environment block.  The concrete
// restrictions (namespaces, landlock, seccomp, rlimits) must be applied
// inside the *child* process because prctl-based syscalls only affect the
// calling thread.  We achieve this without a separate helper binary by
// making the criteria binary itself act as a pre-exec shim:
//
//  1. The loader calls ApplyToCmd before fork+exec.
//  2. ApplyToCmd replaces cmd.Path with the criteria binary and sets
//     CRITERIA_SANDBOX_CONFIG_PATH to a temp JSON file.
//  3. cmd/criteria/main.go detects the env var at start-up and branches to
//     sandbox.RunIfEnv, which reads the JSON config, applies landlock +
//     seccomp + rlimits, then syscall.Exec the real adapter binary.
//
// This was chosen over pidfd/clone3 (requires very recent kernels) and
// over a separate helper binary (violates the single-static-binary
// constraint, D29).  The shim adds one extra exec but keeps the
// deployment footprint minimal.
//
// # Bubblewrap fallback
//
// When the user opts in (environment.sandbox = "bwrap") and bwrap is on
// PATH, MaybeUseBubblewrap returns an *exec.Cmd that runs the adapter
// under bubblewrap instead of the in-process shim.  This is a soft
// dependency; absence is fine.
//
// # Trust boundaries
//
// The shim config file is written to a private temp file (mode 0600) and
// read by the child before it drops privileges.  An attacker with the
// same UID could potentially race the write, but that attacker already has
// full access to the criteria process and can modify environment
// variables, so the temp file does not introduce a new trust boundary.
// The real protection is that the shim runs before the adapter and the
// adapter is the only consumer of the file.
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/elastic/go-seccomp-bpf"
	seccomparch "github.com/elastic/go-seccomp-bpf/arch"
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/sys/unix"

	"github.com/brokenbots/criteria/workflow"
)

// Handler prepares Linux sandbox configuration from a ResolvedPolicy.
type Handler struct{}

// PrepareContext carries the compile-time policy and runtime capabilities
// needed to build a LinuxPrepared configuration.
type PrepareContext struct {
	Policy        *workflow.ResolvedPolicy
	Env           *workflow.EnvironmentNode
	Caps          Capabilities
	AdapterBinary string // adapter binary path used by the shim/bwrap as TargetPath (linux) and for darwin allow-listing
	// ValidateOnly, when true, skips side-effecting preparation steps (e.g.
	// creating transient cgroup directories) so the call can be used for eager
	// host-side validation. The strict-mode primitive-availability checks still
	// run and still fail closed.
	ValidateOnly bool
}

// LinuxPrepared is the result of translating a sandbox policy into
// concrete Linux primitives. It is consumed by the adapter loader to
// configure the exec.Cmd that spawns the adapter binary.
//
// Design note: the plan originally specified storing a *landlock.Config
// directly, but landlock.Config cannot be JSON-serialised for the shim
// config file.  We store the raw ReadPaths / WritePaths / NetPorts
// instead and re-build the landlock.Config inside the child process.
type LinuxPrepared struct {
	SysProcAttr *syscall.SysProcAttr
	SeccompBPF  *seccomp.Filter
	Rlimits     []RlimitConfig
	CgroupV2    *CgroupV2Config
	Mode        string // "strict" or "permissive"
	TargetPath  string // adapter binary path (for shim)

	// Raw landlock data (for shim serialization). The parent
	// validates that these can be turned into a landlock.Config.
	ReadPaths  []string
	WritePaths []string
	NetPorts   []uint16

	// AllowNetwork controls whether the seccomp filter includes
	// socket/connect syscalls.
	AllowNetwork bool

	// SkipShimRestrictions, when true, tells the shim helper to skip
	// in-process landlock/seccomp/rlimits/PR_SET_NO_NEW_PRIVS installation
	// and only syscall.Exec the target. It is used only by the test-only
	// dedicated shim helper path; production leaves it false.
	SkipShimRestrictions bool

	// ShimConfigPath is the temp JSON config file created by ApplyToCmd.
	// Cleanup removes it.
	ShimConfigPath string
}

// RlimitConfig describes a single rlimit to apply in the child.
type RlimitConfig struct {
	Resource int
	Rlimit   syscall.Rlimit
}

// CgroupV2Config carries cgroup limits that must be applied before
// fork by opening the cgroup directory and passing its fd via
// SysProcAttr.CgroupFD.
type CgroupV2Config struct {
	CPUQuota  float64
	MemoryMax uint64
	CgroupDir string
}

// Prepare converts a ResolvedPolicy into LinuxPrepared.
func (h Handler) Prepare(ctx PrepareContext) (LinuxPrepared, error) {
	if ctx.Policy == nil {
		return LinuxPrepared{}, fmt.Errorf("sandbox policy is nil")
	}
	if ctx.Policy.OS != "" && ctx.Policy.OS != "linux" {
		return LinuxPrepared{}, fmt.Errorf("sandbox policy OS %q is not supported on this host", ctx.Policy.OS)
	}

	mode := ctx.Policy.PolicyMode
	if mode == "" {
		mode = "permissive"
	}

	prep := LinuxPrepared{Mode: mode}
	if ctx.AdapterBinary != "" {
		prep.TargetPath = ctx.AdapterBinary
	}

	readPaths, writePaths, netAllow, allowNet, err := extractPolicyPaths(ctx.Policy)
	if err != nil {
		return LinuxPrepared{}, err
	}
	prep.AllowNetwork = allowNet
	prep.SysProcAttr = buildSysProcAttr(allowNet)

	if err := applyLandlockToPrep(&prep, ctx.Caps, mode, readPaths, writePaths, netAllow); err != nil {
		return LinuxPrepared{}, err
	}
	if err := applySeccompToPrep(&prep, ctx.Caps, mode, allowNet); err != nil {
		return LinuxPrepared{}, err
	}
	if err := applyResourcesToPrep(&prep, ctx.Caps, mode, ctx.Policy, ctx.ValidateOnly); err != nil {
		return LinuxPrepared{}, err
	}

	return prep, nil
}

func extractPolicyPaths(policy *workflow.ResolvedPolicy) (readPaths, writePaths, netAllow []string, allowNet bool, err error) {
	fsObj := cty.NilVal
	if policy.TypeSpecific != nil {
		if v, ok := policy.TypeSpecific["filesystem"]; ok {
			fsObj = v
		}
	}
	readPaths = pathListFromObject(fsObj, "read")
	writePaths = pathListFromObject(fsObj, "write")
	for _, p := range append(readPaths, writePaths...) {
		if err = validatePath(p); err != nil {
			return nil, nil, nil, false, fmt.Errorf("sandbox filesystem path: %w", err)
		}
	}

	netObj := cty.NilVal
	if policy.TypeSpecific != nil {
		if v, ok := policy.TypeSpecific["network"]; ok {
			netObj = v
		}
	}
	netAllow = pathListFromObject(netObj, "allow")
	if policy.Network != nil {
		allowNet = policy.Network.AllowEgress
	}
	if !allowNet && len(netAllow) > 0 {
		allowNet = true
	}
	return readPaths, writePaths, netAllow, allowNet, nil
}

func applyLandlockToPrep(prep *LinuxPrepared, caps Capabilities, mode string, readPaths, writePaths, netAllow []string) error {
	if !caps.Landlock {
		if mode == "strict" {
			return fmt.Errorf("landlock not available but policy_mode=strict")
		}
		return nil
	}
	prep.ReadPaths = readPaths
	prep.WritePaths = writePaths
	for _, ep := range netAllow {
		_, portStr := splitHostPort(ep)
		if portStr == "" {
			continue
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			continue
		}
		prep.NetPorts = append(prep.NetPorts, uint16(port))
	}
	return nil
}

func applySeccompToPrep(prep *LinuxPrepared, caps Capabilities, mode string, allowNet bool) error {
	if !caps.Seccomp {
		if mode == "strict" {
			return fmt.Errorf("seccomp not available but policy_mode=strict")
		}
		return nil
	}
	filter, err := buildSeccompFilter(allowNet)
	if err != nil {
		if mode == "strict" {
			return fmt.Errorf("seccomp filter: %w", err)
		}
		return nil
	}
	prep.SeccompBPF = filter
	return nil
}

// resourceStrings extracts raw resource configuration from the policy. It
// centralizes the lookup of TypeSpecific["resources"] and the legacy
// policy.Resources fallback so applyResourcesToPrep stays readable.
type resourceStrings struct {
	memory     string
	cpu        string
	timeout    string
	maxThreads string
	useCgroup  bool
}

func resourceStringsFromPolicy(policy *workflow.ResolvedPolicy) resourceStrings {
	resObj := cty.NilVal
	if policy.TypeSpecific != nil {
		if v, ok := policy.TypeSpecific["resources"]; ok {
			resObj = v
		}
	}
	memStr := stringFromObject(resObj, "memory")
	if memStr == "" && policy.Resources != nil {
		memStr = policy.Resources.MaxMemory
	}
	return resourceStrings{
		memory:     memStr,
		cpu:        stringFromObject(resObj, "cpu"),
		timeout:    stringFromObject(resObj, "timeout"),
		maxThreads: stringFromObject(resObj, "max_threads"),
		useCgroup:  boolFromObject(resObj, "cgroup", false),
	}
}

func applyResourcesToPrep(prep *LinuxPrepared, caps Capabilities, mode string, policy *workflow.ResolvedPolicy, validateOnly bool) error {
	res := resourceStringsFromPolicy(policy)

	memBytes := parseMemoryLimit(res.memory)
	cpuVal := parseCPULimit(res.cpu)
	timeoutDur := parseTimeout(res.timeout)
	maxThreads, err := resolveMaxThreads(res.maxThreads, mode)
	if err != nil {
		return err
	}

	prep.Rlimits = buildRlimits(memBytes, timeoutDur, maxThreads)

	if !res.useCgroup || !caps.Cgroupv2 {
		return nil
	}
	if validateOnly {
		// Eager validation: confirm the policy would attempt to use a cgroup
		// primitive without actually creating the transient cgroup directory.
		return nil
	}
	cg, err := prepareCgroupV2(cpuVal, memBytes)
	if err != nil {
		if mode == "strict" {
			return fmt.Errorf("cgroupv2 setup: %w", err)
		}
		return nil
	}
	prep.CgroupV2 = cg
	prep.SysProcAttr.UseCgroupFD = true
	fd, err := unix.Open(cg.CgroupDir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if mode == "strict" {
			return fmt.Errorf("open cgroup dir: %w", err)
		}
		prep.CgroupV2 = nil
		prep.SysProcAttr.UseCgroupFD = false
		return nil
	}
	prep.SysProcAttr.CgroupFD = fd
	return nil
}

// buildSysProcAttr creates a SysProcAttr that places the child in a new
// user, mount, PID, IPC and UTS namespace. When allowNetwork is false the
// child is also placed in a new network namespace so it has no usable
// interfaces or DNS path; when allowNetwork is true the network namespace
// is left shared with the parent so allowed destinations can be reached.
func buildSysProcAttr(allowNetwork bool) *syscall.SysProcAttr {
	uid := os.Getuid()
	gid := os.Getgid()
	// Map the current user to root (0) inside the user namespace.
	// This is the standard unprivileged user-namespace pattern.
	flags := uintptr(syscall.CLONE_NEWUSER |
		syscall.CLONE_NEWNS |
		syscall.CLONE_NEWPID |
		syscall.CLONE_NEWIPC |
		syscall.CLONE_NEWUTS)
	if !allowNetwork {
		flags |= uintptr(syscall.CLONE_NEWNET)
	}
	return &syscall.SysProcAttr{
		Cloneflags: flags,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: uid, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: gid, Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}
}

// baseSyscalls and networkSyscalls are defined in syscalls_linux.go with
// architecture-specific allow-lists selected at init time.

// filterSyscallsForCurrentArch returns only the syscalls from the provided
// list that are valid on the current runtime architecture. Syscalls that do
// not exist on the host arch (e.g. x86_64-specific names on aarch64) are
// dropped so that seccomp policy assembly does not fail with "unknown syscall"
// errors.
func filterSyscallsForCurrentArch(syscalls []string) []string {
	info, err := seccomparch.GetInfo("")
	if err != nil {
		// Unsupported architecture — return the original list and let the
		// assembler report the real error.
		return syscalls
	}
	out := make([]string, 0, len(syscalls))
	for _, name := range syscalls {
		if _, ok := info.SyscallNames[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// buildSeccompFilter returns a default-deny seccomp filter with a base
// allow-list covering the syscalls needed by the Go runtime, the gRPC
// plugin transport, and basic file/network operations.
func buildSeccompFilter(allowNetwork bool) (*seccomp.Filter, error) {
	syscalls := filterSyscallsForCurrentArch(baseSyscalls)
	if allowNetwork {
		syscalls = append(syscalls, filterSyscallsForCurrentArch(networkSyscalls)...)
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

	filter := seccomp.Filter{
		NoNewPrivs: true,
		Policy:     policy,
	}

	// Validate early so we fail before fork.
	_, err := filter.Policy.Assemble()
	if err != nil {
		return nil, fmt.Errorf("seccomp policy assembly: %w", err)
	}

	return &filter, nil
}

// defaultMaxThreads is the RLIMIT_NPROC value used when no max_threads
// policy is configured. It must remain finite and large enough for
// thread-heavy adapters such as the GitHub Copilot CLI native runtime.
const defaultMaxThreads = 2048

// buildRlimits returns rlimit configs derived from memory, timeout, and
// max_threads values. A maxThreads value of 0 means "use default".
func buildRlimits(memBytes uint64, timeout time.Duration, maxThreads uint64) []RlimitConfig {
	var out []RlimitConfig

	if memBytes > 0 {
		out = append(out, RlimitConfig{
			Resource: unix.RLIMIT_AS,
			Rlimit:   syscall.Rlimit{Cur: memBytes, Max: memBytes},
		})
	}

	if timeout > 0 {
		// RLIMIT_CPU is in seconds of CPU time, not wall-clock.
		// We set it to the ceiling of the timeout duration.
		seconds := uint64(timeout.Seconds())
		if seconds == 0 {
			seconds = 1
		}
		out = append(out, RlimitConfig{
			Resource: unix.RLIMIT_CPU,
			Rlimit:   syscall.Rlimit{Cur: seconds, Max: seconds},
		})
	}

	// Sensible defaults regardless of explicit policy.
	if maxThreads == 0 {
		maxThreads = defaultMaxThreads
	}
	out = append(out,
		RlimitConfig{
			Resource: unix.RLIMIT_NOFILE,
			Rlimit:   syscall.Rlimit{Cur: 1024, Max: 1024},
		},
		RlimitConfig{
			Resource: unix.RLIMIT_NPROC,
			Rlimit:   syscall.Rlimit{Cur: maxThreads, Max: maxThreads},
		},
	)

	return out
}

// prepareCgroupV2 creates a transient cgroup directory, writes limit
// files, and returns the directory path for use as CgroupFD.
func prepareCgroupV2(cpuQuota float64, memMax uint64) (*CgroupV2Config, error) {
	if cpuQuota == 0 && memMax == 0 {
		return nil, fmt.Errorf("no cgroup limits configured")
	}

	// Place the transient cgroup under the current process's cgroup.
	baseCgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, fmt.Errorf("read self cgroup: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(baseCgroup)), "\n")
	var cgroupPath string
	for _, line := range lines {
		parts := strings.Split(line, ":")
		if len(parts) >= 3 {
			cgroupPath = "/sys/fs/cgroup" + parts[2]
			break
		}
	}
	if cgroupPath == "" {
		cgroupPath = "/sys/fs/cgroup"
	}

	dir := filepath.Join(cgroupPath, fmt.Sprintf("criteria-sandbox-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir cgroup: %w", err)
	}

	cg := &CgroupV2Config{CgroupDir: dir}

	if memMax > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatUint(memMax, 10)), 0o644); err != nil {
			return nil, fmt.Errorf("write memory.max: %w", err)
		}
		cg.MemoryMax = memMax
	}

	if cpuQuota > 0 {
		// cpu.max format: "quota period"
		quota := int64(cpuQuota * 100000) // 100ms period
		val := fmt.Sprintf("%d 100000", quota)
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(val), 0o644); err != nil {
			return nil, fmt.Errorf("write cpu.max: %w", err)
		}
		cg.CPUQuota = cpuQuota
	}

	return cg, nil
}

// Cleanup removes transient resources created during preparation (e.g.
// transient cgroup directories and temp shim config files). It is safe
// to call multiple times.
func (prep *LinuxPrepared) Cleanup() error {
	if prep.SysProcAttr != nil && prep.SysProcAttr.CgroupFD != 0 {
		_ = unix.Close(prep.SysProcAttr.CgroupFD)
		prep.SysProcAttr.CgroupFD = 0
		prep.SysProcAttr.UseCgroupFD = false
	}
	if prep.CgroupV2 != nil && prep.CgroupV2.CgroupDir != "" {
		if err := os.RemoveAll(prep.CgroupV2.CgroupDir); err != nil {
			return err
		}
		prep.CgroupV2.CgroupDir = ""
	}
	if prep.ShimConfigPath != "" {
		_ = os.Remove(prep.ShimConfigPath)
		prep.ShimConfigPath = ""
	}
	return nil
}

// ApplyToCmd modifies cmd so that it will run inside the sandbox.
// If targetPath is empty the original command path is preserved.
//
// Approach: the criteria binary itself acts as a pre-exec shim. We
// replace cmd.Path with the criteria binary, set a magic env var that
// tells the shim what to exec, and prepend shim args. The shim
// applies landlock, seccomp and rlimits before calling syscall.Exec.
func (prep *LinuxPrepared) ApplyToCmd(cmd *exec.Cmd, criteriaBin string) error {
	if prep.SysProcAttr != nil {
		cmd.SysProcAttr = prep.SysProcAttr
	}

	// Env var scrub: drop any variable that looks like a secret or
	// that the sandbox should not inherit.
	//
	// go-plugin will append os.Environ() to cmd.Env if cmd.Env is nil
	// and SkipHostEnv is false, so we must seed cmd.Env from the host
	// environment before scrubbing so the filter has data to work on.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = scrubEnv(cmd.Env)

	// Working directory: default to the adapter's own dir if empty.
	if cmd.Dir == "" {
		cmd.Dir = filepath.Dir(prep.TargetPath)
	}

	// Replace command with the shim.
	if criteriaBin == "" {
		// Fallback: assume we are the criteria binary and can re-exec ourselves.
		criteriaBin = os.Args[0]
	}

	// Serialize shim configuration to a private temp file so the
	// child can apply the same restrictions without re-deriving them.
	shimCfg := ShimConfig{
		TargetPath:       prep.TargetPath,
		Mode:             prep.Mode,
		ReadPaths:        prep.ReadPaths,
		WritePaths:       prep.WritePaths,
		NetPorts:         prep.NetPorts,
		AllowNetwork:     prep.AllowNetwork,
		Seccomp:          prep.SeccompBPF != nil,
		Rlimits:          prep.Rlimits,
		SkipRestrictions: prep.SkipShimRestrictions,
	}
	tmpFile, err := os.CreateTemp("", "criteria-sandbox-*.json")
	if err != nil {
		return fmt.Errorf("create shim config file: %w", err)
	}
	if err := json.NewEncoder(tmpFile).Encode(shimCfg); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return fmt.Errorf("encode shim config: %w", err)
	}
	tmpFile.Close()

	prep.ShimConfigPath = tmpFile.Name()
	cmd.Path = criteriaBin
	cmd.Args = append([]string{criteriaBin, "_sandbox_shim_", prep.TargetPath}, cmd.Args[1:]...)
	cmd.Env = append(cmd.Env, "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())

	return nil
}

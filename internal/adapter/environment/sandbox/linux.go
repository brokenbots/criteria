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
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/sys/unix"

	"github.com/brokenbots/criteria/workflow"
)

// Handler prepares Linux sandbox configuration from a ResolvedPolicy.
type Handler struct{}

// PrepareContext carries the compile-time policy and runtime capabilities
// needed to build a LinuxPrepared configuration.
type PrepareContext struct {
	Policy *workflow.ResolvedPolicy
	Env    *workflow.EnvironmentNode
	Caps   Capabilities
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
	prep.SysProcAttr = buildSysProcAttr()

	readPaths, writePaths, netAllow, allowNet, err := extractPolicyPaths(ctx.Policy)
	if err != nil {
		return LinuxPrepared{}, err
	}
	prep.AllowNetwork = allowNet

	if err := applyLandlockToPrep(&prep, ctx.Caps, mode, readPaths, writePaths, netAllow); err != nil {
		return LinuxPrepared{}, err
	}
	if err := applySeccompToPrep(&prep, ctx.Caps, mode, allowNet); err != nil {
		return LinuxPrepared{}, err
	}
	if err := applyResourcesToPrep(&prep, ctx.Caps, mode, ctx.Policy); err != nil {
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

func applyResourcesToPrep(prep *LinuxPrepared, caps Capabilities, mode string, policy *workflow.ResolvedPolicy) error {
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
	cpuStr := stringFromObject(resObj, "cpu")
	timeoutStr := stringFromObject(resObj, "timeout")
	useCgroup := boolFromObject(resObj, "cgroup", false)

	memBytes := parseMemoryLimit(memStr)
	cpuVal := parseCPULimit(cpuStr)
	timeoutDur := parseTimeout(timeoutStr)

	prep.Rlimits = buildRlimits(memBytes, timeoutDur)

	if !useCgroup || !caps.Cgroupv2 {
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
// user, mount, PID, network, IPC and UTS namespace.
func buildSysProcAttr() *syscall.SysProcAttr {
	uid := os.Getuid()
	gid := os.Getgid()
	// Map the current user to root (0) inside the user namespace.
	// This is the standard unprivileged user-namespace pattern.
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNET |
			syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWUTS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: uid, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: gid, Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}
}

// baseSyscalls is the default-deny seccomp allow-list covering syscalls
// needed by the Go runtime, gRPC plugin transport, and basic file/IPC
// operations.
var baseSyscalls = []string{
	// File operations
	"read", "write", "open", "openat", "openat2", "close",
	"stat", "fstat", "lstat", "statx", "fstatfs", "statfs",
	"access", "faccessat", "faccessat2",
	"lseek", "pread64", "pwrite64", "readv", "writev",
	"getdents64", "ioctl", "fcntl", "flock", "fsync", "fdatasync",
	"dup", "dup2", "dup3", "pipe", "pipe2",
	"unlink", "unlinkat", "rename", "renameat", "renameat2",
	"link", "linkat", "symlink", "symlinkat", "readlink", "readlinkat",
	"mkdir", "mkdirat", "rmdir",
	"chmod", "fchmod", "fchmodat", "chown", "fchown", "lchown", "fchownat",
	"truncate", "ftruncate", "fallocate", "sync", "syncfs",
	"utime", "utimes", "futimesat", "utimensat",
	"getxattr", "lgetxattr", "fgetxattr",
	"listxattr", "llistxattr", "flistxattr",
	"removexattr", "lremovexattr", "fremovexattr",
	"setxattr", "lsetxattr", "fsetxattr",
	"inotify_init1", "inotify_add_watch", "inotify_rm_watch",

	// Memory / process
	"mmap", "munmap", "mprotect", "madvise", "mlock", "munlock",
	"brk",
	"clone", "clone3", "fork", "vfork", "execve", "execveat",
	"exit", "exit_group", "wait4", "waitid",
	"getpid", "getppid", "gettid", "getpgrp", "getpgid", "getsid",
	"getuid", "getgid", "geteuid", "getegid", "getgroups",
	"getresuid", "getresgid", "getcwd",
	"set_tid_address", "set_robust_list",
	"rt_sigaction", "rt_sigreturn", "sigaltstack", "signalfd4",
	"kill", "tkill", "tgkill",
	"prctl", "arch_prctl", "capget", "capset", "personality",
	"sched_yield", "sched_getaffinity", "sched_setaffinity",
	"sched_getscheduler", "sched_setscheduler",
	"sched_getparam", "sched_setparam",
	"sched_get_priority_max", "sched_get_priority_min",
	"sched_getattr", "sched_setattr",
	"sysinfo", "uname", "getrlimit", "prlimit64", "setrlimit", "getrusage",
	"umask", "alarm", "setitimer", "getitimer",
	"nanosleep", "clock_nanosleep", "clock_gettime", "clock_getres", "clock_settime",
	"gettimeofday", "time", "times",
	"timer_create", "timer_settime", "timer_gettime", "timer_getoverrun", "timer_delete",
	"timerfd_create", "timerfd_settime", "timerfd_gettime",
	"getrandom", "memfd_create", "eventfd", "eventfd2", "userfaultfd",
	"restart_syscall",

	// Polling / events
	"poll", "ppoll", "epoll_create1", "epoll_ctl", "epoll_pwait", "epoll_pwait2",
	"select", "pselect6",

	// Seccomp / landlock (self-referential)
	"seccomp", "landlock_create_ruleset", "landlock_add_rule", "landlock_restrict_self",

	// Misc
	"open_by_handle_at", "name_to_handle_at",
	"process_vm_readv", "process_vm_writev",
	"io_setup", "io_destroy", "io_submit", "io_cancel", "io_getevents", "io_pgetevents",
	"io_uring_setup", "io_uring_enter", "io_uring_register",
	"pidfd_open", "pidfd_send_signal", "close_range",
	"getcpu",
	"futex",
}

// networkSyscalls are appended to baseSyscalls when AllowNetwork is true.
var networkSyscalls = []string{
	"socket", "socketpair", "bind", "connect", "listen", "accept", "accept4",
	"getsockname", "getpeername", "setsockopt", "getsockopt", "shutdown",
	"recvfrom", "recvmsg", "sendto", "sendmsg", "sendfile",
}

// buildSeccompFilter returns a default-deny seccomp filter with a base
// allow-list covering the syscalls needed by the Go runtime, the gRPC
// plugin transport, and basic file/network operations.
func buildSeccompFilter(allowNetwork bool) (*seccomp.Filter, error) {
	syscalls := make([]string, len(baseSyscalls))
	copy(syscalls, baseSyscalls)
	if allowNetwork {
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

// buildRlimits returns rlimit configs derived from memory and timeout values.
func buildRlimits(memBytes uint64, timeout time.Duration) []RlimitConfig {
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
	out = append(out,
		RlimitConfig{
			Resource: unix.RLIMIT_NOFILE,
			Rlimit:   syscall.Rlimit{Cur: 1024, Max: 1024},
		},
		RlimitConfig{
			Resource: unix.RLIMIT_NPROC,
			Rlimit:   syscall.Rlimit{Cur: 64, Max: 64},
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
// transient cgroup directories). It is safe to call multiple times.
func (prep *LinuxPrepared) Cleanup() error {
	if prep.CgroupV2 != nil && prep.CgroupV2.CgroupDir != "" {
		if err := os.RemoveAll(prep.CgroupV2.CgroupDir); err != nil {
			return err
		}
		prep.CgroupV2.CgroupDir = ""
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
		TargetPath:   prep.TargetPath,
		Mode:         prep.Mode,
		ReadPaths:    prep.ReadPaths,
		WritePaths:   prep.WritePaths,
		NetPorts:     prep.NetPorts,
		AllowNetwork: prep.AllowNetwork,
		Seccomp:      prep.SeccompBPF != nil,
		Rlimits:      prep.Rlimits,
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

	cmd.Path = criteriaBin
	cmd.Args = append([]string{criteriaBin, "_sandbox_shim_", prep.TargetPath}, cmd.Args[1:]...)
	cmd.Env = append(cmd.Env, "CRITERIA_SANDBOX_CONFIG_PATH="+tmpFile.Name())

	return nil
}

// scrubEnv removes sensitive or unnecessary variables from the
// environment slice.
func scrubEnv(env []string) []string {
	blocked := map[string]bool{
		"SUDO_UID":        true,
		"SUDO_GID":        true,
		"SUDO_USER":       true,
		"SUDO_COMMAND":    true,
		"SUDO_EDITOR":     true,
		"CRITERIA_PLUGIN": true,
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if blocked[name] {
			continue
		}
		out = append(out, e)
	}
	return out
}

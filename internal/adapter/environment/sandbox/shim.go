//go:build linux

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/elastic/go-seccomp-bpf"
	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

const shimEnvVar = "CRITERIA_SANDBOX_CONFIG_PATH"

// ShimConfig is the JSON-serialised data passed from the parent process
// to the shim child via a temp file pointed to by shimEnvVar.
type ShimConfig struct {
	TargetPath   string         `json:"target_path"`
	Mode         string         `json:"mode"`
	ReadPaths    []string       `json:"read_paths"`
	WritePaths   []string       `json:"write_paths"`
	NetPorts     []uint16       `json:"net_ports"`
	AllowNetwork bool           `json:"allow_network"`
	Seccomp      bool           `json:"seccomp"`
	Rlimits      []RlimitConfig `json:"rlimits"`
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

func runShim(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read shim config: %w", err)
	}
	var cfg ShimConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse shim config: %w", err)
	}

	if err := applyShimRestrictions(&cfg); err != nil {
		return err
	}

	// Re-exec the real adapter binary, replacing this process image.
	// Unset the shim env var so the re-execed process does not try to
	// apply restrictions again (infinite recursion).
	os.Unsetenv(shimEnvVar)
	os.Remove(configPath)
	argv := []string{cfg.TargetPath}
	if len(os.Args) > 2 {
		argv = append(argv, os.Args[2:]...)
	}
	err = syscall.Exec(cfg.TargetPath, argv, os.Environ())
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

	// Apply rlimits before anything else so later syscalls are bounded.
	for _, rl := range cfg.Rlimits {
		if err := syscall.Setrlimit(rl.Resource, &rl.Rlimit); err != nil {
			return fmt.Errorf("setrlimit %d: %w", rl.Resource, err)
		}
	}

	// Build and apply Landlock restrictions.
	if len(cfg.ReadPaths)+len(cfg.WritePaths)+len(cfg.NetPorts) > 0 {
		var llcfg landlock.Config
		if len(cfg.NetPorts) > 0 {
			llcfg = landlock.V4
		} else {
			llcfg = landlock.V1
		}
		if cfg.Mode == "permissive" {
			llcfg = llcfg.BestEffort()
		}
		var rules []landlock.Rule
		if len(cfg.ReadPaths) > 0 {
			rules = append(rules, landlock.RODirs(cfg.ReadPaths...).IgnoreIfMissing())
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
	}

	// Build and apply seccomp filter.
	if cfg.Seccomp {
		if err := applySeccompFilter(cfg); err != nil {
			return err
		}
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
	base := []string{
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

	if cfg.AllowNetwork {
		base = append(base, []string{
			"socket", "socketpair", "bind", "connect", "listen", "accept", "accept4",
			"getsockname", "getpeername", "setsockopt", "getsockopt", "shutdown",
			"recvfrom", "recvmsg", "sendto", "sendmsg", "sendfile",
		}...)
	}

	policy := seccomp.Policy{
		DefaultAction: seccomp.ActionErrno,
		Syscalls: []seccomp.SyscallGroup{
			{
				Names:  base,
				Action: seccomp.ActionAllow,
			},
		},
	}

	filter := seccomp.Filter{
		NoNewPrivs: true,
		Policy:     policy,
	}
	return &filter
}

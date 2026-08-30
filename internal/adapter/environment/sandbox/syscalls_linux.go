//go:build linux

package sandbox

import "runtime"

// networkSyscalls are appended to the base allow-list when the policy
// permits external network egress. These names are common across the Linux
// architectures supported by the sandbox.
var networkSyscalls = []string{
	"sendfile",
}

// baseSyscalls is the default-deny seccomp allow-list for the current
// architecture. It is populated from one of the arch-specific lists below.
var baseSyscalls []string

// baseSyscallsByArch maps GOARCH to the default-deny allow-list for that
// architecture. Using arch-specific names avoids assembly failures caused by
// syscall names that do not exist on the running kernel ABI (for example,
// legacy names such as "open" and "stat" are not valid on arm64).
var baseSyscallsByArch = map[string][]string{
	"amd64": baseSyscallsAMD64,
	"arm64": baseSyscallsARM64,
}

func init() {
	if list, ok := baseSyscallsByArch[runtime.GOARCH]; ok {
		baseSyscalls = list
	} else {
		// Unsupported Linux architecture: fall back to the amd64 list so the
		// package still compiles. In permissive mode seccomp preparation will
		// degrade gracefully on unknown names; in strict mode it will fail
		// closed with a clear error.
		baseSyscalls = baseSyscallsAMD64
	}
}

// baseSyscallsAMD64 is the allow-list for x86_64 Linux.
var baseSyscallsAMD64 = []string{
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
	"rt_sigaction", "rt_sigprocmask", "rt_sigreturn", "sigaltstack", "signalfd4",
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

	// Local socket (AF_UNIX) — required by go-plugin control socket even when
	// external network egress is denied.
	"socket", "socketpair", "bind", "connect", "listen", "accept", "accept4",
	"getsockname", "getpeername", "setsockopt", "getsockopt", "shutdown",
	"recvfrom", "recvmsg", "sendto", "sendmsg",

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

// baseSyscallsARM64 is the allow-list for aarch64 Linux.
var baseSyscallsARM64 = []string{
	// File operations
	"read", "write", "openat", "openat2", "close",
	"fstat", "statx", "fstatfs", "statfs",
	"fstatat",
	"faccessat", "faccessat2",
	"lseek", "pread64", "pwrite64", "readv", "writev",
	"getdents64", "ioctl", "fcntl", "flock", "fsync", "fdatasync",
	"dup", "dup3", "pipe2",
	"unlinkat", "renameat", "renameat2",
	"linkat", "symlinkat", "readlinkat",
	"mkdirat",
	"fchmod", "fchmodat", "fchown", "fchownat",
	"truncate", "ftruncate", "fallocate", "sync", "syncfs",
	"utimensat",
	"getxattr", "lgetxattr", "fgetxattr",
	"listxattr", "llistxattr", "flistxattr",
	"removexattr", "lremovexattr", "fremovexattr",
	"setxattr", "lsetxattr", "fsetxattr",
	"inotify_init1", "inotify_add_watch", "inotify_rm_watch",

	// Memory / process
	"mmap", "munmap", "mprotect", "madvise", "mlock", "munlock",
	"brk",
	"clone", "clone3", "execve", "execveat",
	"exit", "exit_group", "wait4", "waitid",
	"getpid", "getppid", "gettid", "getpgid", "getsid",
	"getuid", "getgid", "geteuid", "getegid", "getgroups",
	"getresuid", "getresgid", "getcwd",
	"set_tid_address", "set_robust_list",
	"rt_sigaction", "rt_sigprocmask", "rt_sigreturn", "sigaltstack", "signalfd4",
	"kill", "tkill", "tgkill",
	"prctl", "capget", "capset", "personality",
	"sched_yield", "sched_getaffinity", "sched_setaffinity",
	"sched_getscheduler", "sched_setscheduler",
	"sched_getparam", "sched_setparam",
	"sched_get_priority_max", "sched_get_priority_min",
	"sched_getattr", "sched_setattr",
	"sysinfo", "uname", "getrlimit", "prlimit64", "setrlimit", "getrusage",
	"umask", "setitimer", "getitimer",
	"nanosleep", "clock_nanosleep", "clock_gettime", "clock_getres", "clock_settime",
	"gettimeofday", "times",
	"timer_create", "timer_settime", "timer_gettime", "timer_getoverrun", "timer_delete",
	"timerfd_create", "timerfd_settime", "timerfd_gettime",
	"getrandom", "memfd_create", "eventfd2", "userfaultfd",
	"restart_syscall",

	// Polling / events
	"ppoll", "epoll_create1", "epoll_ctl", "epoll_pwait", "epoll_pwait2",
	"pselect6",

	// Local socket (AF_UNIX) — required by go-plugin control socket even when
	// external network egress is denied.
	"socket", "socketpair", "bind", "connect", "listen", "accept", "accept4",
	"getsockname", "getpeername", "setsockopt", "getsockopt", "shutdown",
	"recvfrom", "recvmsg", "sendto", "sendmsg",

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

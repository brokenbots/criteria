//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Capabilities reports which sandbox primitives are available on the
// current host. Results are cached per process because kernel
// configuration does not change at runtime.
type Capabilities struct {
	UserNamespaces bool
	Landlock       bool
	Seccomp        bool
	Cgroupv2       bool
	Bubblewrap     bool // bwrap on PATH
}

var (
	probeOnce     sync.Once
	probeResult   Capabilities
	probeTestHook func() Capabilities // set by tests to override real probe
)

// ResetProbeCache is exported for tests that need to re-evaluate
// capabilities after mutating test hooks.
func ResetProbeCache() {
	probeOnce = sync.Once{}
}

// Probe checks the host kernel for sandbox primitive support. Cached
// per process. Results affect what's logged at session open in
// permissive mode and what's accepted in strict mode.
func Probe() Capabilities {
	probeOnce.Do(func() {
		if probeTestHook != nil {
			probeResult = probeTestHook()
		} else {
			probeResult = doProbe()
		}
	})
	return probeResult
}

func doProbe() Capabilities {
	c := Capabilities{
		UserNamespaces: probeUserNamespaces(),
		Landlock:       probeLandlock(),
		Seccomp:        probeSeccomp(),
		Cgroupv2:       probeCgroupv2(),
		Bubblewrap:     probeBubblewrap(),
	}
	return c
}

func probeUserNamespaces() bool {
	// The canonical check is /proc/sys/kernel/unprivileged_userns_clone
	// (Debian/Ubuntu specific). If absent, assume namespaces are
	// available because most modern kernels enable them by default.
	b, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err == nil {
		val, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return val == 1
	}
	// Fallback: try to unshare a user namespace. This is lightweight.
	err = unix.Unshare(unix.CLONE_NEWUSER)
	if err == nil {
		// We just created a user namespace in the current process.
		// It is harmless; the process remains in the original
		// namespaces for all other purposes.
		return true
	}
	return false
}

func probeLandlock() bool {
	// LandlockCreateRuleset with LANDLOCK_CREATE_RULESET_VERSION returns
	// the supported ABI version (>= 1) or an error.
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno == 0 && v >= 1 {
		return true
	}
	return false
}

func probeSeccomp() bool {
	// Check /proc/self/status for Seccomp: mode (2 = filter mode).
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Seccomp:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				mode, _ := strconv.Atoi(fields[1])
				return mode >= 1 // seccomp is available; filter mode is 2
			}
		}
	}
	// Fallback: attempt to install a trivial no-op filter. If it
	// succeeds, seccomp is supported.
	return probeSeccompViaFilter()
}

func probeSeccompViaFilter() bool {
	// Fork a child to test because applying a seccomp filter to the
	// current process could break subsequent syscalls.
	pid, _, errno := unix.Syscall(unix.SYS_FORK, 0, 0, 0)
	if errno != 0 {
		return false
	}
	if pid == 0 {
		// Child: try a minimal seccomp filter.
		// We use prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, ...)
		// with a trivial BPF program that allows everything.
		bpf := []unix.SockFilter{
			{Code: unix.BPF_RET + unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		}
		prog := unix.SockFprog{Len: uint16(len(bpf)), Filter: &bpf[0]}
		_, _, e1 := unix.Syscall(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)))
		if e1 == 0 {
			unix.Exit(0)
		}
		unix.Exit(1)
	}
	var ws unix.WaitStatus
	_, err := unix.Wait4(int(pid), &ws, 0, nil)
	if err != nil {
		return false
	}
	return ws.ExitStatus() == 0
}

func probeCgroupv2() bool {
	// cgroup v2 unified hierarchy has a single mount with filesystem
	// type "cgroup2".
	var st unix.Statfs_t
	err := unix.Statfs("/sys/fs/cgroup", &st)
	if err != nil {
		return false
	}
	return st.Type == unix.CGROUP2_SUPER_MAGIC
}

func probeBubblewrap() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}

// Missing returns a human-readable list of missing capabilities.
func (c Capabilities) Missing() []string {
	var out []string
	if !c.UserNamespaces {
		out = append(out, "user_namespaces")
	}
	if !c.Landlock {
		out = append(out, "landlock")
	}
	if !c.Seccomp {
		out = append(out, "seccomp")
	}
	if !c.Cgroupv2 {
		out = append(out, "cgroupv2")
	}
	return out
}

// String returns a compact description for logging.
func (c Capabilities) String() string {
	parts := []string{
		fmt.Sprintf("userns=%v", c.UserNamespaces),
		fmt.Sprintf("landlock=%v", c.Landlock),
		fmt.Sprintf("seccomp=%v", c.Seccomp),
		fmt.Sprintf("cgroupv2=%v", c.Cgroupv2),
		fmt.Sprintf("bwrap=%v", c.Bubblewrap),
	}
	return strings.Join(parts, " ")
}

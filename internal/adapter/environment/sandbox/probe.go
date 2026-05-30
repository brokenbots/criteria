//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

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
	// (Debian/Ubuntu specific). If absent, test in a child process so we
	// never pollute the Go thread pool with a thread in a different
	// user namespace.
	b, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err == nil {
		val, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return val == 1
	}
	// Fallback: fork a child into a new user namespace and see if it
	// survives.  The child runs /bin/true (should exist everywhere).
	attr := &os.ProcAttr{
		Sys: &syscall.SysProcAttr{
			Cloneflags:  unix.CLONE_NEWUSER,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		},
	}
	proc, err := os.StartProcess("/bin/true", []string{"true"}, attr)
	if err != nil {
		return false
	}
	_, _ = proc.Wait()
	return true
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
	// Check /proc/self/status for Seccomp: mode.
	// Mode 0 = disabled (available), mode 2 = filter active.
	// We return true only for 0 or 2 because mode 1 (strict) does not
	// allow installing a BPF filter.
	b, err := os.ReadFile("/proc/self/status")
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Seccomp:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					mode, _ := strconv.Atoi(fields[1])
					return mode == 0 || mode == 2
				}
			}
		}
	}
	// Fallback: prctl(PR_GET_SECCOMP) returns the current mode.
	// If seccomp is unsupported, prctl returns an error.
	mode, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	if err != nil {
		return false
	}
	return mode == 0 || mode == 2
}

// probeSeccompViaFilter was removed.  Raw SYS_FORK is unsafe in Go (only
// the calling thread is duplicated; runtime state becomes inconsistent).
// We now rely on /proc/self/status and prctl(PR_GET_SECCOMP) instead.

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

//go:build darwin

package sandbox

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Capabilities reports which sandbox primitives are available on the
// current macOS host. Results are cached per process.
type Capabilities struct {
	SandboxExec bool
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

// Probe checks the host for sandbox-exec availability. Cached per process.
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
	return Capabilities{
		SandboxExec: probeSandboxExec(),
	}
}

func probeSandboxExec() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

// Missing returns a human-readable list of missing capabilities.
func (c Capabilities) Missing() []string {
	var out []string
	if !c.SandboxExec {
		out = append(out, "sandbox_exec")
	}
	return out
}

// String returns a compact description for logging.
func (c Capabilities) String() string {
	parts := []string{
		fmt.Sprintf("sandbox_exec=%v", c.SandboxExec),
	}
	return strings.Join(parts, " ")
}

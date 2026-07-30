//go:build linux

package engine

import (
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

// strictMissingSandboxCaps returns a capability set that lacks landlock,
// which is enough to make a strict-mode sandbox policy fail Prepare on Linux.
func strictMissingSandboxCaps() sandbox.Capabilities {
	return sandbox.Capabilities{UserNamespaces: true, Landlock: false, Seccomp: true, Cgroupv2: true}
}

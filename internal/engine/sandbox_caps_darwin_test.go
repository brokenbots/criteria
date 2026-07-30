//go:build darwin

package engine

import (
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

// strictMissingSandboxCaps returns a capability set that lacks sandbox-exec,
// which makes a strict-mode sandbox policy fail Prepare on macOS.
func strictMissingSandboxCaps() sandbox.Capabilities {
	return sandbox.Capabilities{SandboxExec: false}
}

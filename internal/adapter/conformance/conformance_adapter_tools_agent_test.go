package conformance_test

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

// TestAdapterToolsAgentCallerMatrix drives the CRI-183 adapter-tools
// agent-caller matrix (deny mid-conversation, unknown_tool typed failure
// mid-conversation, two concurrent in-flight calls from one agent turn)
// through the real engine. Host-side and unconditional: it is not gated on
// any adapter's declared capabilities, so it always runs in CI.
func TestAdapterToolsAgentCallerMatrix(t *testing.T) {
	conformance.RunAdapterToolsAgentCallerMatrix(t)
}

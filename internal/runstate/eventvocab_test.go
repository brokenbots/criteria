package runstate

import (
	"testing"
)

func TestSeamEventType_AuditLossRow(t *testing.T) {
	if got := seamEventType("AuditLoss"); got != "auditLoss" {
		t.Fatalf("seam type for AuditLoss = %q, want auditLoss", got)
	}
}

// TestSeamEventType_TableIsLowerCamel asserts the shared producer-to-seam
// vocabulary maps every registered discriminator onto a lowerCamelCase seam
// type without inventing strings.
func TestSeamEventType_TableIsLowerCamel(t *testing.T) {
	for payloadType, seamType := range payloadTypeToSeamType {
		if got := seamEventType(payloadType); got != seamType {
			t.Errorf("seamEventType(%q) = %q, want the registered %q", payloadType, got, seamType)
		}
		if seamType == "" || seamType[0] > 'z' || seamType[0] < 'a' {
			t.Errorf("seam type %q for %q must be lowerCamelCase", seamType, payloadType)
		}
	}
}

// TestTerminalSeamEventTypes excludes auditLoss: the loss verdict rides the
// run record as an informational event and must not terminate a viewer's
// stream walk.
func TestTerminalSeamEventTypes_AuditLossNotTerminal(t *testing.T) {
	if TerminalSeamEventTypes["auditLoss"] {
		t.Error("auditLoss is informational; it must not be a terminal seam event type")
	}
	if !TerminalSeamEventTypes["runCompleted"] || !TerminalSeamEventTypes["runFailed"] {
		t.Error("the terminal vocabulary lost one of runCompleted/runFailed")
	}
}

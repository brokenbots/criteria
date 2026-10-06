package localresume

import (
	"testing"

	"github.com/brokenbots/criteria/internal/tunables"
)

// TestNewAppliesTunablesDefaults pins the zero-value contract of Options
// construction: unset polling and timeout budgets are seeded from the
// tunables registry's defaults, so the resumer never runs with a zero poll
// interval or an unlimited file wait.
func TestNewAppliesTunablesDefaults(t *testing.T) {
	r := New(ModeFile, Options{StateDir: t.TempDir()})
	if r.(*resumer).opts.FilePollingInterval != tunables.DefaultFilePollingInterval {
		t.Errorf("FilePollingInterval = %v, want tunables default %v",
			r.(*resumer).opts.FilePollingInterval, tunables.DefaultFilePollingInterval)
	}
	if r.(*resumer).opts.FileTimeout != tunables.DefaultLocalApprovalFileTimeout {
		t.Errorf("FileTimeout = %v, want tunables default %v",
			r.(*resumer).opts.FileTimeout, tunables.DefaultLocalApprovalFileTimeout)
	}
}

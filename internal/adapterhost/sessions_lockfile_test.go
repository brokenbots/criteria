package adapterhost

import (
	"testing"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestSessionManager_GetSetLockfile verifies the GetLockfile / SetLockfile
// accessors on SessionManager.
func TestSessionManager_GetSetLockfile(t *testing.T) {
	t.Run("initial_nil", func(t *testing.T) {
		sm := NewSessionManager(NewLoader())
		if got := sm.GetLockfile(); got != nil {
			t.Errorf("new SessionManager: GetLockfile() = %v, want nil", got)
		}
	})

	t.Run("set_and_get", func(t *testing.T) {
		sm := NewSessionManager(NewLoader())
		lf := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "shell", Name: "main", ResolvedDigest: "abc123"},
			},
		}
		sm.SetLockfile(lf)
		got := sm.GetLockfile()
		if got != lf {
			t.Errorf("GetLockfile() returned %p, want %p (same pointer)", got, lf)
		}
	})

	t.Run("set_nil", func(t *testing.T) {
		sm := NewSessionManager(NewLoader())
		lf := &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "noop", Name: "default", ResolvedDigest: "def456"},
			},
		}
		sm.SetLockfile(lf)
		sm.SetLockfile(nil)
		if got := sm.GetLockfile(); got != nil {
			t.Errorf("after SetLockfile(nil): GetLockfile() = %v, want nil", got)
		}
	})
}

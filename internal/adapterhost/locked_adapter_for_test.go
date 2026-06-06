package adapterhost

import (
	"testing"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestLockedAdapterForMatchesTypeAndName guards against the regression where the
// digest lookup matched on adapter Type only (first-match-wins), which made two
// instances of the same type at different versions indistinguishable.
func TestLockedAdapterForMatchesTypeAndName(t *testing.T) {
	m := NewSessionManager(NewLoader())
	m.SetLockfile(&lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "shell", Name: "a", ResolvedDigest: "sha256:aaa"},
			{Type: "shell", Name: "b", ResolvedDigest: "sha256:bbb"},
		},
	})

	if got := m.lockedAdapterFor("shell.a"); got == nil || got.ResolvedDigest != "sha256:aaa" {
		t.Fatalf("shell.a resolved to %+v, want digest sha256:aaa", got)
	}
	if got := m.lockedAdapterFor("shell.b"); got == nil || got.ResolvedDigest != "sha256:bbb" {
		t.Fatalf("shell.b resolved to %+v, want digest sha256:bbb", got)
	}
	if got := m.lockedAdapterFor("shell.missing"); got != nil {
		t.Fatalf("shell.missing resolved to %+v, want nil", got)
	}
	if got := m.lockedAdapterFor("noinstanceid"); got != nil {
		t.Fatalf("malformed id resolved to %+v, want nil", got)
	}
}

func TestLockedAdapterForNilLockfile(t *testing.T) {
	m := NewSessionManager(NewLoader())
	if got := m.lockedAdapterFor("shell.a"); got != nil {
		t.Fatalf("got %+v, want nil with no lockfile", got)
	}
}

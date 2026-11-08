package engine

// KB-25: an adapter dial presenting a valid digest for an unregistered scope
// (the post-rotation survivor shape) must be recoverable: the engine-side
// registrar validates the presented token against the run's persisted rotated
// token files and re-registers a match so the dialing pod's re-handshake is
// accepted instead of looping on accept-fail rejections. Deliberately
// released instances and unauthenticated guesses stay rejected.

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

func TestSplitScopeKey(t *testing.T) {
	name, inst, ok := splitScopeKey("run_pr_reviewer_loop/2f0e9d4c-0000-4000-8000-000000000001")
	if !ok || name != "run_pr_reviewer_loop" || inst != "2f0e9d4c-0000-4000-8000-000000000001" {
		t.Fatalf("splitScopeKey = %q, %q, %v", name, inst, ok)
	}
	if _, _, ok := splitScopeKey("noslash"); ok {
		t.Error("scope without an instance half must not split")
	}
	if _, _, ok := splitScopeKey("name/"); ok {
		t.Error("empty instance half must not split")
	}
	if _, _, ok := splitScopeKey("/instance"); ok {
		t.Error("empty scope name half must not split")
	}
}

func TestSurvivingScopeToken_MatchesAndDeclines(t *testing.T) {
	dataDir := t.TempDir()
	scopeName, instanceID, adapterType, token := "run_pr_reviewer_loop", "2f0e9d4c-0000-4000-8000-000000000001", "noop", "survivor-token"
	if _, err := writeRotatedToken(dataDir, scopeName, instanceID, adapterType, token); err != nil {
		t.Fatalf("writeRotatedToken: %v", err)
	}

	got, err := survivingScopeToken(dataDir, scopeName, instanceID, adapterType, token)
	if err != nil || got != token {
		t.Fatalf("survivingScopeToken = %q, %v; want %q, nil", got, err, token)
	}

	if _, err := survivingScopeToken(dataDir, scopeName, instanceID, adapterType, "guessed-token"); err == nil {
		t.Fatal("a non-matching presented token must be declined")
	}
	if _, err := survivingScopeToken(dataDir, scopeName, instanceID, "other-adapter", token); err == nil {
		t.Fatal("a token file for a different adapter type must not match")
	}
	if _, err := survivingScopeToken(dataDir, scopeName, "00000000-0000-0000-8000-ffffffffffff", adapterType, token); err == nil {
		t.Fatal("an instance without a surviving token file must be declined")
	}

	// A deliberately released instance must never be re-registered (CRI-115).
	releaseScopeInstance(dataDir, scopeName, "noop.default", instanceID, adapterType)
	if _, err := survivingScopeToken(dataDir, scopeName, instanceID, adapterType, token); err == nil {
		t.Fatal("a released (tombstoned) instance must not be re-registered")
	}

	// A live claim by another adapter instance must also block re-use.
	other := "11111111-1111-4111-8111-111111111111"
	if _, err := writeRotatedToken(dataDir, scopeName, other, adapterType, "other-token"); err != nil {
		t.Fatalf("writeRotatedToken: %v", err)
	}
	if err := writeCurrentScopeInstance(dataDir, scopeName, "noop.default", currentScopeInstanceRecord{
		ScopeInstanceID: other, AdapterKey: adapterType,
	}); err != nil {
		t.Fatalf("writeCurrentScopeInstance: %v", err)
	}
	if _, err := survivingScopeToken(dataDir, scopeName, other, adapterType, "other-token"); err == nil {
		t.Fatal("an actively claimed instance must not be re-registered")
	}
}

func TestDialScopeRegistrar_RecoversUnregisteredScopeDial(t *testing.T) {
	// End-to-end shape of the KB-25 reproduction: the run's shim has no
	// accept token for the dialed scope (no persisted record; only a
	// surviving rotated token file), the pod re-dials its old scope key, and
	// the registrar re-registers the survivor so the dial is accepted.
	dataDir := t.TempDir()
	scopeName := "run_pr_reviewer_loop"
	instanceID := "2f0e9d4c-0000-4000-8000-000000000001"
	scope := scopeName + "/" + instanceID
	token := "survivor-token"
	if _, err := writeRotatedToken(dataDir, scopeName, instanceID, "noop", token); err != nil {
		t.Fatalf("writeRotatedToken: %v", err)
	}

	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	envKey := "remote.env"
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetEnvHostForEnv(envKey, adapterhost.NewShimHost(shim))
	consults := &registrarConsultCounter{}
	consults.next = &dialScopeRegistrar{dataDir: dataDir, envKey: envKey, sessions: sessions}

	// Trigger recovery through the registrar
	if err := consults.RegisterScopeOnDial("noop", scope, token); err != nil {
		t.Fatalf("RegisterScopeOnDial: %v", err)
	}

	if got := consults.count.Load(); got != 1 {
		t.Errorf("registrar consultations = %d, want exactly 1", got)
	}
	if registeredToken, ok := shim.registered[scope]; !ok || registeredToken != token {
		t.Errorf("registered token = %q, want %q", registeredToken, token)
	}
}

// registrarConsultCounter wraps a remote.ScopeRegistrar and counts
// consultations so a test can assert the seam was exercised exactly once.
type registrarConsultCounter struct {
	next  remote.ScopeRegistrar
	count atomic.Int64
}

func (c *registrarConsultCounter) RegisterScopeOnDial(adapterType, scope, presentedToken string) error {
	c.count.Add(1)
	return c.next.RegisterScopeOnDial(adapterType, scope, presentedToken)
}

func TestDialScopeRegistrar_DeclinesReleasedInstanceEndToEnd(t *testing.T) {
	// CRI-115 guard: a deliberately released (tombstoned) instance's token
	// file still exists for forensics, but the registrar must not resurrect
	// it — the shim keeps rejecting the dial.
	dataDir := t.TempDir()
	scopeName := "run_pr_reviewer_loop"
	instanceID := "2f0e9d4c-0000-4000-8000-000000000001"
	scope := scopeName + "/" + instanceID
	token := "survivor-token"
	if _, err := writeRotatedToken(dataDir, scopeName, instanceID, "noop", token); err != nil {
		t.Fatalf("writeRotatedToken: %v", err)
	}
	releaseScopeInstance(dataDir, scopeName, "noop.default", instanceID, "noop")

	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	envKey := "remote.env"
	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetEnvHostForEnv(envKey, adapterhost.NewShimHost(shim))
	registrar := &dialScopeRegistrar{dataDir: dataDir, envKey: envKey, sessions: sessions}

	err := registrar.RegisterScopeOnDial("noop", scope, token)
	if err == nil {
		t.Fatal("a released instance must not be re-registered")
	}
	if !strings.Contains(err.Error(), "claimed or released") {
		t.Errorf("decline must explain the released-instance reason, got: %v", err)
	}
}

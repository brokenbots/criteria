package adapterhost

// sessions_share_test.go — a parallel subworkflow iteration receives its own
// SessionManager for local session isolation but must still reach the remote
// environment the root SM started: it shares the parent's bound envhost.Host
// entries (the phone-home shim folded behind its host of record) WITHOUT
// transferring ownership. The child can register per-scope tokens on the
// shared shim, but tearing the child down must not stop the shared listener
// the root run and sibling iterations still use.

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/internal/envhost"
)

func TestShareEnvHostsFrom_SharesShimWithoutOwnership(t *testing.T) {
	shim := &cri293Shim{name: "scan", addr: "10.0.0.1:7778", scopes: map[string]string{}}

	parent := NewSessionManager(&cri269Loader{})
	parent.SetEnvHostForEnv("remote.scan", NewShimHost(shim))

	child := NewSessionManager(&cri269Loader{})
	child.ShareEnvHostsFrom(parent)

	// The child reaches the shared shim through the same host fold...
	host, ok := child.EnvHost("remote.scan")
	if !ok {
		t.Fatal("child EnvHost(remote.scan) = nil, want the shared host fold")
	}
	if sh, isShim := host.(shimHost); !isShim || sh.Shim() != shim {
		t.Fatalf("child EnvHost(remote.scan) = %T, want the parent's shim fold", host)
	}
	// ...and can register a per-scope token on it (would error "no remote
	// shim registered" without the share).
	if _, err := host.Control(context.Background(), &envhost.ControlRequest{
		Scope: scopeRefForKey("scan/inst-0"),
		Kind:  envhost.ControlRegisterScope,
		Token: "tok",
	}); err != nil {
		t.Fatalf("child register scope through the shared host: %v", err)
	}
	if got := shim.scopeToken("scan/inst-0"); got != "tok" {
		t.Errorf("shared shim scope token = %q, want tok", got)
	}

	// Tearing the child down must NOT stop the shared shim.
	if err := child.Shutdown(context.Background()); err != nil {
		t.Fatalf("child Shutdown: %v", err)
	}
	if n := shim.stopCount(); n != 0 {
		t.Fatalf("shared shim stopped %d times on child Shutdown, want 0", n)
	}

	// The owning parent stops it (Shim.Stop is idempotent in production, so
	// a repeated stop is harmless; assert it is stopped at least once, in
	// contrast to the borrowing child above).
	if err := parent.Shutdown(context.Background()); err != nil {
		t.Fatalf("parent Shutdown: %v", err)
	}
	if n := shim.stopCount(); n < 1 {
		t.Fatalf("shim stopped %d times after owner Shutdown, want >= 1", n)
	}
}

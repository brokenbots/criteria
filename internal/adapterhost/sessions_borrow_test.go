package adapterhost

// sessions_borrow_test.go — a parallel subworkflow iteration receives its own
// SessionManager for local session isolation but must still reach the remote
// environment the root SM started. BorrowRemoteProvisioningFrom shares the
// parent's phone-home shims and VerifyGraph adapter caches with the child
// WITHOUT transferring ownership: the child can register per-scope tokens on the
// shared shim, but tearing the child down must not stop the shared listener the
// root run and sibling iterations still use.

import (
	"context"
	"testing"
)

func TestBorrowRemoteProvisioningFrom_SharesShimWithoutOwnership(t *testing.T) {
	shim := &cri293Shim{name: "scan", addr: "10.0.0.1:7778", scopes: map[string]string{}}

	parent := NewSessionManager(&cri269Loader{})
	parent.SetRemoteShimForEnv("remote.scan", shim)
	// Simulate the VerifyGraph-populated caches isRemoteAdapter consults.
	parent.mu.Lock()
	parent.graphAdapters = map[string]graphAdapterRef{"shell.w": {}}
	parent.adapterDirs = map[string]string{"shell.w": "/wf"}
	parent.deferredRemoteAdapters = map[string]struct{}{"shell.w": {}}
	parent.mu.Unlock()

	child := NewSessionManager(&cri269Loader{})
	child.BorrowRemoteProvisioningFrom(parent)

	// The child reaches the shared shim...
	if got := child.RemoteShimForEnv("remote.scan"); got != shim {
		t.Fatalf("child RemoteShimForEnv = %v, want the borrowed shim", got)
	}
	// ...can register a per-scope token on it (would error "no remote shim
	// registered" without the borrow)...
	if err := child.RegisterRemoteScopeForEnv("remote.scan", "scan/inst-0", "tok"); err != nil {
		t.Fatalf("child RegisterRemoteScopeForEnv: %v", err)
	}
	// ...and inherits the verify caches so isRemoteAdapter sees the adapter.
	child.mu.Lock()
	_, haveGraph := child.graphAdapters["shell.w"]
	_, haveDir := child.adapterDirs["shell.w"]
	_, haveDeferred := child.deferredRemoteAdapters["shell.w"]
	child.mu.Unlock()
	if !haveGraph || !haveDir || !haveDeferred {
		t.Fatalf("child missing borrowed verify caches: graph=%v dir=%v deferred=%v", haveGraph, haveDir, haveDeferred)
	}

	// Tearing the child down must NOT stop the borrowed shim.
	if err := child.Shutdown(context.Background()); err != nil {
		t.Fatalf("child Shutdown: %v", err)
	}
	if n := shim.stopCount(); n != 0 {
		t.Fatalf("borrowed shim stopped %d times on child Shutdown, want 0", n)
	}

	// The owning parent stops it (Shim.Stop is idempotent in production, so the
	// default-alias double-append in takeForShutdown is harmless; assert it is
	// stopped at least once, in contrast to the borrowing child above).
	if err := parent.Shutdown(context.Background()); err != nil {
		t.Fatalf("parent Shutdown: %v", err)
	}
	if n := shim.stopCount(); n < 1 {
		t.Fatalf("shim stopped %d times after owner Shutdown, want >= 1", n)
	}
}

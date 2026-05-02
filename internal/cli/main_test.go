package cli

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// IgnoreCurrent captures goroutines started before tests run (e.g. race
	// detector, test infrastructure) so they do not trigger false positives.
	//
	// The HTTP/2 transport goroutines (clientConnReadLoop, serverConn.serve,
	// serverConn.readFrames) come from the httptest.Server used in fake-server
	// tests. They exit when the server closes connections, but may still be
	// shutting down during goleak's check window. We filter them to avoid
	// false positives; real transport goroutine leaks would manifest in the
	// internal/transport/server package tests instead.
	//
	// IgnoreAnyFunction is required here instead of IgnoreTopFunction: when a
	// goroutine is blocked in IO wait, goleak reports internal/poll.runtime_pollWait
	// as the top frame, not the h2 function itself. IgnoreAnyFunction with these
	// specific internal h2 function names is narrow in practice because they only
	// appear in HTTP/2 connection-management goroutines, not in user code.
	goleak.VerifyTestMain(m,
		goleak.IgnoreCurrent(),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientConnReadLoop).run"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*serverConn).serve"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*serverConn).readFrames"),
	)
}

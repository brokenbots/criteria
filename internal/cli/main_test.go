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
	goleak.VerifyTestMain(m,
		goleak.IgnoreCurrent(),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*clientConnReadLoop).run"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*serverConn).serve"),
		goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*serverConn).readFrames"),
	)
}
